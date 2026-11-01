// genfib: 生成递归 fib(n) 固件。sp 指向 SRAM 顶部，调用 fib(n)，结果在 a0 后 ebreak。
package main

import (
	"encoding/binary"
	"os"
)

var text []uint32

func emit(w uint32) { text = append(text, w) }

// 编码助手
func iType(rd, rs1 int, imm int32, f3 uint32, op uint32) uint32 {
	return (uint32(imm&0xFFF) << 20) | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | op
}
func rType(rd, rs1, rs2 int, f3, f7 uint32) uint32 {
	return f7<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | 0x33
}
func sType(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0xFFF)
	return (u&0xFE0)<<20 | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | (u & 0x1F)<<7 | 0x23
}
func bType(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0x1FFF)
	return ((u >> 12) & 1) << 31 |   // imm[12]
		((u >> 11) & 1) << 7 |      // imm[11]
		((u >> 5) & 0x3F) << 25 |   // imm[10:5]
		uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 |
		((u >> 1) & 0xF) << 8 | // imm[4:1]
		0x63
}
func uType(rd int, imm20 uint32) uint32 { return imm20<<12 | uint32(rd)<<7 | 0x37 }
func jType(rd int, imm int32) uint32 {
	u := uint32(imm & 0x1FFFFF)
	return ((u >> 20) & 1) << 31 |
		((u >> 1) & 0x3FF) << 21 |
		((u >> 11) & 1) << 20 |
		((u >> 12) & 0xFF) << 12 | uint32(rd)<<7 | 0x6F
}

func main() {
	const (
		sp  = 2
		ra  = 1
		a0  = 10
		s0  = 9
		t0  = 5
		t1  = 6
		n   = 28
	)

	// 0: lui sp, 0x3FCFF  (sp = 0x3FCFF000 = SRAM 顶部)
	emit(uType(sp, 0x3FCFF))
	// 4: addi a0, x0, n
	emit(iType(a0, 0, n, 0, 0x13))
	// 8: jal ra, fib   (fib 在字节 20)
	emit(jType(ra, 20-8))
	// 12: ebreak
	emit(0x00100073)
	// 16: 死循环（兜底）
	emit(jType(0, -4))

	// fib @20
	emit(iType(sp, sp, -16, 0, 0x13))   // 20: addi sp,sp,-16
	emit(sType(sp, ra, 0, 2))            // 24: sw ra,0(sp)
	emit(sType(sp, s0, 4, 2))            // 28: sw s0,4(sp)
	emit(iType(s0, a0, 0, 0, 0x13))      // 32: addi s0,a0,0
	emit(iType(t0, 0, 2, 0, 0x13))       // 36: addi t0,x0,2
	// 40: blt a0,t0, base   base 在后面（字节 76）
	var bltOff int32 = 0
	bltIdx := len(text)
	emit(bType(a0, t0, bltOff, 4))       // 40
	// 44: addi a0,a0,-1
	emit(iType(a0, a0, -1, 0, 0x13))
	// 48: jal ra, fib   (从 48 到 fib@20)
	emit(jType(ra, 20-48))
	// 52: sw a0,8(sp)
	emit(sType(sp, a0, 8, 2))
	// 56: addi a0,s0,-2
	emit(iType(a0, s0, -2, 0, 0x13))
	// 60: jal ra, fib
	emit(jType(ra, 20-60))
	// 64: addi t1,a0,0
	emit(iType(t1, a0, 0, 0, 0x13))
	// 68: lw t0,8(sp)
	emit(iType(t0, sp, 8, 2, 0x03))
	// 72: add a0,t0,t1
	emit(rType(a0, t0, t1, 0, 0))
	// 76: j ret   (jal x0, ret@84)
	emit(jType(0, 84-76))
	// base @80: addi a0,s0,0
	emit(iType(a0, s0, 0, 0, 0x13))
	// ret @84: lw ra,0(sp)
	emit(iType(ra, sp, 0, 2, 0x03))
	// 88: lw s0,4(sp)
	emit(iType(s0, sp, 4, 2, 0x03))
	// 92: addi sp,sp,16
	emit(iType(sp, sp, 16, 0, 0x13))
	// 96: ret = jalr x0,ra,0
	emit(iType(0, ra, 0, 0, 0x67))

	// 回填 blt offset：从 40 到 base@80
	bltOff = int32(80 - 40)
	text[bltIdx] = bType(a0, t0, bltOff, 4)

	// 打包 ELF（ET_EXEC, entry=0x40000000）
	base := uint32(0x40000000)
	textBytes := make([]byte, 0, len(text)*4)
	for _, w := range text {
		textBytes = binary.LittleEndian.AppendUint32(textBytes, w)
	}
	buf := make([]byte, 0, 52+32+len(textBytes))
	w16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); buf = append(buf, b[:]...) }
	buf = append(buf, []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}...)
	w16(2)
	w16(243)
	var b4 [4]byte
	put32 := func(v uint32) { binary.LittleEndian.PutUint32(b4[:], v); buf = append(buf, b4[:]...) }
	put32(1)
	put32(base)
	put32(52)
	put32(0)
	put32(0)
	w16(52)
	w16(32)
	w16(1)
	w16(0)
	w16(0)
	w16(0)
	put32(1) // PT_LOAD
	put32(84)
	put32(base)
	put32(base)
	put32(uint32(len(textBytes)))
	put32(uint32(len(textBytes)))
	put32(7)
	put32(4)
	buf = append(buf, textBytes...)
	os.WriteFile("fib.elf", buf, 0644)
}
