package cpu

import (
	"fmt"

	"rv32emu/internal/mem"
)

// ResetVector ESP32-C3 复位取指地址（ROM 起始）。
const ResetVector uint32 = 0x40000000

// CPU 是 RV32IMC 单核模拟器状态。
type CPU struct {
	x   [32]uint32 // x0 硬零，写忽略
	pc  uint32
	bus mem.Bus
	fr  func(addr uint32, size uint32) uint32      // 快速读（平铺区内直访）
	fw  func(addr uint32, size uint32, val uint32) // 快速写
	csr *CSRFile

	// flat 是总线平铺区的直接引用，索引 = addr - flatBase。
	// 取指与访存都只需一次边界检查，省掉区间比较和间接调用。
	// 非 SystemBus 时钟为 nil，热点路径自动回落到 fr/fw。
	flat     []byte
	flatBase uint32

	// priv 是当前特权级：3 = M-mode，0 = U-mode。
	// RV32EMU 只模拟 M/U 两级；MPP 由 enterTrap 据此设置，
	// mret 时据此恢复，使 ecall 能区分 U-mode(8) / M-mode(11)。
	priv uint32

	// pendingInt 是已挂起、等待 MIE 开时投递的中断 cause（含 IRQ 位）。
	// 由外设（如 C3 定时器）通过 SetPendingInt 写入，每个周期由
	// Step 在取指前采样。0 表示无挂起中断。
	pendingInt uint32

	// curPC 保存当前正在译码的指令地址，供 load/store/非法指令
	// 构造异常 mepc（指向故障指令本身，而非已自增的 pc）。
	curPC uint32

	// pmpEnt 是 PMP 配置解析后的生效区间表（编号顺序，最小者优先）。
	pmpEnt []pmpEntry
	// pmpEnforce 关闭后 U-mode 不再受 PMP 约束（对照调试用，
	// 用来区分「内核逻辑问题」和「PMP 建模问题」）。
	pmpEnforce bool

	// OnTrap 非空时，每次进入 trap 都会被回调（调试用）。
	OnTrap func(cause, epc, tval uint32)

	halted  bool
	haltMsg string

	// excCount 统计"同步异常"进入次数（不含中断），供调试器在第一次
	// 真异常时停下；中断是正常路径，不应算作 fault。
	excCount uint64

	// intCount 统计中断进入次数（含 IRQ 位，不含同步异常）。
	intCount uint64

	instCount uint64
}

func New(bus mem.Bus) *CPU {
	c := &CPU{bus: bus, csr: newCSRFile(), pc: ResetVector, priv: 3, pmpEnforce: true}
	if sb, ok := bus.(*mem.SystemBus); ok {
		c.fr = sb.FastRead
		c.fw = sb.FastWrite
		c.flat = sb.FlatSlice()
		c.flatBase = mem.FlatBase
	} else {
		c.fr = bus.Read
		c.fw = bus.Write
	}
	return c
}

func (c *CPU) Halted() bool     { return c.halted }
func (c *CPU) HaltMsg() string  { return c.haltMsg }
func (c *CPU) PC() uint32       { return c.pc }
func (c *CPU) ExcCount() uint64 { return c.excCount }
func (c *CPU) IntCount() uint64 { return c.intCount }
func (c *CPU) SetPC(p uint32)   { c.pc = p }
func (c *CPU) Reg(i int) uint32 {
	if i == 0 {
		return 0
	}
	return c.x[i]
}
func (c *CPU) SetReg(i int, v uint32) {
	if i != 0 {
		c.x[i] = v
	}
}
func (c *CPU) CSR() *CSRFile { return c.csr }

// SetPMPEnforce 开关 U-mode 的 PMP 强制（对照调试用）。
func (c *CPU) SetPMPEnforce(v bool) { c.pmpEnforce = v }

// SetPendingInt 由外设写入一个挂起中断的 mcause（含 IRQ 位，如
// C3 定时器上报 0x80000002）。Step 在取指前采样，MIE 开时投递。
func (c *CPU) SetPendingInt(cause uint32) { c.pendingInt = cause }

// SetTimerPending 兼容旧接口：仍回填 MTIP（无实际中断路径使用）。
func (c *CPU) SetTimerPending(b bool) {
	if b {
		c.csr.mip |= mipMTIP
	} else {
		c.csr.mip &^= mipMTIP
	}
}

// setReg 内部写回，x0 丢弃。
func (c *CPU) setRd(rd int, v uint32) {
	if rd != 0 {
		c.x[rd] = v
	}
}

