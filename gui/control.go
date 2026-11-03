package main

// ADC + 看门狗控制面板。
//
// ADC 滑块写入 APB_SARADC 通道码值（0..4095）；拖动中只刷新数值，
// 松手才发命令。state 事件不回写滑块——GUI 是 ADC 码值的唯一写入方，
// 20Hz 快照回写会打断拖动。
//
// 看门狗开关用 widget.Bool：程序化赋值 Value 不会触发事件，所以布局前
// 记录旧值、布局后比较，用户真的点了才发命令。

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const (
	adcMaxRaw     = 4095
	controlADCCnt = 5
)

type adcChannel struct {
	f        widget.Float
	value    uint32
	dragging bool
}

type controlPanel struct {
	th *material.Theme

	adc    [controlADCCnt]adcChannel
	adcCmd func(ch int, value uint32) error
	wdtCmd func(target, action string, value *uint32) error
	onErr  func(error)

	invalidate func()

	tg0En    widget.Bool
	tg0Hold  widget.Editor
	tg0Apply widget.Clickable
	tg0Feed  widget.Clickable
	tg0Info  string

	rtcEn    widget.Bool
	rtcHold  widget.Editor
	rtcApply widget.Clickable
	rtcFeed  widget.Clickable
	rtcInfo  string

	swdDis  widget.Bool
	swdFeed widget.Clickable
	swdInfo string

	msg string
}

func newControlPanel(
	th *material.Theme,
	adcCmd func(ch int, value uint32) error,
	wdtCmd func(target, action string, value *uint32) error,
	onErr func(error),
	invalidate func(),
) *controlPanel {
	c := &controlPanel{
		th: th, adcCmd: adcCmd, wdtCmd: wdtCmd, onErr: onErr, invalidate: invalidate,
		tg0Info: "stage0 —", rtcInfo: "stage0 —", swdInfo: "喂狗 0 次",
	}
	for _, ed := range []*widget.Editor{&c.tg0Hold, &c.rtcHold} {
		ed.SingleLine = true
		ed.SetText("200000")
	}
	return c
}

// layoutADC 是 ADC 页：标题、说明和 5 行通道滑块。
func (c *controlPanel) layoutADC(gtx layout.Context) layout.Dimensions {
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(c.th, unit.Sp(12), "ADC · APB_SARADC 12-bit", colAccent).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := label(c.th, unit.Sp(11), "C3 无 DAC；CH0..CH4 对应 GPIO0..4，松手写入。", colMuted)
			l.MaxLines = 1
			return l.Layout(gtx)
		}),
	}
	for i := 0; i < controlADCCnt; i++ {
		i := i
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.adcRow(gtx, i)
		}))
	}
	return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(7)}.Layout(gtx, children...)
}

func (c *controlPanel) adcRow(gtx layout.Context, i int) layout.Dimensions {
	ch := &c.adc[i]
	h := gtx.Dp(26)
	dims := layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(8), Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(gtx.Dp(32), h))
			return label(c.th, unit.Sp(11), fmt.Sprintf("CH%d", i), colMuted).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = image.Pt(gtx.Constraints.Max.X, h)
			gtx.Constraints.Max.Y = h
			s := material.Slider(c.th, &ch.f)
			s.Axis = layout.Horizontal
			s.Color = colAccent
			s.FingerSize = unit.Dp(14)
			return s.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			// 滑块已在本帧更新，这里立刻折算码值，避免显示慢一帧。
			ch.value = uint32(math.Round(float64(ch.f.Value) * adcMaxRaw))
			box := image.Pt(gtx.Dp(44), h)
			gtx.Constraints = layout.Exact(box)
			fillRect(gtx, image.Rectangle{Max: box}, colField, 4)
			paint.FillShape(gtx.Ops, colBorder, clip.Stroke{
				Path:  rrect(image.Rectangle{Max: box}, gtx.Dp(4)).Path(gtx.Ops),
				Width: float32(gtx.Dp(1)),
			}.Op())
			drawTextBox(gtx, c.th, image.Rectangle{Max: box}, unit.Sp(11),
				strconv.Itoa(int(ch.value)), colText, text.End)
			return layout.Dimensions{Size: box}
		}),
	)

	// 拖动结束的那一帧才写寄存器。
	if ch.f.Dragging() {
		ch.dragging = true
	} else {
		if ch.dragging {
			c.sendADC(i, ch.value)
		}
		ch.dragging = false
	}
	return dims
}

// layoutWDT 是看门狗页：TG0 / RTC / super WDT 三块。
func (c *controlPanel) layoutWDT(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(8)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(c.th, unit.Sp(12), "看门狗", colAccent).Layout(gtx)
		}),
		layout.Rigid(c.tg0Block),
		layout.Rigid(c.rtcBlock),
		layout.Rigid(c.swdBlock),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if c.msg == "" {
				return layout.Dimensions{}
			}
			return label(c.th, unit.Sp(11), c.msg, colAlert).Layout(gtx)
		}),
	)
}

func (c *controlPanel) tg0Block(gtx layout.Context) layout.Dimensions {
	was := c.tg0En.Value
	// 先消费点击，再布局（Clickable.Layout 会吞掉点击事件）。
	if c.tg0Apply.Clicked(gtx) {
		c.applyHold("tg0", &c.tg0Hold)
	}
	if c.tg0Feed.Clicked(gtx) {
		c.sendWDT("tg0", "feed", nil)
	}
	dims := layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(4)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.toggleRow(gtx, &c.tg0En, "TG0 WDT（TIMG0）", &c.tg0Feed)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.holdRow(gtx, &c.tg0Hold, &c.tg0Apply)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.infoLine(gtx, c.tg0Info)
		}),
	)
	if c.tg0En.Value != was {
		c.sendWDT("tg0", onOff(c.tg0En.Value), nil)
	}
	return dims
}

