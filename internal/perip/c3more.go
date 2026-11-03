package perip

import (
	"rv32emu/internal/mem"
)

// ESP32-C3 除 WiFi/BLE 之外的标准外设补充模型。
//
// 分两类：
//   - 功能模型：UART1（复用 UART，TX 丢弃、无 RX）、SPI2（GP-SPI）、I2C0；
//   - 寄存器影子：时钟/引脚/总线配置（SYSTEM/SENSITIVE/IO_MUX/SYSCON）以及
//     TIMG1、LEDC、RMT、TWAI、I2S0、UHCI0、RTC_I2C、GDMA、加密加速器等。
//
// 影子模型只保证「写进去能读回来」，让固件的时钟/引脚/外设初始化流程
// 不会撞在未映射地址上；它不产生中断、不驱动引脚，也不模拟触发/完成
// 时序。需要轮询完成位的模块（RMT/TWAI/I2S/GDMA/加密）如果固件按真机
// 时序等待，仍可能停住——目前没有固件使用它们。

// ---- 外设窗口 --------------------------------------------------------

const (
	SYSTEMBase        uint32 = 0x600C0000 // 时钟/复位/CPU 控制
	SENSITIVEBase     uint32 = 0x600C1000
	IOMUXBase         uint32 = 0x60009000 // IO_MUX
	SYSCONBase        uint32 = 0x60026000 // APB_CTRL / SYSCON
	TIMG1Base         uint32 = 0x60020000
	LEDCBase          uint32 = 0x60019000
	RMTBase           uint32 = 0x60016000
	TWAIBase          uint32 = 0x6002B000
	I2S0Base          uint32 = 0x6002D000
	UHCI0Base         uint32 = 0x60014000
	RTCI2CBase        uint32 = 0x6000E000
	DedicatedGPIOBase uint32 = 0x600CF000
	WorldCntlBase     uint32 = 0x600D0000
	GDMABase          uint32 = 0x6003F000
	AESBase           uint32 = 0x6003A000
	SHABase           uint32 = 0x6003B000
	RSABase           uint32 = 0x6003C000
	DSBase            uint32 = 0x6003D000
	HMACBase          uint32 = 0x6003E000
	AESXTSBase        uint32 = 0x600CC000
	AssistDebugBase   uint32 = 0x600CE000

	extraWindowSize uint32 = 0x1000
)

// ---- SPI2（GP-SPI）---------------------------------------------------

const (
	SPI2Base uint32 = 0x60024000
	SPI2Size uint32 = 0x1000

	gspiCmdReg    uint32 = 0x00
	gspiUserReg   uint32 = 0x10
	gspiUser1Reg  uint32 = 0x14
	gspiUser2Reg  uint32 = 0x18
	gspiMsDlenReg uint32 = 0x1C
	gspiIntEna    uint32 = 0x34
	gspiIntClr    uint32 = 0x38
	gspiIntRaw    uint32 = 0x3C
	gspiIntSt     uint32 = 0x40
	gspiW0Reg     uint32 = 0x98
	gspiW15Reg    uint32 = 0xD4

	gspiCmdUsr    uint32 = 1 << 24 // USR：写 1 触发一次传输，自清
	gspiCmdUpdate uint32 = 1 << 23 // UPDATE：写 1 生效，自清

	gspiUserCommand uint32 = 1 << 31
	gspiUserAddr    uint32 = 1 << 30
	gspiUserDummy   uint32 = 1 << 29
	gspiUserMiso    uint32 = 1 << 28
	gspiUserMosi    uint32 = 1 << 27

	gspiTransDone uint32 = 1 << 12 // DMA_INT_RAW.TRANS_DONE

	spi2Src uint32 = 19 // ETS_SPI2_INTR_SOURCE
)

// SPI2 是通用 SPI（GP-SPI）主机。没有接外部器件，传输按“环回”处理：
// 同时打开 MOSI/MISO 时把 W0..W15 里的发送数据原样当作接收数据；
// 只开 MISO 时接收清零；只开 MOSI 时只表示发出去（不改变 W）。
// 传输完成置 TRANS_DONE 中断位，供固件轮询或走 INTMTX。
type SPI2 struct {
	regs map[uint32]uint32
	raw  uint32
	intc *C3Intc
}

func NewSPI2() *SPI2 {
	return &SPI2{regs: make(map[uint32]uint32)}
}

func (s *SPI2) Name() string { return "SPI2" }

func (s *SPI2) SetIntc(i *C3Intc) { s.intc = i }

