package soc

// SoC 把「一台通电的 C3」装进一个可复用的对象：总线、CPU、外设、flash。
// CLI（根目录 main.go）和 GUI（gui/）都通过这里构造机器，避免两份初始化代码。

import (
	"fmt"

	"rv32emu/internal/cpu"
	"rv32emu/internal/elf"
	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
	"rv32emu/internal/rom"
)

// Flash 模拟区的大小与 ramdisk 的默认偏移：littlefs 分区默认在 flash
// 偏移 0x80000（512 KiB），可通过 -flashoff 覆盖。
const (
	DefaultFlashOffset uint32 = 0x80000
	FlashSize          uint32 = 0x400000
)

// Config 描述一次上电加载什么、用哪些调试开关。
type Config struct {
	ELFPath string
	BinPath string
	Entry   uint32 // 使用 BinPath 时的载入基址
	// FlashImage 为真时不加载 ELF，而是把 flash 里的 esp_image 当作
	// 二级引导的产物：按分区表找应用、建 MMU 映射并拷贝 RAM 段。
	FlashImage bool
	NoPMP      bool
	TickDiv    uint32
	OnTrap     func(cause, epc, tval uint32) // nil 表示不记录 trap
}

// SoC 是一台机器的全部状态。复位 = 用同样的 Config 重建一个 SoC，
// flash 内容经由 FlashSnapshot 带到下一次上电。
type SoC struct {
	Bus      *mem.SystemBus
	CPU      *cpu.CPU
	UART     *perip.UART
	RTC      *perip.RTC
	Timer    *perip.C3Timer
	SysTimer *perip.SysTimer
	GPIO     *perip.GPIO
	ADC      *perip.ADC
}

// NewFlash 构造 flash 镜像并把 ramdisk 内容放到 offset 处。
// offset 越界或放不下 ramdisk 时返回错误。
// 未被镜像覆盖的区域保持擦除态 0xFF（真机 SPI flash 出厂/擦除后读回 0xFF），
// 否则文件系统初始化会把全零区域当成损坏的旧文件系统。
func NewFlash(ramdisk []byte, offset uint32) ([]byte, error) {
	if offset >= FlashSize {
		return nil, fmt.Errorf("flash offset 0x%X out of range (flash size 0x%X)", offset, FlashSize)
	}
	if uint64(offset)+uint64(len(ramdisk)) > uint64(FlashSize) {
		return nil, fmt.Errorf("ramdisk %d bytes at 0x%X overflows flash (size 0x%X)",
			len(ramdisk), offset, FlashSize)
	}
	flash := make([]byte, FlashSize)
	for i := range flash {
		flash[i] = 0xFF
	}
	copy(flash[offset:], ramdisk)
	return flash, nil
}

