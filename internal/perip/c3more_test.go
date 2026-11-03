package perip

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

func newTestIntc() (*mem.SystemBus, *C3Intc) {
	bus := mem.NewSystemBus()
	intc := NewC3Intc()
	bus.Attach(mem.DeviceRange{Base: INTMTXBase, Size: INTMTXSize, Dev: intc})
	intc.SetCPU(cpu.New(bus))
	return bus, intc
}

// TestSPI2Loopback 覆盖 GP-SPI 的 CPU 传输：USR 自清、TRANS_DONE 置位、
// INT_CLR 清除，以及“只收”时 W 寄存器被清零。
func TestSPI2Loopback(t *testing.T) {
	_, intc := newTestIntc()
	s := NewSPI2()
	s.SetIntc(intc)

	// 32 bit 传输（MS_DLEN 是 bit_num-1），同时开 MOSI/MISO → 环回。
	s.Write(gspiMsDlenReg, 4, 31)
	s.Write(gspiUserReg, 4, gspiUserMosi|gspiUserMiso)
	s.Write(gspiW0Reg, 4, 0xA1B2C3D4)
	s.Write(gspiIntEna, 4, gspiTransDone)
	s.Write(gspiCmdReg, 4, gspiCmdUsr)

	if got := s.Read(gspiCmdReg, 4); got&gspiCmdUsr != 0 {
		t.Fatalf("USR not self-cleared: 0x%X", got)
	}
	if got := s.Read(gspiW0Reg, 4); got != 0xA1B2C3D4 {
		t.Fatalf("loopback data = 0x%X", got)
	}
	if got := s.Read(gspiIntRaw, 4); got != gspiTransDone {
		t.Fatalf("RAW = 0x%X, want TRANS_DONE", got)
	}
	if got := s.Read(gspiIntSt, 4); got != gspiTransDone {
		t.Fatalf("ST = 0x%X, want TRANS_DONE", got)
	}
	if !intc.pending[spi2Src] {
		t.Fatal("SPI2 source not raised")
	}
	s.Write(gspiIntClr, 4, gspiTransDone)
	if got := s.Read(gspiIntRaw, 4); got != 0 {
		t.Fatalf("RAW after clear = 0x%X", got)
	}
	if intc.pending[spi2Src] {
		t.Fatal("SPI2 source still pending")
	}

	// 只开 MISO：没有外部器件时接收数据清零。
	s.Write(gspiUserReg, 4, gspiUserMiso)
	s.Write(gspiW0Reg, 4, 0x12345678)
	s.Write(gspiCmdReg, 4, gspiCmdUsr)
	if got := s.Read(gspiW0Reg, 4); got != 0 {
		t.Fatalf("MISO-only data = 0x%X, want 0", got)
	}
}

