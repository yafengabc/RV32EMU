package cpu

// ---- 基本块解码缓存 ----
//
// RunHot 把直线代码段预解码成 hOp 数组并缓存，重复执行同一段代码时只做
// 寄存器/内存操作，不再逐条重新解析位域与 opcode。
//
// 块只覆盖平铺区内的单个 4KiB 页，并以该页的版本号为有效期：
// 总线/CPU 每次写平铺区都会把「代码页」的版本号 +1，
// 命中时版本号不一致就重新解码（自修改代码、SPI1 擦 flash 都安全）。
//
// 块内最后一条指令一定是控制转移（分支/跳转/C.J/C.JR…），或者因页边界、
// 指令数上限、遇到不支持编码而中断——两种情况都保证 pc 连续。

const (
	blockSlots  = 512 // 直接映射缓存槽数
	blockMaxOps = 32  // 单块最大指令数
)

type opKind uint8

const (
	kAddi opKind = iota
	kSlti
	kSltiu
	kXori
	kOri
	kAndi
	kSlli
	kSrli
	kSrai
	kAdd
	kSub
	kSll
	kSlt
	kSltu
	kXor
	kSrl
	kSra
	kOr
	kAnd
	kMulExt // M 扩展：imm 存 funct3
	kLui
	kAuipc
	kJal
	kJalr
	kBeq
	kBne
	kBlt
	kBge
	kBltu
	kBgeu
	kLb
	kLh
	kLw
	kLbu
	kLhu
	kSb
	kSh
	kSw
	kNop
	kCAddi4spn
	kCLw
	kCSw
	kCAddi
	kCJal
	kCLi
	kCAddi16sp
	kCLui
	kCSrli
	kCSrai
	kCAndi
	kCSub
	kCXor
	kCOr
	kCAnd
	kCJ
	kCBeqz
	kCBnez
	kCSlli
	kCLwsp
	kCSwsp
	kCJr
	kCMv
	kCJalr
	kCAdd
)

// hOp 是一条预解码指令。ln 是字节数（2/4），imm 是已符号扩展的立即数。
type hOp struct {
	kind opKind
	rd   uint8
	rs1  uint8
	rs2  uint8
	ln   uint8
	imm  int32
}

type hBlock struct {
	start uint32 // 起始 pc（缓存 tag）
	gen   uint32 // 建立时起始页的版本号
	n     uint8
	ops   [blockMaxOps]hOp
}

// blockFor 返回 pc 处可执行的块；未命中则就地解码并装入缓存。
// 返回 nil 表示该位置不支持热执行（交由 Step 处理）。
func (c *CPU) blockFor(pc, off uint32) *hBlock {
	// 直接映射索引：只用 pc 低位会以 1KB 为周期冲突（代码一长就全部
	// 互相挤掉），把页内偏移的高位异或进来，让同一页里的不同块散开。
	blk := &c.blocks[((pc>>1)^(pc>>10))&(blockSlots-1)]
	if blk.start == pc && blk.gen == c.codeGen[off>>12] {
		return blk
	}
	if !c.decodeBlock(blk, pc, off) {
		return nil
	}
	return blk
}

// decodeBlock 从 (pc, off) 起解码一个块，成功返回 true。
func (c *CPU) decodeBlock(blk *hBlock, pc, off uint32) bool {
	mem := c.flat
	page := off >> 12
	lim := uint32(len(mem))
	if pageEnd := (page + 1) << 12; pageEnd < lim {
		lim = pageEnd // 块不跨页，页版本号才够用
	}
	cur, curPC, n := off, pc, 0
	for n < blockMaxOps {
		if cur+2 > lim {
			break
		}
		low := uint32(mem[cur]) | uint32(mem[cur+1])<<8
		var o hOp
		var ok, term bool
		if low&0x3 == 0x3 {
			if cur+4 > lim {
				break
			}
			instr := low | (uint32(mem[cur+2])|uint32(mem[cur+3])<<8)<<16
			o, ok, term = decodeI32(instr)
			o.ln = 4
		} else {
			o, ok, term = decodeI16(low)
			o.ln = 2
		}
		if !ok {
			break
		}
		blk.ops[n] = o
		n++
		cur += uint32(o.ln)
		curPC += uint32(o.ln)
		if term {
			break
		}
	}
	if n == 0 {
		return false
	}
	blk.start = pc
	blk.gen = c.codeGen[page]
	blk.n = uint8(n)
	c.codePage[page] = true
	return true
}

