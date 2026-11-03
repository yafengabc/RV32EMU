package cpu

import (
	"rv32emu/internal/mem"
)

// 异常/中断 cause 编码（mcause）。
const (
	causeInstrMisalign uint32 = 0
	causeIllegalInstr  uint32 = 2
	causeBreakpoint    uint32 = 3
	causeLoadMisalign  uint32 = 4
	causeLoadFault     uint32 = 5
	causeStoreMisalign uint32 = 6
	causeStoreFault    uint32 = 7
	causeEcallU        uint32 = 8
	causeEcallM        uint32 = 11
	causeInterruptBit  uint32 = 0x80000000
	// C3 定时器经 INTMTX 把 TG0 T0 路由到 CPU line 2，
	// esp-riscv 核上报的是"路由后的 line"而非 machine-external(11)，
	// 故 tick 的 mcause = 0x80000000 | 2（真机实测，见 PLATFORM-DESIGN §8.6）。
	causeC3Tick uint32 = causeInterruptBit | 2
)

// enterTrap 进入 trap：记录 mepc/mcause/mtval，设置 MPP、关中断，跳 mtvec。
//   - epc：异常时为故障指令地址（尚未自增）；中断时为被抢占的下一条指令
//     （即 Step 取指前的 c.pc）。
//   - MPP 设为进入 trap 前的特权级（c.priv），随后切到 M-mode。
//   - mtvec 强制按 vectored 处理（C3 硬件只支持 vectored，WARL 行为）：
//     同步异常跳 base，中断跳 base + 4*cause_low。
func (c *CPU) enterTrap(cause, tval, epc uint32) {
	// ROM 区里的非法指令视为「调用 ROM 函数」：模拟器用 Go 例程顶替
	// 缺失的 ROM 镜像，执行后直接返回到 ra（不进 trap）。
	if cause == causeIllegalInstr && c.RomCall != nil &&
		epc >= mem.ROMBase && epc < mem.ROMBase+mem.ROMSize {
		c.RomCall(c, epc)
		c.pc = c.Reg(1)
		return
	}
	if cause&causeInterruptBit == 0 {
		c.excCount++
	} else {
		c.intCount++
	}
	c.csr.mepc = epc
	c.csr.mcause = cause
	c.csr.mtval = tval
	// mstatus.MPP <- 进入异常前的特权级
	c.csr.mstatus = (c.csr.mstatus &^ mstatusMPP) | (c.priv << 11)
	// MPIE <- MIE, MIE <- 0
	if c.csr.mstatus&mstatusMIE != 0 {
		c.csr.mstatus |= mstatusMPIE
	}
	c.csr.mstatus &^= mstatusMIE
	c.priv = 3 // 进入 M-mode

	if c.OnTrap != nil {
		c.OnTrap(cause, epc, tval)
	}

	// vectored 派发（C3 只支持 vectored）
	base := c.csr.mtvec &^ 0x3
	mode := c.csr.mtvec & 0x3
	if mode == 1 && (cause&causeInterruptBit) != 0 {
		base += 4 * (cause & 0x1f)
	}
	c.pc = base
}

// mret：从 M 级异常返回，恢复特权级。
func (c *CPU) mret() {
	// MIE <- MPIE, MPIE <- 1
	if c.csr.mstatus&mstatusMPIE != 0 {
		c.csr.mstatus |= mstatusMIE
	} else {
		c.csr.mstatus &^= mstatusMIE
	}
	c.csr.mstatus |= mstatusMPIE
	// 返回到 MPP 记录的特权级（M=3 / U=0）
	c.priv = (c.csr.mstatus >> 11) & 3
	c.pc = c.csr.mepc
}

// checkInterrupts：取指前采样。若有挂起中断且 MIE 开，则投递。
func (c *CPU) checkInterrupts() {
	if c.pendingInt == 0 {
		return
	}
	if c.csr.mstatus&mstatusMIE == 0 {
		return
	}
	cause := c.pendingInt
	c.pendingInt = 0
	// 中断的 mepc 指向被抢占的指令（Step 传入的 c.pc）。
	c.enterTrap(cause, 0, c.pc)
}