// New 按 Config 装一台机器。加载失败时返回错误，而不是退出进程，
// 这样 GUI 可以在界面上报告问题。
func New(flash []byte, uart *perip.UART, cfg Config) (*SoC, error) {
	bus := mem.NewSystemBus()
	gpio := perip.NewGPIO()
	usj := perip.NewUSJ()
	rtc := perip.NewRTC()
	adc := perip.NewADC()
	efuse := perip.NewEFUSE()
	bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
	bus.Attach(mem.DeviceRange{Base: perip.GPIOBase, Size: perip.GPIOSize, Dev: gpio})
	bus.Attach(mem.DeviceRange{Base: perip.USJBase, Size: perip.USJSize, Dev: usj})
	bus.Attach(mem.DeviceRange{Base: perip.RTCBase, Size: perip.RTCSize, Dev: rtc})
	bus.Attach(mem.DeviceRange{Base: perip.EFUSEBase, Size: perip.EFUSESize, Dev: efuse})
	bus.Attach(mem.DeviceRange{Base: perip.ADCBase, Size: perip.ADCSize, Dev: adc})

	// flash 模拟区：ROM 桩把 (FlashEmuBase + flash_addr) 当物理 flash。
	bus.AddRAM(perip.FlashEmuBase, FlashSize)
	bus.CopyInto(perip.FlashEmuBase, 0, flash)

	// ROM 桩：实现 esp_rom_spiflash_read/write/config_param。
	bus.LoadROM(0, rom.BuildROM())

	// ROM 数据表：IDF 的 heap_caps_init 会读 ets_rom_layout_p（ROM 数据区
	// 0x3FF1FFFC）取 ROM 保留的 DRAM 区间。模拟器没有真实 ROM 数据，
	// 这里按 C3 ROM 的实际布局表填好，否则堆初始化会把整段 DRAM
	// 误判为保留区并触发 SOC_RESERVE_MEMORY_REGION 重叠断言。
	loadROMData(bus)

	// C3 私有外设：TIMG0 定时器 + SYSTIMER + INTMTX + SPI1（硬件擦除）。
	c3timer := perip.NewC3Timer(nil) // cpu 在下面补设
	c3intc := perip.NewC3Intc()
	systimer := perip.NewSysTimer()
	c3spi1 := perip.NewC3SPI1(bus, perip.FlashEmuBase)
	bus.Attach(mem.DeviceRange{Base: perip.TIMG0Base, Size: perip.TIMG0Size, Dev: c3timer})
	bus.Attach(mem.DeviceRange{Base: perip.SysTimerBase, Size: perip.SysTimerSize, Dev: systimer})
	bus.Attach(mem.DeviceRange{Base: perip.INTMTXBase, Size: perip.INTMTXSize, Dev: c3intc})
	bus.Attach(mem.DeviceRange{Base: perip.SPI1Base, Size: perip.SPISize, Dev: c3spi1})

	// 其余标准外设：UART1、SPI2（GP-SPI）、I2C0 与一组寄存器影子窗口
	// （SYSTEM/SENSITIVE/IO_MUX/SYSCON/TIMG1/LEDC/RMT/TWAI/I2S0/UHCI0/
	//   GDMA/加密加速器等）。WiFi/BLE 不在建模范围内。
	perip.NewExtras(bus, c3intc)

	// flash cache（XIP）：EXTMEM 控制寄存器 + 128 项 MMU + DROM/IROM 窗口。
	// 必须在加载 ELF 之前挂好：IROM/DROM 段会写进窗口的 overlay，
	// 而不是直接改 flash 镜像。
	mmu := perip.NewFlashMMU(bus, perip.FlashEmuBase, FlashSize)
	bus.Attach(mem.DeviceRange{Base: perip.CacheRegBase, Size: perip.CacheRegSize, Dev: perip.NewCacheCtrl(mmu)})
	bus.Attach(mem.DeviceRange{Base: perip.DROMBase, Size: perip.XIPWindowSize, Dev: perip.NewXIPWindow(mmu, perip.DROMBase, "DROM")})
	bus.Attach(mem.DeviceRange{Base: perip.IROMBase, Size: perip.XIPWindowSize, Dev: perip.NewXIPWindow(mmu, perip.IROMBase, "IROM")})

	// SPI0 是 cache/flash 控制器（IDF 用它走 USER 模式读 ID、读数据）。
	c3spi0 := perip.NewC3SPI0(bus, perip.FlashEmuBase)
	bus.Attach(mem.DeviceRange{Base: perip.SPI0Base, Size: perip.SPISize, Dev: c3spi0})

	c := cpu.New(bus)
	if cfg.NoPMP {
		c.SetPMPEnforce(false)
	}
	c.OnTrap = cfg.OnTrap
	c3timer.SetCPU(c)
	c3timer.SetDiv(cfg.TickDiv)
	c3timer.SetIntc(c3intc)
	c3intc.SetCPU(c)
	systimer.SetIntc(c3intc)
	uart.SetIntc(c3intc)

	// ROM 服务：模拟器没有真实 ROM 镜像，把固件引用到的 ROM 函数交给
	// Go 实现（printf / flash / cache / crc / intr_matrix_set 等）。
	c.RomCall = rom.NewService(bus, uart).Call

	switch {
	case cfg.FlashImage:
		entry, err := loadAppImage(flash, bus, mmu)
		if err != nil {
			return nil, fmt.Errorf("load app image: %w", err)
		}
		c.SetPC(entry)
	case cfg.ELFPath != "":
		e, err := elf.Load(cfg.ELFPath, bus)
		if err != nil {
			return nil, fmt.Errorf("load elf: %w", err)
		}
		c.SetPC(e)
	case cfg.BinPath != "":
		if err := elf.LoadBin(cfg.BinPath, cfg.Entry, bus); err != nil {
			return nil, fmt.Errorf("load bin: %w", err)
		}
		c.SetPC(cfg.Entry)
	default:
		return nil, fmt.Errorf("need ELF or bin")
	}

	// ESP-IDF 应用 ELF 的入口（call_start_cpu0）假定二级引导已经准备好
	// 一根栈；直接加载应用镜像时没有引导程序，这里补上初始 SP。
	// 0x3FCDE000 位于 DRAM 顶端空闲区，向下生长不会碰到应用静态数据
	// （mpy 的 .bss/_heap_start 结束于 0x3FCA12C0）。
	c.SetReg(2, 0x3FCDE000)

	return &SoC{
		Bus: bus, CPU: c, UART: uart, RTC: rtc,
		Timer: c3timer, SysTimer: systimer, GPIO: gpio, ADC: adc,
	}, nil
}

