package main

// RV32EMU 图形界面（Gio 版）：窗口自绘的紧凑浅色仪表盘。
//
// 模拟器以进程内会话（internal/session）运行，不再启动 rv32emu.exe：
// 单个 rv32gui.exe 就能加载 ELF/ramdisk、驱动 CPU 并显示 UART 输出。
//
// 线程模型：会话回调都在模拟循环 goroutine 上执行，只把事件/日志追加到
// 队列并调用 Window.Invalidate；所有 UI 状态变更都发生在帧循环里。

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type ui struct {
	win *app.Window
	emu *Emu
	th  *material.Theme

	elfED, ramED, offED, stepED widget.Editor

	startBtn widget.Clickable
	runBtn   widget.Clickable
	pauseBtn widget.Clickable
	stepBtn  widget.Clickable
	resetBtn widget.Clickable
	stopBtn  widget.Clickable
	cfgBtn   widget.Clickable

	tabTerm widget.Clickable
	tabADC  widget.Clickable
	tabWDT  widget.Clickable

	cfgOpen bool
	tab     int

	statusText string
	statusCol  color.NRGBA

	board *boardView
	term  *terminalPanel
	ctrl  *controlPanel

	mu     sync.Mutex
	events []Event
	logs   []string
	exited bool
}

func main() {
	w := new(app.Window)
	w.Option(
		app.Title("RV32EMU · ESP32-C3"),
		app.Size(unit.Dp(1200), unit.Dp(760)),
		app.MinSize(unit.Dp(880), unit.Dp(560)),
	)
	u := newUI(w)

	go func() {
		if err := u.loop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func newUI(w *app.Window) *ui {
	u := &ui{
		win:        w,
		emu:        &Emu{},
		th:         newTheme(),
		statusText: "未运行",
		statusCol:  colMuted,
	}

	for _, ed := range []*widget.Editor{&u.elfED, &u.ramED, &u.offED, &u.stepED} {
		ed.SingleLine = true
	}
	const tinyOSBuild = `D:\projects\TinyOS\build`
	u.elfED.SetText(filepath.Join(tinyOSBuild, "kernel-esp32c3.elf"))
	u.ramED.SetText(filepath.Join(tinyOSBuild, "ramdisk-esp32c3.bin"))
	u.offED.SetText("0x80000")
	u.stepED.SetText("1000")

	u.term = newTerminalPanel(func(s string) {
		if err := u.emu.UARTWrite(s); err != nil {
			u.term.appendLine("[gui] " + err.Error())
		}
	})
	u.board = newBoardView(func(pin int, level bool) {
		if err := u.emu.SetGPIO(pin, level); err != nil {
			u.term.appendLine("[gui] " + err.Error())
		}
	})
	u.ctrl = newControlPanel(
		u.th,
		func(ch int, value uint32) error { return u.emu.SetADC(ch, value) },
		func(target, action string, value *uint32) error {
			return u.emu.WDT(target, action, value)
		},
		func(err error) { u.term.appendLine("[gui] " + err.Error()) },
		func() { u.win.Invalidate() },
	)

	u.emu.OnEvent = func(ev Event) {
		u.queue(func() { u.events = append(u.events, ev) })
	}
	u.emu.OnExit = func(err error) {
		u.queue(func() {
			if err != nil {
				u.logs = append(u.logs, "[gui] 模拟器退出: "+err.Error())
			} else {
				u.logs = append(u.logs, "[gui] 模拟器已退出")
			}
			u.exited = true
		})
	}
	return u
}

// queue 从任意 goroutine 追加队列并请求重绘。
func (u *ui) queue(f func()) {
	u.mu.Lock()
	f()
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *ui) loop() error {
	var ops op.Ops
	for {
		e := u.win.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			u.emu.Stop()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			u.drain()
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

// drain 把后台队列搬进 UI 状态，只在帧循环里调用。
func (u *ui) drain() {
	u.mu.Lock()
	evs := u.events
	u.events = nil
	logs := u.logs
	u.logs = nil
	exited := u.exited
	u.exited = false
	u.mu.Unlock()

	for _, l := range logs {
		u.term.appendLine(l)
	}
	for _, ev := range evs {
		u.apply(ev)
	}
	if exited {
		u.afterExit()
	}
}

func (u *ui) apply(ev Event) {
	switch ev.Event {
	case "hello":
		u.term.appendLine(fmt.Sprintf("[gui] 已连接 %s · %d 引脚 · %d 路 ADC",
			ev.Arch, ev.Pins, ev.ADCChannels))
	case "state":
		u.board.Update(ev)
		u.ctrl.Update(ev)
		u.term.apply(ev)
		mode, col := "暂停", colMuted
		switch {
		case ev.Halted:
			mode, col = "停机", colAlert
		case ev.Running:
			mode, col = "运行", colHigh
		}
		u.statusText = fmt.Sprintf("%s · PC 0x%08X · inst %d", mode, ev.PC, ev.Inst)
		u.statusCol = col
	case "uart":
		u.term.append(ev.Data)
	case "reset":
		u.term.appendLine("[reset] " + ev.Reason)
		u.ctrl.resetADC()
	case "halt":
		u.term.appendLine("[halt] " + ev.Message)
	case "warning":
		u.term.appendLine("[warn] " + ev.Message)
	}
}

func (u *ui) afterExit() {
	if u.emu.Running() {
		return
	}
	u.board.setOffline()
	u.statusText, u.statusCol = "未运行", colMuted
}

func (u *ui) layout(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, colBg, clip.Rect{Max: gtx.Constraints.Max}.Op())

	return layout.Inset{Top: 10, Bottom: 8, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{layout.Rigid(u.toolbar)}
		if u.cfgOpen {
			children = append(children, layout.Rigid(u.configPanel))
		}
		children = append(children,
			layout.Flexed(1, u.mainArea),
			layout.Rigid(u.statusBar),
		)
		return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(8)}.Layout(gtx, children...)
	})
}

func (u *ui) toolbar(gtx layout.Context) layout.Dimensions {
	on := u.emu.Running()

	// Gio v0.10 的 Clickable.Layout 会消费点击事件，Clickable.Clicked 则负责
	// 消费并返回，二者谁先调用谁拿到事件。因此必须先检查 Clicked 再布局，
	// 否则点击会在布局阶段被悄悄丢掉。
	if u.startBtn.Clicked(gtx) && !on {
		u.start()
	}
	if u.runBtn.Clicked(gtx) && on {
		u.doCmd(u.emu.Run)
	}
	if u.pauseBtn.Clicked(gtx) && on {
		u.doCmd(u.emu.Pause)
	}
	if u.stepBtn.Clicked(gtx) && on {
		u.step()
	}
	if u.resetBtn.Clicked(gtx) && on {
		u.doCmd(u.emu.Reset)
	}
	if u.stopBtn.Clicked(gtx) && on {
		u.emu.Stop()
		u.afterExit()
	}
	if u.cfgBtn.Clicked(gtx) {
		u.cfgOpen = !u.cfgOpen
		u.win.Invalidate()
	}

	return layout.Flex{Spacing: layout.SpaceBetween, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(6)}.Layout(gtx,
				layout.Rigid(u.pill(&u.startBtn, "启动", true, !on)),
				layout.Rigid(u.pill(&u.runBtn, "运行", false, on)),
				layout.Rigid(u.pill(&u.pauseBtn, "暂停", false, on)),
				layout.Rigid(u.pill(&u.stepBtn, "单步", false, on)),
				layout.Rigid(u.pill(&u.resetBtn, "复位", false, on)),
				layout.Rigid(u.pill(&u.stopBtn, "停止", false, on)),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(8), Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return label(u.th, unit.Sp(11), u.statusText, u.statusCol).Layout(gtx)
				}),
				layout.Rigid(u.pill(&u.cfgBtn, "配置", false, true)),
			)
		}),
	)
}