// Step 执行一条指令（16 或 32 位）。返回 false 表示已停机。
func (c *CPU) Step() bool {
	if c.halted {
		return false
	}
	// 取指前采样挂起中断：仅当 MIE 开时投递。中断的 mepc 指向
	// 当前 pc（即被抢占的、尚未执行的指令），与 RISC-V 规范一致。
	if c.pendingInt != 0 {
		c.checkInterrupts()
	}

	curPC := c.pc
	c.curPC = curPC
	low := c.fetch16(c.pc)
	var instr uint32
	var compressed bool

	switch low & 0x3 {
	case 0x0, 0x1, 0x2: // 16-bit 压缩指令
		instr = low
		compressed = true
		c.pc += 2
	default: // 0x3 -> 32-bit
		high := c.fetch16(c.pc + 2)
		instr = low | (high << 16)
		compressed = false
		c.pc += 4
	}

	// 执行（异常会改写 c.pc 为 trap 向量）
	if compressed {
		c.execC(uint16(instr))
	} else {
		c.execI(instr, curPC)
	}

	c.csr.mcycle++
	c.csr.minstret++
	c.instCount++
	return !c.halted
}

// Run 一直跑到停机或达到上限。
func (c *CPU) Run(max uint64) {
	for (max == 0 || c.instCount < max) && c.Step() {
	}
}

