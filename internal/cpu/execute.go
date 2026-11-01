package cpu

import "math/bits"

// 译码字段。
func opcode(i uint32) uint32 { return i & 0x7f }
func rd(i uint32) int         { return int((i >> 7) & 0x1f) }
func funct3(i uint32) uint32 { return (i >> 12) & 0x7 }
func rs1(i uint32) int        { return int((i >> 15) & 0x1f) }
func rs2(i uint32) int         { return int((i >> 20) & 0x1f) }
func funct7(i uint32) uint32 { return i >> 25 }

// 立即量符号扩展。
func immI(i uint32) uint32 { return signExtend(i >> 20, 12) }
func immS(i uint32) uint32 {
	return signExtend((i>>25)<<5|(i>>7)&0x1f, 12)
}
func immB(i uint32) uint32 {
	return signExtend(((i>>31)&1)<<12|((i>>7)&1)<<11|((i>>25)&0x3f)<<5|((i>>8)&0xf)<<1, 13)
}
func immU(i uint32) uint32 { return i & 0xfffff000 }
func immJ(i uint32) uint32 {
	return signExtend(((i>>31)&1)<<20|((i>>12)&0xff)<<12|((i>>20)&1)<<11|((i>>21)&0x3ff)<<1, 21)
}

func signExtend(x uint32, bitsN uint) uint32 {
	if x&(1<<(bitsN-1)) != 0 {
		return x | (^uint32(0) << bitsN)
	}
	return x
}

// execI 执行一条 32-bit 指令。curPC 为该指令地址。
func (c *CPU) execI(instr, curPC uint32) {
	op := opcode(instr)
	rd_ := rd(instr)
	f3 := funct3(instr)
	r1 := rs1(instr)
	r2 := rs2(instr)

	switch op {
	case 0x37: // LUI
		c.setRd(rd_, immU(instr))
	case 0x17: // AUIPC
		c.setRd(rd_, curPC+immU(instr))
	case 0x6f: // JAL
		c.setRd(rd_, curPC+4)
		c.pc = curPC + immJ(instr)
	case 0x67: // JALR
		t := (c.Reg(r1) + immI(instr)) &^ 1
		c.setRd(rd_, curPC+4)
		c.pc = t
	case 0x63: // BRANCH
		Taken := false
		a, b := c.Reg(r1), c.Reg(r2)
		switch f3 {
		case 0:
			Taken = a == b
		case 1:
			Taken = a != b
		case 4:
			Taken = int32(a) < int32(b)
		case 5:
			Taken = int32(a) >= int32(b)
		case 6:
			Taken = a < b
		case 7:
			Taken = a >= b
		}
		if Taken {
			c.pc = curPC + immB(instr)
		}
	case 0x03: // LOAD
		addr := c.Reg(r1) + immI(instr)
		switch f3 {
		case 0:
			c.setRd(rd_, c.load(addr, 1, true))
		case 1:
			c.setRd(rd_, c.load(addr, 2, true))
		case 2:
			c.setRd(rd_, c.load(addr, 4, false))
		case 4:
			c.setRd(rd_, c.load(addr, 1, false))
		case 5:
			c.setRd(rd_, c.load(addr, 2, false))
		default:
			c.illegal(instr)
		}
	case 0x23: // STORE
		addr := c.Reg(r1) + immS(instr)
		v := c.Reg(r2)
		switch f3 {
		case 0:
			c.store(addr, 1, v)
		case 1:
			c.store(addr, 2, v)
		case 2:
			c.store(addr, 4, v)
		default:
			c.illegal(instr)
		}
	case 0x13: // OP-IMM
		a := c.Reg(r1)
		imm := immI(instr)
		switch f3 {
		case 0: // ADDI
			c.setRd(rd_, a+imm)
		case 2: // SLTI
			c.setRd(rd_, boolToU32(int32(a) < int32(imm)))
		case 3: // SLTIU
			c.setRd(rd_, boolToU32(a < imm))
		case 4: // XORI
			c.setRd(rd_, a^imm)
		case 6: // ORI
			c.setRd(rd_, a|imm)
		case 7: // ANDI
			c.setRd(rd_, a&imm)
		case 1: // SLLI
			c.setRd(rd_, a<<(imm&0x1f))
		case 5: // SRLI / SRAI
			if imm&0x400 != 0 {
				c.setRd(rd_, uint32(int32(a)>>(imm&0x1f))) // SRAI
			} else {
				c.setRd(rd_, a>>(imm&0x1f)) // SRLI
			}
		}
	case 0x33: // OP / M 扩展
		a, b := c.Reg(r1), c.Reg(r2)
		if funct7(instr) == 0x01 { // M extension
			c.execM(f3, rd_, a, b)
			return
		}
		switch f3 {
		case 0: // ADD / SUB
			if funct7(instr)&0x20 != 0 {
				c.setRd(rd_, a-b)
			} else {
				c.setRd(rd_, a+b)
			}
		case 1: // SLL
			c.setRd(rd_, a<<(b&0x1f))
		case 2: // SLT
			c.setRd(rd_, boolToU32(int32(a) < int32(b)))
		case 3: // SLTU
			c.setRd(rd_, boolToU32(a < b))
		case 4: // XOR
			c.setRd(rd_, a^b)
		case 5: // SRL / SRA
			if funct7(instr)&0x20 != 0 {
				c.setRd(rd_, uint32(int32(a)>>(b&0x1f)))
			} else {
				c.setRd(rd_, a>>(b&0x1f))
			}
		case 6: // OR
			c.setRd(rd_, a|b)
		case 7: // AND
			c.setRd(rd_, a&b)
		}
	case 0x0f: // MISC-MEM (fence) -> NOP
		// 单核弱序模型下 fence 视为空操作
	case 0x73: // SYSTEM
		c.execSystem(instr, curPC)
	default:
		c.illegal(instr)
	}
}

