package main

// UART 终端面板：上方滚动输出，下方输入框。输入文本通过 uart_write
// 注入模拟器 RX；输出来自 uart 事件（模拟器已按批合并）。

import (
	"fmt"
	"image"
	"io"
	"strings"
	"unicode/utf8"

	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// terminalMaxLines 限制回滚行数，避免长时间运行后无限增长。
const terminalMaxLines = 2000

type terminalPanel struct {
	lines []string
	cur   string // 尚未收到 \n 的当前半行
	cr    bool   // 收到过裸 \r，下一个字符前要覆盖当前行

	// out 是只读编辑器：用自带的拖选/双击/Ctrl+C 支持选中与复制。
	out   widget.Editor
	input widget.Editor

	sendBtn  widget.Clickable
	copyBtn  widget.Clickable
	clearBtn widget.Clickable

	tx, rx uint64
	send   func(string)
}

func newTerminalPanel(send func(string)) *terminalPanel {
	t := &terminalPanel{send: send}
	t.out.ReadOnly = true
	t.input.SingleLine = true
	t.input.Submit = true
	return t
}

func (t *terminalPanel) Layout(gtx layout.Context, th *material.Theme) layout.Dimensions {
	// 先消费输入框事件，回车即发送。
	for {
		ev, ok := t.input.Update(gtx)
		if !ok {
			break
		}
		if sub, ok := ev.(widget.SubmitEvent); ok {
			t.submit(sub.Text)
		}
	}

	// 点击事件必须先于布局消费（布局里的 Clickable.Layout 会吞掉点击）。
	if t.clearBtn.Clicked(gtx) {
		t.lines = nil
		t.cur = ""
		t.sync()
	}
	if t.copyBtn.Clicked(gtx) {
		txt := t.out.SelectedText()
		if txt == "" {
			txt = t.fullText()
		}
		if txt != "" {
			gtx.Execute(clipboard.WriteCmd{
				Type: "application/text",
				Data: io.NopCloser(strings.NewReader(txt)),
			})
		}
	}
	if t.sendBtn.Clicked(gtx) {
		t.submit(t.input.Text())
	}

	dims := layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(6)}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.header(gtx, th)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return t.output(gtx, th)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.inputRow(gtx, th)
		}),
	)
	return dims
}

func (t *terminalPanel) header(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(8), Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(th, unit.Sp(12), "UART 终端", colText).Layout(gtx)
		}),
		layout.Flexed(1, spacer),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(th, unit.Sp(11), fmt.Sprintf("tx %d · rx %d", t.tx, t.rx), colMuted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return drawPill(gtx, th, &t.copyBtn, "复制", colPanelHi, colMuted, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return drawPill(gtx, th, &t.clearBtn, "清屏", colPanelHi, colMuted, true)
		}),
	)
}

func (t *terminalPanel) output(gtx layout.Context, th *material.Theme) layout.Dimensions {
	size := gtx.Constraints.Max
	drawDarkWell(gtx, image.Rectangle{Max: size}, 8)

	area := clip.Rect{Max: size}.Push(gtx.Ops)
	defer area.Pop()

	gtx.Constraints.Min = image.Point{}
	gtx.Constraints.Max = image.Pt(size.X-gtx.Dp(12), size.Y-gtx.Dp(8))
	trans := op.Offset(image.Pt(gtx.Dp(6), gtx.Dp(5))).Push(gtx.Ops)
	defer trans.Pop()

	if len(t.lines) == 0 && t.cur == "" {
		label(th, unit.Sp(12), "等待模拟器输出…", colTermMuted).Layout(gtx)
		return layout.Dimensions{Size: size}
	}
	// 只读编辑器负责显示：可拖选、双击选词，Ctrl+C 或“复制”按钮复制。
	es := material.Editor(th, &t.out, "")
	es.TextSize = unit.Sp(13)
	es.Color = colTermText
	es.SelectionColor = withAlpha(colAccent, 0x99)
	es.Layout(gtx)
	return layout.Dimensions{Size: size}
}

func (t *terminalPanel) inputRow(gtx layout.Context, th *material.Theme) layout.Dimensions {
	h := gtx.Dp(28)
	return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(6), Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(gtx.Constraints.Max.X, h)
			editorField(gtx, th, &t.input, "输入命令并回车（发送到 UART RX）", size, true)
			return layout.Dimensions{Size: size}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return drawPill(gtx, th, &t.sendBtn, "发送", colAccentD, colAccent, true)
		}),
	)
}

func (t *terminalPanel) submit(s string) {
	if s == "" {
		return
	}
	if t.send != nil {
		// 终端回车等价于串口上的 CR；固件侧的行规程按 CR 断行。
		t.send(s + "\r")
	}
	t.input.SetText("")
}

// append 追加一段输出。UART 事件是任意批次的字节，不一定以换行结尾，
// 所以未完成的半行保存在 cur 里，等后续批次补齐后再提交，避免把一行拆成多行。
func (t *terminalPanel) append(s string) {
	for _, r := range s {
		switch r {
		case '\n':
			t.lines = append(t.lines, t.cur)
			t.cur = ""
			t.cr = false
		case '\r':
			// CRLF 里的 \r 先记下；若后面不是 \n，就覆盖当前行（进度条式输出）。
			t.cr = true
		default:
			if t.cr {
				t.cur = ""
				t.cr = false
			}
			t.cur += string(r)
		}
	}
	if len(t.lines) > terminalMaxLines {
		t.lines = t.lines[len(t.lines)-terminalMaxLines:]
	}
	t.sync()
}

// appendLine 追加一条完整的内部消息。UART 若还留着半行，先把它断行，
// 避免 "[gui] ..." 之类的内容粘到固件输出上。
func (t *terminalPanel) appendLine(s string) {
	if t.cur != "" {
		t.lines = append(t.lines, t.cur)
		t.cur = ""
	}
	t.cr = false
	t.lines = append(t.lines, s)
	if len(t.lines) > terminalMaxLines {
		t.lines = t.lines[len(t.lines)-terminalMaxLines:]
	}
	t.sync()
}

// fullText 把已提交的行和未完成的半行拼成完整文本。
func (t *terminalPanel) fullText() string {
	var b strings.Builder
	for _, l := range t.lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString(t.cur)
	return b.String()
}

// sync 把文本同步进只读编辑器；光标停在末尾以自动跟随最新输出。
func (t *terminalPanel) sync() {
	s := t.fullText()
	t.out.SetText(s)
	n := utf8.RuneCountInString(s)
	t.out.SetCaret(n, n)
}

// apply 同步 TX/RX 计数。
func (t *terminalPanel) apply(ev Event) {
	t.tx, t.rx = ev.UART.TxBytes, ev.UART.RxBytes
}
