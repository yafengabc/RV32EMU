package main

// 板图 / GPIO：自绘一块浅色 PCB，中间 ESP32-C3，两侧 22 个引脚。
// 引脚可点击注入外部电平；绿色=高、灰色=低、橙色环=片内输出驱动。

import (
	"fmt"
	"image"
	"image/color"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

const boardPinCount = 22

type pinState struct {
	level  bool
	driven bool
}

type boardView struct {
	pins [boardPinCount]pinState
	ext  uint32
	tag  int

	onPin func(pin int, level bool)

	pc        uint32
	inst      uint64
	tx, rx    uint64
	adc0      uint32
	hasADC    bool
	status    string
	statusCol color.NRGBA

	pinPos [boardPinCount]image.Point
	pinR   int
}

func newBoardView(onPin func(pin int, level bool)) *boardView {
	return &boardView{onPin: onPin, status: "OFFLINE", statusCol: colMuted}
}

func (b *boardView) Update(ev Event) {
	b.ext = ev.GPIO.Ext
	for i := range b.pins {
		b.pins[i].level = ev.GPIO.In&(1<<uint(i)) != 0
		b.pins[i].driven = ev.GPIO.Enable&(1<<uint(i)) != 0
	}
	b.pc = ev.PC
	b.inst = ev.Inst
	b.tx, b.rx = ev.UART.TxBytes, ev.UART.RxBytes
	b.hasADC = len(ev.ADC) > 0
	if b.hasADC {
		b.adc0 = ev.ADC[0]
	}
	switch {
	case ev.Halted:
		b.status, b.statusCol = "HALTED", colAlert
	case ev.Running:
		b.status, b.statusCol = "RUNNING", colHigh
	default:
		b.status, b.statusCol = "PAUSED", colMuted
	}
}

func (b *boardView) setOffline() {
	b.status, b.statusCol = "OFFLINE", colMuted
	for i := range b.pins {
		b.pins[i] = pinState{}
	}
	b.ext = 0
}

func (b *boardView) toggle(pin int) {
	level := b.ext&(1<<uint(pin)) == 0
	if level {
		b.ext |= 1 << uint(pin)
	} else {
		b.ext &^= 1 << uint(pin)
	}
	b.pins[pin].level = level
	if b.onPin != nil {
		b.onPin(pin, level)
	}
}

func (b *boardView) hit(p f32.Point) {
	r := float32(b.pinR + 6)
	for i := range b.pinPos {
		dx := float32(b.pinPos[i].X) - p.X
		dy := float32(b.pinPos[i].Y) - p.Y
		if dx*dx+dy*dy <= r*r {
			b.toggle(i)
			return
		}
	}
}

// Layout 画整块板子；指针事件按上一帧记录的引脚坐标命中。
func (b *boardView) Layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &b.tag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if pe, ok := ev.(pointer.Event); ok {
			b.hit(pe.Position)
		}
	}

	size := gtx.Constraints.Max
	w, h := size.X, size.Y
	drawCard(gtx, colPanel)

	pad := gtx.Dp(14)
	// 标题行
	title := label(th, unit.Sp(15), "ESP32-C3", colText)
	titlePos := image.Pt(pad, pad)
	drawText(gtx, title, titlePos)
	sub := label(th, unit.Sp(12), "RV32IMC · 22 GPIO", colMuted)
	drawText(gtx, sub, image.Pt(pad, pad+gtx.Dp(22)))

	// 状态胶囊（右上角）
	stW := gtx.Dp(84)
	stH := gtx.Dp(24)
	stRect := image.Rect(w-pad-stW, pad, w-pad, pad+stH)
	fillRect(gtx, stRect, withAlpha(b.statusCol, 0x33), 6)
	dotR := gtx.Dp(3)
	cx := stRect.Min.X + gtx.Dp(12)
	cy := stRect.Min.Y + stH/2
	paint.FillShape(gtx.Ops, b.statusCol,
		clip.Ellipse(image.Rect(cx-dotR, cy-dotR, cx+dotR, cy+dotR)).Op(gtx.Ops))
	drawTextBox(gtx, th,
		image.Rect(stRect.Min.X+gtx.Dp(20), stRect.Min.Y, stRect.Max.X-gtx.Dp(6), stRect.Max.Y),
		unit.Sp(11), b.status, b.statusCol, text.Middle)

	// 芯片矩形
	chipW := min(gtx.Dp(190), int(float32(w)*0.5))
	chipH := min(gtx.Dp(168), int(float32(h)*0.46))
	chip := image.Rect(
		(w-chipW)/2, (h-chipH)/2+gtx.Dp(4),
		(w+chipW)/2, (h+chipH)/2+gtx.Dp(4),
	)
	fillRect(gtx, chip, colPanelHi, 10)
	paint.FillShape(gtx.Ops, colBorder, clip.Stroke{
		Path:  rrect(chip, gtx.Dp(10)).Path(gtx.Ops),
		Width: float32(gtx.Dp(1)),
	}.Op())
	fillRect(gtx, image.Rect(chip.Min.X+gtx.Dp(14), chip.Min.Y,
		chip.Max.X-gtx.Dp(14), chip.Min.Y+gtx.Dp(2)), withAlpha(colAccent, 0xCC), 1)

	// 引脚位置：左右各 11 个，沿板高均分
	b.pinR = gtx.Dp(8)
	top := gtx.Dp(78)
	bot := h - gtx.Dp(40)
	step := float32(bot-top) / float32(boardPinCount/2+1)
	leftX := chip.Min.X - gtx.Dp(50)
	rightX := chip.Max.X + gtx.Dp(50)
	if leftX < pad+b.pinR {
		leftX = pad + b.pinR
	}
	if rightX > w-pad-b.pinR {
		rightX = w - pad - b.pinR
	}
	for i := 0; i < boardPinCount/2; i++ {
		y := top + int(step*float32(i+1))
		b.pinPos[i] = image.Pt(leftX, y)
		b.pinPos[i+boardPinCount/2] = image.Pt(rightX, y)
	}

	// 走线与引脚
	for i := 0; i < boardPinCount; i++ {
		p := b.pinPos[i]
		lineCol := withAlpha(colBorder, 0xCC)
		if b.pins[i].driven {
			lineCol = withAlpha(colDriven, 0x99)
		}
		if i < boardPinCount/2 {
			fillRect(gtx, image.Rect(p.X+b.pinR, p.Y-gtx.Dp(1), chip.Min.X, p.Y+gtx.Dp(1)), lineCol, 1)
		} else {
			fillRect(gtx, image.Rect(chip.Max.X, p.Y-gtx.Dp(1), p.X-b.pinR, p.Y+gtx.Dp(1)), lineCol, 1)
		}
		b.drawPin(gtx, th, i, p)
	}

	// 芯片文字
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(22), chip.Max.X, chip.Min.Y+gtx.Dp(46)),
		unit.Sp(17), "ESP32-C3", colText, text.Middle)
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(48), chip.Max.X, chip.Min.Y+gtx.Dp(66)),
		unit.Sp(11), "RISC-V · RV32IMC", colMuted, text.Middle)
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(80), chip.Max.X, chip.Min.Y+gtx.Dp(98)),
		unit.Sp(11), fmt.Sprintf("PC 0x%08X", b.pc), colText, text.Middle)
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(98), chip.Max.X, chip.Min.Y+gtx.Dp(116)),
		unit.Sp(11), fmt.Sprintf("INST %d", b.inst), colMuted, text.Middle)
	adc := "ADC0 —"
	if b.hasADC {
		adc = fmt.Sprintf("ADC0 %d / 4095", b.adc0)
	}
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(120), chip.Max.X, chip.Min.Y+gtx.Dp(138)),
		unit.Sp(11), adc, colMuted, text.Middle)
	drawTextBox(gtx, th, image.Rect(chip.Min.X, chip.Min.Y+gtx.Dp(142), chip.Max.X, chip.Min.Y+gtx.Dp(160)),
		unit.Sp(11), fmt.Sprintf("UART tx %d · rx %d", b.tx, b.rx), colMuted, text.Middle)

	// 底部图例
	drawTextBox(gtx, th, image.Rect(pad, h-gtx.Dp(28), w-pad, h-gtx.Dp(10)),
		unit.Sp(11), "点击引脚注入外部电平 · 绿=高 · 灰=低 · 橙环=片内驱动", colMuted, text.Middle)

	// 注册指针命中区域（用整块板子做 tag）
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &b.tag)
	area.Pop()

	return layout.Dimensions{Size: size}
}