// decodeI32 预解码一条 32-bit 指令。term 表示控制转移（块到此结束）。
func decodeI32(instr uint32) (o hOp, ok bool, term bool) {
	o.rd = uint8((instr >> 7) & 0x1f)
	o.rs1 = uint8((instr >> 15) & 0x1f)
	o.rs2 = uint8((instr >> 20) & 0x1f)
	f3 := (instr >> 12) & 0x7
	switch instr & 0x7f {
	case 0x13: // OP-IMM
		o.imm = int32(immI(instr))
		switch f3 {
		case 0:
			o.kind = kAddi
		case 2:
			o.kind = kSlti
		case 3:
			o.kind = kSltiu
		case 4:
			o.kind = kXori
		case 6:
			o.kind = kOri
		case 7:
			o.kind = kAndi
		case 1:
			o.kind = kSlli
		case 5:
			if o.imm&0x400 != 0 {
				o.kind = kSrai
			} else {
				o.kind = kSrli
			}
		}
		return o, true, false
	case 0x33: // OP / M
		if f7 := instr >> 25; f7 == 0x01 {
			o.kind = kMulExt
			o.imm = int32(f3)
			return o, true, false
		} else {
			switch f3 {
			case 0:
				if f7&0x20 != 0 {
					o.kind = kSub
				} else {
					o.kind = kAdd
				}
			case 1:
				o.kind = kSll
			case 2:
				o.kind = kSlt
			case 3:
				o.kind = kSltu
			case 4:
				o.kind = kXor
			case 5:
				if f7&0x20 != 0 {
					o.kind = kSra
				} else {
					o.kind = kSrl
				}
			case 6:
				o.kind = kOr
			case 7:
				o.kind = kAnd
			}
		}
		return o, true, false
	case 0x63: // BRANCH
		o.imm = int32(immB(instr))
		switch f3 {
		case 0:
			o.kind = kBeq
		case 1:
			o.kind = kBne
		case 4:
			o.kind = kBlt
		case 5:
			o.kind = kBge
		case 6:
			o.kind = kBltu
		case 7:
			o.kind = kBgeu
		default: // 保留编码：execI 当作"不跳转"，交给慢路径保持一致
			return o, false, true
		}
		return o, true, true
	case 0x03: // LOAD
		o.imm = int32(immI(instr))
		if o.rd == 0 { // rd=x0 也要真正访存（可能触发异常/MMIO 副作用）
			return o, false, true
		}
		switch f3 {
		case 0:
			o.kind = kLb
		case 1:
			o.kind = kLh
		case 2:
			o.kind = kLw
		case 4:
			o.kind = kLbu
		case 5:
			o.kind = kLhu
		default:
			return o, false, true
		}
		return o, true, false
	case 0x23: // STORE
		o.imm = int32(immS(instr))
		switch f3 {
		case 0:
			o.kind = kSb
		case 1:
			o.kind = kSh
		case 2:
			o.kind = kSw
		default:
			return o, false, true
		}
		return o, true, false
	case 0x37: // LUI
		o.kind = kLui
		o.imm = int32(instr & 0xfffff000)
		return o, true, false
	case 0x17: // AUIPC
		o.kind = kAuipc
		o.imm = int32(instr & 0xfffff000)
		return o, true, false
	case 0x6f: // JAL
		o.kind = kJal
		o.imm = int32(immJ(instr))
		return o, true, true
	case 0x67: // JALR
		o.kind = kJalr
		o.imm = int32(immI(instr))
		return o, true, true
	case 0x0f: // MISC-MEM：单核弱序模型下视为 NOP
		o.kind = kNop
		return o, true, false
	}
	return o, false, true
}