// execM 处理 M 扩展。
func (c *CPU) execM(f3 uint32, rd_ int, a, b uint32) {
	switch f3 {
	case 0: // MUL
		c.setRd(rd_, a*b)
	case 1: // MULH
		pa := int64(int32(a)) * int64(int32(b))
		c.setRd(rd_, uint32(pa>>32))
	case 2: // MULHSU
		pa := int64(int32(a)) * int64(b)
		c.setRd(rd_, uint32(pa>>32))
	case 3: // MULHU
		pa := uint64(a) * uint64(b)
		c.setRd(rd_, uint32(pa>>32))
	case 4: // DIV
		minInt := int32(-0x80000000)
		if b == 0 {
			c.setRd(rd_, ^uint32(0))
		} else if int32(a) == minInt && int32(b) == -1 {
			c.setRd(rd_, a) // overflow INT_MIN/-1
		} else {
			c.setRd(rd_, uint32(int32(a)/int32(b)))
		}
	case 5: // DIVU
		if b == 0 {
			c.setRd(rd_, ^uint32(0))
		} else {
			c.setRd(rd_, a/b)
		}
	case 6: // REM
		minInt := int32(-0x80000000)
		if b == 0 {
			c.setRd(rd_, a)
		} else if int32(a) == minInt && int32(b) == -1 {
			c.setRd(rd_, 0)
		} else {
			c.setRd(rd_, uint32(int32(a)%int32(b)))
		}
	case 7: // REMU
		if b == 0 {
			c.setRd(rd_, a)
		} else {
			c.setRd(rd_, a%b)
		}
	}
}

// execSystem：ecall/ebreak/CSR 指令。
func (c *CPU) execSystem(instr, curPC uint32) {
	f3 := funct3(instr)
	rd_ := rd(instr)
	r1 := rs1(instr)
	csr := (instr >> 20) & 0xfff

	switch f3 {
	case 0: // ECALL / EBREAK / MRET / WFI
		switch csr {
		case 0x0: // ECALL
			// 按当前特权级区分：U-mode 用户态系统调用(cause 8)，
			// M-mode 内核自身调用(cause 11)。mepc 指向 ecall
			// 指令本身，内核 trap_handler 会 +4 跳过它。
			if c.priv == 0 {
				c.enterTrap(causeEcallU, 0, c.curPC)
			} else {
				c.enterTrap(causeEcallM, 0, c.curPC)
			}
		case 0x1: // EBREAK —— 模拟器约定：停机，作为固件 exit
			c.halted = true
			c.haltMsg = "EBREAK"
		case 0x302: // MRET
			c.mret()
		case 0x105: // WFI —— C3 无独立休眠；视作空操作，下一个 tick
			// 仍由 checkInterrupts 正常投递（MIE 开时）。
		default:
			c.illegal(instr)
		}
	case 1: // CSRRW
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, c.Reg(r1))
	case 2: // CSRRS
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, v|c.Reg(r1))
	case 3: // CSRRC
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, v&^c.Reg(r1))
	case 5: // CSRRWI
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, uint32(r1))
	case 6: // CSRRSI
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, v|uint32(r1))
	case 7: // CSRRCI
		v, ok := c.csr.Read(csr)
		if !ok {
			c.illegal(instr)
			return
		}
		c.setRd(rd_, v)
		c.csr.Write(csr, v&^uint32(r1))
	default:
		c.illegal(instr)
	}
}

func (c *CPU) illegal(instr uint32) {
	c.enterTrap(causeIllegalInstr, instr, c.curPC)
}

func boolToU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// bits 包保留位：后续可扩展以用 bits.RotateLeft32 等。
var _ = bits.RotateLeft32
