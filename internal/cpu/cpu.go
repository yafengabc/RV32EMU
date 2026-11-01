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

	halted  bool
	haltMsg string

	instCount uint64
}

func New(bus mem.Bus) *CPU {
	c := &CPU{bus: bus, csr: newCSRFile(), pc: ResetVector}
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

// SetTimerPending 由外层（调度器/CLINT）回填 MTIP。
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
	// 热路径：仅在有未决中断且全局开中断时才做完整中断检查
	if c.csr.mip != 0 && c.csr.mstatus&mstatusMIE != 0 {
		c.checkInterrupts()
	}

	curPC := c.pc
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
		c.enterTrap(causeLoadMisalign, addr)
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
		c.enterTrap(causeStoreMisalign, addr)
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
	s += fmt.Sprintf("pc = 0x%08X  mstatus=0x%08X  mtvec=0x%08X  mepc=0x%08X  mcause=0x%08X\n",
		c.pc, c.csr.mstatus, c.csr.mtvec, c.csr.mepc, c.csr.mcause)
	return s
}
