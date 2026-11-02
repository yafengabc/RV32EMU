package perip

import "testing"

// TestTG0WDT 覆盖 TIMG0 看门狗的真机语义：CONFIG 要先用 WPROTECT 解锁，
// FEED 不受保护且清零计数，stage0 超时后立一次复位请求。
// 注意 CONFIG1(0x4C) 是预分频，CONFIG2(0x50) 才是 stage0 超时值。
func TestTG0WDT(t *testing.T) {
	tr := NewC3Timer(nil)

	// 复位值就是解锁字；显式写一个非密钥值把寄存器上锁。
	tr.Write(t0WdtProtect, 4, 0)
	tr.Write(t0WdtCfg2, 4, 10)
	tr.Write(t0WdtCfg0, 4, t0WdtEn)
	tr.TickN(20)
	if tr.WDTResetPending() || tr.WDTFires() != 0 {
		t.Fatal("locked CONFIG write must not enable the watchdog")
	}

	// 按真机流程解锁 → 配置 stage0 超时 → 开狗。
	tr.Write(t0WdtProtect, 4, t0WdtProtectKey)
	tr.Write(t0WdtCfg2, 4, 10)
	tr.Write(t0WdtCfg0, 4, t0WdtEn)
	tr.TickN(9)
	if tr.WDTResetPending() {
		t.Fatal("reset before stage0 timeout")
	}

	// FEED 写任意值都把计数清零。
	tr.Write(t0WdtFeed, 4, 0)
	tr.TickN(9)
	if tr.WDTResetPending() {
		t.Fatal("feed did not clear the watchdog count")
	}

	tr.TickN(1)
	if !tr.WDTResetPending() || tr.WDTFires() != 1 {
		t.Fatalf("expected one watchdog reset, got pending=%v fires=%d",
			tr.WDTResetPending(), tr.WDTFires())
	}
}

// TestTG0WDTDisable 确认关狗后不再计时（内核早期初始化做的正是这一步）。
func TestTG0WDTDisable(t *testing.T) {
	tr := NewC3Timer(nil)
	tr.Write(t0WdtProtect, 4, t0WdtProtectKey)
	tr.Write(t0WdtCfg2, 4, 10)
	tr.Write(t0WdtCfg0, 4, t0WdtEn)
	tr.Write(t0WdtCfg0, 4, 0) // 清 EN
	tr.TickN(1000)
	if tr.WDTResetPending() {
		t.Fatal("disabled watchdog must not reset")
	}
}

// TestTG0WDTPrescale 确认 CONFIG1 的预分频参与超时换算。
func TestTG0WDTPrescale(t *testing.T) {
	tr := NewC3Timer(nil)
	tr.Write(t0WdtProtect, 4, t0WdtProtectKey)
	tr.Write(t0WdtCfg1, 4, 2<<16) // CLK_PRESCALE = 2
	tr.Write(t0WdtCfg2, 4, 10)
	tr.Write(t0WdtCfg0, 4, t0WdtEn)
	tr.TickN(19)
	if tr.WDTResetPending() {
		t.Fatal("reset before stage0 timeout with prescale=2")
	}
	tr.TickN(1)
	if !tr.WDTResetPending() {
		t.Fatal("expected reset at 20 cycles with prescale=2")
	}
}

// TestGPIO 覆盖 C3 的输出/使能/输入语义：W1TS/W1TC 写 1 生效、
// 片内驱动与外部注入按 enable 合并成 IN。
func TestGPIO(t *testing.T) {
	g := NewGPIO()

	g.Write(gpioEnableW1TSReg, 4, 1<<3)
	g.Write(gpioOutW1TSReg, 4, 1<<3)
	if got := g.EffectiveIn(); got&(1<<3) == 0 {
		t.Fatalf("driven pin should read high, in=0x%X", got)
	}
	if got := g.Read(gpioOutW1TSReg, 4); got != 0 {
		t.Fatalf("W1TS should read back 0, got 0x%X", got)
	}

	// 关掉输出使能后，外部注入电平才反映到 IN。
	g.Write(gpioEnableW1TCReg, 4, 1<<3)
	if !g.SetInput(3, true) {
		t.Fatal("SetInput(3) rejected")
	}
	if got := g.EffectiveIn(); got&(1<<3) == 0 {
		t.Fatalf("external high level lost, in=0x%X", got)
	}
	if !g.SetInput(3, false) {
		t.Fatal("SetInput(3) rejected")
	}
	if got := g.EffectiveIn(); got&(1<<3) != 0 {
		t.Fatalf("external low level lost, in=0x%X", got)
	}

	if g.SetInput(GPIOPinCount, true) {
		t.Fatal("out-of-range pin accepted")
	}

	g.Write(gpioOutW1TCReg, 4, 1<<3)
	if g.State().Out != 0 {
		t.Fatalf("OUT should be cleared, got 0x%X", g.State().Out)
	}
}

