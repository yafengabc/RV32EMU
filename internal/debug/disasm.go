package debug

import "fmt"

var abi = []string{
	"zero", "ra", "sp", "gp", "tp", "t0", "t1", "t2",
	"s0", "s1", "a0", "a1", "a2", "a3", "a4", "a5",
	"a6", "a7", "s2", "s3", "s4", "s5", "s6", "s7",
	"s8", "s9", "s10", "s11", "t3", "t4", "t5", "t6",
}

func rn(i int) string {
	if i >= 0 && i < len(abi) {
		return abi[i]
	}
	return fmt.Sprintf("x%d", i)
}

// DisasmI 反汇编一条 32-bit 指令为可读字符串。
func DisasmI(pc, instr uint32) string {
	op := instr & 0x7f
	rd := int((instr >> 7) & 0x1f)
	f3 := (instr >> 12) & 0x7
	rs1 := int((instr >> 15) & 0x1f)
	rs2 := int((instr >> 20) & 0x1f)
	immI := signExt(instr>>20, 12)

	switch op {
	case 0x37:
		return fmt.Sprintf("lui   %s, 0x%x", rn(rd), instr&0xfffff000)
	case 0x17:
		return fmt.Sprintf("auipc %s, 0x%x", rn(rd), instr&0xfffff000)
	case 0x6f:
		return fmt.Sprintf("jal   %s, 0x%x", rn(rd), pc+immJ(instr))
	case 0x67:
		return fmt.Sprintf("jalr  %s, %s(%d)", rn(rd), rn(rs1), immI)
	case 0x63:
		return branchMnemonic(f3, pc, instr, rs1, rs2)
	case 0x03:
		return loadMnemonic(f3, rd, rs1, immI)
	case 0x23:
		return storeMnemonic(f3, rs1, rs2, immS(instr))
	case 0x13:
		return aluImmMnemonic(f3, rd, rs1, immI, instr)
	case 0x33:
		return aluRegMnemonic(f3, funct7(instr), rd, rs1, rs2)
	case 0x73:
		return sysMnemonic(instr, f3, rd, rs1)
	}
	return fmt.Sprintf(".word 0x%08X", instr)
}

// DisasmC 反汇编一条 16-bit 指令。
func DisasmC(in uint16) string {
	return fmt.Sprintf(".c 0x%04X", in)
}

// ---- helpers ----
func signExt(x uint32, n uint) uint32 {
	if x&(1<<(n-1)) != 0 {
		return x | (^uint32(0) << n)
	}
	return x
}
func immJ(i uint32) uint32 {
	return signExt(((i>>31)&1)<<20|((i>>12)&0xff)<<12|((i>>20)&1)<<11|((i>>21)&0x3ff)<<1, 21)
}
func immS(i uint32) uint32 {
	return signExt(((i>>25)<<5)|((i>>7)&0x1f), 12)
}
func funct7(i uint32) uint32 { return i >> 25 }

func branchMnemonic(f3, pc, instr uint32, rs1, rs2 int) string {
	base := pc + signExt(((instr>>31)&1)<<12|((instr>>7)&1)<<11|((instr>>25)&0x3f)<<5|((instr>>8)&0xf)<<1, 13)
	m := []string{"beq", "bne", "?", "?", "blt", "bge", "bltu", "bgeu"}
	return fmt.Sprintf("%s  %s, %s, 0x%x", m[f3], rn(rs1), rn(rs2), base)
}
func loadMnemonic(f3 uint32, rd, rs1 int, imm uint32) string {
	m := []string{"lb", "lh", "lw", "?", "lbu", "lhu", "?", "?"}
	return fmt.Sprintf("%s  %s, %d(%s)", m[f3], rn(rd), int(int32(imm)), rn(rs1))
}
func storeMnemonic(f3 uint32, rs1, rs2 int, imm uint32) string {
	m := []string{"sb", "sh", "sw", "?", "?", "?", "?", "?"}
	return fmt.Sprintf("%s  %s, %d(%s)", m[f3], rn(rs2), int(int32(imm)), rn(rs1))
}
func aluImmMnemonic(f3 uint32, rd, rs1 int, imm, instr uint32) string {
	switch f3 {
	case 0:
		return fmt.Sprintf("addi  %s, %s, %d", rn(rd), rn(rs1), int(int32(imm)))
	case 2:
		return fmt.Sprintf("slti  %s, %s, %d", rn(rd), rn(rs1), int(int32(imm)))
	case 3:
		return fmt.Sprintf("sltiu %s, %s, %d", rn(rd), rn(rs1), int(int32(imm)))
	case 4:
		return fmt.Sprintf("xori  %s, %s, %d", rn(rd), rn(rs1), int(int32(imm)))
	case 6:
		return fmt.Sprintf("ori   %s, %s, %d", rn(rd), rn(rs1), int(int32(imm)))
	case 7:
		return fmt.Sprintf("andi  %s, %s, 0x%x", rn(rd), rn(rs1), imm)
	case 1:
		return fmt.Sprintf("slli  %s, %s, %d", rn(rd), rn(rs1), imm&0x1f)
	case 5:
		if imm&0x400 != 0 {
			return fmt.Sprintf("srai  %s, %s, %d", rn(rd), rn(rs1), imm&0x1f)
		}
		return fmt.Sprintf("srli  %s, %s, %d", rn(rd), rn(rs1), imm&0x1f)
	}
	return "?"
}
func aluRegMnemonic(f3, f7 uint32, rd, rs1, rs2 int) string {
	if f7 == 0x01 {
		m := []string{"mul", "mulh", "mulhsu", "mulhu", "div", "divu", "rem", "remu"}
		return fmt.Sprintf("%s  %s, %s, %s", m[f3], rn(rd), rn(rs1), rn(rs2))
	}
	m := []string{"add/sub", "sll", "slt", "sltu", "xor", "srl/sra", "or", "and"}
	if f3 == 0 && f7&0x20 != 0 {
		return fmt.Sprintf("sub   %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
	}
	if f3 == 5 && f7&0x20 != 0 {
		return fmt.Sprintf("sra   %s, %s, %s", rn(rd), rn(rs1), rn(rs2))
	}
	_ = m
	return fmt.Sprintf("%s  %s, %s, %s", []string{"add", "sll", "slt", "sltu", "xor", "srl", "or", "and"}[f3], rn(rd), rn(rs1), rn(rs2))
}
func sysMnemonic(instr, f3 uint32, rd, rs1 int) string {
	if f3 == 0 {
		switch instr >> 20 {
		case 0:
			return "ecall"
		case 1:
			return "ebreak"
		case 0x302:
			return "mret"
		}
	}
	return fmt.Sprintf("csr   %s, %s", rn(rd), rn(rs1))
}
