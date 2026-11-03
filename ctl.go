package main

// -ctl 模式：把模拟器当作一个子进程，通过 stdin/stdout 上的 JSON Lines
// 协议驱动。stdout 只走协议事件（一行一个 JSON 对象），人类日志走 stderr。
// 具体执行由 internal/session 完成，这里只做 JSON 与 struct 的转换，
// 因此 GUI 可以复用同一套会话逻辑而不再启动子进程。
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

	"rv32emu/internal/session"
)

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

func runCtl(w io.Writer, flash []byte) {
	out := &ctlOut{w: w}
	s, err := session.New(flash, session.Options{
		ELFPath:  *elfPath,
		BinPath:  *binPath,
		Entry:    uint32(*entry),
		NoPMP:    *noPMP,
		TickDiv:  uint32(*tickDiv),
		RXRate:   *rxRate,
		RXPeriod: *rxPeriod,
		OnEvent:  func(ev session.Event) { out.emit(ev) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctl init: %v\n", err)
		return
	}
	if err := s.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ctl start: %v\n", err)
		return
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c session.Command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			out.emit(session.Event{Event: "warning", Message: "bad command: " + err.Error()})
			continue
		}
		if c.Cmd == "quit" || c.Cmd == "exit" {
			break
		}
		if err := s.Exec(c); err != nil {
			out.emit(session.Event{Event: "warning", Message: err.Error()})
			break
		}
	}
	s.Close()
	<-s.Done()
}
