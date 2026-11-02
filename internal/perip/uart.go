package perip

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// ESP32-C3 UART0 基址（技术参考手册）。
const (
	UART0Base uint32 = 0x60000000
	UARTSize  uint32 = 0x1000
)

// UART 寄存器偏移（只实现控制台最小子集）。
const (
	uartFifo   uint32 = 0x00
	uartIntRaw uint32 = 0x04
	uartIntSt  uint32 = 0x08
	uartIntEna uint32 = 0x0C
	uartIntClr uint32 = 0x10
	uartClkdiv uint32 = 0x14
	uartStatus uint32 = 0x1C
)

// UART0：TX FIFO 写一个字节即输出到模拟器 stdout；RX 由后台协程把
// 注入的 reader 数据搬进缓冲，FIFO 读/FIFO 状态计数都走这个缓冲，
// 做到非阻塞——内核的 uart_tx_count()/uart_rx_count() 据此判断，
// flush 不会死等，控制台输入也能在 tick 轮询里被读到。
type UART struct {
	tx io.Writer
	rx io.Reader

	mu    sync.Mutex
	rxIn  bytes.Buffer // 从 stdin 读到的原始字节（尚未进入 FIFO）
	rxBuf bytes.Buffer // RX FIFO 里可被内核读走的字节
	// rxStop 关闭以停掉旧的 RX 搬运协程
	rxStop chan struct{}

	txBytes uint64 // 写进 TX FIFO 的字节数（诊断/UI）
	rxBytes uint64 // 从 RX FIFO 被内核读走的字节数
}

// RxFIFOMax 是硬件 RX FIFO 的深度（C3 为 128 字节）。
const RxFIFOMax = 128

// NewUART 创建一个不接 stdin/stdout 的 UART：供控制协议（GUI）注入
// TX 输出与 RX 输入时使用，避免后台协程抢读进程的 stdin。
func NewUART() *UART {
	return &UART{tx: io.Discard}
}

// NewUART0 是 CLI 默认构造：TX 写 stdout，RX 由后台协程从 stdin 搬运。
func NewUART0() *UART {
	u := &UART{}
	u.SetIO(os.Stdout, os.Stdin)
	return u
}

// SetSink 替换 TX 输出去向。应在模拟开始前调用。
func (u *UART) SetSink(tx io.Writer) {
	if tx == nil {
		tx = io.Discard
	}
	u.tx = tx
}

// PushRX 把主机侧输入直接放进“尚未进入 FIFO”的缓冲；
// 之后由 RxFeed 按硬件深度分批搬进 RX FIFO。
func (u *UART) PushRX(p []byte) {
	if len(p) == 0 {
		return
	}
	u.mu.Lock()
	u.rxIn.Write(p)
	u.mu.Unlock()
}

// RXFIFOLevel 返回当前 RX FIFO 中待内核读取的字节数。
func (u *UART) RXFIFOLevel() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rxBuf.Len()
}

// TXBytes / RXBytes 返回累计搬运字节数（诊断/UI）。
func (u *UART) TXBytes() uint64 { return u.txBytes }
func (u *UART) RXBytes() uint64 { return u.rxBytes }

// SetIO 注入测试用 IO：停掉旧 RX 协程，改从给定 reader 搬运。
func (u *UART) SetIO(tx io.Writer, rx io.Reader) {
	u.tx = tx
	u.rx = rx
	u.startRX(rx)
}

func (u *UART) startRX(src io.Reader) {
	if u.rxStop != nil {
		close(u.rxStop)
		u.rxStop = nil
	}
	if src == nil {
		return
	}
	u.rxStop = make(chan struct{})
	stop := u.rxStop
	go func() {
		buf := make([]byte, 256)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := src.Read(buf)
			if n > 0 {
				u.mu.Lock()
				u.rxIn.Write(buf[:n])
				u.mu.Unlock()
			}
			if err != nil {
				return // EOF 或错误：停止搬运（缓冲里残留的还能被读走）
			}
		}
	}()
}

func (u *UART) Name() string { return "UART0" }

// Reset 模拟芯片复位：RX FIFO 清空，但主机侧尚未送进 FIFO 的字节保留
// （rxIn 建模的是主机那一端，复位不会把已经在路上的字符收回去）。
// 内核复位后重新打开 UART 中断时，这些字节会照常被搬进新的 FIFO。
func (u *UART) Reset() {
	u.mu.Lock()
	u.rxBuf.Reset()
	u.mu.Unlock()
}

// RxFeed 把最多 limit 个字节从输入缓冲搬进 RX FIFO（不超过硬件深度）。
//
// 必须节流，不能一次把 stdin 全倒进 FIFO：内核的行规程只有 8 行队列，
// 队满会丢行（kernel/tty.c 有意如此——读者跟不上时更多预输入没有意义）。
// 真机上字节是沿线速陆续到达的，所以这里由主循环按 tick 分批放行，
// 让 shell 有机会在两批之间取走一行。-rxrate 0 可关闭节流用于背压测试。
func (u *UART) RxFeed(limit int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for limit > 0 && u.rxIn.Len() > 0 && u.rxBuf.Len() < RxFIFOMax {
		b, err := u.rxIn.ReadByte()
		if err != nil {
			break
		}
		_ = u.rxBuf.WriteByte(b)
		limit--
	}
}

func (u *UART) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case uartFifo:
		u.mu.Lock()
		b, err := u.rxBuf.ReadByte()
		if err == nil {
			u.rxBytes++
		}
		u.mu.Unlock()
		if err == nil {
			return uint32(b)
		}
		return 0
	case uartIntSt:
		u.mu.Lock()
		n := u.rxBuf.Len()
		u.mu.Unlock()
		return uint32(n) & 0xFF // 简化：pending = 收到的字节数
	case uartStatus:
		// TXFIFO 始终空闲（TXFIFO_CNT=0）以免 flush 死等；
		// RXFIFO_CNT 反映缓冲里待读字节数（封顶 0x3FF）。
		u.mu.Lock()
		n := u.rxBuf.Len()
		u.mu.Unlock()
		rxCnt := uint32(n)
		if rxCnt > 0x3FF {
			rxCnt = 0x3FF
		}
		return rxCnt // bit[9:0] = RXFIFO_CNT, TXFIFO_CNT(bit[25:16]) = 0
	}
	return 0
}

func (u *UART) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case uartFifo:
		c := byte(val)
		if _, err := u.tx.Write([]byte{c}); err == nil {
			u.txBytes++
		}
	case uartIntEna:
		// 影子存储，无副作用
	case uartIntClr:
		// 清中断挂起（模拟里无操作）
	case uartClkdiv:
		// 波特率分频最小化，不影响字符 IO
	default:
		fmt.Fprintf(os.Stderr, "[uart] unhandled write off=0x%02X val=0x%08X\n", offset, val)
	}
}