// decodeI16 预解码一条压缩指令。
func decodeI16(inst uint32) (o hOp, ok bool, term bool) {
	switch inst & 0x3 {
	case 0x0:
		switch (inst >> 13) & 0x7 {
		case 0x0: // C.ADDI4SPN
			imm := cAddi4spnImm(inst)
			if imm == 0 { // 保留编码
				return o, false, true
			}
			o.kind = kCAddi4spn
			o.rd = uint8(cReg((inst >> 2) & 0x7))
			o.imm = int32(imm)
			return o, true, false
		case 0x2: // C.LW
			o.kind = kCLw
			o.rd = uint8(cReg((inst >> 2) & 0x7))
			o.rs1 = uint8(cReg((inst >> 7) & 0x7))
			o.imm = int32(cLWOff(inst))
			return o, true, false
		case 0x6: // C.SW
			o.kind = kCSw
			o.rs2 = uint8(cReg((inst >> 2) & 0x7))
			o.rs1 = uint8(cReg((inst >> 7) & 0x7))
			o.imm = int32(cLWOff(inst))
			return o, true, false
		}
	case 0x1:
		rd5 := (inst >> 7) & 0x1f
		cd := uint8(cReg((inst >> 7) & 0x7))
		simm := int32(signExtend(((inst>>12)&1)<<5|((inst>>2)&0x1f), 6))
		switch (inst >> 13) & 0x7 {
		case 0x0: // C.ADDI
			if rd5 == 0 {
				return o, false, true
			}
			o.kind = kCAddi
			o.rd = uint8(rd5)
			o.rs1 = uint8(rd5)
			o.imm = simm
			return o, true, false
		case 0x1: // C.JAL
			o.kind = kCJal
			o.imm = int32(cJOffset(inst))
			return o, true, true
		case 0x2: // C.LI
			if rd5 == 0 {
				return o, false, true
			}
			o.kind = kCLi
			o.rd = uint8(rd5)
			o.imm = simm
			return o, true, false
		case 0x3:
			if rd5 == 2 { // C.ADDI16SP
				o.kind = kCAddi16sp
				o.imm = int32(cAddi16spImm(inst))
				return o, true, false
			}
			if rd5 == 0 { // C.LUI rd=x0 非法
				return o, false, true
			}
			o.kind = kCLui
			o.rd = uint8(rd5)
			o.imm = simm
			return o, true, false
		case 0x4:
			switch (inst >> 10) & 0x3 {
			case 0x0: // C.SRLI
				o.kind = kCSrli
				o.rd = cd
				o.rs1 = cd
				o.imm = int32((inst >> 2) & 0x1f)
				return o, true, false
			case 0x1: // C.SRAI
				o.kind = kCSrai
				o.rd = cd
				o.rs1 = cd
				o.imm = int32((inst >> 2) & 0x1f)
				return o, true, false
			case 0x2: // C.ANDI
				o.kind = kCAndi
				o.rd = cd
				o.rs1 = cd
				o.imm = simm
				return o, true, false
			default: // C.SUB/XOR/OR/AND
				o.rd = cd
				o.rs1 = cd
				o.rs2 = uint8(cReg((inst >> 2) & 0x7))
				switch (inst >> 5) & 0x3 {
				case 0x0:
					o.kind = kCSub
				case 0x1:
					o.kind = kCXor
				case 0x2:
					o.kind = kCOr
				case 0x3:
					o.kind = kCAnd
				}
				return o, true, false
			}
		case 0x5: // C.J
			o.kind = kCJ
			o.imm = int32(cJOffset(inst))
			return o, true, true
		case 0x6: // C.BEQZ
			o.kind = kCBeqz
			o.rs1 = uint8(cReg((inst >> 7) & 0x7))
			o.imm = int32(cBOffset(inst))
			return o, true, true
		case 0x7: // C.BNEZ
			o.kind = kCBnez
			o.rs1 = uint8(cReg((inst >> 7) & 0x7))
			o.imm = int32(cBOffset(inst))
			return o, true, true
		}
	case 0x2:
		rd5 := (inst >> 7) & 0x1f
		shamt := int32((inst >> 2) & 0x1f)
		switch (inst >> 13) & 0x7 {
		case 0x0: // C.SLLI
			if rd5 == 0 {
				return o, false, true
			}
			o.kind = kCSlli
			o.rd = uint8(rd5)
			o.rs1 = uint8(rd5)
			o.imm = shamt
			return o, true, false
		case 0x2: // C.LWSP
			if rd5 == 0 {
				return o, false, true
			}
			o.kind = kCLwsp
			o.rd = uint8(rd5)
			o.rs1 = 2
			o.imm = int32(cLWSPOff(inst))
			return o, true, false
		case 0x6: // C.SWSP
			o.kind = kCSwsp
			o.rs2 = uint8((inst >> 2) & 0x1f)
			o.rs1 = 2
			o.imm = int32(cSWSPOff(inst))
			return o, true, false
		case 0x4:
			rs2 := (inst >> 2) & 0x1f
			if (inst>>12)&1 == 0 { // C.JR / C.MV
				if rs2 == 0 {
					if rd5 == 0 { // C.JR x0 保留
						return o, false, true
					}
					o.kind = kCJr
					o.rd = uint8(rd5)
					o.rs1 = uint8(rd5)
					return o, true, true
				}
				if rd5 == 0 {
					return o, false, true
				}
				o.kind = kCMv
				o.rd = uint8(rd5)
				o.rs2 = uint8(rs2)
				return o, true, false
			}
			if rs2 == 0 {
				if rd5 == 0 { // C.EBREAK：交给 Step 停机
					return o, false, true
				}
				o.kind = kCJalr
				o.rd = uint8(rd5)
				o.rs1 = uint8(rd5)
				return o, true, true
			}
			if rd5 == 0 {
				return o, false, true
			}
			o.kind = kCAdd
			o.rd = uint8(rd5)
			o.rs1 = uint8(rd5)
			o.rs2 = uint8(rs2)
			return o, true, false
		}
	}
	return o, false, true
}

