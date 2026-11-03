package perip

// SYSTIMER（系统定时器）：2 个 52 位递增计数器 + 3 个比较器。
// ESP-IDF 的 esp_timer / FreeRTOS tick 都建立在 TARGET0 报警上，
// MicroPython 也走同一条路径，所以这里按 C3 的寄存器语义建模。
//
// 时间基准与 TIMG0 一致：1 条指令 = 1 个 APB 周期（80 MHz），
// 计数器每个周期 +1。真机上 SYSTIMER 还可以选 XTAL 时钟，本模型不区分。
//
// 中断不直接投递：TARGET0..2 对应 INTMTX 的 source 37..39，
// 由 C3Intc 按固件配置的映射/优先级/阈值转换成 CPU line。
const (
	SysTimerBase uint32 = 0x60023000
	SysTimerSize uint32 = 0x1000
)

// SYSTIMER 寄存器偏移（soc/esp32c3/register/systimer_reg.h）。
const (
	stConf         uint32 = 0x00
	stUnit0Op      uint32 = 0x04
	stUnit1Op      uint32 = 0x08
	stUnit0LoadHi  uint32 = 0x0C
	stUnit0LoadLo  uint32 = 0x10
	stUnit1LoadHi  uint32 = 0x14
	stUnit1LoadLo  uint32 = 0x18
	stTarget0Hi    uint32 = 0x1C
	stTarget0Lo    uint32 = 0x20
	stTarget1Hi    uint32 = 0x24
	stTarget1Lo    uint32 = 0x28
	stTarget2Hi    uint32 = 0x2C
	stTarget2Lo    uint32 = 0x30
	stTarget0Conf  uint32 = 0x34
	stTarget1Conf  uint32 = 0x38
	stTarget2Conf  uint32 = 0x3C
	stUnit0ValHi   uint32 = 0x40
	stUnit0ValLo   uint32 = 0x44
	stUnit1ValHi   uint32 = 0x48
	stUnit1ValLo   uint32 = 0x4C
	stComp0Load    uint32 = 0x50
	stComp1Load    uint32 = 0x54
	stComp2Load    uint32 = 0x58
	stUnit0Load    uint32 = 0x5C
	stUnit1Load    uint32 = 0x60
	stIntEna       uint32 = 0x64
	stIntRaw       uint32 = 0x68
	stIntClr       uint32 = 0x6C
	stIntSt        uint32 = 0x70
	stDate         uint32 = 0xFC
	stDateResetVal uint32 = 33579377
)

const (
	stClkEn         uint32 = 1 << 31 // 寄存器时钟门控（不影响计数，真机亦然）
	stUnit0Work     uint32 = 1 << 30
	stUnit1Work     uint32 = 1 << 29
	stTarget0En     uint32 = 1 << 24
	stTarget1En     uint32 = 1 << 25
	stTarget2En     uint32 = 1 << 26
	stOpUpdate      uint32 = 1 << 30
	stOpValidBit    uint32 = 1 << 29
	stTgtPeriodM    uint32 = 1 << 30
	stTgtUnitSel    uint32 = 1 << 31
	stUnitMask      uint64 = (1 << 52) - 1
	stTgtHiMask     uint32 = 0x000FFFFF
	stTgtPeriodMask uint32 = 0x03FFFFFF
)

// INTMTX source：ETS_SYSTIMER_TARGET0_INTR_SOURCE。
const (
	SysTimerTarget0Src uint32 = 37
)

type SysTimer struct {
	conf      uint32
	units     [2]uint64
	loads     [2][2]uint32 // [单元][HI,LO]
	targets   [3]uint64
	targetCfg [3]uint32
	comps     [3]uint64
	fired     [3]bool // 一次性报警是否已消费（写 COMPn_LOAD 时重新布防）
	intEna    uint32
	intRaw    uint32
	fires     [3]uint64
	regs      map[uint32]uint32

	intc *C3Intc
}

func NewSysTimer() *SysTimer {
	return &SysTimer{
		conf: stUnit0Work, // UNIT0 复位默认使能（与 true ROM 一致）
		regs: map[uint32]uint32{stDate: stDateResetVal},
	}
}

// SetIntc 注入中断矩阵，报警时按固件配置的 source→CPU line 投递。
func (s *SysTimer) SetIntc(i *C3Intc) { s.intc = i }

func (s *SysTimer) Name() string { return "C3SYSTIMER" }

// Tick 推进一个周期。
func (s *SysTimer) Tick() { s.TickN(1) }

// TickN 推进 n 个周期：两个计数器按各自的 WORK_EN 递增，并检查
// TARGET0..2 是否达到比较值。批量执行时每批只采样一次，
// 而不是逐周期比较，避免每条指令一次回调。
func (s *SysTimer) TickN(n uint64) {
	if n == 0 {
		return
	}
	if s.conf&stUnit0Work != 0 {
		s.units[0] = (s.units[0] + n) & stUnitMask
	}
	if s.conf&stUnit1Work != 0 {
		s.units[1] = (s.units[1] + n) & stUnitMask
	}

	for t := 0; t < 3; t++ {
		if s.conf&stTargetEn(t) == 0 {
			continue
		}
		unit := 0
		if s.targetCfg[t]&stTgtUnitSel != 0 {
			unit = 1
		}
		if s.conf&stUnitEn(unit) == 0 {
			continue
		}
		cur := s.units[unit]
		comp := s.comps[t]
		// 计数器只递增：报警是「计数达到比较值」的电平事件。比较值允许
		// 就是 0（固件只写 TARGET_CONF 的 period 字段、再 COMP_LOAD，
		// 初始比较值保持 0；此时第一个周期立即到点），因此这里用
		// cur >= comp 而不是「本批区间跨过 comp」。
		if cur < comp {
			continue
		}
		// 一次性报警按「边沿」语义：比较值被计数器命中的那一刻触发一次，
		// 之后保持 RAW 直到固件清中断，不会因为计数器还停在比较值之上
		// 就反复触发。周期模式不需要这个锁存：触发后比较值自动前移。
		if s.targetCfg[t]&stTgtPeriodM == 0 {
			if s.fired[t] {
				continue
			}
			s.fired[t] = true
		}
		s.fires[t]++
		s.intRaw |= 1 << uint(t)
		if s.targetCfg[t]&stTgtPeriodM != 0 {
			period := uint64(s.targetCfg[t] & stTgtPeriodMask)
			if period == 0 {
				s.comps[t] = cur + 1
			} else {
				for s.comps[t] <= cur {
					s.comps[t] += period
				}
			}
		}
	}
	s.syncIRQ()
}

