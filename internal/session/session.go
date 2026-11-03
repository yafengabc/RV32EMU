package session

// session 是模拟器的一次「上电会话」：SoC + 控制命令 + 事件输出。
// CLI 的 -ctl JSON Lines 协议（ctl.go）和 GUI（gui/emu.go）都跑在它上面，
// 因此 GUI 不再需要启动 rv32emu.exe 子进程。
//
// 线程模型：SoC 只在 loop goroutine 里被访问；外部通过方法投递命令，
// 事件通过 Options.OnEvent 回调（同样在 loop goroutine 上）送出。

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"rv32emu/internal/perip"
	"rv32emu/internal/soc"
)

const (
	// Batch 是自由运行时一次批量执行的指令数。
	Batch = 1000
	// StatePeriod 是自由运行时状态快照的最小间隔（GUI 刷新率上限）。
	StatePeriod = 50 * time.Millisecond
)

// ErrNotRunning 表示会话尚未启动或已经结束。
var ErrNotRunning = errors.New("模拟器未运行")

// 外设快照直接复用 perip 里的类型，GUI 侧再做类型别名。
type (
	GPIOState   = perip.GPIOState
	TG0WDTState = perip.WDTState
	RTCWDTState = perip.RTCWDTState
	SWDState    = perip.SWDState
)

type UARTStats struct {
	TxBytes uint64 `json:"txBytes"`
	RxBytes uint64 `json:"rxBytes"`
	RxFifo  int    `json:"rxFifo"`
}

type WDTSnapshot struct {
	TG0 TG0WDTState `json:"tg0"`
	RTC RTCWDTState `json:"rtc"`
	SWD SWDState    `json:"swd"`
}

// Event 是协议事件的并集：不同 event 只填自己的字段。
type Event struct {
	Event string `json:"event"`

	// hello
	Protocol    string `json:"protocol"`
	Arch        string `json:"arch"`
	Pins        int    `json:"pins"`
	ADCChannels int    `json:"adcChannels"`

	// state
	Running bool        `json:"running"`
	Halted  bool        `json:"halted"`
	HaltMsg string      `json:"haltMsg"`
	PC      uint32      `json:"pc"`
	Inst    uint64      `json:"inst"`
	GPIO    GPIOState   `json:"gpio"`
	ADC     []uint32    `json:"adc"`
	UART    UARTStats   `json:"uart"`
	WDT     WDTSnapshot `json:"wdt"`

	// uart
	Data string `json:"data"`

	// reset
	Reason string `json:"reason"`

	// halt / warning
	Message string `json:"message"`
}

// Command 是 JSON Lines 协议的命令行形状（ctl.go 直接解析它）。
type Command struct {
	Cmd     string  `json:"cmd"`
	N       uint64  `json:"n"`
	Pin     *int    `json:"pin"`
	Level   *bool   `json:"level"`
	Channel *int    `json:"channel"`
	Value   *uint32 `json:"value"`
	Data    string  `json:"data"`
	Target  string  `json:"target"`
	Action  string  `json:"action"`
}

// Options 是构建会话所需的全部参数。
type Options struct {
	ELFPath  string
	BinPath  string
	Entry    uint32
	NoPMP    bool
	TickDiv  uint32
	RXRate   int    // 每个放行点释放的控制台输入字节数（0 = 不放行）
	RXPeriod uint64 // 每多少条指令放行一次
	OnTrap   func(cause, epc, tval uint32)
	OnEvent  func(Event)
}

type Session struct {
	opts Options
	cfig soc.Config
	uart *perip.UART
	ubuf *uartBuffer
	s    *soc.SoC

	flash []byte
	inst  uint64
	rxAcc uint64
	run   bool // 自由运行中（受 run/pause 控制）

	cmds chan Command
	quit chan struct{}
	done chan struct{}

	mu      sync.Mutex
	started bool
	closed  bool

	alive atomic.Bool
}