// RunHot 是 Step 的批量内联版本：常见指令就地解码执行，其余（CSR/系统指令、
// 非法编码、U-mode 访存、越出平铺区的取指）整条退回 Step 重新解码执行，
// 因此异常语义（PMP、对齐、mepc、mtval）与 Step 完全一致。
//
// 每 1024 条回到批边界采样一次 halted 与中断：中断的 mepc 指向批次首条
// 指令，偏差最多 1024 条；tick 本身是指令计数近似，实际无影响。
//
// 前提：CPU 绑定了 SystemBus（有平铺区）；否则直接退回 Run。
// 返回本次实际执行的指令数（供外层推进定时器/输入节流）。
func (c *CPU) RunHot(max uint64) uint64 {
	mem := c.flat
	if mem == nil {
		var i uint64
		for ; (max == 0 || i < max) && c.Step(); i++ {
		}
		return i
	}

	x := &c.x
	base := c.flatBase
	pc := c.pc
	size := uint32(len(mem))
	var count, cold uint64 // count=已处理指令；cold=其中退回 Step 的条数

hotloop:
	for max == 0 || count < max {
		if c.halted {
			break
		}
		if c.pendingInt != 0 {
			c.pc = pc
			c.checkInterrupts()
			pc = c.pc
			if c.halted {
				break
			}
		}
		chunkEnd := count + 1024
		if max != 0 && chunkEnd > max {
			chunkEnd = max
		}
		for count < chunkEnd {
			curPC := pc
			off := curPC - base
			// 取指越出平铺区（含 pc 低于 base 时的下溢）则退回 Step。
			// 用 off >= size-3 而不是 off+3 >= size：后者在 off 下溢时会回绕。
			if off >= size-3 {
				c.pc = curPC
				c.Step()
				pc = c.pc
				count++
				cold++
				if c.halted {
					break hotloop
				}
				continue
			}

			low := uint32(mem[off]) | uint32(mem[off+1])<<8
			hot := false

			if low&0x3 == 0x3 {
				instr := low | (uint32(mem[off+2])|uint32(mem[off+3])<<8)<<16
				pc = curPC + 4
				op := instr & 0x7f
				f3 := (instr >> 12) & 0x7
				r1 := int((instr >> 15) & 0x1f)
				di := int((instr >> 7) & 0x1f)
				switch op {
				case 0x13: // OP-IMM
					a := x[r1]
					imm := immI(instr)
					v := a
					switch f3 {
					case 0:
						v = a + imm
					case 2:
						v = boolToU32(int32(a) < int32(imm))
					case 3:
						v = boolToU32(a < imm)
					case 4:
						v = a ^ imm
					case 6:
						v = a | imm
					case 7:
						v = a & imm
					case 1:
						v = a << (imm & 0x1f)
					case 5:
						if imm&0x400 != 0 {
							v = uint32(int32(a) >> (imm & 0x1f))
						} else {
							v = a >> (imm & 0x1f)
						}
					}
					if di != 0 {
						x[di] = v
						hot = true
					}
				case 0x63: // BRANCH
					a := x[r1]
					b := x[int((instr>>20)&0x1f)]
					taken := false
					switch f3 {
					case 0:
						taken = a == b
					case 1:
						taken = a != b
					case 4:
						taken = int32(a) < int32(b)
					case 5:
						taken = int32(a) >= int32(b)
					case 6:
						taken = a < b
					case 7:
						taken = a >= b
					}
					if taken {
						pc = curPC + immB(instr)
					}
					hot = true
				case 0x03: // LOAD
					addr := x[r1] + immI(instr)
					sz := uint32(4)
					valid := true
					switch f3 {
					case 0:
						sz = 1
					case 1:
						sz = 2
					case 2:
					case 4:
						sz = 1
					case 5:
						sz = 2
					default:
						valid = false
					}
					// U-mode 必须逐条做 PMP 检查，整条退回 Step；
					// M-mode 下 pmpCheck 恒放行，可安全直访。
					if valid && di != 0 && c.priv != 0 && addr&(sz-1) == 0 {
						if doff := addr - base; doff < size-sz+1 {
							switch f3 {
							case 0: // LB
								x[di] = uint32(int8(mem[doff]))
							case 1: // LH
								x[di] = uint32(int16(uint16(mem[doff]) | uint16(mem[doff+1])<<8))
							case 2: // LW
								x[di] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
									uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
							case 4: // LBU
								x[di] = uint32(mem[doff])
							case 5: // LHU
								x[di] = uint32(mem[doff]) | uint32(mem[doff+1])<<8
							}
							hot = true
						}
					}
				case 0x23: // STORE
					addr := x[r1] + immS(instr)
					val := x[int((instr>>20)&0x1f)]
					sz := uint32(4)
					valid := true
					switch f3 {
					case 0:
						sz = 1
					case 1:
						sz = 2
					case 2:
					default:
						valid = false
					}
					if valid && c.priv != 0 && addr&(sz-1) == 0 {
						if doff := addr - base; doff < size-sz+1 {
							switch f3 {
							case 0: // SB
								mem[doff] = uint8(val)
							case 1: // SH
								mem[doff] = uint8(val)
								mem[doff+1] = uint8(val >> 8)
							case 2: // SW
								mem[doff] = uint8(val)
								mem[doff+1] = uint8(val >> 8)
								mem[doff+2] = uint8(val >> 16)
								mem[doff+3] = uint8(val >> 24)
							}
							hot = true
						}
					}
				case 0x37: // LUI
					if di != 0 {
						x[di] = instr & 0xfffff000
						hot = true
					}
				case 0x17: // AUIPC
					if di != 0 {
						x[di] = curPC + (instr & 0xfffff000)
						hot = true
					}
				case 0x6f: // JAL
					if di != 0 {
						x[di] = curPC + 4
					}
					pc = curPC + immJ(instr)
					hot = true
				case 0x67: // JALR
					t := (x[r1] + immI(instr)) &^ 1
					if di != 0 {
						x[di] = curPC + 4
					}
					pc = t
					hot = true
				case 0x33: // OP / M
					a := x[r1]
					b := x[int((instr>>20)&0x1f)]
					f7 := instr >> 25
					if f7 == 0x01 {
						c.execM(f3, di, a, b) // execM 内部经 setRd，rd=0 自动丢弃
						hot = true
					} else if di != 0 {
						v := a
						switch f3 {
						case 0:
							if f7&0x20 != 0 {
								v = a - b
							} else {
								v = a + b
							}
						case 1:
							v = a << (b & 0x1f)
						case 2:
							v = boolToU32(int32(a) < int32(b))
						case 3:
							v = boolToU32(a < b)
						case 4:
							v = a ^ b
						case 5:
							if f7&0x20 != 0 {
								v = uint32(int32(a) >> (b & 0x1f))
							} else {
								v = a >> (b & 0x1f)
							}
						case 6:
							v = a | b
						case 7:
							v = a & b
						}
						x[di] = v
						hot = true
					}
				case 0x0f: // FENCE -> NOP
					hot = true
				}
			} else {
				// 16-bit 压缩指令：就地解码，curPC 用本地量。
				inst := low
				pc = curPC + 2
				switch inst & 0x3 {
				case 0x0:
					switch (inst >> 13) & 0x7 {
					case 0x0: // C.ADDI4SPN
						if imm := cAddi4spnImm(inst); imm != 0 {
							x[cReg((inst>>2)&0x7)] = x[2] + imm
							hot = true
						}
					case 0x2: // C.LW
						rd := cReg((inst >> 2) & 0x7)
						addr := x[cReg((inst>>7)&0x7)] + cLWOff(inst)
						if c.priv != 0 && addr&3 == 0 {
							if doff := addr - base; doff < size-3 {
								x[rd] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
									uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
								hot = true
							}
						}
					case 0x6: // C.SW
						v := x[cReg((inst>>2)&0x7)]
						addr := x[cReg((inst>>7)&0x7)] + cLWOff(inst)
						if c.priv != 0 && addr&3 == 0 {
							if doff := addr - base; doff < size-3 {
								mem[doff] = uint8(v)
								mem[doff+1] = uint8(v >> 8)
								mem[doff+2] = uint8(v >> 16)
								mem[doff+3] = uint8(v >> 24)
								hot = true
							}
						}
					}
				case 0x1:
					rd5 := (inst >> 7) & 0x1f
					cd := cReg((inst >> 7) & 0x7)
					simm := signExtend(((inst>>12)&1)<<5|((inst>>2)&0x1f), 6)
					switch (inst >> 13) & 0x7 {
					case 0x0: // C.ADDI
						if rd5 != 0 {
							x[rd5] = x[rd5] + simm
							hot = true
						}
					case 0x1: // C.JAL
						x[1] = curPC + 2
						pc = curPC + cJOffset(inst)
						hot = true
					case 0x2: // C.LI
						if rd5 != 0 {
							x[rd5] = simm
							hot = true
						}
					case 0x3:
						if rd5 == 2 { // C.ADDI16SP
							x[2] = x[2] + cAddi16spImm(inst)
							hot = true
						} else if rd5 != 0 { // C.LUI
							x[rd5] = simm << 12
							hot = true
						}
					case 0x4: // C.SRLI/C.SRAI/C.ANDI/寄存器运算
						shamt := (inst >> 2) & 0x1f
						switch (inst >> 10) & 0x3 {
						case 0x0: // C.SRLI
							x[cd] = x[cd] >> shamt
							hot = true
						case 0x1: // C.SRAI
							x[cd] = uint32(int32(x[cd]) >> shamt)
							hot = true
						case 0x2: // C.ANDI
							x[cd] = x[cd] & simm
							hot = true
						case 0x3:
							rs2 := cReg((inst >> 2) & 0x7)
							switch (inst >> 5) & 0x3 {
							case 0x0:
								x[cd] = x[cd] - x[rs2]
							case 0x1:
								x[cd] = x[cd] ^ x[rs2]
							case 0x2:
								x[cd] = x[cd] | x[rs2]
							case 0x3:
								x[cd] = x[cd] & x[rs2]
							}
							hot = true
						}
					case 0x5: // C.J
						pc = curPC + cJOffset(inst)
						hot = true
					case 0x6: // C.BEQZ
						if x[cd] == 0 {
							pc = curPC + cBOffset(inst)
						}
						hot = true
					case 0x7: // C.BNEZ
						if x[cd] != 0 {
							pc = curPC + cBOffset(inst)
						}
						hot = true
					}
				case 0x2:
					rd5 := (inst >> 7) & 0x1f
					switch (inst >> 13) & 0x7 {
					case 0x0: // C.SLLI
						if rd5 != 0 {
							x[rd5] = x[rd5] << ((inst >> 2) & 0x1f)
							hot = true
						}
					case 0x2: // C.LWSP
						addr := x[2] + cLWSPOff(inst)
						if rd5 != 0 && c.priv != 0 && addr&3 == 0 {
							if doff := addr - base; doff < size-3 {
								x[rd5] = uint32(mem[doff]) | uint32(mem[doff+1])<<8 |
									uint32(mem[doff+2])<<16 | uint32(mem[doff+3])<<24
								hot = true
							}
						}
					case 0x6: // C.SWSP
						v := x[(inst>>2)&0x1f]
						addr := x[2] + cSWSPOff(inst)
						if c.priv != 0 && addr&3 == 0 {
							if doff := addr - base; doff < size-3 {
								mem[doff] = uint8(v)
								mem[doff+1] = uint8(v >> 8)
								mem[doff+2] = uint8(v >> 16)
								mem[doff+3] = uint8(v >> 24)
								hot = true
							}
						}
					case 0x4:
						rs2 := (inst >> 2) & 0x1f
						if (inst>>12)&1 == 0 {
							if rs2 == 0 { // C.JR
								if rd5 != 0 {
									pc = x[rd5] &^ 1
									hot = true
								}
							} else if rd5 != 0 { // C.MV
								x[rd5] = x[rs2]
								hot = true
							}
						} else {
							if rs2 == 0 { // C.JALR
								if rd5 != 0 {
									x[1] = curPC + 2
									pc = x[rd5] &^ 1
									hot = true
								}
							} else if rd5 != 0 { // C.ADD
								x[rd5] = x[rd5] + x[rs2]
								hot = true
							}
						}
					}
				}
			}

			if !hot {
				// 退回 Step 重新解码本条（含异常/非法/权限/越界等全部检查）
				c.pc = curPC
				c.Step()
				pc = c.pc
				count++
				cold++
				if c.halted {
					break hotloop
				}
				continue
			}
			count++
		}
	}

	c.pc = pc
	// 只有就地执行的指令需要补计数（cold 部分已由 Step 自行记账）
	c.csr.mcycle += count - cold
	c.csr.minstret += count - cold
	c.instCount += count - cold
	return count
}