// runBlock 在 budget 条指令的预算内连续执行基本块链：一个块跑完就直接
// 就地取它的后继块继续跑，不再返回 RunHot 重新进入。
//
// 短块（2~3 条）切换频繁，若每块都调用一次函数，prologue/epilogue 的
// 开销会吃掉解码缓存带来的收益；链式执行把这次开销摊到整批预算上。
//
// 返回就地执行的条数与是否额外回退执行了一条 Step（此时 c.pc 已由 Step
// 更新，调用方只需给计数 +1）。
func (c *CPU) runBlock(blk *hBlock, budget int) (int, bool) {
	mem := c.flat
	base := c.flatBase
	size := uint32(len(mem))
	x := &c.x
	curPC := blk.start
	done := 0
	for {
		n := int(blk.n)
		if remain := budget - done; n > remain {
			n = remain
		}
		for i := 0; i < n; i++ {
			o := &blk.ops[i&31]
			next := curPC + uint32(o.ln)
			switch o.kind {
			case kAddi:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] + uint32(o.imm)
				}
				curPC = next
			case kSlti:
				if o.rd != 0 {
					x[o.rd&31] = boolToU32(int32(x[o.rs1&31]) < o.imm)
				}
				curPC = next
			case kSltiu:
				if o.rd != 0 {
					x[o.rd&31] = boolToU32(x[o.rs1&31] < uint32(o.imm))
				}
				curPC = next
			case kXori:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] ^ uint32(o.imm)
				}
				curPC = next
			case kOri:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] | uint32(o.imm)
				}
				curPC = next
			case kAndi:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] & uint32(o.imm)
				}
				curPC = next
			case kSlli:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] << (uint32(o.imm) & 0x1f)
				}
				curPC = next
			case kSrli:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] >> (uint32(o.imm) & 0x1f)
				}
				curPC = next
			case kSrai:
				if o.rd != 0 {
					x[o.rd&31] = uint32(int32(x[o.rs1&31]) >> (uint32(o.imm) & 0x1f))
				}
				curPC = next
			case kAdd:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] + x[o.rs2&31]
				}
				curPC = next
			case kSub:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] - x[o.rs2&31]
				}
				curPC = next
			case kSll:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] << (x[o.rs2&31] & 0x1f)
				}
				curPC = next
			case kSlt:
				if o.rd != 0 {
					x[o.rd&31] = boolToU32(int32(x[o.rs1&31]) < int32(x[o.rs2&31]))
				}
				curPC = next
			case kSltu:
				if o.rd != 0 {
					x[o.rd&31] = boolToU32(x[o.rs1&31] < x[o.rs2&31])
				}
				curPC = next
			case kXor:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] ^ x[o.rs2&31]
				}
				curPC = next
			case kSrl:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] >> (x[o.rs2&31] & 0x1f)
				}
				curPC = next
			case kSra:
				if o.rd != 0 {
					x[o.rd&31] = uint32(int32(x[o.rs1&31]) >> (x[o.rs2&31] & 0x1f))
				}
				curPC = next
			case kOr:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] | x[o.rs2&31]
				}
				curPC = next
			case kAnd:
				if o.rd != 0 {
					x[o.rd&31] = x[o.rs1&31] & x[o.rs2&31]
				}
				curPC = next
			case kMulExt:
				c.execM(uint32(o.imm), int(o.rd&31), x[o.rs1&31], x[o.rs2&31])
				curPC = next
			case kLui:
				if o.rd != 0 {
					x[o.rd&31] = uint32(o.imm)
				}
				curPC = next
			case kAuipc:
				if o.rd != 0 {
					x[o.rd&31] = curPC + uint32(o.imm)
				}
				curPC = next
			case kJal:
				if o.rd != 0 {
					x[o.rd&31] = curPC + 4
				}
				curPC += uint32(o.imm)
			case kJalr:
				t := (x[o.rs1&31] + uint32(o.imm)) &^ 1
				if o.rd != 0 {
					x[o.rd&31] = curPC + 4
				}
				curPC = t
			case kBeq:
				if x[o.rs1&31] == x[o.rs2&31] {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kBne:
				if x[o.rs1&31] != x[o.rs2&31] {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kBlt:
				if int32(x[o.rs1&31]) < int32(x[o.rs2&31]) {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kBge:
				if int32(x[o.rs1&31]) >= int32(x[o.rs2&31]) {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kBltu:
				if x[o.rs1&31] < x[o.rs2&31] {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kBgeu:
				if x[o.rs1&31] >= x[o.rs2&31] {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kLb:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 {
					if doff := addr - base; doff < size {
						x[o.rd&31] = uint32(int8(mem[doff]))
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kLh:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&1 == 0 {
					if doff := addr - base; doff < size-1 {
						x[o.rd&31] = uint32(int16(uint16(mem[doff]) | uint16(mem[doff+1])<<8))
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kLw:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						x[o.rd&31] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
							uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kLbu:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 {
					if doff := addr - base; doff < size {
						x[o.rd&31] = uint32(mem[doff])
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kLhu:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&1 == 0 {
					if doff := addr - base; doff < size-1 {
						x[o.rd&31] = uint32(mem[doff]) | uint32(mem[doff+1])<<8
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kSb:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 {
					if doff := addr - base; doff < size {
						mem[doff] = uint8(x[o.rs2&31])
						c.touchCode(doff)
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kSh:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&1 == 0 {
					if doff := addr - base; doff < size-1 {
						v := x[o.rs2&31]
						mem[doff] = uint8(v)
						mem[doff+1] = uint8(v >> 8)
						c.touchCode(doff)
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kSw:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						v := x[o.rs2&31]
						mem[doff] = uint8(v)
						mem[doff+1] = uint8(v >> 8)
						mem[doff+2] = uint8(v >> 16)
						mem[doff+3] = uint8(v >> 24)
						c.touchCode(doff)
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kNop:
				curPC = next
			case kCAddi4spn:
				x[o.rd&31] = x[2] + uint32(o.imm)
				curPC = next
			case kCLw:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						x[o.rd&31] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
							uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kCSw:
				addr := x[o.rs1&31] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						v := x[o.rs2&31]
						mem[doff] = uint8(v)
						mem[doff+1] = uint8(v >> 8)
						mem[doff+2] = uint8(v >> 16)
						mem[doff+3] = uint8(v >> 24)
						c.touchCode(doff)
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kCAddi:
				x[o.rd&31] = x[o.rs1&31] + uint32(o.imm)
				curPC = next
			case kCJal:
				x[1] = curPC + 2
				curPC += uint32(o.imm)
			case kCLi:
				x[o.rd&31] = uint32(o.imm)
				curPC = next
			case kCAddi16sp:
				x[2] = x[2] + uint32(o.imm)
				curPC = next
			case kCLui:
				x[o.rd&31] = uint32(o.imm) << 12
				curPC = next
			case kCSrli:
				x[o.rd&31] = x[o.rs1&31] >> (uint32(o.imm) & 0x1f)
				curPC = next
			case kCSrai:
				x[o.rd&31] = uint32(int32(x[o.rs1&31]) >> (uint32(o.imm) & 0x1f))
				curPC = next
			case kCAndi:
				x[o.rd&31] = x[o.rs1&31] & uint32(o.imm)
				curPC = next
			case kCSub:
				x[o.rd&31] = x[o.rs1&31] - x[o.rs2&31]
				curPC = next
			case kCXor:
				x[o.rd&31] = x[o.rs1&31] ^ x[o.rs2&31]
				curPC = next
			case kCOr:
				x[o.rd&31] = x[o.rs1&31] | x[o.rs2&31]
				curPC = next
			case kCAnd:
				x[o.rd&31] = x[o.rs1&31] & x[o.rs2&31]
				curPC = next
			case kCJ:
				curPC += uint32(o.imm)
			case kCBeqz:
				if x[o.rs1&31] == 0 {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kCBnez:
				if x[o.rs1&31] != 0 {
					curPC += uint32(o.imm)
				} else {
					curPC = next
				}
			case kCSlli:
				x[o.rd&31] = x[o.rs1&31] << (uint32(o.imm) & 0x1f)
				curPC = next
			case kCLwsp:
				addr := x[2] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						x[o.rd&31] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
							uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kCSwsp:
				addr := x[2] + uint32(o.imm)
				if c.priv != 0 && addr&3 == 0 {
					if doff := addr - base; doff < size-3 {
						v := x[o.rs2&31]
						mem[doff] = uint8(v)
						mem[doff+1] = uint8(v >> 8)
						mem[doff+2] = uint8(v >> 16)
						mem[doff+3] = uint8(v >> 24)
						c.touchCode(doff)
						curPC = next
						break
					}
				}
				c.pc = curPC
				c.Step()
				return done + i, true
			case kCJr:
				curPC = x[o.rd&31] &^ 1
			case kCMv:
				x[o.rd&31] = x[o.rs2&31]
				curPC = next
			case kCJalr:
				x[1] = curPC + 2
				curPC = x[o.rs1&31] &^ 1
			case kCAdd:
				x[o.rd&31] = x[o.rs1&31] + x[o.rs2&31]
				curPC = next
			}
		}
		done += n
		// 预算用完（含块被截断）就交还 RunHot。
		if done >= budget {
			c.pc = curPC
			return done, false
		}
		// 块正常执行完：curPC 已是控制转移的结果，直接取后继块继续。
		off := curPC - base
		if off >= size-3 {
			c.pc = curPC
			return done, false // 越出平铺区，交给 RunHot 的逐条回退
		}
		succ := &c.blocks[((curPC>>1)^(curPC>>10))&(blockSlots-1)]
		if succ.start == curPC && succ.gen == c.codeGen[off>>12] {
			blk = succ
			continue
		}
		if blk = c.blockFor(curPC, off); blk == nil {
			c.pc = curPC
			return done, false // 不支持热执行（系统指令/非法编码）
		}
	}
}

// touchCode 与总线共用同一份代码页表：CPU 直写平铺区时也要让本页的
// 已解码块失效。只对曾作为代码页的地址有开销。
func (c *CPU) touchCode(off uint32) {
	if p := off >> 12; c.codePage[p] {
		c.codeGen[p]++
	}
}
