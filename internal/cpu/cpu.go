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

	// blocks 是基本块解码缓存（直接映射，见 block.go）。
	// codePage/codeGen 与总线共享同一份底层数组：CPU 直写平铺区时也要
	// 让该页已解码的块失效，否则自修改代码会执行到旧指令。
	blocks   [blockSlots]hBlock
	codePage []bool
	codeGen  []uint32

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
		c.codePage, c.codeGen = sb.CodePages()
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

// RunHot 是 Step 的批量版本：按基本块执行（块由 block.go 预解码并缓存），
// 块内指令就地执行；CSR/系统指令、非法编码、越出平铺区的取指、U-mode
// 访存整条退回 Step，因此异常语义（PMP、对齐、mepc、mtval）与 Step 一致。
//
// 每个块边界采样一次 halted 与挂起中断（中断 mepc 指向块首条指令）。
// 前提：CPU 绑定了 SystemBus（有平铺区）；否则直接退回逐条 Step。
// 返回本次实际执行的指令数（供外层推进定时器/输入节流）。
func (c *CPU) RunHot(max uint64) uint64 {
	if c.flat == nil {
		var i uint64
		for ; (max == 0 || i < max) && c.Step(); i++ {
		}
		return i
	}
	size := uint32(len(c.flat))
	if c.halted {
		return 0
	}
	// 挂起中断只在批次之间变化（外设在 RunHot 返回后才推进），因此
	// 每次调用采样一次即可；块边界只需检查 halted。
	if c.pendingInt != 0 {
		c.checkInterrupts()
		if c.halted {
			return 0
		}
	}
	var count, hot uint64
	for max == 0 || count < max {
		if c.halted {
			break
		}
		pc := c.pc
		off := pc - c.flatBase
		if off >= size-3 {
			// 平铺区外取指（如 0x80000000 这类为 ELF 加载器分配的宿主内存）：
			// 连续逐条 Step，直到回到平铺区、停机或达到上限。本次调用期间
			// 挂起中断不会变化（外设在批次之间注入），因此无需逐条采样。
			for max == 0 || count < max {
				c.Step()
				count++
				if c.halted || c.pc-c.flatBase < size-3 {
					break
				}
			}
			continue
		}
		blk := c.blockFor(pc, off)
		if blk == nil {
			// 系统指令 / 非法编码：交给慢路径，异常语义由它负责
			c.Step()
			count++
			continue
		}
		// 整批剩余预算交给 runBlock，它会在块链上连续执行到预算用完，
		// 避免短块每切换一次就付出一次函数调用开销。
		k := 1<<31 - 1
		if max != 0 && max-count < uint64(k) {
			k = int(max - count)
		}
		n, stepped := c.runBlock(blk, k)
		count += uint64(n)
		hot += uint64(n)
		if stepped {
			count++ // 回退执行的一条 Step 已由 Step 自己记账
		}
	}
	// 只有就地执行的指令需要补计数（回退部分已由 Step 记账）
	c.csr.mcycle += hot
	c.csr.minstret += hot
	c.instCount += hot
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
