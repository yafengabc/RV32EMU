package perip

// ESP32-C3 的 SAR ADC（APB_SARADC）最小模型。
//
// C3 只有 ADC，没有 DAC；ADC 通道由 GUI/测试注入原始码值，数据寄存器
// 读回注入值。真机的采样序列/仲裁器/DMA 没有建模——当前内核并不使用
// ADC，GUI 只需要“设置并显示当前值”这一层。
const (
	ADCBase uint32 = 0x60040000
	ADCSize uint32 = 0x1000

	adcData1Status uint32 = 0x2C // APB_SARADC_1_DATA_STATUS，ADC1_DATA[16:0]
	adcData2Status uint32 = 0x30 // APB_SARADC_2_DATA_STATUS
	adcIntRaw      uint32 = 0x44 // 转换完成中断原始状态，bit31/30 = ADC1/ADC2 done

	ADCChannelCount = 5 // C3 ADC1_CH0..CH4（对应 GPIO0..GPIO4）
	adcMaxRaw       = 0xFFF
)

type ADC struct {
	regs     map[uint32]uint32
	channels [ADCChannelCount]uint32
}

func NewADC() *ADC {
	return &ADC{regs: make(map[uint32]uint32)}
}

func (a *ADC) Name() string { return "APB_SARADC" }

func (a *ADC) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case adcData1Status:
		return a.channels[0] & adcMaxRaw
	case adcIntRaw:
		// 固件启动时用阻塞式采样：写启动位后轮询 INT_RAW 的 done 位。
		// 模拟器不建模采样时序，直接报告转换完成。
		return 0xC0000000
	}
	if v, ok := a.regs[offset]; ok {
		return v
	}
	return 0
}

func (a *ADC) Write(offset uint32, size uint32, val uint32) {
	a.regs[offset] = val
}

// SetChannel 注入某个 ADC 通道的原始码值（12-bit，超出范围会截断）。
func (a *ADC) SetChannel(ch int, raw uint32) bool {
	if ch < 0 || ch >= ADCChannelCount {
		return false
	}
	a.channels[ch] = raw & adcMaxRaw
	return true
}

// Channels 返回所有通道的当前码值（供控制协议/GUI 快照）。
func (a *ADC) Channels() []uint32 {
	out := make([]uint32, ADCChannelCount)
	copy(out, a.channels[:])
	return out
}
