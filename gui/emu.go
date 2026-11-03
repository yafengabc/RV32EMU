package main

// 进程内模拟器封装：GUI 直接驱动 internal/session，不再启动 rv32emu.exe。
// 事件回调在模拟循环的 goroutine 上触发，UI 层负责切回帧循环。

import (
	"errors"
	"sync"

	"rv32emu/internal/session"
	"rv32emu/internal/soc"
)

// 事件类型直接复用会话层定义，UI 无需再解析 JSON。
type (
	Event       = session.Event
	GPIOState   = session.GPIOState
	UARTStats   = session.UARTStats
	TG0WDTState = session.TG0WDTState
	RTCWDTState = session.RTCWDTState
	SWDState    = session.SWDState
	WDTSnapshot = session.WDTSnapshot
)

// 控制台输入放行节奏与 CLI 默认一致：每 10000 条指令放行 16 字节。
const (
	guiRXRate   = 16
	guiRXPeriod = 10000
)

var errNotRunning = errors.New("模拟器未运行")

// Emu 管理一个进程内模拟会话。Start 会先停掉旧会话。
type Emu struct {
	mu sync.Mutex
	s  *session.Session

	OnEvent func(Event)
	OnExit  func(error)
}

// Start 用 ELF 与 ramdisk 内容启动一次新会话。flashOffset 决定 ramdisk
// 在 flash 里的位置。
func (e *Emu) Start(elfPath string, ramdisk []byte, flashOffset uint32) error {
	e.Stop()

	flash, err := soc.NewFlash(ramdisk, flashOffset)
	if err != nil {
		return err
	}
	s, err := session.New(flash, session.Options{
		ELFPath:  elfPath,
		RXRate:   guiRXRate,
		RXPeriod: guiRXPeriod,
		OnEvent: func(ev session.Event) {
			if e.OnEvent != nil {
				e.OnEvent(ev)
			}
		},
	})
	if err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}

	e.mu.Lock()
	e.s = s
	e.mu.Unlock()

	go func() {
		<-s.Done()
		e.mu.Lock()
		current := e.s == s
		if current {
			e.s = nil
		}
		e.mu.Unlock()
		if current && e.OnExit != nil {
			e.OnExit(nil)
		}
	}()
	return nil
}

func (e *Emu) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.s != nil
}

func (e *Emu) Stop() {
	e.mu.Lock()
	s := e.s
	e.s = nil
	e.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

func (e *Emu) current() (*session.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.s == nil {
		return nil, errNotRunning
	}
	return e.s, nil
}

func (e *Emu) Run() error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.Run()
}

func (e *Emu) Pause() error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.Pause()
}

func (e *Emu) Step(n uint64) error {
	if n == 0 {
		n = 1
	}
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.Step(n)
}

func (e *Emu) Reset() error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.Reset()
}

func (e *Emu) SetGPIO(pin int, level bool) error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.SetGPIO(pin, level)
}

func (e *Emu) UARTWrite(data string) error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.UARTWrite(data)
}

func (e *Emu) SetADC(ch int, value uint32) error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.SetADC(ch, value)
}

func (e *Emu) WDT(target, action string, value *uint32) error {
	s, err := e.current()
	if err != nil {
		return err
	}
	return s.WDT(target, action, value)
}
