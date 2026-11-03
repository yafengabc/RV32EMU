package perip

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

func TestSysTimerCounterAndLoad(t *testing.T) {
	s := NewSysTimer()
	// UNIT0 复位默认使能，直接跟着周期走。
	s.TickN(10)
	if got := s.Read(stUnit0ValLo, 4); got != 10 {
		t.Fatalf("unit0 lo = %d, want 10", got)
	}

	// UNITn_LOAD 的 bit0 才真正把 LOAD_HI/LO 装进计数器；OP 的 UPDATE 位
	// 只把当前计数值锁存到只读的 VALUE 影子寄存器，不改动计数器本身。
	s.Write(stUnit0LoadLo, 4, 0x80000000)
	s.Write(stUnit0LoadHi, 4, 0x1)
	s.Write(stUnit0Load, 4, 1)
	want := uint64(0x180000000)
	if s.units[0] != want {
		t.Fatalf("unit0 = 0x%X, want 0x%X", s.units[0], want)
	}
	if got := s.Read(stUnit0ValHi, 4); got != 0x1 {
		t.Fatalf("unit0 hi = 0x%X, want 0x1", got)
	}
	s.Write(stUnit0Op, 4, stOpUpdate)
	if s.regs[stUnit0Op]&stOpValidBit == 0 {
		t.Fatalf("OP UPDATE should latch VALID")
	}
	if s.units[0] != want {
		t.Fatalf("OP UPDATE changed counter to 0x%X", s.units[0])
	}

	// UNIT1 默认关闭，打开后才计数。
	s.Write(stConf, 4, stClkEn|stUnit0Work|stUnit1Work)
	s.TickN(5)
	if got := s.Read(stUnit1ValLo, 4); got != 5 {
		t.Fatalf("unit1 lo = %d, want 5", got)
	}
}

func TestSysTimerAlarmFiresOnce(t *testing.T) {
	s := NewSysTimer()
	intc := NewC3Intc()
	intc.SetCPU(cpu.New(mem.NewSystemBus()))
	s.SetIntc(intc)

	// 固件视角：TARGET0 → INTMTX source 37 → CPU line 2。
	intc.Write(SysTimerTarget0Src*4, 4, 2)
	intc.Write(intcEna, 4, 1<<2)
	intc.Write(intcPriBase+4*2, 4, 1)
	intc.Write(intcThresh, 4, 0)

	s.Write(stTarget0Lo, 4, 50)
	s.Write(stConf, 4, stClkEn|stUnit0Work|stTarget0En)
	s.Write(stComp0Load, 4, 1)
	s.Write(stIntEna, 4, 1)

	s.TickN(49)
	if got := s.Read(stIntSt, 4); got != 0 {
		t.Fatalf("interrupt fired early: int_st=0x%X", got)
	}
	s.TickN(1)
	if got := s.Read(stIntSt, 4); got != 1 {
		t.Fatalf("int_st = 0x%X, want 0x1", got)
	}
	if got := intc.Read(intcEip, 4); got != 1<<2 {
		t.Fatalf("eip = 0x%X, want line 2", got)
	}

	// 非周期模式：越过比较值后不再重复触发。
	s.TickN(100)
	if got := s.Read(stIntRaw, 4); got != 1 {
		t.Fatalf("int_raw = 0x%X, want 0x1", got)
	}

	// INT_CLR 同时撤掉 INTMTX 上的 source 电平。
	s.Write(stIntClr, 4, 1)
	if got := s.Read(stIntRaw, 4); got != 0 {
		t.Fatalf("int_raw after clear = 0x%X", got)
	}
	if got := intc.Read(intcEip, 4); got != 0 {
		t.Fatalf("eip after clear = 0x%X", got)
	}
}

func TestSysTimerPeriodMode(t *testing.T) {
	s := NewSysTimer()
	s.Write(stTarget0Lo, 4, 100)
	s.Write(stTarget0Conf, 4, stTgtPeriodM|100) // period mode, period=100
	s.Write(stConf, 4, stClkEn|stUnit0Work|stTarget0En)
	s.Write(stComp0Load, 4, 1)
	s.Write(stIntEna, 4, 1)

	s.TickN(100)
	if got := s.Read(stIntSt, 4); got != 1 {
		t.Fatalf("first period fire: int_st=0x%X", got)
	}
	if s.comps[0] != 200 {
		t.Fatalf("comp0 = %d, want 200", s.comps[0])
	}
	s.Write(stIntClr, 4, 1)

	// 一个批次跨过多个周期也只锁存一次 RAW。
	s.TickN(300)
	if got := s.Read(stIntSt, 4); got != 1 {
		t.Fatalf("period fire in batch: int_st=0x%X", got)
	}
	if s.comps[0] != 500 {
		t.Fatalf("comp0 = %d, want 500", s.comps[0])
	}
}
