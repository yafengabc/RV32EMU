package main

// 无头逻辑测试：不构建真实窗口，只验证与 Gio 无关的状态逻辑。

import (
	"strings"
	"testing"
)

func TestBoardToggleAndUpdate(t *testing.T) {
	var (
		gotPin   = -1
		gotLevel bool
	)
	b := newBoardView(func(pin int, level bool) {
		gotPin, gotLevel = pin, level
	})

	b.toggle(7)
	if gotPin != 7 || !gotLevel {
		t.Fatalf("first toggle: pin=%d level=%v", gotPin, gotLevel)
	}
	if b.ext&(1<<7) == 0 || !b.pins[7].level {
		t.Fatal("pin 7 should be injected high")
	}

	// 模拟器回报 ext 位已置 1，下一次点击应变成低电平。
	b.Update(Event{GPIO: GPIOState{Ext: 1 << 7}})
	b.toggle(7)
	if gotPin != 7 || gotLevel {
		t.Fatalf("second toggle: pin=%d level=%v", gotPin, gotLevel)
	}
}

func TestBoardUpdate(t *testing.T) {
	b := newBoardView(nil)
	b.Update(Event{
		Running: true,
		PC:      0x40000000,
		Inst:    42,
		GPIO:    GPIOState{In: 1 << 5, Enable: 1 << 5, Ext: 0},
		ADC:     []uint32{1234},
		UART:    UARTStats{TxBytes: 10, RxBytes: 2},
	})

	if !b.pins[5].level || !b.pins[5].driven {
		t.Fatal("pin 5 should be high and driven")
	}
	if b.pins[4].level {
		t.Fatal("pin 4 should stay low")
	}
	if b.pc != 0x40000000 || b.inst != 42 {
		t.Fatalf("pc=%#x inst=%d", b.pc, b.inst)
	}
	if b.status != "RUNNING" {
		t.Fatalf("status = %q", b.status)
	}
}

func TestControlPanelSync(t *testing.T) {
	c := newControlPanel(nil, nil, nil, nil, nil)
	c.Update(Event{WDT: WDTSnapshot{
		TG0: TG0WDTState{Enabled: true, Stage0: 1000, Prescale: 1},
		RTC: RTCWDTState{Enabled: false, Stage0: 200000},
		SWD: SWDState{Disabled: true, Feeds: 3, ResetFlag: true},
	}})

	if !c.tg0En.Value || c.rtcEn.Value || !c.swdDis.Value {
		t.Fatalf("sync: tg0=%v rtc=%v swd=%v",
			c.tg0En.Value, c.rtcEn.Value, c.swdDis.Value)
	}
	if !strings.Contains(c.tg0Info, "1000") || !strings.Contains(c.swdInfo, "3") {
		t.Fatalf("info: tg0=%q swd=%q", c.tg0Info, c.swdInfo)
	}

	// 复位归零只改本地滑块，不发命令。
	c.adc[2].f.Value = 0.5
	c.adc[2].dragging = true
	c.resetADC()
	if c.adc[2].f.Value != 0 || c.adc[2].value != 0 || c.adc[2].dragging {
		t.Fatalf("resetADC: value=%v raw=%d dragging=%v",
			c.adc[2].f.Value, c.adc[2].value, c.adc[2].dragging)
	}
}

func TestTerminalAppend(t *testing.T) {
	tp := newTerminalPanel(nil)
	tp.append("boot ok\n")
	tp.append("a\nb")
	if len(tp.lines) != 2 || tp.cur != "b" {
		t.Fatalf("lines = %#v cur = %q", tp.lines, tp.cur)
	}
	if tp.lines[0] != "boot ok" || tp.lines[1] != "a" {
		t.Fatalf("lines = %#v", tp.lines)
	}

	// 后半行由后续批次补齐，不应拆成新的行。
	tp.append("cd\n")
	if len(tp.lines) != 3 || tp.cur != "" || tp.lines[2] != "bcd" {
		t.Fatalf("lines = %#v cur = %q", tp.lines, tp.cur)
	}
	// 显示层是只读编辑器，内容应与累积的行一致。
	if got := tp.out.Text(); got != "boot ok\na\nbcd\n" {
		t.Fatalf("out = %q", got)
	}

	// CRLF 与裸 CR：\r 覆盖当前行，不应产生空行。
	tp.append("CRLF\r\n")
	if len(tp.lines) != 4 || tp.lines[3] != "CRLF" || tp.cur != "" {
		t.Fatalf("crlf: lines = %#v cur = %q", tp.lines, tp.cur)
	}
	tp.append("old\rdone\n")
	if len(tp.lines) != 5 || tp.lines[4] != "done" {
		t.Fatalf("cr overwrite: lines = %#v", tp.lines)
	}

	// 超出上限时丢弃最旧的行。
	for i := 0; i < terminalMaxLines+10; i++ {
		tp.append("x\n")
	}
	if len(tp.lines) != terminalMaxLines {
		t.Fatalf("len = %d", len(tp.lines))
	}
	if tp.lines[len(tp.lines)-1] != "x" {
		t.Fatalf("last = %q", tp.lines[len(tp.lines)-1])
	}
}

func TestTerminalAppendLine(t *testing.T) {
	tp := newTerminalPanel(nil)
	tp.append("TinyO")
	tp.appendLine("[gui] 已启动模拟器")
	if len(tp.lines) != 2 || tp.lines[0] != "TinyO" || tp.lines[1] != "[gui] 已启动模拟器" {
		t.Fatalf("lines = %#v cur = %q", tp.lines, tp.cur)
	}
	// 后续固件输出另起一行，不会再粘到内部消息上。
	tp.append("S\r\n")
	if len(tp.lines) != 3 || tp.lines[2] != "S" {
		t.Fatalf("lines = %#v", tp.lines)
	}
	if got := tp.out.Text(); got != "TinyO\n[gui] 已启动模拟器\nS\n" {
		t.Fatalf("out = %q", got)
	}
}
