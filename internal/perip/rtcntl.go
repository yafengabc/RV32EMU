package perip

// RTC_CNTL（0x6000_8000）模型。
//
// 内核在 plat_early_init 里关三个看门狗：TG0WDT、RTC WDT、super WDT。
// TG0WDT 在 C3Timer 里建模；RTC WDT 与 super WDT 在这里建模。三个看门狗
// 的配置寄存器都受 WPROTECT 保护，解锁字复位值就是寄存器初值：写回密钥
// 解锁，写任何其它值上锁。
//
// RTC WDT 寄存器偏移（ESP-IDF soc/esp32c3/register/soc/rtc_cntl_reg.h 与
// esp-rs/esp-pacs 一致）：
//
//	WDTCONFIG0   0x90  EN=bit31, STG0[30:28]..STG3[21:19]
//	WDTCONFIG1   0x94  STG0_HOLD
//	WDTCONFIG2   0x98  STG1_HOLD
//	WDTCONFIG3   0x9C  STG2_HOLD
//	WDTCONFIG4   0xA0  STG3_HOLD
//	WDTFEED      0xA4  写 bit31 喂狗
//	WDTWPROTECT  0xA8  WKEY = 0x50D83AA1
//
// super WDT（SWD）：
//
//	SWD_CONF     0xAC  AUTO_FEED_EN=31, DISABLE=30, FEED=29(WO),
//	                   RST_FLAG_CLR=28(WO), SIGNAL_WIDTH[27:18],
//	                   BYPASS_RST=17, FEED_INT=1(RO), RESET_FLAG=0(RO)
//	SWD_WPROTECT 0xB0  WKEY = 0x8F1D312A
//	SW_CPU_STALL 0xB4
//
// 时钟近似：RTC WDT 按慢时钟计数，模拟器把 1 条指令当作 1 个慢时钟周期
// （与 TG0WDT 的“1 条指令 ≈ 1 个 APB 周期”是同一类简化）。
//
// OPTIONS0 的 SW_SYS_RST 是唯一有整机语义的位：内核 `reboot` 靠它把机器
// 拉回复位状态（真机实测 rst:0x3 RTC_SW_SYS_RST）。RTC WDT 超时也走同一条
// 机器重装路径，只是复位原因不同。

const (
	RTCBase uint32 = 0x60008000
	RTCSize uint32 = 0x1000

	rtcOptions0   uint32 = 0x00
	rtcWdtCfg0    uint32 = 0x90
	rtcWdtCfg1    uint32 = 0x94
	rtcWdtCfg2    uint32 = 0x98
	rtcWdtCfg3    uint32 = 0x9C
	rtcWdtCfg4    uint32 = 0xA0
	rtcWdtFeed    uint32 = 0xA4
	rtcWdtProtect uint32 = 0xA8
	rtcSwdConf    uint32 = 0xAC
	rtcSwdProtect uint32 = 0xB0
	rtcSwCpuStall uint32 = 0xB4

	rtcSwSysRst uint32 = 1 << 31
	rtcWdtEn    uint32 = 1 << 31
	rtcWdtFeedB uint32 = 1 << 31
	rtcWdtKey   uint32 = 0x50D83AA1
	rtcSwdKey   uint32 = 0x8F1D312A

	rtcSwdDisable   uint32 = 1 << 30
	rtcSwdFeed      uint32 = 1 << 29
	rtcSwdRstFlagCl uint32 = 1 << 28
	rtcSwdResetFlag uint32 = 1 << 0
)

type RTC struct {
	regs map[uint32]uint32
	// reset 记录 SW_SYS_RST 被写进来的事实：真正把机器拉回复位状态的是
	// 外部主循环，这里只立旗。
	reset bool

	// RTC WDT
	wdtCfg0   uint32
	wdtHold   [4]uint32 // CONFIG1..4 = STG0..3_HOLD
	wdtUnlock bool
	wdtCount  uint64
	wdtFires  uint64
	wdtReset  bool

	// super WDT
	swdConf      uint32
	swdUnlock    bool
	swdResetFlag bool
	swdFeeds     uint64
}

func NewRTC() *RTC {
	return &RTC{
		// WKEY 复位值就是解锁字，复位后处于未上锁状态。
		regs: map[uint32]uint32{
			rtcWdtProtect: rtcWdtKey,
			rtcSwdProtect: rtcSwdKey,
		},
		wdtUnlock: true,
		swdUnlock: true,
		wdtHold:   [4]uint32{200000, 80000, 0xFFF, 0xFFF},
	}
}

func (r *RTC) Name() string { return "RTC_CNTL" }

func (r *RTC) Read(offset uint32, size uint32) uint32 {
	off := offset &^ 0x3
	if off == rtcSwdConf {
		// RESET_FLAG 是只读位，由内部状态提供。
		v := r.swdConf &^ rtcSwdResetFlag
		if r.swdResetFlag {
			v |= rtcSwdResetFlag
		}
		return v
	}
	if off == rtcWdtFeed {
		return 0
	}
	return r.regs[off]
}

