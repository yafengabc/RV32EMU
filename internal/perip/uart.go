package perip

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// ESP32-C3 UART0 / UART1 基址（技术参考手册）。
const (
	UART0Base uint32 = 0x60000000
	UART1Base uint32 = 0x60010000
	UARTSize  uint32 = 0x1000
)

// UART 寄存器偏移（只实现控制台最小子集）。
const (
	uartFifo    uint32 = 0x00
	uartIntRaw  uint32 = 0x04
	uartIntSt   uint32 = 0x08
	uartIntEna  uint32 = 0x0C
	uartIntClr  uint32 = 0x10
	uartClkdiv  uint32 = 0x14
	uartStatus  uint32 = 0x1C
	uartConf1   uint32 = 0x24
	uartMemConf uint32 = 0x60
)

// UART0/UART1 中断：source 21/22 经 INTMTX 路由（TinyOS 把 UART0 取到
// CPU line 3，IDF 自己配）。只建模控制台真正用到的两位：RX FIFO 满与接收超时。
const (
	uartSrc           = 21
	uart1Src          = 22
	uartIntRxfifoFull = 1 << 0
	uartIntRxfifoTout = 1 << 8

	uartConf1RxToutEn = 1 << 21
)

// source 返回本 UART 实例在 INTMTX 里的中断源编号。
func (u *UART) source() uint32 {
	if u.src != 0 {
		return u.src
	}
	return uartSrc
}

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

	// 中断状态：UART0 = source 21，UART1 = source 22。regs 只存
	// INT_ENA/CONF1/MEM_CONF，tout 是接收超时锁存（写 INT_CLR 才清）。
	intc *C3Intc
	regs map[uint32]uint32
	tout bool
	name string
	src  uint32
}

// RxFIFOMax 是硬件 RX FIFO 的深度（C3 为 128 字节）。
const RxFIFOMax = 128

// NewUART 创建一个不接 stdin/stdout 的 UART：供控制协议（GUI）注入
// TX 输出与 RX 输入时使用，避免后台协程抢读进程的 stdin。
func NewUART() *UART {
	return &UART{tx: io.Discard, regs: make(map[uint32]uint32)}
}

// NewUART0 是 CLI 默认构造：TX 写 stdout，RX 由后台协程从 stdin 搬运。
func NewUART0() *UART {
	u := &UART{regs: make(map[uint32]uint32), name: "UART0", src: uartSrc}
	u.SetIO(os.Stdout, os.Stdin)
	return u
}

// NewUART1 构造 UART1：模拟器没有接第二路串口，TX 直接丢弃、没有 RX。
// 中断 source 22（ETS_UART1_INTR_SOURCE），固件照常可以配置/查询状态。
func NewUART1() *UART {
	return &UART{tx: io.Discard, regs: make(map[uint32]uint32), name: "UART1", src: uart1Src}
}

// SetIntc 注入 INTMTX，用于把 RX 中断投递到 CPU。
func (u *UART) SetIntc(i *C3Intc) { u.intc = i }

// intRawLocked 计算当前 RAW 状态（调用方需持 u.mu）。
// FULL 是电平：FIFO 里的字节数达到阈值就置位；TOUT 是锁存：收到字节后
// 置位，直到写 INT_CLR 或 FIFO 被读空。
func (u *UART) intRawLocked() uint32 {
	var raw uint32
	n := u.rxBuf.Len()
	if thr := int(u.regs[uartConf1]) & 0x1F; n > 0 && n >= thr {
		raw |= uartIntRxfifoFull
	}
	if u.tout {
		raw |= uartIntRxfifoTout
	}
	return raw
}