func (s *SPI2) syncIRQ() {
	if s.intc == nil {
		return
	}
	if s.raw&s.regs[gspiIntEna] != 0 {
		s.intc.Raise(spi2Src)
	} else {
		s.intc.ClearSource(spi2Src)
	}
	s.intc.Deliver()
}

func (s *SPI2) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case gspiCmdReg:
		return s.regs[offset] &^ (gspiCmdUsr | gspiCmdUpdate)
	case gspiIntRaw:
		return s.raw
	case gspiIntSt:
		return s.raw & s.regs[gspiIntEna]
	}
	return s.regs[offset]
}

func (s *SPI2) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case gspiCmdReg:
		// USR 与 UPDATE 都是写 1 自清；读回时保持 0。
		s.regs[gspiCmdReg] = val &^ (gspiCmdUsr | gspiCmdUpdate)
		if val&gspiCmdUsr != 0 {
			s.transfer()
		}
	case gspiIntClr:
		s.raw &^= val
		s.syncIRQ()
	case gspiIntEna:
		s.regs[offset] = val
		s.syncIRQ()
	default:
		s.regs[offset] = val
	}
}

// transfer 执行一次 CPU 控制的 SPI 传输（MS_DLEN 给出的比特数）。
func (s *SPI2) transfer() {
	user := s.regs[gspiUserReg]
	bitLen := int(s.regs[gspiMsDlenReg]&0x3FFFF) + 1
	n := (bitLen + 7) / 8
	if n > 64 { // W0..W15 一共 16 个字 = 64 字节
		n = 64
	}
	if user&gspiUserMiso != 0 {
		if user&gspiUserMosi != 0 {
			// 环回：没有外部器件，把发送数据当作接收数据。
		} else {
			for i := 0; i < n; i++ {
				reg := gspiW0Reg + uint32(i/4)*4
				shift := uint(8 * (i % 4))
				s.regs[reg] &^= 0xFF << shift
			}
		}
	}
	s.raw |= gspiTransDone
	s.syncIRQ()
}

// ---- I2C0 ------------------------------------------------------------

const (
	I2C0Base uint32 = 0x60013000
	I2CSize  uint32 = 0x1000

	i2cCtrReg    uint32 = 0x04
	i2cSrReg     uint32 = 0x08
	i2cToReg     uint32 = 0x0C
	i2cSlaveReg  uint32 = 0x10
	i2cFifoConf  uint32 = 0x18
	i2cDataReg   uint32 = 0x1C
	i2cIntRawReg uint32 = 0x20
	i2cIntClrReg uint32 = 0x24
	i2cIntEnaReg uint32 = 0x28
	i2cIntStReg  uint32 = 0x2C
	i2cComd0Reg  uint32 = 0x58 // COMD0..7 连续 8 个，每 4 字节一个

	i2cTransStart uint32 = 1 << 5  // CTR，只写
	i2cTxFifoRst  uint32 = 1 << 13 // FIFO_CONF，自清
	i2cRxFifoRst  uint32 = 1 << 12

	i2cIntTxWm          uint32 = 1 << 1
	i2cIntEndDetect     uint32 = 1 << 3
	i2cIntTransComplete uint32 = 1 << 7
	i2cIntTransStart    uint32 = 1 << 9
	i2cIntNack          uint32 = 1 << 10

	i2cCmdDone     uint32 = 1 << 31
	i2cOpcodeShift        = 11
	i2cFifoDepth          = 16
	i2cSrc         uint32 = 29 // ETS_I2C_EXT0_INTR_SOURCE

	// I2C 命令操作码（C3 TRM / i2c_ll.h）。
	i2cOpRestart = 0
	i2cOpWrite   = 1
	i2cOpRead    = 2
	i2cOpStop    = 3
	i2cOpEnd     = 4
)

// I2C 是 I2C0 主机的命令列表模型：写 TRANS_START 后按 COMD0..7 逐条执行。
// 没有接外部从机，WRITE 的数据被消费掉，READ 按字节数填 0；每条命令在
// 执行后置 DONE，同时置 TRANS_START/TRANS_COMPLETE/END_DETECT 中断位。
// 这样固件无论是轮询 DONE 还是等中断都能继续，不会卡在传输里。
type I2C struct {
	regs map[uint32]uint32
	tx   []byte
	rx   []byte
	raw  uint32
	intc *C3Intc
}

func NewI2C() *I2C {
	return &I2C{regs: make(map[uint32]uint32)}
}

func (i *I2C) Name() string { return "I2C0" }

func (i *I2C) SetIntc(c *C3Intc) { i.intc = c }

func (i *I2C) syncIRQ() {
	if i.intc == nil {
		return
	}
	if i.raw&i.regs[i2cIntEnaReg] != 0 {
		i.intc.Raise(i2cSrc)
	} else {
		i.intc.ClearSource(i2cSrc)
	}
	i.intc.Deliver()
}

