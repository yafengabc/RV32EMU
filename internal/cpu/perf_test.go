package cpu

import (
	"rv32emu/internal/mem"
	"testing"
)

// BenchmarkTightLoop 用 Step() 测紧循环吞吐。
// 代码: addi x1, x0, 0; addi x1, x1, 1; bne x1, x0, -8
// 正确的 bne -8 编码是 0xFE0098E3 (小端: E3 98 00 FE)
func BenchmarkTightLoop(b *testing.B) {
	bus := mem.NewSystemBus()
	c := New(bus)
	code := []byte{
		0x93, 0x00, 0x00, 0x00, // addi x1, x0, 0
		0x93, 0x80, 0x10, 0x00, // addi x1, x1, 1
		0xE3, 0x9C, 0x00, 0xFE, // bne x1, x0, -8 (正确编码)
	}
	bus.LoadROM(0, code)
	c.SetPC(mem.ROMBase)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Step()
	}
}

// BenchmarkPiLoop 测含 M 扩展的计算密集循环（Step()）。
func BenchmarkPiLoop(b *testing.B) {
	bus := mem.NewSystemBus()
	c := New(bus)
	code := []byte{
		0x93, 0x00, 0xcf, 0x3f, // lui sp
		0x93, 0x00, 0x00, 0x00, // addi x5, x0, 0
		0x93, 0x00, 0x00, 0x00, // addi x6, x0, 0
		0xb7, 0x00, 0x10, 0x00, // lui x7, 0x10
		0xb7, 0x00, 0x02, 0x00, // lui x28, 0x2
		0x93, 0xe0, 0xc0, 0x27, // addi x28, x28, 0x710
		0x13, 0x10, 0x63, 0x00, // slli x29, x6, 1
		0x93, 0x80, 0x10, 0x01, // addi x29, x29, 1
		0xb7, 0x00, 0x10, 0x00, // lui x30, 0x10
		0x33, 0xd0, 0xd0, 0x02, // divu x30, x30, x29
		0xb3, 0x00, 0xe0, 0x02, // mul x31, x7, x30
		0xb3, 0x82, 0xf0, 0x00, // add x5, x5, x31
		0x93, 0x80, 0x10, 0x01, // addi x6, x6, 1
		0x33, 0x00, 0x70, 0x40, // sub x7, x0, x7
		0x63, 0x44, 0xc3, 0xfe, // blt x6, x28, loop
		0x13, 0x15, 0x50, 0x00, // slli a0, x5, 2
		0x73, 0x00, 0x10, 0x00, // ebreak
	}
	bus.LoadROM(0, code)
	c.SetPC(mem.ROMBase)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Step()
	}
}

// BenchmarkTightLoopRunHot 用 RunHot 跑紧循环，一次执行 10000 条。
func BenchmarkTightLoopRunHot(b *testing.B) {
	const batch = 10000
	bus := mem.NewSystemBus()
	c := New(bus)
	code := []byte{
		0x93, 0x00, 0x00, 0x00,
		0x93, 0x80, 0x10, 0x00,
		0xE3, 0x9C, 0x00, 0xFE,
	}
	bus.LoadROM(0, code)
	c.SetPC(mem.ROMBase)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.RunHot(batch)
		c.SetPC(mem.ROMBase) // 重置以便下一次
	}
}

// ---- 小工具：把 32-bit 指令打包进 []byte（小端） ----

