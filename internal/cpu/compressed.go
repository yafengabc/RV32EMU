package cpu

import "fmt"

// cReg 把 3-bit 压缩寄存器号（0..7）映射到 x8..x15。
func cReg(b uint32) int { return int(b&0x7) + 8 }

// 立即数/偏移解析集中在这里，execC（慢路径）与 RunHot（快路径）共用，
// 避免两处位域各写一遍导致行为分叉。

// cAddi4spnImm 解析 C.ADDI4SPN 的 nzuimm（返回 0 表示保留编码）。
func cAddi4spnImm(inst uint32) uint32 {
	imm := ((inst >> 7) & 0xF) << 6
	imm |= ((inst >> 11) & 0x3) << 4
	imm |= ((inst >> 6) & 1) << 2
	imm |= ((inst >> 5) & 1) << 3
	return imm
}

// cLWOff 解析 C.LW/C.SW 的 uimm[6:2]。
func cLWOff(inst uint32) uint32 {
	off := ((inst >> 5) & 1) << 6
	off |= ((inst >> 12) & 1) << 5
	off |= ((inst >> 11) & 1) << 4
	off |= ((inst >> 10) & 1) << 3
	off |= ((inst >> 6) & 1) << 2
	return off
}

// cAddi16spImm 解析 C.ADDI16SP 的 nzimm[9:4]（整体按 10-bit 有符号）。
func cAddi16spImm(inst uint32) uint32 {
	imm := ((inst >> 12) & 1) << 9
	imm |= ((inst >> 4) & 1) << 8
	imm |= ((inst >> 3) & 1) << 7
	imm |= ((inst >> 5) & 1) << 6
	imm |= ((inst >> 2) & 1) << 5
	imm |= ((inst >> 6) & 1) << 4
	return signExtend(imm, 10)
}

// cLWSPOff 解析 C.LWSP 的 uimm[7:2]。
func cLWSPOff(inst uint32) uint32 {
	off := ((inst >> 12) & 1) << 5
	off |= ((inst >> 6) & 1) << 4
	off |= ((inst >> 5) & 1) << 3
	off |= ((inst >> 4) & 1) << 2
	off |= ((inst >> 3) & 1) << 7
	off |= ((inst >> 2) & 1) << 6
	return off
}

// cSWSPOff 解析 C.SWSP 的 uimm[7:2]。
func cSWSPOff(inst uint32) uint32 {
	off := ((inst >> 9) & 0xF) << 2
	off |= ((inst >> 7) & 0x3) << 6
	return off
}

// execC 执行一条 16-bit 压缩指令。不支持的进入非法指令 trap。
func (c *CPU) execC(in uint16) {
	inst := uint32(in)
	op := inst & 0x3
	f3 := inst >> 13

	switch op {
	case 0x00: // 象限 0
		switch f3 {
		case 0x00: // C.ADDI4SPN: addi rd', sp, nzuimm
			rd := cReg((inst >> 2) & 0x7)
			imm := cAddi4spnImm(inst)
			if imm == 0 { // 保留编码：nzuimm==0 视为非法
				c.illegalC(inst)
			} else {
				c.setRd(rd, c.Reg(2)+imm)
			}
		case 0x02: // C.LW: lw rd', off(rs1')
			// rd'=inst[4:2]  off[6]=inst[5] off[5:3]=inst[12:10] off[2]=inst[6]
			rd := cReg((inst >> 2) & 0x7)
			rs := cReg((inst >> 7) & 0x7)
			c.setRd(rd, c.load(c.Reg(rs)+cLWOff(inst), 4, false))
		case 0x06: // C.SW: sw rs2', off(rs1')
			rs2 := cReg((inst >> 2) & 0x7)
			rs1 := cReg((inst >> 7) & 0x7)
			c.store(c.Reg(rs1)+cLWOff(inst), 4, c.Reg(rs2))
		default:
			c.illegalC(inst)
		}

	case 0x01: // 象限 1
		rd := (inst >> 7) & 0x1f // 5-bit 标准 rd/sp
		cd := cReg((inst >> 7) & 0x7)
		imm5 := (inst >> 12) & 0x1
		imm40 := (inst >> 2) & 0x1f
		simm := signExtend((imm5<<5)|imm40, 6)
		switch f3 {
		case 0x00: // C.ADDI
			c.setRd(int(rd), c.Reg(int(rd))+simm)
		case 0x01: // C.JAL: ra=pc+2, pc=curPC+offset
			off := cJOffset(inst)
			c.setRd(1, c.pc)
			c.pc = c.curPC + off
		case 0x02: // C.LI: addi rd, x0, imm  (rd 是 5-bit，可任意寄存器)
			c.setRd(int(rd), simm)
		case 0x03:
			if rd == 2 { // C.ADDI16SP
				// imm[9]=inst[12] imm[8]=inst[4] imm[7]=inst[3]
				// imm[6]=inst[5] imm[5]=inst[2] imm[4]=inst[6], imm[3:0]=0
				c.setRd(2, c.Reg(2)+cAddi16spImm(inst))
			} else { // C.LUI
				c.setRd(int(rd), simm<<12)
			}
		case 0x04: // C.SRLI / C.SRAI / C.ANDI / 寄存器运算
			sub := (inst >> 10) & 0x3
			shamt := imm40
			switch sub {
			case 0x00: // C.SRLI
				c.setRd(cd, c.Reg(cd)>>shamt)
			case 0x01: // C.SRAI
				c.setRd(cd, uint32(int32(c.Reg(cd))>>shamt))
			case 0x02: // C.ANDI
				c.setRd(cd, c.Reg(cd)&simm)
			case 0x03: // SUB/XOR/OR/AND
				rs2 := cReg((inst >> 2) & 0x7)
				f2 := (inst >> 5) & 0x3
				switch f2 {
				case 0x00:
					c.setRd(cd, c.Reg(cd)-c.Reg(rs2))
				case 0x01:
					c.setRd(cd, c.Reg(cd)^c.Reg(rs2))
				case 0x02:
					c.setRd(cd, c.Reg(cd)|c.Reg(rs2))
				case 0x03:
					c.setRd(cd, c.Reg(cd)&c.Reg(rs2))
				}
			}
		case 0x05: // C.J
			off := cJOffset(inst)
			c.pc = c.curPC + off
		case 0x06: // C.BEQZ: if rs1'==0 branch
			rs1 := cReg((inst >> 7) & 0x7)
			off := cBOffset(inst)
			if c.Reg(rs1) == 0 {
				c.pc = c.curPC + off
			}
		case 0x07: // C.BNEZ: if rs1'!=0 branch
			rs1 := cReg((inst >> 7) & 0x7)
			off := cBOffset(inst)
			if c.Reg(rs1) != 0 {
				c.pc = c.curPC + off
			}
		default:
			c.illegalC(inst)
		}

	case 0x02: // 象限 2
		rd := (inst >> 7) & 0x1f
		shamt := (inst >> 2) & 0x1f
		switch f3 {
		case 0x00: // C.SLLI
			c.setRd(int(rd), c.Reg(int(rd))<<(shamt&0x1f))
		case 0x02: // C.LWSP: lw rd, off(sp)
			// off[5]=inst[12] off[4:2]=inst[6:4] off[7:6]=inst[3:2]
			c.setRd(int(rd), c.load(c.Reg(2)+cLWSPOff(inst), 4, false))
		case 0x06: // C.SWSP: sw rs2, off(sp)
			// off[5:2]=inst[12:9] off[7:6]=inst[8:7]
			rs2 := (inst >> 2) & 0x1f
			c.store(c.Reg(2)+cSWSPOff(inst), 4, c.Reg(int(rs2)))
		case 0x04:
			b12 := (inst >> 12) & 1
			rs2 := (inst >> 2) & 0x1f
			if b12 == 0 { // C.JR / C.MV
				if rs2 == 0 { // C.JR: jalr x0, rd, 0
					c.pc = c.Reg(int(rd)) &^ 1
				} else { // C.MV: rd = rs2
					c.setRd(int(rd), c.Reg(int(rs2)))
				}
			} else { // C.JALR / C.ADD / C.EBREAK
				if rd == 0 && rs2 == 0 { // C.EBREAK
					c.halted = true
					c.haltMsg = "C.EBREAK"
				} else if rs2 == 0 { // C.JALR: ra=pc; pc=rs1
					c.setRd(1, c.pc)
					c.pc = c.Reg(int(rd)) &^ 1
				} else { // C.ADD
					c.setRd(int(rd), c.Reg(int(rd))+c.Reg(int(rs2)))
				}
			}
		default:
			c.illegalC(inst)
		}
	}
}

