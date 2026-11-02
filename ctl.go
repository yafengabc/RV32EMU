package main

// -ctl 模式：把模拟器当作一个子进程，通过 stdin/stdout 上的 JSON Lines
// 协议驱动。stdout 只走协议事件（一行一个 JSON 对象），人类日志走 stderr。
//
// 命令（stdin，一行一个 JSON）：
//
//	{"cmd":"run"}                          开始/继续自由运行
//	{"cmd":"pause"}                        暂停
//	{"cmd":"step","n":1000}                执行 n 条指令后暂停（默认 1）
//	{"cmd":"reset"}                        整机复位
//	{"cmd":"state"}                        请求一份状态快照
//	{"cmd":"gpio_input","pin":3,"level":1} 注入引脚外部电平
//	{"cmd":"adc","channel":0,"value":2048} 设置 ADC 通道原始码值
//	{"cmd":"uart_write","data":"ls\r"}     向 UART RX 注入字节
//	{"cmd":"wdt","target":"tg0","action":"enable"}   enable/disable/feed/hold
//	{"cmd":"wdt","target":"rtc","action":"hold","value":200000}
//	{"cmd":"wdt","target":"swd","action":"disable"}
//	{"cmd":"quit"}                         退出子进程
//
// 事件（stdout）：
//
//	{"event":"hello",...}                  能力声明，第一条
//	{"event":"state",...}                  周期/命令后快照
//	{"event":"uart","data":"..."}          UART TX 输出（按批合并）
//	{"event":"reset","reason":"..."}       复位（request/wdt_tg0/wdt_rtc/sw_sys_rst）
//	{"event":"halt","message":"..."}       CPU 停机
//	{"event":"warning","message":"..."}    协议/参数错误

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"rv32emu/internal/perip"
)

const ctlBatch = 1000

// ctlStatePeriod 是自由运行时状态快照的最小间隔（GUI 刷新率上限）。
const ctlStatePeriod = 50 * time.Millisecond

type ctlCommand struct {
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

// ctlOut 串行化 stdout 写入（状态事件、UART 事件都可能来自不同路径）。
type ctlOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *ctlOut) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctl: marshal: %v\n", err)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.w.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "ctl: write: %v\n", err)
	}
}

// uartBuffer 收集 UART TX 字节，由主循环按批合并成一条 uart 事件。
// 不逐字节发事件：内核打印一行会产生几十次 Write，逐字节会让协议通道
// 变成瓶颈，GUI 终端也只需要成批的文本。
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

type ctlRunner struct {
	out     *ctlOut
	flash   []byte
	uart    *perip.UART
	uartBuf *uartBuffer
	m       *machine
	cmds    <-chan ctlCommand

	running bool
	inst    uint64 // 本次上电以来执行的指令数
	rxAcc   uint64
}

func runCtl(w io.Writer, flash []byte) {
	out := &ctlOut{w: w}
	uart := perip.NewUART()
	ub := &uartBuffer{}
	uart.SetSink(ub)

	cmds := make(chan ctlCommand, 64)
	go readCtlCommands(cmds, out)

	r := &ctlRunner{
		out:     out,
		flash:   flash,
		uart:    uart,
		uartBuf: ub,
		m:       newMachine(flash, uart),
		cmds:    cmds,
	}
	r.loop()
}

func readCtlCommands(ch chan<- ctlCommand, out *ctlOut) {
	defer close(ch)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c ctlCommand
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			out.emit(map[string]any{"event": "warning", "message": "bad command: " + err.Error()})
			continue
		}
		ch <- c
	}
}

func (r *ctlRunner) loop() {
	r.emit(map[string]any{
		"event":       "hello",
		"protocol":    "jsonl",
		"arch":        "esp32c3",
		"pins":        perip.GPIOPinCount,
		"adcChannels": perip.ADCChannelCount,
	})
	r.emitState()

	stateTick := time.NewTicker(ctlStatePeriod)
	defer stateTick.Stop()

	for {
		// 先吃掉所有已到达的命令，让 pause/step 能尽快生效；
		// 命令通道关闭表示前端退出（stdin EOF），子进程也随之结束。
		for {
			select {
			case c, ok := <-r.cmds:
				if !ok {
					return
				}
				if !r.handle(c) {
					return
				}
				continue
			default:
			}
			break
		}

		if r.running {
			reason := r.advance(ctlBatch)
			r.flushUART()
			switch reason {
			case "":
			case "halt":
				r.running = false
				r.emitHalt()
			default:
				r.reboot(reason)
			}
			select {
			case <-stateTick.C:
				r.flushUART()
				r.emitState()
			default:
			}
			continue
		}

		r.flushUART()
		select {
		case c, ok := <-r.cmds:
			if !ok {
				return
			}
			if !r.handle(c) {
				return
			}
		case <-stateTick.C:
			r.emitState()
		}
	}
}

// advance 执行一批指令并推进外设。返回非空字符串表示这批之后需要
// 停机或整机复位（halt/wdt_tg0/wdt_rtc/sw_sys_rst）。
func (r *ctlRunner) advance(budget uint64) string {
	if r.m.cpu.Halted() {
		return "halt"
	}
	ran := r.m.cpu.RunHot(budget)
	if ran == 0 {
		return "halt"
	}
	r.m.timer.TickN(ran)
	r.m.rtc.Tick(ran)
	r.inst += ran
	r.feedUART(ran)

	switch {
	case r.m.timer.WDTResetPending():
		return "wdt_tg0"
	case r.m.rtc.WDTResetPending():
		return "wdt_rtc"
	case r.m.rtc.ResetPending():
		return "sw_sys_rst"
	case r.m.cpu.Halted():
		return "halt"
	}
	return ""
}