// uartBuffer 收集 UART TX 字节，由主循环按批合并成一条 uart 事件。
// 不逐字节发事件：内核打印一行会产生几十次 Write，逐字节会让协议通道
// 变成瓶颈，终端也只需要成批的文本。
type uartBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *uartBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	return len(p), nil
}

func (b *uartBuffer) drain() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return nil
	}
	out := b.buf
	b.buf = nil
	return out
}

// New 构造会话并装好第一台机器（不启动 goroutine）。
// flash 由调用方持有：复位时从这里重新装载。
func New(flash []byte, opts Options) (*Session, error) {
	s := &Session{
		opts: opts,
		uart: perip.NewUART(),
		cmds: make(chan Command, 64),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	s.ubuf = &uartBuffer{}
	s.uart.SetSink(s.ubuf)
	s.flash = flash
	s.cfig = soc.Config{
		ELFPath: opts.ELFPath,
		BinPath: opts.BinPath,
		Entry:   opts.Entry,
		NoPMP:   opts.NoPMP,
		TickDiv: opts.TickDiv,
		OnTrap:  opts.OnTrap,
	}
	m, err := soc.New(flash, s.uart, s.cfig)
	if err != nil {
		return nil, err
	}
	s.s = m
	return s, nil
}

// Start 启动模拟循环。事件从 Options.OnEvent 回调送出。
func (s *Session) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("模拟器已在运行")
	}
	if s.closed {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.started = true
	s.alive.Store(true)
	s.mu.Unlock()

	go s.loop()
	return nil
}

// Done 在模拟循环退出后关闭。
func (s *Session) Done() <-chan struct{} { return s.done }

// Running 报告会话是否仍然存活。
func (s *Session) Running() bool { return s.alive.Load() }

// Close 结束会话（线程安全、可重复调用）。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.started {
		close(s.quit)
	} else {
		// 没启动过就不会有人关闭 done；这里补上，保证 Done 可用。
		close(s.done)
	}
	return nil
}

func (s *Session) emit(ev Event) {
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(ev)
	}
}

func (s *Session) send(c Command) error {
	s.mu.Lock()
	ok := s.started && !s.closed
	s.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	select {
	case s.cmds <- c:
		return nil
	case <-s.done:
		return ErrNotRunning
	}
}

func (s *Session) loop() {
	defer func() {
		s.alive.Store(false)
		close(s.done)
	}()

	s.emit(Event{
		Event:       "hello",
		Protocol:    "jsonl",
		Arch:        "esp32c3",
		Pins:        perip.GPIOPinCount,
		ADCChannels: perip.ADCChannelCount,
	})
	s.emitState()

	stateTick := time.NewTicker(StatePeriod)
	defer stateTick.Stop()

	for {
		select {
		case <-s.quit:
			s.drainCommands()
			s.flushUART()
			return
		default:
		}

		// 先吃掉所有已到达的命令，让 pause/step 能尽快生效。
		for {
			select {
			case c := <-s.cmds:
				s.handle(c)
				continue
			default:
			}
			break
		}

		if s.run {
			reason := s.advance(Batch)
			s.flushUART()
			switch reason {
			case "":
			case "halt":
				s.run = false
				s.emitHalt()
			default:
				s.reboot(reason)
			}
			select {
			case <-stateTick.C:
				s.flushUART()
				s.emitState()
			default:
			}
			continue
		}

		s.flushUART()
		select {
		case <-s.quit:
			// 退出前把已排队的命令执行完，保证 -ctl 的 quit 之前的
			// step/state 不会因为进程结束而被吞掉。
			s.drainCommands()
			s.flushUART()
			return
		case c := <-s.cmds:
			s.handle(c)
		case <-stateTick.C:
			s.emitState()
		}
	}
}

// drainCommands 非阻塞地执行所有已到达的命令。
func (s *Session) drainCommands() {
	for {
		select {
		case c := <-s.cmds:
			s.handle(c)
		default:
			return
		}
	}
}