// cJOffset 解析 C.J/C.JAL 的 12-bit 偏移（RVC CJ 格式）。
func cJOffset(inst uint32) uint32 {
	// 规范位映射：
	//  imm[11]=inst[12] imm[4]=inst[11] imm[9:8]=inst[10:9] imm[10]=inst[8]
	//  imm[6]=inst[7]   imm[7]=inst[6]  imm[3:1]=inst[5:3]       imm[5]=inst[2]
	off := uint32(0)
	off |= ((inst >> 12) & 1) << 11
	off |= ((inst >> 11) & 1) << 4
	off |= ((inst >> 10) & 1) << 9
	off |= ((inst >> 9) & 1) << 8
	off |= ((inst >> 8) & 1) << 10
	off |= ((inst >> 7) & 1) << 6
	off |= ((inst >> 6) & 1) << 7
	off |= ((inst >> 5) & 1) << 3
	off |= ((inst >> 4) & 1) << 2
	off |= ((inst >> 3) & 1) << 1
	off |= ((inst >> 2) & 1) << 5
	return signExtend(off&0xFFF, 12)
}

// cBOffset 解析 C.BEQZ/C.BNEZ 的分支偏移（CB 格式）。
// 规范位映射（imm[8:0]，imm[0] 恒 0；见 RVC 规范 CB-type）：
//
//	imm[8]=inst[12]  imm[7]=inst[6]  imm[6]=inst[5]  imm[5]=inst[2]
//	imm[4]=inst[11]  imm[3]=inst[10] imm[2]=inst[4]  imm[1]=inst[3]
//
// 注意 imm[7:6] 来自 inst[6:5]，imm[4:3] 来自 inst[11:10]——
// 与直觉相反（后者不是 inst[6:5]）。偏移相对本指令地址（c.curPC）。
func cBOffset(inst uint32) uint32 {
	off := uint32(0)
	off |= ((inst >> 12) & 1) << 8
	off |= ((inst >> 6) & 1) << 7
	off |= ((inst >> 5) & 1) << 6
	off |= ((inst >> 2) & 1) << 5
	off |= ((inst >> 11) & 1) << 4
	off |= ((inst >> 10) & 1) << 3
	off |= ((inst >> 4) & 1) << 2
	off |= ((inst >> 3) & 1) << 1
	return signExtend(off&0x1FF, 9)
}

func (c *CPU) illegalC(inst uint32) {
	c.enterTrap(causeIllegalInstr, inst, c.curPC)
}

func (c *CPU) disasmC(in uint16) string {
	return fmt.Sprintf(".c 0x%04X", in)
}