// TestI2CCommandList 覆盖 I2C0 的命令列表：WRITE 消费 TX FIFO、READ 填充
// RX FIFO、每条命令置 DONE，并产生传输完成中断。
func TestI2CCommandList(t *testing.T) {
	_, intc := newTestIntc()
	i := NewI2C()
	i.SetIntc(intc)

	i.Write(i2cDataReg, 4, 0x11)
	i.Write(i2cDataReg, 4, 0x22)
	i.Write(i2cComd0Reg, 4, i2cOpWrite<<i2cOpcodeShift|2)
	i.Write(i2cComd0Reg+4, 4, i2cOpRead<<i2cOpcodeShift|1)
	i.Write(i2cComd0Reg+8, 4, i2cOpStop<<i2cOpcodeShift)
	i.Write(i2cIntEnaReg, 4, i2cIntTransComplete)
	i.Write(i2cCtrReg, 4, i2cTransStart)

	for n := 0; n < 3; n++ {
		if got := i.Read(i2cComd0Reg+uint32(4*n), 4); got&i2cCmdDone == 0 {
			t.Fatalf("COMD%d DONE not set: 0x%X", n, got)
		}
	}
	if got := i.Read(i2cCtrReg, 4); got&i2cTransStart != 0 {
		t.Fatalf("TRANS_START not self-cleared: 0x%X", got)
	}
	sr := i.Read(i2cSrReg, 4)
	if tx := (sr >> 18) & 0x3F; tx != 0 {
		t.Fatalf("TXFIFO_CNT = %d, want 0", tx)
	}
	if rx := (sr >> 8) & 0x3F; rx != 1 {
		t.Fatalf("RXFIFO_CNT = %d, want 1", rx)
	}
	if got := i.Read(i2cDataReg, 4); got != 0 {
		t.Fatalf("RX data = 0x%X, want 0", got)
	}
	if got := i.Read(i2cSrReg, 4); (got>>8)&0x3F != 0 {
		t.Fatalf("RXFIFO_CNT after read = %d", (got>>8)&0x3F)
	}

	raw := i.Read(i2cIntRawReg, 4)
	want := i2cIntTransStart | i2cIntTransComplete | i2cIntEndDetect | i2cIntTxWm
	if raw&want != want {
		t.Fatalf("RAW = 0x%X, want bits 0x%X", raw, want)
	}
	if got := i.Read(i2cIntStReg, 4); got&i2cIntTransComplete == 0 {
		t.Fatalf("ST = 0x%X, want TRANS_COMPLETE", got)
	}
	if !intc.pending[i2cSrc] {
		t.Fatal("I2C source not raised")
	}
	i.Write(i2cIntClrReg, 4, raw)
	if got := i.Read(i2cIntRawReg, 4); got != 0 {
		t.Fatalf("RAW after clear = 0x%X", got)
	}
	if intc.pending[i2cSrc] {
		t.Fatal("I2C source still pending")
	}

	// FIFO 复位位是自清的，且真的清空 FIFO。
	i.Write(i2cDataReg, 4, 0x33)
	i.Write(i2cFifoConf, 4, i2cTxFifoRst|i2cRxFifoRst)
	if got := i.Read(i2cFifoConf, 4); got&(i2cTxFifoRst|i2cRxFifoRst) != 0 {
		t.Fatalf("FIFO reset bits not self-cleared: 0x%X", got)
	}
	if sr := i.Read(i2cSrReg, 4); sr != 0 {
		t.Fatalf("SR after FIFO reset = 0x%X", sr)
	}
}

// TestExtrasShadowWindows 覆盖寄存器影子与新增外设窗口的挂载：
// SYSTEM/IO_MUX 等写入后能读回，UART1 写入不报未映射错误。
func TestExtrasShadowWindows(t *testing.T) {
	bus, intc := newTestIntc()
	NewExtras(bus, intc)

	for _, tc := range []struct {
		base uint32
		off  uint32
		val  uint32
	}{
		{SYSTEMBase, 0x58, 0x12345678},
		{SENSITIVEBase, 0x10, 0x0F0F0F0F},
		{IOMUXBase, 4 * 5, 0x1D00},
		{SYSCONBase, 0x20, 0xCAFEBABE},
		{LEDCBase, 0x08, 0x0000FFFF},
		{GDMABase, 0x00, 0x80000000},
	} {
		bus.Write(tc.base+tc.off, 4, tc.val)
		if got := bus.Read(tc.base+tc.off, 4); got != tc.val {
			t.Fatalf("shadow @0x%08X = 0x%X, want 0x%X", tc.base+tc.off, got, tc.val)
		}
	}

	// UART1 的 TX FIFO 是一个黑洞：写入成功，STATUS 报告 FIFO 空闲。
	bus.Write(UART1Base+uartFifo, 1, 'x')
	if got := bus.Read(UART1Base+uartStatus, 4); got != 0 {
		t.Fatalf("UART1 STATUS = 0x%X, want 0", got)
	}
	if NewUART1().source() != uart1Src {
		t.Fatalf("UART1 source = %d, want %d", NewUART1().source(), uart1Src)
	}
}