// ---- 取指：平铺区内一次边界检查，区外回落总线 ----

// fetch16 取 16-bit 指令字。
func (c *CPU) fetch16(pc uint32) uint32 {
	if off := pc - c.flatBase; off+1 < uint32(len(c.flat)) {
		return uint32(c.flat[off]) | uint32(c.flat[off+1])<<8
	}
	return c.fr(pc, 2)
}

// fetch32 取完整 32-bit 指令。
func (c *CPU) fetch32(pc uint32) uint32 {
	if off := pc - c.flatBase; off+3 < uint32(len(c.flat)) {
		return uint32(c.flat[off]) | uint32(c.flat[off+1])<<8 |
			uint32(c.flat[off+2])<<16 | uint32(c.flat[off+3])<<24
	}
	low := c.fr(pc, 2)
	high := c.fr(pc+2, 2)
	return low | high<<16
}

// ---- 访存辅助（带对齐检查，违例则进 trap） ----

func (c *CPU) load(addr uint32, size uint32, signed bool) uint32 {
	if badAlign(addr, size) {
		c.enterTrap(causeLoadMisalign, addr, c.curPC)
		return 0
	}
	// U-mode 必须命中 PMP，否则是 load access fault（真机 C3 行为；
	// 这也是 prot 用例里「越界读」被杀掉的机制）。
	if !c.pmpCheck(addr, size, pmpRead) {
		c.enterTrap(causeLoadFault, addr, c.curPC)
		return 0
	}
	var v uint32
	if n := uint32(len(c.flat)); n >= size && addr-c.flatBase < n-size+1 {
		m := c.flat
		off := addr - c.flatBase
		switch size {
		case 1:
			v = uint32(m[off])
		case 2:
			v = uint32(m[off]) | uint32(m[off+1])<<8
		case 4:
			v = uint32(m[off]) | uint32(m[off+1])<<8 |
				uint32(m[off+2])<<16 | uint32(m[off+3])<<24
		}
	} else {
		v = c.fr(addr, size)
	}
	if signed {
		switch size {
		case 1:
			v = uint32(int8(v))
		case 2:
			v = uint32(int16(v))
		}
	}
	return v
}