func (r *RTC) Write(offset uint32, size uint32, val uint32) {
	off := offset &^ 0x3
	if off == rtcWdtFeed {
		// 真机是写 bit31 喂狗；写 0 不产生动作。
		if val&rtcWdtFeedB != 0 {
			r.wdtCount = 0
		}
		return
	}
	if off == rtcSwdConf {
		if r.swdUnlock {
			if val&rtcSwdFeed != 0 {
				r.swdFeeds++
			}
			if val&rtcSwdRstFlagCl != 0 {
				r.swdResetFlag = false
			}
			// FEED/RST_FLAG_CLR 是只写位，不保存在配置影子中。
			r.swdConf = val &^ (rtcSwdFeed | rtcSwdRstFlagCl | rtcSwdResetFlag)
			r.regs[off] = r.swdConf
		}
		return
	}
	switch off {
	case rtcOptions0:
		r.regs[off] = val
		// OPTIONS0 bit31 = RTC_CNTL_SW_SYS_RST（只写位）。
		if val&rtcSwSysRst != 0 {
			r.reset = true
		}
	case rtcWdtCfg0:
		if !r.wdtUnlock {
			return // 上锁时配置寄存器保持原值（读回也不变）
		}
		r.regs[off] = val
		if val&rtcWdtEn != 0 && r.wdtCfg0&rtcWdtEn == 0 {
			r.wdtCount = 0
		}
		r.wdtCfg0 = val
		if val&rtcWdtEn == 0 {
			r.wdtCount = 0
		}
	case rtcWdtCfg1, rtcWdtCfg2, rtcWdtCfg3, rtcWdtCfg4:
		if !r.wdtUnlock {
			return
		}
		r.regs[off] = val
		r.wdtHold[(off-rtcWdtCfg1)>>2] = val
	case rtcWdtProtect:
		r.regs[off] = val
		r.wdtUnlock = val == rtcWdtKey
	case rtcSwdProtect:
		r.regs[off] = val
		r.swdUnlock = val == rtcSwdKey
	default:
		r.regs[off] = val
	}
}

// Tick 按指令数推进 RTC WDT。只有 stage0 动作为 reset 类（>=2）时才真正
// 触发整机复位；动作为 interrupt/off 时只计数不动作（模拟器没有对应的
// RTC 中断线）。
func (r *RTC) Tick(n uint64) {
	if r.wdtCfg0&rtcWdtEn == 0 || r.wdtHold[0] == 0 || r.wdtReset {
		return
	}
	action := (r.wdtCfg0 >> 28) & 0x7
	if action < 2 { // 0=off, 1=interrupt
		return
	}
	r.wdtCount += n
	if r.wdtCount >= uint64(r.wdtHold[0]) {
		r.wdtCount = uint64(r.wdtHold[0])
		r.wdtFires++
		r.wdtReset = true
	}
}

// ResetPending 报告内核请求了软件复位（SW_SYS_RST）。
func (r *RTC) ResetPending() bool { return r.reset }

// WDTResetPending 报告 RTC WDT 已超时并要求整机复位。
func (r *RTC) WDTResetPending() bool { return r.wdtReset }

// WDTFires 返回 RTC WDT 超时次数（诊断/UI）。
func (r *RTC) WDTFires() uint64 { return r.wdtFires }

// RTCWDTState 是 RTC WDT 的 UI 快照。
type RTCWDTState struct {
	Enabled      bool   `json:"enabled"`
	Stage0       uint32 `json:"stage0"`
	Action       uint32 `json:"action"`
	Fires        uint64 `json:"fires"`
	ResetPending bool   `json:"resetPending"`
}

func (r *RTC) WDTState() RTCWDTState {
	return RTCWDTState{
		Enabled:      r.wdtCfg0&rtcWdtEn != 0,
		Stage0:       r.wdtHold[0],
		Action:       (r.wdtCfg0 >> 28) & 0x7,
		Fires:        r.wdtFires,
		ResetPending: r.wdtReset,
	}
}

// SetWDTEnabled 供控制协议（GUI）直接开关 RTC WDT。打开时若 stage0 动作
// 还是 off（0），顺手改成 reset system(3)，否则开关不会产生任何效果。
func (r *RTC) SetWDTEnabled(on bool) {
	if on {
		if action := (r.wdtCfg0 >> 28) & 0x7; action == 0 {
			r.wdtCfg0 |= 3 << 28
		}
		r.wdtCfg0 |= rtcWdtEn
		r.wdtCount = 0
	} else {
		r.wdtCfg0 &^= rtcWdtEn
		r.wdtCount = 0
	}
}

func (r *RTC) SetWDTHold(hold uint32) { r.wdtHold[0] = hold }

func (r *RTC) FeedWDT() { r.wdtCount = 0 }

// SWDState 是 super WDT 的 UI 快照。模拟器不建模它的超时复位
// （真机上它由硬件持续喂狗；这里只保留使能/禁用、喂狗计数与复位标志）。
type SWDState struct {
	Disabled  bool   `json:"disabled"`
	Feeds     uint64 `json:"feeds"`
	ResetFlag bool   `json:"resetFlag"`
}

func (r *RTC) SWDState() SWDState {
	return SWDState{
		Disabled:  r.swdConf&rtcSwdDisable != 0,
		Feeds:     r.swdFeeds,
		ResetFlag: r.swdResetFlag,
	}
}

// SetSWDDisabled 供控制协议开关 super WDT（true = 关掉）。
func (r *RTC) SetSWDDisabled(disabled bool) {
	if disabled {
		r.swdConf |= rtcSwdDisable
	} else {
		r.swdConf &^= rtcSwdDisable
	}
	r.regs[rtcSwdConf] = r.swdConf
}

// FeedSWD 供控制协议喂 super WDT。
func (r *RTC) FeedSWD() {
	r.swdFeeds++
	r.swdResetFlag = false
}