// FlashSnapshot 取回 flash 模拟区的当前内容，供复位后灌进新总线。
func (s *SoC) FlashSnapshot() []byte {
	buf := make([]byte, FlashSize)
	s.Bus.CopyOut(perip.FlashEmuBase, buf)
	return buf
}

// ROM 布局表在 C3 ROM 数据区里的位置（取自 esp32c3_rev3_rom.elf 的符号）：
// ets_rom_layout_p 是指向 ets_rom_layout_t 的常量指针。
// C3 的 ROM 数据区是 0x3FF00000~0x3FF1FFFF，真机上只读；模拟器把它当
// 一段 RAM 挂到总线上，供 loadROMData 填入下面这张表。
const (
	romDataBase     uint32 = 0x3FF00000
	romDataSize     uint32 = 128 * 1024
	romLayoutPtrAddr uint32 = 0x3FF1FFFC
	romLayoutAddr    uint32 = 0x3FF1BE3C
)

// romLayout 是 ets_rom_layout_t 的实际内容（C3 rev3 ROM 的 .rodata）。
// 启动阶段关键的是 dram0_rtos_reserved_start = 0x3FCDF060：ROM 只保留
// 0x3FCDF060~0x3FCE0000，剩下的 DRAM 才能被 heap_caps_init 用作堆。
var romLayout = [40]uint32{
	0x3FCDC704, 0x3FCDF060, 0x3FCDC710, 0x3FCDE710, 0x00000000, 0x00000000,
	0x3FCDF09C, 0x3FCDF0A8, 0x3FCDF0A8, 0x3FCDF0D0, 0x400591FC, 0x40059200,
	0x3FCDF96C, 0x3FCDFA28, 0x3FCDFA28, 0x3FCDFFD0, 0x3FCDF300, 0x3FCDF5BC,
	0x3FCDF0D0, 0x3FCDF0D4, 0x3FCDF0D4, 0x3FCDF0D8, 0x3FCDF0D8, 0x3FCDF300,
	0x3FCDF834, 0x3FCDF834, 0x3FCDF834, 0x3FCDF84C, 0x3FCDF84C, 0x3FCDF84C,
	0x3FCDF84C, 0x3FCDF870, 0x3FCDF870, 0x3FCDF870, 0x3FCDF870, 0x3FCDF96C,
	0x3FCDF060, 0x3FCDF060, 0x3FCDF060, 0x3FCDF09C,
}

// loadROMData 把 ROM 数据区里的布局表写进模拟内存。ROM 代码/常量本身
// 仍由 internal/rom 的 Go 服务提供，这里只补 app 会直接读的那部分数据。
func loadROMData(bus *mem.SystemBus) {
	bus.AddRAM(romDataBase, romDataSize)
	for i, v := range romLayout {
		bus.Write(romLayoutAddr+uint32(i)*4, 4, v)
	}
	bus.Write(romLayoutPtrAddr, 4, romLayoutAddr)
}