// pill 生成一个紧凑胶囊按钮。
func (u *ui) pill(clk *widget.Clickable, txt string, primary, enabled bool) layout.Widget {
	bg, fg := colPanelHi, colText
	if primary {
		bg, fg = colAccent, colPanel
	}
	return func(gtx layout.Context) layout.Dimensions {
		return drawPill(gtx, u.th, clk, txt, bg, fg, enabled)
	}
}

func (u *ui) configPanel(gtx layout.Context) layout.Dimensions {
	return drawCardSized(gtx, colPanel, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(6)}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.field(gtx, "ELF", &u.elfED)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.field(gtx, "Ramdisk", &u.ramED)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(10)}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.fixedField(gtx, "偏移", &u.offED, 200)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.fixedField(gtx, "单步", &u.stepED, 160)
						}),
					)
				}),
			)
		})
	})
}

// field 画一行「标签 + 输入框」；输入框宽度撑满剩余空间。
func (u *ui) field(gtx layout.Context, name string, ed *widget.Editor) layout.Dimensions {
	h := gtx.Dp(28)
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(6), Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(gtx.Dp(60), h))
			l := label(u.th, unit.Sp(12), name, colMuted)
			l.MaxLines = 1
			l.Truncator = ""
			return l.Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, h)
			if size.X < gtx.Dp(40) {
				size.X = gtx.Dp(40)
			}
			editorField(gtx, u.th, ed, "", size, false)
			return layout.Dimensions{Size: size}
		}),
	)
}

// fixedField 是宽度固定的 field，用于「单步」这类短输入。
func (u *ui) fixedField(gtx layout.Context, name string, ed *widget.Editor, width unit.Dp) layout.Dimensions {
	gtx.Constraints.Min.X = 0
	gtx.Constraints.Max.X = gtx.Dp(width)
	return u.field(gtx, name, ed)
}

// mainArea 左侧板图固定宽度，窗口变宽时只伸展右侧终端/输入区。
func (u *ui) mainArea(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(8)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Dp(330)
			gtx.Constraints.Max.X = gtx.Dp(330)
			return u.board.Layout(gtx, u.th)
		}),
		layout.Flexed(1, u.sidePanel),
	)
}

