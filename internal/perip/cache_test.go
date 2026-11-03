package perip

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// newFlashBus 建一个只挂 flash 模拟区的总线，供 cache/SPI 测试用。
func newFlashBus() *mem.SystemBus {
	b := mem.NewSystemBus()
	b.AddRAM(FlashEmuBase, 0x400000)
	return b
}

// TestFlashMMULinearMap 覆盖复位默认的线性映射和越界页（无效项）。
func TestFlashMMULinearMap(t *testing.T) {
	b := newFlashBus()
	b.Write(FlashEmuBase, 4, 0x00500513) // addi a0, zero, 5
	mmu := NewFlashMMU(b, FlashEmuBase, 0x400000)
	w := NewXIPWindow(mmu, IROMBase, "IROM")

	if got := w.Read(0, 4); got != 0x00500513 {
		t.Fatalf("IROM[0] = 0x%08X, want flash word", got)
	}
	// 第 64 页（flash 偏移 4 MiB）已超出 flash：按未映射读 0xFF。
	if got := w.Read(64*cachePageSize, 1); got != 0xFF {
		t.Fatalf("unmapped page read = 0x%X, want 0xFF", got)
	}
}

// TestFlashMMUOverlay 覆盖 ELF 段写入窗口时落进 overlay、不改 flash 本体，
// 以及通过 MMU 表把虚拟页重映射到别的 flash 页。
func TestFlashMMUOverlay(t *testing.T) {
	b := newFlashBus()
	b.Write(FlashEmuBase, 4, 0x00500513)
	mmu := NewFlashMMU(b, FlashEmuBase, 0x400000)
	ctrl := NewCacheCtrl(mmu)
	w := NewXIPWindow(mmu, IROMBase, "IROM")

	w.Write(0, 4, 0x00A00513)
	if got := w.Read(0, 4); got != 0x00A00513 {
		t.Fatalf("overlay read = 0x%08X", got)
	}
	if got := b.Read(FlashEmuBase, 4); got != 0x00500513 {
		t.Fatalf("flash was modified by window write: 0x%08X", got)
	}

	// MMU 表项：窗口页 1 → flash 页 0。
	ctrl.Write(MMUTableBase-CacheRegBase+4, 4, 0)
	if got := w.Read(cachePageSize, 4); got != 0x00500513 {
		t.Fatalf("remapped page = 0x%08X", got)
	}
	// 置无效位后读到 0xFF。
	ctrl.Write(MMUTableBase-CacheRegBase+4, 4, mmuInvalidBit)
	if got := w.Read(cachePageSize, 4); got != 0xFFFFFFFF {
		t.Fatalf("invalid entry read = 0x%08X", got)
	}
}

// TestXIPInstructionFetch 确认 CPU 能从 IROM 窗口取指执行。
func TestXIPInstructionFetch(t *testing.T) {
	b := newFlashBus()
	b.Write(FlashEmuBase, 4, 0x00500513) // addi a0, zero, 5
	mmu := NewFlashMMU(b, FlashEmuBase, 0x400000)
	b.Attach(mem.DeviceRange{Base: IROMBase, Size: XIPWindowSize, Dev: NewXIPWindow(mmu, IROMBase, "IROM")})

	c := cpu.New(b)
	c.SetPC(IROMBase)
	if !c.Step() {
		t.Fatal("cpu halted on XIP instruction")
	}
	if c.PC() != IROMBase+4 {
		t.Fatalf("pc = 0x%08X, want 0x%08X", c.PC(), IROMBase+4)
	}
	if got := c.Reg(10); got != 5 {
		t.Fatalf("a0 = %d, want 5", got)
	}
}

// TestSPIUserMode 覆盖 SPI0 USER 模式的 RDID、READ、PP。
func TestSPIUserMode(t *testing.T) {
	b := newFlashBus()
	b.Write(FlashEmuBase+0x10000, 4, 0x44332211)
	s := NewC3SPI0(b, FlashEmuBase)

	fire := func(cmd uint32, addr uint32, mosiBits, misoBits uint32) {
		s.Write(spiUser2Reg, 4, cmd)
		s.Write(spiUserReg, 4, 1<<23|1<<22) // usr_command | usr_addr
		s.Write(spiMosiDlenReg, 4, mosiBits)
		s.Write(spiMisoDlenReg, 4, misoBits)
		s.Write(spiAddrReg, 4, addr)
		s.Write(spiCmdReg, 4, spiCmdUsr)
		if got := s.Read(spiCmdReg, 4); got != 0 {
			t.Fatalf("cmd bits did not self-clear: 0x%08X", got)
		}
	}

	// RDID (0x9F) 3 字节 → W0 = C8 40 16。
	fire(0x9F, 0, 0, 3*8-1)
	if got := s.Read(spiW0Reg, 4); got != 0x001640C8 {
		t.Fatalf("RDID = 0x%08X", got)
	}

	// READ (0x03) 从 flash 偏移 0x10000 读 4 字节。
	fire(0x03, 0x10000, 0, 4*8-1)
	if got := s.Read(spiW0Reg, 4); got != 0x44332211 {
		t.Fatalf("READ = 0x%08X", got)
	}

	// PP (0x02) 把 W0 的 4 字节写进 flash 偏移 0x20000。
	s.Write(spiW0Reg, 4, 0xAABBCCDD)
	fire(0x02, 0x20000, 4*8-1, 0)
	if got := b.Read(FlashEmuBase+0x20000, 4); got != 0xAABBCCDD {
		t.Fatalf("PP wrote 0x%08X", got)
	}
}

// TestSPIHardwareCommands 覆盖 SPI1 的硬件命令位：ID 读、扇区擦除。
func TestSPIHardwareCommands(t *testing.T) {
	b := newFlashBus()
	b.Write(FlashEmuBase+0x20000, 4, 0xAABBCCDD)
	s := NewC3SPI1(b, FlashEmuBase)

	s.Write(spiCmdReg, 4, spiCmdFlashID)
	if got := s.Read(spiRdStatusReg, 4); got != c3FlashJEDECID {
		t.Fatalf("JEDEC ID = 0x%08X", got)
	}

	s.Write(spiAddrReg, 4, 0x20000)
	s.Write(spiCmdReg, 4, spiCmdFlashSE)
	if got := b.Read(FlashEmuBase+0x20000, 1); got != 0xFF {
		t.Fatalf("sector not erased: 0x%02X", got)
	}
	if got := s.Read(spiCmdReg, 4); got != 0 {
		t.Fatalf("SE cmd bit did not clear: 0x%08X", got)
	}
}
