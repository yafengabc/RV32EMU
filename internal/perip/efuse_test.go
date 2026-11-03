package perip

import (
	"testing"

	"rv32emu/internal/mem"
)

// TestEFUSEMACAndRevision 覆盖只读 eFuse：MAC、芯片版本，以及写入被丢弃。
func TestEFUSEMACAndRevision(t *testing.T) {
	bus := mem.NewSystemBus()
	bus.Attach(mem.DeviceRange{Base: EFUSEBase, Size: EFUSESize, Dev: NewEFUSE()})

	if got := bus.Read(EFUSEBase+efuseRdMacSpiSys0, 4); got != efuseMACLow {
		t.Fatalf("MAC low = 0x%08X", got)
	}
	if got := bus.Read(EFUSEBase+efuseRdMacSpiSys1, 4); got != efuseMACHHigh {
		t.Fatalf("MAC high = 0x%08X", got)
	}
	// v0.4：MINOR_LO = 4。
	if got := bus.Read(EFUSEBase+efuseRdMacSpiSys3, 4); got&(0x7<<18) != 4<<18 {
		t.Fatalf("wafer minor = 0x%08X", got)
	}
	// 未定义字段读 0（BLK0 的 DIS_* 全是 0）。
	if got := bus.Read(EFUSEBase+0x30, 4); got != 0 {
		t.Fatalf("BLK0 repeat data = 0x%08X", got)
	}
	// 只读：写入不改变任何字段。
	bus.Write(EFUSEBase+efuseRdMacSpiSys0, 4, 0xDEADBEEF)
	if got := bus.Read(EFUSEBase+efuseRdMacSpiSys0, 4); got != efuseMACLow {
		t.Fatalf("efuse accepted write: 0x%08X", got)
	}
}
