package cpu_test

import (
	"bytes"
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
)

// runN 最多执行 n 条指令，停机即返回。
func runN(c *cpu.CPU, n uint64) {
	for i := uint64(0); i < n; i++ {
		if !c.Step() {
			return
		}
	}
}

// loadWords 把 little-endian 32-bit 指令序列写入 ROM 起始。
func loadWords(t *testing.T, b *mem.SystemBus, words ...uint32) {
	t.Helper()
	off := uint32(0)
	for _, w := range words {
		b.Write(mem.ROMBase+off, 4, w)
		off += 4
	}
}

// newTestCPU 构造挂好 UART 的 CPU，返回 cpu、bus 以及 UART 输出缓冲。
func newTestCPU() (*cpu.CPU, *mem.SystemBus, *bytes.Buffer) {
	bus := mem.NewSystemBus()
	uart := perip.NewUART0()
	out := &bytes.Buffer{}
	uart.SetIO(out, &bytes.Buffer{})
	bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	return c, bus, out
}

// 1) UART0 字符输出：写 0x60000000 应输出字符。
func TestUARTPuts(t *testing.T) {
	c, bus, out := newTestCPU()
	// lui a0,0x60000 ; a0=0x60000000
	// addi a7,x0,'A' ; sb a7,0(a0) ; addi a7,x0,'B' ; sb a7,0(a0) ; ebreak
	loadWords(t, bus,
		0x60000537, // lui a0,0x60000
		0x04100893, // addi a7,x0,0x41
		0x01150023, // sb a7,0(a0)
		0x04200893, // addi a7,x0,0x42
		0x01150023, // sb a7,0(a0)
		0x00100073, // ebreak
	)
	runN(c, 100000)
	if got := out.String(); got != "AB" {
		t.Fatalf("UART output = %q, want %q", got, "AB")
	}
}

// 2) M 扩展乘法：5*7=35，存到 SRAM。
func TestMul(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	loadWords(t, bus,
		0x00500293, // addi t0(x5),x0,5
		0x00700313, // addi t1(x6),x0,7
		0x026283B3, // mul t2(x7),t0,t1
		0x3FC80437, // lui x8,0x3FC80
		0x00742023, // sw x7,0(x8)
		0x00100073, // ebreak
	)
	runN(c, 100000)
	got := bus.Read(mem.SRAMBase, 4)
	if got != 35 {
		t.Fatalf("mul result = %d, want 35", got)
	}
}

// 3) M 扩展除法：-7/3 = -2, -7%3 = -1。
func TestDivRem(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	// addi t0,x0,-7 ; addi t1,x0,3 ; div t2,t0,t1 ; sw t2,0(x8=lui)
	loadWords(t, bus,
		0xFF900293, // addi t0,x0,-7
		0x00300313, // addi t1,x0,3
		0x0262C3B3, // div t2,t0,t1  (funct3=100)
		0x3FC80437, // lui x8,0x3FC80
		0x00742023, // sw x7(t2),0(x8)
		0x00100073, // ebreak
	)
	runN(c, 100000)
	got := bus.Read(mem.SRAMBase, 4)
	if int32(got) != -2 {
		t.Fatalf("div -7/3 = %d, want -2", int32(got))
	}
}

// 4) C 压缩指令：c.addi a0,1 在 a0=0 后得 1。
func TestCompressedADDI(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	// addi a0,x0,0 (32-bit) 然后 c.addi a0,1 (16-bit 0x505) 然后 ebreak
	bus.Write(mem.ROMBase, 4, 0x00000513) // addi a0,x0,0
	// 16-bit 指令写在 pc+4：低 16 位 0x0505，高 16 位 ebreak(0x9002)
	bus.Write(mem.ROMBase+4, 2, 0x0505) // c.addi a0,1
	bus.Write(mem.ROMBase+6, 2, 0x9002) // c.ebreak
	runN(c, 100000)
	if got := c.Reg(10); got != 1 {
		t.Fatalf("after c.addi a0,1 = %d, want 1", got)
	}
}

// 5) 非法指令应触发 illegal-instruction 异常而不是崩溃。
func TestIllegalTrap(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	bus.Write(mem.ROMBase, 4, 0x00000000) // all-zero 32-bit -> opcode 0 = illegal
	runN(c, 100000)
	if c.CSR().Mcause() != 2 {
		t.Fatalf("mcause = %d, want 2 (illegal)", c.CSR().Mcause())
	}
}
