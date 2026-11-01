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

const (
	RTCBase uint32 = 0x60008000
	RTCSize uint32 = 0x1000
)

type RTC struct {
	regs map[uint32]uint32
}

func NewRTC() *RTC { return &RTC{regs: make(map[uint32]uint32)} }

func (r *RTC) Name() string { return "RTC_CNTL" }

func (r *RTC) Read(offset uint32, size uint32) uint32 {
	return r.regs[offset&^0x3]
}

func (r *RTC) Write(offset uint32, size uint32, val uint32) {
	r.regs[offset&^0x3] = val
}