func (u *ui) sidePanel(gtx layout.Context) layout.Dimensions {
	drawCard(gtx, colPanel)
	return layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(8)}.Layout(gtx,
			layout.Rigid(u.tabBar),
			layout.Flexed(1, u.tabContent),
		)
	})
}

func (u *ui) tabBar(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(6)}.Layout(gtx,
		layout.Rigid(u.tabPill(&u.tabTerm, "UART 终端", 0)),
		layout.Rigid(u.tabPill(&u.tabADC, "ADC", 1)),
		layout.Rigid(u.tabPill(&u.tabWDT, "看门狗", 2)),
	)
}

func (u *ui) tabPill(clk *widget.Clickable, txt string, idx int) layout.Widget {
	bg, fg := colPanelHi, colMuted
	if u.tab == idx {
		bg, fg = colAccentD, colAccent
	}
	return func(gtx layout.Context) layout.Dimensions {
		// 与工具栏同理：先消费点击，再布局。
		if clk.Clicked(gtx) && u.tab != idx {
			u.tab = idx
			u.win.Invalidate()
		}
		return drawPill(gtx, u.th, clk, txt, bg, fg, true)
	}
}

func (u *ui) tabContent(gtx layout.Context) layout.Dimensions {
	// 终端不做禁用：停止状态下也要能选中、复制历史输出。
	if u.tab == 0 {
		return u.term.Layout(gtx, u.th)
	}
	if !u.emu.Running() {
		gtx = gtx.Disabled()
	}
	switch u.tab {
	case 1:
		return u.ctrl.layoutADC(gtx)
	default:
		return u.ctrl.layoutWDT(gtx)
	}
}

func (u *ui) statusBar(gtx layout.Context) layout.Dimensions {
	gtx.Constraints.Min.Y = gtx.Dp(16)
	hint := "启动后：点引脚注入电平 · UART 回车发送 · 配置可改路径"
	return layout.Flex{Spacing: layout.SpaceBetween, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, unit.Sp(11), hint, colMuted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, unit.Sp(11), "Gio · 进程内模拟器", colMuted).Layout(gtx)
		}),
	)
}

func (u *ui) start() {
	elf := strings.TrimSpace(u.elfED.Text())
	ram := strings.TrimSpace(u.ramED.Text())
	if elf == "" || ram == "" {
		u.term.appendLine("[gui] 需要 ELF 与 ramdisk 两个路径")
		return
	}
	absELF, err := filepath.Abs(elf)
	if err != nil {
		u.term.appendLine("[gui] " + err.Error())
		return
	}
	absRAM, err := filepath.Abs(ram)
	if err != nil {
		u.term.appendLine("[gui] " + err.Error())
		return
	}
	ramdisk, err := os.ReadFile(absRAM)
	if err != nil {
		u.term.appendLine("[gui] 读取 ramdisk 失败: " + err.Error())
		return
	}
	if _, err := os.Stat(absELF); err != nil {
		u.term.appendLine("[gui] 文件不存在: " + absELF)
		return
	}
	off, err := strconv.ParseUint(strings.TrimSpace(u.offED.Text()), 0, 32)
	if err != nil {
		u.term.appendLine("[gui] 偏移格式错误（十进制或 0x 十六进制）")
		return
	}

	if err := u.emu.Start(absELF, ramdisk, uint32(off)); err != nil {
		u.term.appendLine("[gui] 启动模拟器失败: " + err.Error())
		return
	}
	u.ctrl.resetADC() // 新机器 ADC 通道全为 0
	u.term.appendLine("[gui] 已启动模拟器")
	u.win.Invalidate()
}

func (u *ui) step() {
	n, err := strconv.ParseUint(strings.TrimSpace(u.stepED.Text()), 10, 64)
	if err != nil || n == 0 {
		n = 1000
	}
	u.doCmd(func() error { return u.emu.Step(n) })
}

func (u *ui) doCmd(f func() error) {
	if err := f(); err != nil {
		u.term.appendLine("[gui] " + err.Error())
	}
}

// editorField 在给定矩形里画输入框底色，并把编辑器居中放进去。
// dark=true 用深色底浅色字（终端输入行），否则用浅色底深色字。
func editorField(gtx layout.Context, th *material.Theme, ed *widget.Editor, hint string, size image.Point, dark bool) {
	if dark {
		drawDarkWell(gtx, image.Rectangle{Max: size}, 6)
	} else {
		drawWell(gtx, image.Rectangle{Max: size}, 6)
	}
	inner := image.Pt(size.X-gtx.Dp(16), gtx.Dp(18))
	if inner.X < 0 {
		inner.X = 0
	}
	c := gtx
	c.Constraints = layout.Exact(inner)
	defer op.Offset(image.Pt(gtx.Dp(8), (size.Y-inner.Y)/2+gtx.Dp(1))).Push(gtx.Ops).Pop()
	es := material.Editor(th, ed, hint)
	es.TextSize = unit.Sp(12)
	es.Color = colText
	es.HintColor = colMuted
	if dark {
		es.Color = colTermText
		es.HintColor = colTermMuted
	}
	es.Layout(c)
}
