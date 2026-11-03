package main

// 亮色仪表盘主题：颜色常量 + 自绘卡片/胶囊按钮/文本辅助函数。
// Gio 没有现成的“卡片”控件，这里用 clip.RRect + paint.FillShape 画。

import (
	"image"
	"image/color"
	"os"

	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

var (
	colBg      = rgb(0xE9EEF3)
	colPanel   = rgb(0xFFFFFF)
	colPanelHi = rgb(0xE4EBF1)
	colBorder  = rgb(0xB9C7D4)
	colText    = rgb(0x0B141C)
	colMuted   = rgb(0x415666)
	colAccent  = rgb(0x077D6D)
	colAccentD = rgb(0xCFEDE7)
	colHigh    = rgb(0x0F8A4A)
	colLow     = rgb(0x94A4B3)
	colDriven  = rgb(0xAA6200)
	colAlert   = rgb(0xC03228)
	colField   = rgb(0xE8EEF4)

	// 终端输出区：黑底浅字，长时间盯屏比浅灰底更清楚。
	colTermBg     = rgb(0x0B1015)
	colTermBorder = rgb(0x2A3A46)
	colTermText   = rgb(0xE8F2F9)
	colTermMuted  = rgb(0x7E93A4)
)

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xFF}
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

// newTheme 载入一款带 CJK 字形的系统字体；找不到就退回默认字体。
func newTheme() *material.Theme {
	th := material.NewTheme()
	candidates := []string{
		`C:\Windows\Fonts\simhei.ttf`,
		`C:\Windows\Fonts\Deng.ttf`,
		`C:\Windows\Fonts\msyh.ttc`,
		"/usr/share/fonts/truetype/wqy/wqy-microhei.ttf",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttf",
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		faces, err := opentype.ParseCollection(data)
		if err != nil || len(faces) == 0 {
			continue
		}
		th.Shaper = text.NewShaper(text.WithCollection(faces))
		th.Face = faces[0].Font.Typeface
		break
	}
	th.Palette.Bg = colBg
	th.Palette.Fg = colText
	th.Palette.ContrastBg = colAccent
	th.Palette.ContrastFg = colPanel
	th.TextSize = 13
	th.FingerSize = unit.Dp(28)
	return th
}

func label(th *material.Theme, size unit.Sp, txt string, col color.NRGBA) material.LabelStyle {
	l := material.Label(th, size, txt)
	l.Color = col
	return l
}

// drawCard 用当前约束的最大尺寸画一张圆角卡片（带细边框）。
func drawCard(gtx layout.Context, col color.NRGBA) {
	r := image.Rectangle{Max: gtx.Constraints.Max}
	rr := rrect(r, gtx.Dp(12))
	paint.FillShape(gtx.Ops, col, rr.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, colBorder, clip.Stroke{
		Path:  rr.Path(gtx.Ops),
		Width: float32(gtx.Dp(1)),
	}.Op())
}

// drawCardSized 按内容尺寸画卡片：先记录内容，再在背后画圆角底色。
func drawCardSized(gtx layout.Context, col color.NRGBA, w layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := w(gtx)
	call := macro.Stop()
	rr := rrect(image.Rectangle{Max: dims.Size}, gtx.Dp(12))
	paint.FillShape(gtx.Ops, col, rr.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, colBorder, clip.Stroke{
		Path:  rr.Path(gtx.Ops),
		Width: float32(gtx.Dp(1)),
	}.Op())
	call.Add(gtx.Ops)
	return dims
}

// spacer 返回约束最小尺寸；配合 layout.Flexed(1, spacer) 撑开剩余空间。
func spacer(gtx layout.Context) layout.Dimensions {
	return layout.Dimensions{Size: gtx.Constraints.Min}
}

// drawPill 是紧凑胶囊按钮；enabled=false 时只变灰，点击由调用方拦掉。
func drawPill(gtx layout.Context, th *material.Theme, clk *widget.Clickable, txt string,
	bg, fg color.NRGBA, enabled bool) layout.Dimensions {
	if !enabled {
		bg, fg = colPanelHi, colMuted
	}
	gtx.Constraints.Min.Y = gtx.Dp(28)
	return clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.Inset{
			Top: unit.Dp(5), Bottom: unit.Dp(5),
			Left: unit.Dp(10), Right: unit.Dp(10),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := label(th, unit.Sp(12), txt, fg)
			return l.Layout(gtx)
		})
		call := macro.Stop()

		fill := bg
		if enabled && clk.Pressed() {
			fill = darken(fill, 18)
		} else if enabled && clk.Hovered() {
			fill = darken(fill, 8)
		}
		rr := rrect(image.Rectangle{Max: image.Pt(dims.Size.X, dims.Size.Y)}, gtx.Dp(7))
		paint.FillShape(gtx.Ops, fill, rr.Op(gtx.Ops))
		call.Add(gtx.Ops)
		return dims
	})
}

func darken(c color.NRGBA, by uint8) color.NRGBA {
	sub := func(v uint8) uint8 {
		if int(v)-int(by) < 0 {
			return 0
		}
		return v - by
	}
	return color.NRGBA{R: sub(c.R), G: sub(c.G), B: sub(c.B), A: c.A}
}

// fillRect 在给定矩形里画圆角底色，用于输入框/数据条。
func fillRect(gtx layout.Context, r image.Rectangle, col color.NRGBA, radius unit.Dp) {
	paint.FillShape(gtx.Ops, col, rrect(r, gtx.Dp(radius)).Op(gtx.Ops))
}

// drawWell 画输入/输出凹槽：浅灰底 + 1px 边框，避免白卡片上分不清边界。
func drawWell(gtx layout.Context, r image.Rectangle, radius unit.Dp) {
	rr := rrect(r, gtx.Dp(radius))
	paint.FillShape(gtx.Ops, colField, rr.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, colBorder, clip.Stroke{
		Path:  rr.Path(gtx.Ops),
		Width: float32(gtx.Dp(1)),
	}.Op())
}

// drawDarkWell 画深色凹槽，用于终端输出区。
func drawDarkWell(gtx layout.Context, r image.Rectangle, radius unit.Dp) {
	rr := rrect(r, gtx.Dp(radius))
	paint.FillShape(gtx.Ops, colTermBg, rr.Op(gtx.Ops))
	paint.FillShape(gtx.Ops, colTermBorder, clip.Stroke{
		Path:  rr.Path(gtx.Ops),
		Width: float32(gtx.Dp(1)),
	}.Op())
}

// rrect 生成四角同半径的圆角矩形；v0.10 用像素级四角字段。
func rrect(r image.Rectangle, radius int) clip.RRect {
	return clip.RRect{Rect: r, NW: radius, NE: radius, SW: radius, SE: radius}
}
