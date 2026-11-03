package perip

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// TestUARTInterruptRouting 覆盖 UART0 RX 中断：source 21 → INTMTX line 3，
// FULL 电平位、TOUT 锁存位、INT_CLR 与 FIFO 读空后的撤销。
func TestUARTInterruptRouting(t *testing.T) {
	bus := mem.NewSystemBus()
	uart := NewUART()
	intc := NewC3Intc()
	bus.Attach(mem.DeviceRange{Base: UART0Base, Size: UARTSize, Dev: uart})
	bus.Attach(mem.DeviceRange{Base: INTMTXBase, Size: INTMTXSize, Dev: intc})
	intc.SetCPU(cpu.New(bus))
	uart.SetIntc(intc)

	// 源 21 → CPU line 3，优先级 1，阈值 1。
	intc.Write(uartSrc*4, 4, 3)
	intc.Write(intcPriBase+4*3, 4, 1)
	intc.Write(intcThresh, 4, 1)
	intc.Write(intcEna, 4, 1<<3)

	// 阈值 4 字节，使能 RX 超时 + FIFO 满。
	uart.Write(uartConf1, 4, 4|uartConf1RxToutEn)
	uart.Write(uartIntEna, 4, uartIntRxfifoFull|uartIntRxfifoTout)

	uart.PushRX([]byte("abcdef"))
	uart.RxFeed(6)

	if !intc.pending[uartSrc] {
		t.Fatal("UART source not raised")
	}
	if got := intc.eipStatus(); got != 1<<3 {
		t.Fatalf("EIP status = 0x%X, want line 3", got)
	}
	if got := uart.Read(uartIntRaw, 4); got != uartIntRxfifoFull|uartIntRxfifoTout {
		t.Fatalf("RAW = 0x%X", got)
	}
	if got := uart.Read(uartIntSt, 4); got != uartIntRxfifoFull|uartIntRxfifoTout {
		t.Fatalf("ST = 0x%X", got)
	}

	// 写 INT_CLR 清 TOUT 锁存；FIFO 仍 ≥ 阈值，FULL 保持。
	uart.Write(uartIntClr, 4, uartIntRxfifoTout)
	if got := uart.Read(uartIntRaw, 4); got != uartIntRxfifoFull {
		t.Fatalf("RAW after clear = 0x%X, want FULL only", got)
	}

	// 读空 FIFO 后 FULL 撤销，source 也随之撤销。
	for i := 0; i < 6; i++ {
		uart.Read(uartFifo, 1)
	}
	if got := uart.Read(uartIntRaw, 4); got != 0 {
		t.Fatalf("RAW after drain = 0x%X", got)
	}
	if intc.pending[uartSrc] {
		t.Fatal("UART source still pending after drain")
	}
}
