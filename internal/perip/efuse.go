package perip

// ESP32-C3 eFuse：只读模型。
//
// IDF / MicroPython 启动早期会读 eFuse 拿 MAC（BLK1 RD_MAC_SPI_SYS_0/1）、
// 芯片版本（BLK1 RD_MAC_SPI_SYS_3/5）以及各种 DIS_* 选项位（BLK0）。
// 这里给出一份固定值：Espressif OUI 的 MAC + v0.4 晶圆版本 + 全部选项位为 0。
// 编程寄存器（WRITE_OP 等）写入直接丢弃——烧写 eFuse 不在模拟范围内。

const (
	EFUSEBase uint32 = 0x60008800
	EFUSESize uint32 = 0x200

	efuseRdMacSpiSys0 uint32 = 0x44 // MAC[31:0]
	efuseRdMacSpiSys1 uint32 = 0x48 // MAC[47:32]
	efuseRdMacSpiSys3 uint32 = 0x50 // WAFER_VERSION_MINOR_LO[20:18] / PKG_VERSION[23:21]
	efuseRdMacSpiSys5 uint32 = 0x58 // WAFER_VERSION_MINOR_HI[23] / WAFER_VERSION_MAJOR[25:24]

	// 固定 MAC：AC:67:B2:12:34:56。
	efuseMACLow   uint32 = 0xAC67B212
	efuseMACHHigh uint32 = 0x00003456

	// 芯片版本 v0.4：MINOR_LO = 4（v0.3/v0.4 是 IDF 5.1 支持的版本）。
	efuseWaferMinorLo uint32 = 4 << 18
)

type EFUSE struct {
	regs map[uint32]uint32
}

func NewEFUSE() *EFUSE {
	e := &EFUSE{regs: make(map[uint32]uint32)}
	e.regs[efuseRdMacSpiSys0] = efuseMACLow
	e.regs[efuseRdMacSpiSys1] = efuseMACHHigh
	e.regs[efuseRdMacSpiSys3] = efuseWaferMinorLo
	return e
}

func (e *EFUSE) Name() string { return "EFUSE" }

func (e *EFUSE) Read(offset uint32, size uint32) uint32 {
	return e.regs[offset]
}

// Write 丢弃：eFuse 只读，编程寄存器在模拟器里没有实现。
func (e *EFUSE) Write(offset uint32, size uint32, val uint32) {}