// advance 执行一批指令并推进外设。返回非空字符串表示这批之后需要
// 停机或整机复位（halt/wdt_tg0/wdt_rtc/sw_sys_rst）。
func (s *Session) advance(budget uint64) string {
	if s.s.CPU.Halted() {
		return "halt"
	}
	ran := s.s.CPU.RunHot(budget)
	if ran == 0 {
		return "halt"
	}
	s.s.Timer.TickN(ran)
	s.s.SysTimer.TickN(ran)
	s.s.RTC.Tick(ran)
	s.inst += ran
	s.feedUART(ran)

	switch {
	case s.s.Timer.WDTResetPending():
		return "wdt_tg0"
	case s.s.RTC.WDTResetPending():
		return "wdt_rtc"
	case s.s.RTC.ResetPending():
		return "sw_sys_rst"
	case s.s.CPU.Halted():
		return "halt"
	}
	return ""
}

// feedUART 按 tick 分批把主机注入的字节搬进 RX FIFO，语义与 CLI 模式一致。
func (s *Session) feedUART(ran uint64) {
	if s.opts.RXRate <= 0 {
		return
	}
	p := s.opts.RXPeriod
	if p == 0 {
		p = 1
	}
	s.rxAcc += ran
	for s.rxAcc >= p {
		s.rxAcc -= p
		s.s.UART.RxFeed(s.opts.RXRate)
	}
}

func (s *Session) reboot(reason string) {
	s.flash = s.s.FlashSnapshot()
	s.uart.Reset()
	m, err := soc.New(s.flash, s.uart, s.cfig)
	if err != nil {
		s.run = false
		s.emit(Event{Event: "warning", Message: "复位失败: " + err.Error()})
		return
	}
	s.s = m
	s.inst = 0
	s.rxAcc = 0
	s.run = false
	s.emit(Event{Event: "reset", Reason: reason})
	s.emitState()
}

func (s *Session) flushUART() {
	if data := s.ubuf.drain(); len(data) > 0 {
		s.emit(Event{Event: "uart", Data: string(data)})
	}
}

func (s *Session) emitHalt() {
	s.emit(Event{Event: "halt", Message: s.s.CPU.HaltMsg()})
	s.emitState()
}

func (s *Session) emitState() {
	gpio := s.s.GPIO.State()
	s.emit(Event{
		Event:   "state",
		Running: s.run,
		Halted:  s.s.CPU.Halted(),
		HaltMsg: s.s.CPU.HaltMsg(),
		PC:      s.s.CPU.PC(),
		Inst:    s.inst,
		GPIO: GPIOState{
			Out: gpio.Out, Enable: gpio.Enable, In: gpio.In, Ext: gpio.Ext, Status: gpio.Status,
		},
		ADC: s.s.ADC.Channels(),
		UART: UARTStats{
			TxBytes: s.s.UART.TXBytes(),
			RxBytes: s.s.UART.RXBytes(),
			RxFifo:  s.s.UART.RXFIFOLevel(),
		},
		WDT: WDTSnapshot{
			TG0: s.s.Timer.WDTState(),
			RTC: s.s.RTC.WDTState(),
			SWD: s.s.RTC.SWDState(),
		},
	})
}

func (s *Session) handle(c Command) {
	switch c.Cmd {
	case "run":
		s.run = true
	case "pause":
		s.run = false
		s.emitState()
	case "step":
		s.step(c.N)
	case "reset":
		s.reboot("request")
	case "state":
		s.emitState()
	case "gpio_input", "gpio":
		s.setGPIO(c)
	case "adc":
		s.setADC(c)
	case "uart_write":
		s.s.UART.PushRX([]byte(c.Data))
	case "wdt":
		s.setWDT(c)
	default:
		s.emit(Event{Event: "warning", Message: "unknown cmd: " + c.Cmd})
	}
}

func (s *Session) step(n uint64) {
	s.run = false
	if n == 0 {
		n = 1
	}
	for n > 0 {
		b := uint64(Batch)
		if n < b {
			b = n
		}
		reason := s.advance(b)
		s.flushUART()
		switch reason {
		case "":
			n -= b
		case "halt":
			s.emitHalt()
			return
		default:
			s.reboot(reason)
			return
		}
	}
	s.emitState()
}