func (b *boardView) drawPin(gtx layout.Context, th *material.Theme, i int, p image.Point) {
	ring := colMuted
	ringW := gtx.Dp(2)
	if b.pins[i].driven {
		ring = colDriven
		ringW = gtx.Dp(2)
	}
	// 外圈
	outer := b.pinR + ringW
	paint.FillShape(gtx.Ops, ring, clip.Ellipse(
		image.Rect(p.X-outer, p.Y-outer, p.X+outer, p.Y+outer),
	).Op(gtx.Ops))
	// 内圆
	inner := b.pinR
	fill := colLow
	if b.pins[i].level {
		fill = colHigh
	}
	paint.FillShape(gtx.Ops, fill, clip.Ellipse(
		image.Rect(p.X-inner, p.Y-inner, p.X+inner, p.Y+inner),
	).Op(gtx.Ops))

	// 引脚编号
	lbl := fmt.Sprintf("%d", i)
	if i < boardPinCount/2 {
		drawTextBox(gtx, th, image.Rect(p.X-gtx.Dp(34), p.Y-gtx.Dp(8), p.X-gtx.Dp(12), p.Y+gtx.Dp(8)),
			unit.Sp(10), lbl, colMuted, text.End)
	} else {
		drawTextBox(gtx, th, image.Rect(p.X+gtx.Dp(12), p.Y-gtx.Dp(8), p.X+gtx.Dp(34), p.Y+gtx.Dp(8)),
			unit.Sp(10), lbl, colMuted, text.Start)
	}
}

// drawText 在局部坐标处画一行文本。
func drawText(gtx layout.Context, l material.LabelStyle, pos image.Point) {
	defer op.Offset(pos).Push(gtx.Ops).Pop()
	l.Layout(gtx)
}

// drawTextBox 在指定矩形内按对齐方式画文本。
func drawTextBox(gtx layout.Context, th *material.Theme, r image.Rectangle,
	size unit.Sp, txt string, col color.NRGBA, align text.Alignment) {
	defer op.Offset(r.Min).Push(gtx.Ops).Pop()
	gtx.Constraints = layout.Exact(r.Size())
	l := label(th, size, txt, col)
	l.Alignment = align
	l.Layout(gtx)
}
