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
	fr  func(addr uint32, size uint32) uint32  // 快速读（ROM/SRAM 直访）
	fw  func(addr uint32, size uint32, val uint32) // 快速写
	csr *CSRFile

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
	} else {
		c.fr = bus.Read
		c.fw = bus.Write
	}
	return c
}

func (c *CPU) Halted() bool  { return c.halted }
func (c *CPU) HaltMsg() string { return c.haltMsg }
func (c *CPU) PC() uint32     { return c.pc }
func (c *CPU) ExcCount() uint64 { return c.excCount }
func (c *CPU) IntCount() uint64 { return c.intCount }
func (c *CPU) SetPC(p uint32) { c.pc = p }
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
	low := c.fr(c.pc, 2)
	var instr uint32
	var compressed bool

	switch low & 0x3 {
	case 0x0, 0x1, 0x2: // 16-bit 压缩指令
		instr = low
		compressed = true
		c.pc += 2
	default: // 0x3 -> 32-bit
		high := c.fr(c.pc+2, 2)
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
	v := c.fr(addr, size)
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