func encI(imm int32, rs1, f3, rd int, op uint32) uint32 {
	return (uint32(imm&0xFFF) << 20) | uint32(rs1)<<15 | uint32(f3)<<12 | uint32(rd)<<7 | op
}
func encS(imm int32, rs2, rs1, f3 int, op uint32) uint32 {
	u := uint32(imm & 0xFFF)
	return ((u>>5)&0x7F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | uint32(f3)<<12 | (u&0x1F)<<7 | op
}
func encB(imm int32, rs2, rs1 int, f3 uint32, op uint32) uint32 {
	u := uint32(imm & 0x1FFF)
	return ((u>>12)&1)<<31 | ((u>>5)&0x3F)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 |
		uint32(f3)<<12 | ((u>>1)&0xF)<<8 | ((u>>11)&1)<<7 | op
}
func encR(f7, rs2, rs1, f3, rd int, op uint32) uint32 {
	return uint32(f7)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | uint32(f3)<<12 | uint32(rd)<<7 | op
}
func encU(imm uint32, rd int, op uint32) uint32 {
	return (imm << 12) | uint32(rd)<<7 | op
}

// encCB 编码 C.BEQZ/C.BNEZ（CB 格式），rs1p 取 0..7 对应 x8..x15。
func encCB(f3, rs1p int, imm int32) uint16 {
	u := uint32(imm) & 0x1FF
	var h uint32
	h = uint32(f3)<<13 | 1
	h |= ((u >> 8) & 1) << 12
	h |= ((u >> 3) & 3) << 10
	h |= uint32(rs1p) << 7
	h |= ((u >> 6) & 3) << 5
	h |= ((u >> 1) & 3) << 3
	h |= ((u >> 5) & 1) << 2
	return uint16(h)
}

// words 转成小端字节流。
func words(ws ...uint32) []byte {
	out := make([]byte, 0, len(ws)*4)
	for _, w := range ws {
		out = append(out, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
	}
	return out
}

// BenchmarkMemLoopRunHot 模拟真实固件的访存密集循环：
// 遍历 SRAM 中 256 个 word，读-加-写回。
//
//	lui  x2, 0x3FC80 ; addi x1, x0, 256 ; addi x3, x0, 0
//
// loop: lw x4, 0(x2) ; add x3, x3, x4 ; sw x3, 0(x2)
//
//	addi x2, x2, 4 ; addi x1, x1, -1 ; bne x1, x0, loop
func BenchmarkMemLoopRunHot(b *testing.B) {
	const batch = 10000
	bus := mem.NewSystemBus()
	c := New(bus)
	code := words(
		encU(0x3FC80, 2, 0x37),    // lui x2, 0x3FC80
		encI(0, 0, 0, 3, 0x13),    // addi x3, x0, 0
		encI(0, 2, 2, 4, 0x03),    // lw x4, 0(x2)
		encR(0, 4, 3, 0, 3, 0x33), // add x3, x3, x4
		encS(0, 3, 2, 2, 0x23),    // sw x3, 0(x2)
		encI(4, 2, 0, 2, 0x13),    // addi x2, x2, 4
		encI(-1, 1, 0, 1, 0x13),   // addi x1, x1, -1
		encB(-20, 0, 1, 1, 0x63),  // bne x1, x0, loop(-20)
	)
	bus.LoadROM(0, code)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SetPC(mem.ROMBase)
		c.SetReg(1, 1<<30)
		c.SetReg(2, mem.SRAMBase)
		c.RunHot(batch)
	}
}

// BenchmarkCompressedLoopRunHot 只用已内联的压缩指令（C.ADDI + C.BNEZ）。
// 每条 2 字节，考验取指/压缩解码路径。
func BenchmarkCompressedLoopRunHot(b *testing.B) {
	const batch = 10000
	bus := mem.NewSystemBus()
	c := New(bus)
	// 0x0000: c.addi x8, 1   (f3=000, rd=8, imm=1)
	// 0x0002: c.bnez x8, -2
	bus.Write(mem.ROMBase+0, 2, (8<<7)|(1<<2)|1)
	bus.Write(mem.ROMBase+2, 2, uint32(encCB(7, 0, -2)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SetPC(mem.ROMBase)
		c.RunHot(batch)
	}
}

// BenchmarkCompressedFallbackRunHot 用当前未内联的压缩指令（C.LW/C.SW/C.BNEZ）。
// 两条访存都 fallback 到 execC，用来量化 fallback 成本。
func BenchmarkCompressedFallbackRunHot(b *testing.B) {
	const batch = 10000
	bus := mem.NewSystemBus()
	c := New(bus)
	// c.lw x10, 0(x8)  : f3=010, rs1'=0(x8), rd'=2(x10), uimm=0
	// c.sw x10, 0(x8)  : f3=110, rs2'=2(x10), rs1'=0(x8), uimm=0
	lw := uint16(0x4000 | 2<<2)
	sw := uint16(0xC000 | 2<<2)
	bus.Write(mem.ROMBase+0, 2, uint32(lw))
	bus.Write(mem.ROMBase+2, 2, uint32(sw))
	bus.Write(mem.ROMBase+4, 2, uint32(encCB(7, 0, -4))) // c.bnez x8, -4
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SetPC(mem.ROMBase)
		c.SetReg(8, mem.SRAMBase)
		c.RunHot(batch)
	}
}
