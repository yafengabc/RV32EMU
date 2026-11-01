package perip

// ESP32-C3 GPIO 基址。
const (
	GPIOBase uint32 = 0x60004000
	GPIOSize uint32 = 0x1000
)

// GPIO 最小子集：只实现 output 置位/清零/输出使能 的影子寄存器，
// 读取回影子值，不接真实引脚。
type GPIO struct {
	enable uint32 // 输出使能
	out    uint32 // 输出电平
}

func NewGPIO() *GPIO { return &GPIO{} }

func (g *GPIO) Name() string { return "GPIO" }

func (g *GPIO) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case 0x04: // enable
		return g.enable
	case 0x08: // out
		return g.out
	}
	return 0
}

func (g *GPIO) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case 0x04:
		g.enable = val
	case 0x08:
		g.out = val
	}
}
