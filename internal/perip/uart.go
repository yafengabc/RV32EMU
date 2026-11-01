package perip

import (
	"fmt"
	"io"
	"os"
)

// ESP32-C3 UART0 基址（技术参考手册）。
const (
	UART0Base uint32 = 0x60000000
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
)

// UART0：TX FIFO 写一个字节即输出到模拟器 stdout；RX 从 stdin 读。
type UART struct {
	tx io.Writer
	rx io.Reader
	// 内部状态（大部分寄存器最小化为可写可清的影子）
	intEna uint32
	intRaw uint32
}

func NewUART0() *UART {
	return &UART{tx: os.Stdout, rx: os.Stdin}
}

// SetIO 注入测试用 IO。
func (u *UART) SetIO(tx io.Writer, rx io.Reader) {
	u.tx = tx
	u.rx = rx
}

func (u *UART) Name() string { return "UART0" }

func (u *UART) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case uartFifo:
		// RX：阻塞读一个字节；读不到返回 0（真实硬件有 RXFIFO 空标志，这里最小化）
		var b [1]byte
		n, _ := u.rx.Read(b[:])
		if n > 0 {
			return uint32(b[0])
		}
		return 0
	case uartIntSt:
		return u.intRaw & u.intEna
	case uartStatus:
		// TXFIFO 空/可用位置位，让固件认为可以一直发
		return 0x000C0000
	}
	return 0
}

func (u *UART) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case uartFifo:
		c := byte(val)
		_, _ = u.tx.Write([]byte{c})
	case uartIntEna:
		u.intEna = val
	case uartIntClr:
		u.intRaw &^= val
	case uartClkdiv:
		// 波特率分频最小化，不影响字符 IO
	default:
		fmt.Fprintf(os.Stderr, "[uart] unhandled write off=0x%02X val=0x%08X\n", offset, val)
	}
}
