package perip

// ESP32-C3 GPIO 基址（技术参考手册）。
const (
	GPIOBase uint32 = 0x60004000
	GPIOSize uint32 = 0x1000
)

// C3 的 GPIO 寄存器偏移（ESP-IDF soc/esp32c3/register/soc/gpio_reg.h）。
const (
	gpioOutReg        uint32 = 0x04 // 输出电平
	gpioOutW1TSReg    uint32 = 0x08 // 写 1 置位
	gpioOutW1TCReg    uint32 = 0x0C // 写 1 清零
	gpioEnableReg     uint32 = 0x20 // 输出使能
	gpioEnableW1TSReg uint32 = 0x24
	gpioEnableW1TCReg uint32 = 0x28
	gpioInReg         uint32 = 0x3C // 引脚输入电平（只读）
	gpioStatusReg     uint32 = 0x44
	gpioStatusW1TSReg uint32 = 0x48
	gpioStatusW1TCReg uint32 = 0x4C
	gpioPin0Reg       uint32 = 0x74 // PIN0，之后每个引脚 +4
)

// GPIOPinCount 是 C3 的引脚数：GPIO0..GPIO21。
const GPIOPinCount = 22

const gpioPinMask uint32 = 1<<GPIOPinCount - 1

// GPIO 实现 C3 的输出/使能/输入最小集合，并把“外部注入电平”与
// “片内输出电平”分开建模：
//   - enable 为 1 的引脚，IN 读回输出锁存值（引脚被片内驱动）；
//   - enable 为 0 的引脚，IN 读回外部注入值（GUI/测试可以点击翻转）。
//
// 真机上 IN 读的是焊盘电平，输出使能时由片内驱动决定；这里按同一语义
// 合并成 EffectiveIn()。W1TS/W1TC 是写 1 生效的置位/清零寄存器。
type GPIO struct {
	out    uint32
	enable uint32
	ext    uint32 // 外部注入电平（enable=0 时反映到 IN）
	status uint32
	pinCfg [GPIOPinCount]uint32
}

func NewGPIO() *GPIO { return &GPIO{} }

func (g *GPIO) Name() string { return "GPIO" }

// GPIOState 是供控制协议/GUI 使用的快照。
type GPIOState struct {
	Out    uint32 `json:"out"`
	Enable uint32 `json:"enable"`
	In     uint32 `json:"in"`
	Ext    uint32 `json:"ext"`
	Status uint32 `json:"status"`
}

func (g *GPIO) State() GPIOState {
	return GPIOState{Out: g.out, Enable: g.enable, In: g.effectiveIn(), Ext: g.ext, Status: g.status}
}

// SetInput 设置某个引脚的外部输入电平（0..GPIOPinCount-1）。
// 返回 false 表示引脚号越界。
func (g *GPIO) SetInput(pin int, level bool) bool {
	if pin < 0 || pin >= GPIOPinCount {
		return false
	}
	bit := uint32(1) << uint(pin)
	if level {
		g.ext |= bit
	} else {
		g.ext &^= bit
	}
	return true
}

// EffectiveIn 返回 IN 寄存器语义的引脚电平。
func (g *GPIO) EffectiveIn() uint32 { return g.effectiveIn() }

func (g *GPIO) effectiveIn() uint32 {
	return (g.out & g.enable) | (g.ext &^ g.enable)
}

func (g *GPIO) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case gpioOutReg:
		return g.out
	case gpioEnableReg:
		return g.enable
	case gpioInReg:
		return g.effectiveIn()
	case gpioStatusReg:
		return g.status
	case gpioOutW1TSReg, gpioOutW1TCReg,
		gpioEnableW1TSReg, gpioEnableW1TCReg,
		gpioStatusW1TSReg, gpioStatusW1TCReg:
		// 写 1 生效的寄存器读回 0。
		return 0
	}
	if offset >= gpioPin0Reg && offset < gpioPin0Reg+4*GPIOPinCount {
		return g.pinCfg[(offset-gpioPin0Reg)>>2]
	}
	return 0
}

func (g *GPIO) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case gpioOutReg:
		g.out = val & gpioPinMask
	case gpioOutW1TSReg:
		g.out |= val & gpioPinMask
	case gpioOutW1TCReg:
		g.out &^= val & gpioPinMask
	case gpioEnableReg:
		g.enable = val & gpioPinMask
	case gpioEnableW1TSReg:
		g.enable |= val & gpioPinMask
	case gpioEnableW1TCReg:
		g.enable &^= val & gpioPinMask
	case gpioStatusReg:
		g.status = val & gpioPinMask
	case gpioStatusW1TSReg:
		g.status |= val & gpioPinMask
	case gpioStatusW1TCReg:
		g.status &^= val & gpioPinMask
	case gpioInReg:
		// 只读；写入忽略。
	default:
		if offset >= gpioPin0Reg && offset < gpioPin0Reg+4*GPIOPinCount {
			// IO MUX / 上拉等配置只做影子，当前不影响数字电平模型。
			g.pinCfg[(offset-gpioPin0Reg)>>2] = val
		}
	}
}