func (c *CPU) store(addr uint32, size uint32, val uint32) {
	if badAlign(addr, size) {
		c.enterTrap(causeStoreMisalign, addr, c.curPC)
		return
	}
	if !c.pmpCheck(addr, size, pmpWrite) {
		c.enterTrap(causeStoreFault, addr, c.curPC)
		return
	}
	if n := uint32(len(c.flat)); n >= size && addr-c.flatBase < n-size+1 {
		m := c.flat
		off := addr - c.flatBase
		switch size {
		case 1:
			m[off] = uint8(val)
		case 2:
			m[off] = uint8(val)
			m[off+1] = uint8(val >> 8)
		case 4:
			m[off] = uint8(val)
			m[off+1] = uint8(val >> 8)
			m[off+2] = uint8(val >> 16)
			m[off+3] = uint8(val >> 24)
		}
		return
	}
	c.fw(addr, size, val)
}

func badAlign(addr, size uint32) bool {
	switch size {
	case 2:
		return addr&1 != 0
	case 4:
		return addr&3 != 0
	}
	return false
}

// dumpRegs 供调试 CLI 使用。
func (c *CPU) Dump() string {
	s := ""
	for i := 0; i < 32; i++ {
		s += fmt.Sprintf("x%-2d = 0x%08X   ", i, c.Reg(i))
		if i%4 == 3 {
			s += "\n"
		}
	}
	s += fmt.Sprintf("pc = 0x%08X  mstatus=0x%08X  mtvec=0x%08X  mepc=0x%08X  mcause=0x%08X  mtval=0x%08X\n",
		c.pc, c.csr.mstatus, c.csr.mtvec, c.csr.mepc, c.csr.mcause, c.csr.mtval)
	return s
}