func (i *I2C) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case i2cSrReg:
		return uint32(len(i.rx)&0x3F)<<8 | uint32(len(i.tx)&0x3F)<<18
	case i2cFifoConf:
		// FIFO 复位位是自清的，读回恒为 0。
		return i.regs[offset] &^ (i2cTxFifoRst | i2cRxFifoRst)
	case i2cDataReg:
		if len(i.rx) == 0 {
			return 0
		}
		b := i.rx[0]
		i.rx = i.rx[1:]
		return uint32(b)
	case i2cIntRawReg:
		return i.raw
	case i2cIntStReg:
		return i.raw & i.regs[i2cIntEnaReg]
	case i2cIntEnaReg, i2cSlaveReg, i2cToReg:
		return i.regs[offset]
	}
	return i.regs[offset]
}

func (i *I2C) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case i2cCtrReg:
		// TRANS_START 是只写位：写 1 触发命令列表，读回恒为 0。
		i.regs[offset] = val &^ i2cTransStart
		if val&i2cTransStart != 0 {
			i.runCommands()
		}
	case i2cFifoConf:
		i.regs[offset] = val &^ (i2cTxFifoRst | i2cRxFifoRst)
		if val&i2cTxFifoRst != 0 {
			i.tx = nil
		}
		if val&i2cRxFifoRst != 0 {
			i.rx = nil
		}
	case i2cDataReg:
		if len(i.tx) < i2cFifoDepth {
			i.tx = append(i.tx, byte(val))
		}
	case i2cIntClrReg:
		i.raw &^= val
		i.syncIRQ()
	case i2cIntEnaReg:
		i.regs[offset] = val
		i.syncIRQ()
	default:
		i.regs[offset] = val
	}
}

// runCommands 按 COMD0..7 执行一次传输。写新的命令字（DONE=0）会自然
// 清掉上一轮的 DONE，所以下一笔传输不需要额外的清除步骤。
func (i *I2C) runCommands() {
	for n := 0; n < 8; n++ {
		reg := i2cComd0Reg + uint32(4*n)
		cmd := i.regs[reg]
		op := (cmd >> i2cOpcodeShift) & 0x7
		byteNum := int(cmd & 0xFF)
		switch op {
		case i2cOpWrite:
			if byteNum > len(i.tx) {
				byteNum = len(i.tx)
			}
			i.tx = i.tx[byteNum:]
		case i2cOpRead:
			for k := 0; k < byteNum && len(i.rx) < i2cFifoDepth; k++ {
				i.rx = append(i.rx, 0)
			}
		}
		i.regs[reg] |= i2cCmdDone
	}
	i.raw |= i2cIntTransStart | i2cIntTransComplete | i2cIntEndDetect | i2cIntTxWm
	i.syncIRQ()
}

// ---- 寄存器影子 ------------------------------------------------------

// Shadow 是「只保存寄存器值」的外设：写入按字保存，读取原样返回。
// 用于时钟/引脚/总线配置这类只需要读回、没有触发时序的模块。
type Shadow struct {
	name string
	regs map[uint32]uint32
}

func NewShadow(name string) *Shadow {
	return &Shadow{name: name, regs: make(map[uint32]uint32)}
}

func (s *Shadow) Name() string { return s.name }

func (s *Shadow) Read(offset uint32, size uint32) uint32 {
	return s.regs[offset&^0x3]
}

func (s *Shadow) Write(offset uint32, size uint32, val uint32) {
	s.regs[offset&^0x3] = val
}

// ---- SYSTEM（CPU 软件中断）--------------------------------------------

// SYSTEM_CPU_INTR_FROM_CPU_n_REG：写 1 触发「来自 CPU 的软件中断」，
// 中断服务程序返回前写 0 清除。FreeRTOS 的 vPortYield 用它强制一次
// 上下文切换：写 1 后轮询该寄存器，直到 ISR 把它清 0（即中断已被取走）。
//
// C3 上这些寄存器对应 INTMTX 的 source 50/51/52，由 esp_intr_alloc 在
// 启动时通过 intr_matrix_set 选一条 CPU line 并注册 ISR。
const (
	sysCpuIntr0    uint32 = 0x28
	sysCpuIntr1    uint32 = 0x2C
	sysCpuIntr2    uint32 = 0x30
	sysFromCpuSrc0 uint32 = 50
)

// C3Sys 是 SYSTEM 窗口：软件中断触发；其余寄存器按影子处理。
type C3Sys struct {
	regs map[uint32]uint32
	intc *C3Intc
}