// syncIRQ 把 RAW & ENA 折算成 INTMTX source 的电平。
func (u *UART) syncIRQ() {
	if u.intc == nil {
		return
	}
	u.mu.Lock()
	active := u.intRawLocked()&u.regs[uartIntEna] != 0
	u.mu.Unlock()
	if active {
		u.intc.Raise(u.source())
	} else {
		u.intc.ClearSource(u.source())
	}
	u.intc.Deliver()
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

func (u *UART) Name() string {
	if u.name != "" {
		return u.name
	}
	return "UART0"
}

// Reset 模拟芯片复位：RX FIFO 清空，但主机侧尚未送进 FIFO 的字节保留
// （rxIn 建模的是主机那一端，复位不会把已经在路上的字符收回去）。
// 内核复位后重新打开 UART 中断时，这些字节会照常被搬进新的 FIFO。
func (u *UART) Reset() {
	u.mu.Lock()
	u.rxBuf.Reset()
	u.tout = false
	u.regs = make(map[uint32]uint32)
	u.mu.Unlock()
	u.syncIRQ()
}

// RxFeed 把最多 limit 个字节从输入缓冲搬进 RX FIFO（不超过硬件深度）。
//
// 必须节流，不能一次把 stdin 全倒进 FIFO：内核的行规程只有 8 行队列，
// 队满会丢行（kernel/tty.c 有意如此——读者跟不上时更多预输入没有意义）。
// 真机上字节是沿线速陆续到达的，所以这里由主循环按 tick 分批放行，
// 让 shell 有机会在两批之间取走一行。-rxrate 0 可关闭节流用于背压测试。
func (u *UART) RxFeed(limit int) {
	moved := false
	u.mu.Lock()
	for limit > 0 && u.rxIn.Len() > 0 && u.rxBuf.Len() < RxFIFOMax {
		b, err := u.rxIn.ReadByte()
		if err != nil {
			break
		}
		_ = u.rxBuf.WriteByte(b)
		moved = true
		limit--
	}
	// 新字节到达即置接收超时锁存（这里不建模波特率，按“一批到达”算）。
	if moved && u.regs[uartConf1]&uartConf1RxToutEn != 0 {
		u.tout = true
	}
	u.mu.Unlock()
	if moved {
		u.syncIRQ()
	}
}

func (u *UART) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case uartFifo:
		u.mu.Lock()
		b, err := u.rxBuf.ReadByte()
		if err == nil {
			u.rxBytes++
			// FIFO 读空时硬件自动撤销接收超时锁存。
			if u.rxBuf.Len() == 0 {
				u.tout = false
			}
		}
		u.mu.Unlock()
		u.syncIRQ()
		if err == nil {
			return uint32(b)
		}
		return 0
	case uartIntRaw:
		u.mu.Lock()
		v := u.intRawLocked()
		u.mu.Unlock()
		return v
	case uartIntSt:
		u.mu.Lock()
		v := u.intRawLocked() & u.regs[uartIntEna]
		u.mu.Unlock()
		return v
	case uartIntEna:
		u.mu.Lock()
		v := u.regs[uartIntEna]
		u.mu.Unlock()
		return v
	case uartConf1, uartMemConf:
		u.mu.Lock()
		v := u.regs[offset]
		u.mu.Unlock()
		return v
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
	// 其余配置寄存器（CONF0/HWFC/SWFC/CLK_CONF…）只做影子，读回写入值：
	// 固件初始化后读回配置能通过，不会被当成未实现外设。
	u.mu.Lock()
	v := u.regs[offset]
	u.mu.Unlock()
	return v
}

func (u *UART) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case uartFifo:
		c := byte(val)
		if _, err := u.tx.Write([]byte{c}); err == nil {
			u.txBytes++
		}
	case uartIntEna, uartConf1, uartMemConf, uartClkdiv:
		// 影子存储；INT_ENA/CONF1 变化会影响 RAW，需要重新折算中断。
		u.mu.Lock()
		u.regs[offset] = val
		u.mu.Unlock()
		u.syncIRQ()
	case uartIntClr:
		// TOUT 是锁存，写 1 清零；FULL 是电平，FIFO 降低后自然消失。
		u.mu.Lock()
		if val&uartIntRxfifoTout != 0 {
			u.tout = false
		}
		u.mu.Unlock()
		u.syncIRQ()
	default:
		// 其余配置寄存器做影子存储（读回一致）。真正用到的只有上面
		// FIFO/INT/CONF1，其余按“配置能读回”处理即可。
		u.mu.Lock()
		u.regs[offset] = val
		u.mu.Unlock()
	}
}
