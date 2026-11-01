package cpu_test

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// 本文件用「手写已知正确的压缩指令位模式」验证解码器，
// 并与等价的 32-bit 指令行为对齐。

const sramTop uint32 = 0x3FC80000

// newCPUWithSP 构造 CPU，PC=ROM 起始，sp 指向 SRAM 中部。
func newCPUWithSP() (*cpu.CPU, *mem.SystemBus) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	c.SetReg(2, sramTop+0x100)
	return c, bus
}

func put16(bus *mem.SystemBus, off uint32, h uint16) {
	bus.Write(mem.ROMBase+off, 2, uint32(h))
}

// 1) C.LWSP / C.SWSP 的 SP 相对偏移必须正确。
// 规范位布局：f3|uimm[5]|rd|uimm[4:2]|uimm[7:6]|op
func TestCompressedLWSPSWSP(t *testing.T) {
	c, bus := newCPUWithSP()
	sp := c.Reg(2)
	bus.Write(sp+0, 4, 0x11)
	bus.Write(sp+4, 4, 0x22)
	bus.Write(sp+8, 4, 0x33)

	// c.lwsp a0, 4(sp) = 0x4512
	put16(bus, 0, 0x4512)
	// c.ebreak = 0x9002
	put16(bus, 2, 0x9002)

	c.Step()
	if got := c.Reg(10); got != 0x22 {
		t.Fatalf("c.lwsp a0,4(sp) = 0x%X, want 0x22（读到 sp+4）", got)
	}

	// c.swsp a0, 8(sp) = 0xC02A + (8<<9)?  用规范布局计算：uimm[5:2]=2 → bit10
	c2, bus2 := newCPUWithSP()
	c2.SetReg(10, 0xAB)
	put16(bus2, 0, 0xC22A) // c.swsp a0, 4(sp)
	put16(bus2, 2, 0x9002)
	c2.Step()
	if got := bus2.Read(c2.Reg(2)+4, 4); got != 0xAB {
		t.Fatalf("c.swsp a0,4(sp) 写到 sp+4 = 0x%X, want 0xAB", got)
	}
}

// 2) C.BEQZ / C.BNEZ 的分支偏移必须按 CB 格式解析。
// 规范：imm[8]→bit12, imm[4:3]→bit11:10, imm[7:6]→bit6:5, imm[2:1]→bit4:3, imm[5]→bit2
func encodeCB(f3 uint32, rs1p, imm int32) uint16 {
	u := uint32(imm) & 0x1FF
	var h uint32
	h = (f3 << 13) | (1 << 0) // quadrant 1
	h |= ((u >> 8) & 1) << 12
	h |= ((u >> 3) & 3) << 10
	h |= uint32(rs1p) << 7
	h |= ((u >> 6) & 3) << 5
	h |= ((u >> 1) & 3) << 3
	h |= ((u >> 5) & 1) << 2
	return uint16(h)
}

// c.bnez a0, +6 后应跳过 6 字节，落到 pc+8 的那条指令。
func TestCompressedBranchOffset(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	c.SetReg(10, 1) // a0 != 0，nz 分支成立

	// 0x40000000: c.bnez a0, +6
	put16(bus, 0, encodeCB(7, 2, 6)) // rs1'=2 → x10
	// 0x40000002: addi a1,x0,0x77  （应被跳过）
	bus.Write(mem.ROMBase+2, 4, 0x07700593)
	// 0x40000006: c.ebreak
	put16(bus, 6, 0x9002)

	c.Step()
	if got := c.PC(); got != mem.ROMBase+6 {
		t.Fatalf("c.bnez a0,+6 后 pc=0x%08X, want 0x%08X（说明 CB 偏移解析错误）",
			got, mem.ROMBase+6)
	}
}

