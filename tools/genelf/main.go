// genelf: 把一段手写 RV32IMC 固件汇编编码成结构完整的 32-bit LE ELF，
// 供 rv32emu -elf 加载。内存布局对齐 ESP32-C3：代码 0x40000000，SRAM 0x3FC80000。
package main

import (
	"encoding/binary"
	"os"
)

// lui: imm 为高 20 位（地址>>12）
func lui(rd int, imm uint32) uint32 {
	return imm<<12 | uint32(rd)<<7 | 0x37
}

// 指令编码小助手
func iType(op uint32, rd int, rs1 int, imm, funct3 uint32) uint32 {
	return imm<<20 | uint32(rs1)<<15 | funct3<<12 | uint32(rd)<<7 | op
}
func sType(op uint32, rs1, rs2 int, imm, funct3 uint32) uint32 {
	return (imm&0xFE0)<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | funct3<<12 | (imm&0x1F)<<7 | op
}
func rType(op uint32, rd, rs1, rs2 int, funct3, funct7 uint32) uint32 {
	return funct7<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | funct3<<12 | uint32(rd)<<7 | op
}

func main() {
	const (
		opIMM = 0x13
		opOP  = 0x33
		opLUI = 0x37
		opST  = 0x23
		opBR  = 0x63
	)
	// 寄存器
	const sp, a0, a1, a7, t0, t1, t2, s8 = 2, 10, 11, 17, 5, 6, 7, 24

	code := []uint32{}
	add := func(w uint32) { code = append(code, w) }

	// ---- _start ----
	add(lui(sp, 0x3FCa0))        // sp = 0x3FCa0000（SRAM 顶）
	add(iType(opIMM, a7, 0, 0, 0)) // a7 = 0
	// 调用 main 前先打印：直接在 main 里做

	// ---- main：打印字符串，做乘除，停机 ----
	// lui a1, 0x60000 ; a1 = UART0 基址
	add(lui(a1, 0x60000))

	// 打印 "C3-OK " （逐字符 sb）
	for _, c := range "C3-OK " {
		add(iType(opIMM, a7, 0, uint32(uint8(c)), 0)) // addi a7, x0, 'c'
		add(sType(opST, a1, a7, 0, 0))                 // sb a7, 0(a1)
	}

	// 乘除：t0=123, t1=45, t2=t0*t1=5535（立即数 < 2048，可装 addi）
	add(iType(opIMM, t0, 0, 123, 0))
	add(iType(opIMM, t1, 0, 45, 0))
	add(rType(opOP, t2, t0, t1, 0, 0x01)) // mul t2,t0,t1
	// 把结果写到 SRAM 0x3FC80000 验证
	add(lui(s8, 0x3FC80))
	add(sType(opST, s8, t2, 0, 2)) // sw t2,0(s8)
	// 再打印一个字符 'M'（mul done）
	add(iType(opIMM, a7, 0, 'M', 0))
	add(sType(opST, a1, a7, 0, 0))

	// 停机
	add(iType(0x73, 0, 0, 0, 0)) // ecall/ebreak? 用 ebreak=0x00100073
	// 上面 iType 拼出的是 ecall；正确 ebreak：
	code[len(code)-1] = 0x00100073

	// 序列终止后再补一条 ebreak 占位，避免取指越界
	add(0x00100073)

	// 编码为小端字节
	text := make([]byte, 0, len(code)*4)
	for _, w := range code {
		text = binary.LittleEndian.AppendUint32(text, w)
	}

	// ---- 写 ELF32 LE ----
	entry := uint32(0x40000000)
	phoff := 52
	// 一个 PT_LOAD 覆盖 .text，直接落在入口地址
	filesz := len(text)

	buf := make([]byte, 0, phoff+32+filesz)
	// Ehdr
	ident := []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	buf = append(buf, ident...)
	w16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); buf = append(buf, b[:]...) }
	w32 := func(v uint32) { var b [4]byte; binary.LittleEndian.PutUint32(b[:], v); buf = append(buf, b[:]...) }
	w16(2)    // e_type EXEC
	w16(243)  // e_machine EM_RISCV
	w32(1)    // e_version
	w32(entry)
	w32(uint32(phoff))
	w32(0) // e_shoff
	w32(0) // e_flags
	w16(52)
	w16(32)
	w16(1)
	w16(0)
	w16(0)
	w16(0)

	// Phdr
	phoffNow := uint32(phoff)
	_ = phoffNow
	w32(1)           // PT_LOAD
	w32(uint32(phoff + 32)) // p_offset
	w32(entry)       // p_vaddr
	w32(entry)       // p_paddr
	w32(uint32(filesz))
	w32(uint32(filesz)) // memsz
	w32(7)           // RWX
	w32(4)           // align

	buf = append(buf, text...)

	if err := os.WriteFile("c3demo.elf", buf, 0644); err != nil {
		panic(err)
	}
}