func (s *Session) setGPIO(c Command) {
	if c.Pin == nil || c.Level == nil {
		s.emit(Event{Event: "warning", Message: "gpio_input needs pin and level"})
		return
	}
	if !s.s.GPIO.SetInput(*c.Pin, *c.Level) {
		s.emit(Event{Event: "warning", Message: fmt.Sprintf("gpio pin out of range: %d", *c.Pin)})
		return
	}
	s.emitState()
}

func (s *Session) setADC(c Command) {
	if c.Channel == nil || c.Value == nil {
		s.emit(Event{Event: "warning", Message: "adc needs channel and value"})
		return
	}
	if !s.s.ADC.SetChannel(*c.Channel, *c.Value) {
		s.emit(Event{Event: "warning", Message: fmt.Sprintf("adc channel out of range: %d", *c.Channel)})
		return
	}
	s.emitState()
}

func (s *Session) setWDT(c Command) {
	switch c.Target {
	case "", "tg0":
		switch c.Action {
		case "enable":
			s.s.Timer.SetWDTEnabled(true)
		case "disable":
			s.s.Timer.SetWDTEnabled(false)
		case "feed":
			s.s.Timer.FeedWDT()
		case "hold":
			if c.Value == nil {
				s.emit(Event{Event: "warning", Message: "wdt hold needs value"})
				return
			}
			s.s.Timer.SetWDTHold(*c.Value)
		default:
			s.emit(Event{Event: "warning", Message: "wdt action must be enable/disable/feed/hold"})
			return
		}
	case "rtc":
		switch c.Action {
		case "enable":
			s.s.RTC.SetWDTEnabled(true)
		case "disable":
			s.s.RTC.SetWDTEnabled(false)
		case "feed":
			s.s.RTC.FeedWDT()
		case "hold":
			if c.Value == nil {
				s.emit(Event{Event: "warning", Message: "wdt hold needs value"})
				return
			}
			s.s.RTC.SetWDTHold(*c.Value)
		default:
			s.emit(Event{Event: "warning", Message: "wdt action must be enable/disable/feed/hold"})
			return
		}
	case "swd":
		switch c.Action {
		case "enable":
			s.s.RTC.SetSWDDisabled(false)
		case "disable":
			s.s.RTC.SetSWDDisabled(true)
		case "feed":
			s.s.RTC.FeedSWD()
		default:
			s.emit(Event{Event: "warning", Message: "swd action must be enable/disable/feed"})
			return
		}
	default:
		s.emit(Event{Event: "warning", Message: "wdt target must be tg0/rtc/swd"})
		return
	}
	s.emitState()
}

// ---- 供 GUI/ctl 调用的便捷方法（全部线程安全）----

func (s *Session) Run() error { return s.send(Command{Cmd: "run"}) }

func (s *Session) Pause() error { return s.send(Command{Cmd: "pause"}) }

func (s *Session) Step(n uint64) error {
	if n == 0 {
		n = 1
	}
	return s.send(Command{Cmd: "step", N: n})
}

func (s *Session) Reset() error { return s.send(Command{Cmd: "reset"}) }

func (s *Session) RequestState() error { return s.send(Command{Cmd: "state"}) }

func (s *Session) SetGPIO(pin int, level bool) error {
	return s.send(Command{Cmd: "gpio_input", Pin: &pin, Level: &level})
}

func (s *Session) SetADC(ch int, value uint32) error {
	return s.send(Command{Cmd: "adc", Channel: &ch, Value: &value})
}

func (s *Session) UARTWrite(data string) error {
	return s.send(Command{Cmd: "uart_write", Data: data})
}

func (s *Session) WDT(target, action string, value *uint32) error {
	return s.send(Command{Cmd: "wdt", Target: target, Action: action, Value: value})
}

// Exec 投递一条原始命令（JSON 适配层用）。
func (s *Session) Exec(c Command) error { return s.send(c) }