func (c *controlPanel) rtcBlock(gtx layout.Context) layout.Dimensions {
	was := c.rtcEn.Value
	if c.rtcApply.Clicked(gtx) {
		c.applyHold("rtc", &c.rtcHold)
	}
	if c.rtcFeed.Clicked(gtx) {
		c.sendWDT("rtc", "feed", nil)
	}
	dims := layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(4)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.toggleRow(gtx, &c.rtcEn, "RTC WDT", &c.rtcFeed)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.holdRow(gtx, &c.rtcHold, &c.rtcApply)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.infoLine(gtx, c.rtcInfo)
		}),
	)
	if c.rtcEn.Value != was {
		c.sendWDT("rtc", onOff(c.rtcEn.Value), nil)
	}
	return dims
}

func (c *controlPanel) swdBlock(gtx layout.Context) layout.Dimensions {
	was := c.swdDis.Value
	if c.swdFeed.Clicked(gtx) {
		c.sendWDT("swd", "feed", nil)
	}
	dims := layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(4)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.toggleRow(gtx, &c.swdDis, "禁用 super WDT", &c.swdFeed)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return c.infoLine(gtx, c.swdInfo)
		}),
	)
	if c.swdDis.Value != was {
		// SWD_CONF.DISABLE 是“禁用”位，勾选时发 disable。
		c.sendWDT("swd", onOff(!c.swdDis.Value), nil)
	}
	return dims
}

// toggleRow 是「开关 + 右侧喂狗按钮」一行。
func (c *controlPanel) toggleRow(gtx layout.Context, b *widget.Bool, txt string, feed *widget.Clickable) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(8), Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.CheckBox(c.th, b, txt).Layout(gtx)
		}),
		layout.Flexed(1, spacer),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return drawPill(gtx, c.th, feed, "喂狗", colPanelHi, colText, true)
		}),
	)
}

// holdRow 是「stage0 输入框 + 应用按钮」一行。
func (c *controlPanel) holdRow(gtx layout.Context, ed *widget.Editor, apply *widget.Clickable) layout.Dimensions {
	h := gtx.Dp(26)
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(6), Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(gtx.Dp(42), h))
			return label(c.th, unit.Sp(11), "stage0", colMuted).Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, h)
			editorField(gtx, c.th, ed, "", size, false)
			return layout.Dimensions{Size: size}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return drawPill(gtx, c.th, apply, "应用", colPanelHi, colText, true)
		}),
	)
}

func (c *controlPanel) infoLine(gtx layout.Context, txt string) layout.Dimensions {
	l := label(c.th, unit.Sp(11), txt, colMuted)
	l.MaxLines = 1
	return l.Layout(gtx)
}

func (c *controlPanel) sendADC(ch int, value uint32) {
	if c.adcCmd == nil {
		return
	}
	if err := c.adcCmd(ch, value); err != nil && c.onErr != nil {
		c.onErr(err)
	}
}

func (c *controlPanel) sendWDT(target, action string, value *uint32) {
	if c.wdtCmd == nil {
		return
	}
	if err := c.wdtCmd(target, action, value); err != nil && c.onErr != nil {
		c.onErr(err)
	}
}

// applyHold 解析 stage0 超时值并发送 hold 命令。
func (c *controlPanel) applyHold(target string, ed *widget.Editor) {
	n, err := strconv.ParseUint(strings.TrimSpace(ed.Text()), 10, 32)
	if err != nil || n == 0 {
		c.setMsg("stage0 超时必须是非零整数")
		return
	}
	c.setMsg("")
	v := uint32(n)
	c.sendWDT(target, "hold", &v)
}

func (c *controlPanel) setMsg(s string) {
	if c.msg == s {
		return
	}
	c.msg = s
	if c.invalidate != nil {
		c.invalidate()
	}
}

// Update 用 state 事件同步开关与状态文字；ADC 滑块保持本地值。
func (c *controlPanel) Update(ev Event) {
	c.tg0En.Value = ev.WDT.TG0.Enabled
	c.rtcEn.Value = ev.WDT.RTC.Enabled
	c.swdDis.Value = ev.WDT.SWD.Disabled

	c.tg0Info = fmt.Sprintf("stage0=%d · prescale=%d · 超时 %d 次%s",
		ev.WDT.TG0.Stage0, ev.WDT.TG0.Prescale, ev.WDT.TG0.Fires,
		pendingText(ev.WDT.TG0.ResetPending))
	c.rtcInfo = fmt.Sprintf("stage0=%d · action=%d · 超时 %d 次%s",
		ev.WDT.RTC.Stage0, ev.WDT.RTC.Action, ev.WDT.RTC.Fires,
		pendingText(ev.WDT.RTC.ResetPending))
	c.swdInfo = fmt.Sprintf("喂狗 %d 次 · 复位标志 %v",
		ev.WDT.SWD.Feeds, ev.WDT.SWD.ResetFlag)
}

func pendingText(p bool) string {
	if p {
		return " · 复位待处理"
	}
	return ""
}

// resetADC 在整机/重新启动时把滑块归零（新机器的 ADC 通道全为 0）。
func (c *controlPanel) resetADC() {
	for i := range c.adc {
		c.adc[i].f.Value = 0
		c.adc[i].value = 0
		c.adc[i].dragging = false
	}
}

// onOff 把布尔开关映射成协议动作名。
func onOff(on bool) string {
	if on {
		return "enable"
	}
	return "disable"
}
