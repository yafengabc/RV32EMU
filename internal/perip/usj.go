package perip

// USJ（USB-Serial/JTAG）控制台的最小模型。
//
// TinyOS 在 board/esp32c3/usj_console.c 里把 USJ 当作第二个控制台：
// 初始化只置 CONF0 的 DP_PULLUP / USB_PAD_ENABLE，输出路径每次先读
// EP1_CONF 的 IN_DATA_FREE，没空间就丢字符（非阻塞，绝不等待）。
//
// 真机上没插 USB 线时，这个端口就是一个黑洞：写进去的字节没有去处。
// 这里如实建模成「永远有空位、写进去就丢弃、永远没有输入」，好处是
// 内核的 USJ 路径被真正执行（读到的状态位合理），又不会把控制台输出
// 复制一遍。等真机插线验证时再决定要不要把它接到 stdout。
//
// 地址来自 usj_console.c：EP1(0x00) / EP1_CONF(0x04) / CONF0(0x18)。

const (
	USJBase uint32 = 0x60043000
	USJSize uint32 = 0x1000

	usjEp1Reg     uint32 = 0x00
	usjEp1ConfReg uint32 = 0x04
	usjConf0Reg   uint32 = 0x18

	usjWrDone       uint32 = 1 << 0
	usjInDataFree   uint32 = 1 << 1
	usjOutDataAvail uint32 = 1 << 2
)

type USJ struct {
	conf0 uint32
	ep1   uint32 // 最后写入的字节（读回 FIFO 时用，便于调试）
}

func NewUSJ() *USJ { return &USJ{} }

func (u *USJ) Name() string { return "USJ" }

func (u *USJ) Read(offset uint32, size uint32) uint32 {
	switch offset &^ 0x3 {
	case usjEp1Reg:
		return u.ep1 & 0xFF
	case usjEp1ConfReg:
		// TX FIFO 恒有空位，RX 恒无数据（线没插）。
		return usjInDataFree
	case usjConf0Reg:
		return u.conf0
	}
	return 0
}

func (u *USJ) Write(offset uint32, size uint32, val uint32) {
	switch offset &^ 0x3 {
	case usjEp1Reg:
		u.ep1 = val & 0xFF // 黑洞：丢弃
	case usjEp1ConfReg:
		// WR_DONE 等位是「写 1 触发后自清」，读回恒为 0；
		// 读到的始终是 IN_DATA_FREE（见 Read）。
	case usjConf0Reg:
		u.conf0 = val
	}
}