// syncIRQ 把「RAW & ENA」同步到 INTMTX 的对应 source 上。
// 这里按 source 逐个 raise/clear，交给 C3Intc 决定最终用哪条 CPU line。
func (s *SysTimer) syncIRQ() {
	if s.intc == nil {
		return
	}
	for t := 0; t < 3; t++ {
		src := SysTimerTarget0Src + uint32(t)
		if s.intRaw&(1<<uint(t)) != 0 && s.intEna&(1<<uint(t)) != 0 {
			s.intc.Raise(src)
		} else {
			s.intc.ClearSource(src)
		}
	}
	s.intc.Deliver()
}

func stTargetEn(t int) uint32 {
	return [3]uint32{stTarget0En, stTarget1En, stTarget2En}[t]
}

func stUnitEn(u int) uint32 {
	if u == 0 {
		return stUnit0Work
	}
	return stUnit1Work
}

func (s *SysTimer) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case stConf:
		return s.conf
	case stUnit0Op, stUnit1Op:
		return s.regs[offset]
	case stUnit0ValHi, stUnit0ValLo, stUnit1ValHi, stUnit1ValLo:
		i := 0
		if offset == stUnit1ValHi || offset == stUnit1ValLo {
			i = 1
		}
		if offset == stUnit0ValHi || offset == stUnit1ValHi {
			return uint32(s.units[i]>>32) & stTgtHiMask
		}
		return uint32(s.units[i])
	case stIntEna:
		return s.intEna
	case stIntRaw:
		return s.intRaw
	case stIntSt:
		return s.intRaw & s.intEna
	case stIntClr:
		return 0
	}
	if v, ok := s.regs[offset]; ok {
		return v
	}
	return 0
}

func (s *SysTimer) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case stConf:
		s.conf = val & (stClkEn | stUnit0Work | stUnit1Work |
			stTarget0En | stTarget1En | stTarget2En)
	case stUnit0Op, stUnit1Op:
		// OP 的 UPDATE 位只是把当前计数值锁存到只读的 UNITn_VALUE 影子
		// 寄存器（HAL 用「写 UPDATE → 等 VALID → 读 VALUE」拿一次原子快照），
		// 不改动计数器本身。计数器重装走 UNITn_LOAD 的 bit0。
		// 我们直接按 units 实时读，所以这里只需回填 VALID 状态位。
		s.regs[offset] = val &^ stOpUpdate
		if val&stOpUpdate != 0 {
			s.regs[offset] |= stOpValidBit
		}
	case stUnit0LoadHi, stUnit0LoadLo, stUnit1LoadHi, stUnit1LoadLo:
		s.regs[offset] = val
		i := 0
		if offset == stUnit1LoadHi || offset == stUnit1LoadLo {
			i = 1
		}
		if offset == stUnit0LoadHi || offset == stUnit1LoadHi {
			s.loads[i][0] = val & stTgtHiMask
		} else {
			s.loads[i][1] = val
		}
	case stTarget0Hi, stTarget0Lo, stTarget1Hi, stTarget1Lo, stTarget2Hi, stTarget2Lo:
		s.regs[offset] = val
		t := int((offset - stTarget0Hi) / 8)
		if (offset-stTarget0Hi)%8 == 0 {
			s.targets[t] = uint64(val&stTgtHiMask)<<32 | s.targets[t]&0xFFFFFFFF
		} else {
			s.targets[t] = s.targets[t]&^0xFFFFFFFF | uint64(val)
		}
	case stTarget0Conf, stTarget1Conf, stTarget2Conf:
		s.regs[offset] = val
		s.targetCfg[int((offset-stTarget0Conf)/4)] = val
	case stComp0Load, stComp1Load, stComp2Load:
		t := int((offset - stComp0Load) / 4)
		s.comps[t] = s.targets[t]
		s.fired[t] = false // 重新布防
	case stUnit0Load, stUnit1Load:
		i := 0
		if offset == stUnit1Load {
			i = 1
		}
		if val&1 != 0 {
			s.units[i] = (uint64(s.loads[i][1]) | uint64(s.loads[i][0])<<32) & stUnitMask
		}
	case stIntEna:
		s.intEna = val & 0x7
		s.regs[offset] = s.intEna
		s.syncIRQ()
	case stIntClr:
		s.intRaw &^= val & 0x7
		s.syncIRQ()
	case stIntRaw:
		// R/WTC：写 1 也相当于清（硬件上 raw 由事件置位）。
		s.intRaw &^= val & 0x7
		s.syncIRQ()
	case stDate:
		s.regs[offset] = val
	}
}