// feedUART 按 tick 分批把主机注入的字节搬进 RX FIFO，语义与 CLI 模式一致。
func (r *ctlRunner) feedUART(ran uint64) {
	if *rxRate <= 0 {
		return
	}
	p := *rxPeriod
	if p == 0 {
		p = 1
	}
	r.rxAcc += ran
	for r.rxAcc >= p {
		r.rxAcc -= p
		r.m.uart.RxFeed(*rxRate)
	}
}

func (r *ctlRunner) reboot(reason string) {
	r.flash = r.m.flashSnapshot()
	uart := r.m.uart
	uart.Reset()
	r.m = newMachine(r.flash, uart)
	r.inst = 0
	r.rxAcc = 0
	r.running = false
	r.emit(map[string]any{"event": "reset", "reason": reason})
	r.emitState()
}

func (r *ctlRunner) flushUART() {
	if data := r.uartBuf.drain(); len(data) > 0 {
		r.emit(map[string]any{"event": "uart", "data": string(data)})
	}
}

func (r *ctlRunner) emit(v any) { r.out.emit(v) }

func (r *ctlRunner) emitHalt() {
	r.emit(map[string]any{"event": "halt", "message": r.m.cpu.HaltMsg()})
	r.emitState()
}

func (r *ctlRunner) warn(msg string) {
	r.emit(map[string]any{"event": "warning", "message": msg})
}

func (r *ctlRunner) emitState() {
	r.emit(map[string]any{
		"event":   "state",
		"running": r.running,
		"halted":  r.m.cpu.Halted(),
		"haltMsg": r.m.cpu.HaltMsg(),
		"pc":      r.m.cpu.PC(),
		"inst":    r.inst,
		"gpio":    r.m.gpio.State(),
		"adc":     r.m.adc.Channels(),
		"uart": map[string]any{
			"txBytes": r.m.uart.TXBytes(),
			"rxBytes": r.m.uart.RXBytes(),
			"rxFifo":  r.m.uart.RXFIFOLevel(),
		},
		"wdt": map[string]any{
			"tg0": r.m.timer.WDTState(),
			"rtc": r.m.rtc.WDTState(),
			"swd": r.m.rtc.SWDState(),
		},
	})
}

// handle 返回 false 表示收到 quit，调用方应结束进程。
func (r *ctlRunner) handle(c ctlCommand) bool {
	switch c.Cmd {
	case "run":
		r.running = true
	case "pause":
		r.running = false
		r.emitState()
	case "step":
		r.step(c.N)
	case "reset":
		r.reboot("request")
	case "state":
		r.emitState()
	case "gpio_input", "gpio":
		r.setGPIO(c)
	case "adc":
		r.setADC(c)
	case "uart_write":
		r.uart.PushRX([]byte(c.Data))
	case "wdt":
		r.setWDT(c)
	case "quit", "exit":
		return false
	default:
		r.warn("unknown cmd: " + c.Cmd)
	}
	return true
}

func (r *ctlRunner) step(n uint64) {
	r.running = false
	if n == 0 {
		n = 1
	}
	for n > 0 {
		b := uint64(ctlBatch)
		if n < b {
			b = n
		}
		reason := r.advance(b)
		r.flushUART()
		switch reason {
		case "":
			n -= b
		case "halt":
			r.emitHalt()
			return
		default:
			r.reboot(reason)
			return
		}
	}
	r.emitState()
}

func (r *ctlRunner) setGPIO(c ctlCommand) {
	if c.Pin == nil || c.Level == nil {
		r.warn("gpio_input needs pin and level")
		return
	}
	if !r.m.gpio.SetInput(*c.Pin, *c.Level) {
		r.warn(fmt.Sprintf("gpio pin out of range: %d", *c.Pin))
		return
	}
	r.emitState()
}

func (r *ctlRunner) setADC(c ctlCommand) {
	if c.Channel == nil || c.Value == nil {
		r.warn("adc needs channel and value")
		return
	}
	if !r.m.adc.SetChannel(*c.Channel, *c.Value) {
		r.warn(fmt.Sprintf("adc channel out of range: %d", *c.Channel))
		return
	}
	r.emitState()
}

func (r *ctlRunner) setWDT(c ctlCommand) {
	switch c.Target {
	case "", "tg0":
		switch c.Action {
		case "enable":
			r.m.timer.SetWDTEnabled(true)
		case "disable":
			r.m.timer.SetWDTEnabled(false)
		case "feed":
			r.m.timer.FeedWDT()
		case "hold":
			if c.Value == nil {
				r.warn("wdt hold needs value")
				return
			}
			r.m.timer.SetWDTHold(*c.Value)
		default:
			r.warn("wdt action must be enable/disable/feed/hold")
			return
		}
	case "rtc":
		switch c.Action {
		case "enable":
			r.m.rtc.SetWDTEnabled(true)
		case "disable":
			r.m.rtc.SetWDTEnabled(false)
		case "feed":
			r.m.rtc.FeedWDT()
		case "hold":
			if c.Value == nil {
				r.warn("wdt hold needs value")
				return
			}
			r.m.rtc.SetWDTHold(*c.Value)
		default:
			r.warn("wdt action must be enable/disable/feed/hold")
			return
		}
	case "swd":
		switch c.Action {
		case "enable":
			r.m.rtc.SetSWDDisabled(false)
		case "disable":
			r.m.rtc.SetSWDDisabled(true)
		case "feed":
			r.m.rtc.FeedSWD()
		default:
			r.warn("swd action must be enable/disable/feed")
			return
		}
	default:
		r.warn("wdt target must be tg0/rtc/swd")
		return
	}
	r.emitState()
}