// TestADC 覆盖最小 SAR ADC 模型：通道注入后 0x2C 读回通道 0 的码值。
func TestADC(t *testing.T) {
	a := NewADC()
	if !a.SetChannel(0, 2048) {
		t.Fatal("SetChannel(0) rejected")
	}
	if got := a.Read(adcData1Status, 4); got != 2048 {
		t.Fatalf("data status = %d, want 2048", got)
	}
	if a.SetChannel(ADCChannelCount, 1) {
		t.Fatal("out-of-range channel accepted")
	}
	ch := a.Channels()
	if len(ch) != ADCChannelCount || ch[0] != 2048 {
		t.Fatalf("Channels() = %v", ch)
	}
}

// TestRTCWDT 覆盖 RTC WDT 的 WPROTECT 语义与 stage0 超时复位。
func TestRTCWDT(t *testing.T) {
	r := NewRTC()

	// 写非密钥值上锁，之后 CONFIG 写入被忽略。
	r.Write(rtcWdtProtect, 4, 0)
	r.Write(rtcWdtCfg1, 4, 10)
	r.Write(rtcWdtCfg0, 4, rtcWdtEn|3<<28)
	r.Tick(1000)
	if r.WDTResetPending() {
		t.Fatal("locked CONFIG write must not enable the watchdog")
	}

	// 解锁后配置 stage0 超时（CONFIG1 = STG0_HOLD）并开狗。
	r.Write(rtcWdtProtect, 4, rtcWdtKey)
	r.Write(rtcWdtCfg1, 4, 10)
	r.Write(rtcWdtCfg0, 4, rtcWdtEn|3<<28)
	r.Tick(9)
	if r.WDTResetPending() {
		t.Fatal("reset before stage0 timeout")
	}
	r.FeedWDT()
	r.Tick(9)
	if r.WDTResetPending() {
		t.Fatal("feed did not clear the watchdog count")
	}
	r.Tick(1)
	if !r.WDTResetPending() || r.WDTFires() != 1 {
		t.Fatalf("expected one reset, pending=%v fires=%d", r.WDTResetPending(), r.WDTFires())
	}
}

// TestRTCSuperWDT 覆盖 super WDT 的禁用位、喂狗计数与复位标志清除。
func TestRTCSuperWDT(t *testing.T) {
	r := NewRTC()
	if r.SWDState().Disabled {
		t.Fatal("super WDT should start enabled")
	}
	r.SetSWDDisabled(true)
	if !r.SWDState().Disabled {
		t.Fatal("SetSWDDisabled(true) ignored")
	}
	r.FeedSWD()
	if got := r.SWDState().Feeds; got != 1 {
		t.Fatalf("feeds = %d, want 1", got)
	}
	r.swdResetFlag = true
	if !r.SWDState().ResetFlag {
		t.Fatal("reset flag not reflected in state")
	}
	r.Write(rtcSwdConf, 4, rtcSwdRstFlagCl|rtcSwdDisable)
	if r.SWDState().ResetFlag {
		t.Fatal("RST_FLAG_CLR did not clear the reset flag")
	}
}

// TestRTCSoftReset 确认 OPTIONS0 bit31 会立起软件复位请求。
func TestRTCSoftReset(t *testing.T) {
	r := NewRTC()
	r.Write(rtcOptions0, 4, rtcSwSysRst)
	if !r.ResetPending() {
		t.Fatal("SW_SYS_RST did not request a reset")
	}
}
