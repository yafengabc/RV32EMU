// genqemu: 生成 QEMU virt 固件：在 DRAM 0x80000000 跑固定循环，
// 结束后写 poweroff 寄存器（0x100000）让 QEMU 退出，便于计时。
package main

import (
	"encoding/binary"
	"os"
)

func iOp(rd, rs1 int, imm, funct3 uint32) uint32 {
	return ((imm & 0xFFF) << 20) | uint32(rs1)<<15 | funct3<<12 | uint32(rd)<<7 | 0x13
}

func main() {
	base := uint32(0x80000000)
	var code []uint32
	add := func(w uint32) { code = append(code, w) }

	add(iOp(5, 0, 0, 0))                  // cnt=0
	add(0x09890537)                        // lui x6,0x989 -> 0x989000
	add(iOp(6, 6, 0x680, 0))               // addi x6,x6,0x680 -> 10,000,000
	add(0x00128293)                        // loop: addi cnt,cnt,1
	add(0xFE62CCE3)                        // blt cnt,limit,loop(-8)
	// poweroff: lui a7,0x10 ; sw zero,0(a7)
	add(0x00100537)                        // lui a7,0x10  -> 0x100000
	add(0x00052023)                        // sw zero,0(a7)
	add(0x00100073)                        // ebreak

	text := make([]byte, 0, len(code)*4)
	for _, w := range code {
		text = binary.LittleEndian.AppendUint32(text, w)
	}

	buf := make([]byte, 0, 52+32+len(text))
	w16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); buf = append(buf, b[:]...) }
	w32 := func(v uint32) { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); buf = append(buf, b[:]...) }
	buf = append(buf, []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}...)
	w16(2)
	w16(243)
	w32(1)
	w32(base)
	w32(52)
	w32(0)
	w32(0)
	w16(52)
	w16(32)
	w16(1)
	w16(0)
	w16(0)
	w16(0)
	w32(1) // PT_LOAD
	w32(84)
	w32(base)
	w32(base)
	w32(uint32(len(text)))
	w32(uint32(len(text)))
	w32(7)
	w32(4)
	buf = append(buf, text...)
	os.WriteFile("qemu_fw.elf", buf, 0644)
}
