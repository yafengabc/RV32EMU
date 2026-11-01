package cpu

// 异常/中断 cause 编码（mcause）。
const (
	causeInstrMisalign   uint32 = 0
	causeIllegalInstr    uint32 = 2
	causeBreakpoint      uint32 = 3
	causeLoadMisalign    uint32 = 4
	causeLoadFault       uint32 = 5
	causeStoreMisalign   uint32 = 6
	causeStoreFault      uint32 = 7
	causeEcallU          uint32 = 8
	causeEcallM          uint32 = 11
	causeInterruptBit    uint32 = 0x80000000
	causeMachineTimerInt uint32 = causeInterruptBit | 7
)

// enterTrap 进入 trap：记录 mepc/mcause/mtval，关中断，跳 mtvec。
// interrupt=true 时 mepc 指向被中断的下一条指令（这里用当前 pc）。
func (c *CPU) enterTrap(cause, tval uint32) {
	c.csr.mepc = c.pc
	c.csr.mcause = cause
	c.csr.mtval = tval
	// mstatus: MPIE <- MIE, MIE <- 0
	c.csr.mstatus = (c.csr.mstatus &^ mstatusMPIE) |
		((c.csr.mstatus & mstatusMIE) << 4) // MIE(bit3)->MPIE(bit7)
	c.csr.mstatus &^= mstatusMIE
	c.pc = c.csr.mtvec &^ 0x3 // 直接取 base，忽略 vectored 模式的极简处理
}

// mret：从 M 级异常返回。
func (c *CPU) mret() {
	// MIE <- MPIE, MPIE <- 1
	if c.csr.mstatus&mstatusMPIE != 0 {
		c.csr.mstatus |= mstatusMIE
	} else {
		c.csr.mstatus &^= mstatusMIE
	}
	c.csr.mstatus |= mstatusMPIE
	c.pc = c.csr.mepc
}

// checkInterrupts：在取指前采样。若 MIE 开且 MTIP 挂起，则进入定时器中断。
func (c *CPU) checkInterrupts() {
	if c.csr.mstatus&mstatusMIE == 0 {
		return
	}
	// mip.MTIP 由外设/内核回填
	if c.csr.mip&mipMTIP != 0 && c.csr.mie&mieMTIE != 0 {
		c.enterTrap(causeMachineTimerInt, 0)
	}
}