func NewC3Sys(intc *C3Intc) *C3Sys {
	return &C3Sys{regs: make(map[uint32]uint32), intc: intc}
}

func (s *C3Sys) Name() string { return "SYSTEM" }

func (s *C3Sys) Read(offset uint32, size uint32) uint32 {
	return s.regs[offset&^0x3]
}

func (s *C3Sys) Write(offset uint32, size uint32, val uint32) {
	off := offset &^ 0x3
	s.regs[off] = val
	// C3 是单核：FROM_CPU_INTR1/2 在本模拟器里只做影子。
	if off != sysCpuIntr0 || s.intc == nil {
		return
	}
	if val&1 != 0 {
		s.intc.Raise(sysFromCpuSrc0)
	} else {
		s.intc.ClearSource(sysFromCpuSrc0)
	}
	s.intc.Deliver()
}

// ---- 挂载 ------------------------------------------------------------

// Extras 汇总新增的 C3 外设实例，供 SoC/GUI 查询状态。
type Extras struct {
	UART1 *UART
	SPI2  *SPI2
	I2C   *I2C
	Sys   *C3Sys
}

// NewExtras 把 UART1、SPI2、I2C0 与一组寄存器影子模块挂到总线上。
// intc 可为 nil（不路由中断）。soc.New 与 benchrun 共用这一份列表。
func NewExtras(bus *mem.SystemBus, intc *C3Intc) *Extras {
	e := &Extras{UART1: NewUART1(), SPI2: NewSPI2(), I2C: NewI2C(), Sys: NewC3Sys(intc)}
	e.UART1.SetIntc(intc)
	e.SPI2.SetIntc(intc)
	e.I2C.SetIntc(intc)

	bus.Attach(mem.DeviceRange{Base: SYSTEMBase, Size: extraWindowSize, Dev: e.Sys})
	bus.Attach(mem.DeviceRange{Base: UART1Base, Size: UARTSize, Dev: e.UART1})
	bus.Attach(mem.DeviceRange{Base: SPI2Base, Size: SPI2Size, Dev: e.SPI2})
	bus.Attach(mem.DeviceRange{Base: I2C0Base, Size: I2CSize, Dev: e.I2C})

	for _, d := range shadowRanges() {
		bus.Attach(d)
	}
	return e
}

// shadowRanges 是尚未做功能建模、但固件会访问的 MMIO 窗口。
// 不含 WiFi/BLE（NRX/BB/FE/FE2），也不重复已建模的外设。
func shadowRanges() []mem.DeviceRange {
	return []mem.DeviceRange{
		{Base: SENSITIVEBase, Size: extraWindowSize, Dev: NewShadow("SENSITIVE")},
		{Base: IOMUXBase, Size: extraWindowSize, Dev: NewShadow("IO_MUX")},
		{Base: SYSCONBase, Size: extraWindowSize, Dev: NewShadow("SYSCON")},
		{Base: TIMG1Base, Size: extraWindowSize, Dev: NewShadow("TIMG1")},
		{Base: LEDCBase, Size: extraWindowSize, Dev: NewShadow("LEDC")},
		{Base: RMTBase, Size: extraWindowSize, Dev: NewShadow("RMT")},
		{Base: TWAIBase, Size: extraWindowSize, Dev: NewShadow("TWAI")},
		{Base: I2S0Base, Size: extraWindowSize, Dev: NewShadow("I2S0")},
		{Base: UHCI0Base, Size: extraWindowSize, Dev: NewShadow("UHCI0")},
		{Base: RTCI2CBase, Size: extraWindowSize, Dev: NewShadow("RTC_I2C")},
		{Base: DedicatedGPIOBase, Size: extraWindowSize, Dev: NewShadow("DEDICATED_GPIO")},
		{Base: WorldCntlBase, Size: extraWindowSize, Dev: NewShadow("WORLD_CNTL")},
		{Base: GDMABase, Size: extraWindowSize, Dev: NewShadow("GDMA")},
		{Base: AESBase, Size: extraWindowSize, Dev: NewShadow("AES")},
		{Base: SHABase, Size: extraWindowSize, Dev: NewShadow("SHA")},
		{Base: RSABase, Size: extraWindowSize, Dev: NewShadow("RSA")},
		{Base: DSBase, Size: extraWindowSize, Dev: NewShadow("DS")},
		{Base: HMACBase, Size: extraWindowSize, Dev: NewShadow("HMAC")},
		{Base: AESXTSBase, Size: extraWindowSize, Dev: NewShadow("AES_XTS")},
		{Base: AssistDebugBase, Size: extraWindowSize, Dev: NewShadow("ASSIST_DEBUG")},
	}
}