// 3) C.J / C.JAL 的 12-bit 偏移。
func encodeCJ(f3 uint32, imm int32) uint16 {
	u := uint32(imm) & 0xFFF
	var h uint32
	h = (f3 << 13) | 1
	h |= ((u >> 11) & 1) << 12
	h |= ((u >> 4) & 1) << 11
	h |= ((u >> 8) & 3) << 9
	h |= ((u >> 10) & 1) << 8
	h |= ((u >> 6) & 1) << 7
	h |= ((u >> 7) & 1) << 6
	h |= ((u >> 1) & 7) << 3
	h |= ((u >> 5) & 1) << 2
	return uint16(h)
}

func TestCompressedJumpOffset(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)

	// 0x40000000: c.j +8
	put16(bus, 0, encodeCJ(5, 8))
	// 跳过的垃圾
	bus.Write(mem.ROMBase+2, 4, 0x00100073)
	// 0x40000008: c.ebreak
	put16(bus, 8, 0x9002)

	c.Step()
	if got := c.PC(); got != mem.ROMBase+8 {
		t.Fatalf("c.j +8 后 pc=0x%08X, want 0x%08X", got, mem.ROMBase+8)
	}
}

// encodeCL 编码 C.LW/C.SW（quadrant 0）：f3=2 为 LW，f3=6 为 SW。
func encodeCL(f3 uint32, rs1p, rs2p int32, imm int32) uint16 {
	u := uint32(imm) & 0x7F
	var h uint32
	h = (f3 << 13) | 0 // quadrant 0
	h |= ((u >> 3) & 0x7) << 10
	h |= uint32(rs1p) << 7
	h |= ((u >> 6) & 1) << 5
	h |= ((u >> 2) & 1) << 6
	h |= uint32(rs2p) << 2
	return uint16(h)
}

// 4) C.LW / C.SW 的 uimm[5:3]/uimm[2]/uimm[6] 必须位域正确。
func TestCompressedLWSW(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	base := sramTop + 0x100
	c.SetReg(8, base)
	bus.Write(base+40, 4, 0xABCD)

	put16(bus, 0, encodeCL(2, 0, 2, 40)) // c.lw x10, 40(x8)
	put16(bus, 2, 0x9002)
	c.Step()
	if got := c.Reg(10); got != 0xABCD {
		t.Fatalf("c.lw x10,40(x8) = 0x%X, want 0xABCD（LW 偏移位域错误）", got)
	}

	bus2 := mem.NewSystemBus()
	c2 := cpu.New(bus2)
	c2.SetPC(mem.ROMBase)
	c2.SetReg(8, base)
	c2.SetReg(10, 0x1234)
	put16(bus2, 0, encodeCL(6, 0, 2, 40)) // c.sw x10, 40(x8)
	put16(bus2, 2, 0x9002)
	c2.Step()
	if got := bus2.Read(base+40, 4); got != 0x1234 {
		t.Fatalf("c.sw x10,40(x8) 写到 = 0x%X, want 0x1234（SW 偏移位域错误）", got)
	}
}

// 5) C.ADDI16SP 的 nzimm 位域。
func TestCompressedADDI16SP(t *testing.T) {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)
	c.SetReg(2, sramTop+0x100)

	// c.addi16sp -16（nzimm 的 6-bit 值为 -1）
	var h uint32 = (0x3 << 13) | (2 << 7) | 1 // f3=011, rd=2
	v := uint32(0x3F)                         // nzimm[9:4] = -1
	h |= ((v >> 5) & 1) << 12
	h |= ((v >> 3) & 3) << 3
	h |= ((v >> 2) & 1) << 5
	h |= ((v >> 1) & 1) << 2
	h |= (v & 1) << 6
	put16(bus, 0, uint16(h))
	put16(bus, 2, 0x9002)

	c.Step()
	if want, got := sramTop+0x100-16, c.Reg(2); got != want {
		t.Fatalf("c.addi16sp -16 后 sp=0x%08X, want 0x%08X", got, want)
	}
}
