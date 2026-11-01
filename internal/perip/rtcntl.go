package perip

// RTC_CNTL（0x6000_8000）的被动模型。
//
// 内核在 plat_early_init 里关三个看门狗：TG0WDT、RTC WDT、super WDT。
// 做法都是「先写保护字解锁 → 改配置 / 写 0 关掉」。模拟器里没有看门狗
// 会复位，所以这些写只需要被接受并保留读回值，否则内核会一直盯着一个
// 未映射区间写日志。
//
// 真机上真正会咬人的是 TG0WDT（ROM 交出来时是开着的，约 1 秒复位），
// 已在 C3Timer 一侧单独处理；这里的 RTC/super WDT 只是让关狗代码跑通。
//
// 唯一有真实语义的是 OPTIONS0 的 SW_SYS_RST：内核的 `reboot` 命令靠它
// 把机器拉回复位状态（真机实测 rst:0x3 RTC_SW_SYS_RST）。模拟器里没有
// 「复位」这回事可做，只能立旗让 main 重装一台机器——这正是复位在硬件上
// 的效果：SRAM、外设、CPU 全部重来，只有 SPI flash 的内容留着。

const (
	RTCBase uint32 = 0x60008000
	RTCSize uint32 = 0x1000

	rtcOptions0 uint32 = 0x00
	rtcSwSysRst uint32 = 1 << 31
)

type RTC struct {
	regs map[uint32]uint32
	// reset 记录 SW_SYS_RST 被写进来的事实：RTC_CNTL 只是一个寄存器，
	// 真正把机器拉回复位状态的是外部主循环（看门狗、时钟、CPU 寄存器
	// 全都要重来），所以这里只立旗，由 main 的机器循环消费。
	reset bool
}

func NewRTC() *RTC { return &RTC{regs: make(map[uint32]uint32)} }

func (r *RTC) Name() string { return "RTC_CNTL" }

func (r *RTC) Read(offset uint32, size uint32) uint32 {
	return r.regs[offset&^0x3]
}

func (r *RTC) Write(offset uint32, size uint32, val uint32) {
	r.regs[offset&^0x3] = val
	// OPTIONS0 bit31 = RTC_CNTL_SW_SYS_RST（只写位）。真机写下去立即复位，
	// 复位原因寄存器报 rst:0x3 (RTC_SW_SYS_RST)，ROM 随即从 flash 重新启动。
	if offset&^0x3 == rtcOptions0 && val&rtcSwSysRst != 0 {
		r.reset = true
	}
}

// ResetPending 报告内核请求了软件复位。
func (r *RTC) ResetPending() bool { return r.reset }
