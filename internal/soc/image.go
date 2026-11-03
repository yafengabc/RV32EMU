package soc

// 按 ESP-IDF 分区表从 flash 镜像加载应用（等价于二级引导 bootloader 的工作）。
//
// 直接加载 ELF 时，DROM/IROM 段由 elf 包写进 cache 窗口的 overlay，应用
// 自己的 flash 区域是空的；而真实固件启动早期会通过 XIP 窗口读自己的
// 镜像头（cpu_start 里的 0xE9 magic 校验、esp_app_desc 等）。模拟器没有
// 真实 ROM 引导程序，所以这里按分区表找到 factory 应用，像 bootloader
// 一样：XIP 段只建立 MMU 映射（读 flash），RAM 段拷贝到目标地址。

import (
	"encoding/binary"
	"fmt"

	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
)

const (
	partTableBase  uint32 = 0x8000
	partTableMax   uint32 = 0x1000
	partEntrySize  uint32 = 32
	partTypeApp    uint8  = 0
	partSubFactory uint8  = 0
	imageHeaderSize uint32 = 24
)

// partMagic 是分区表项的魔数（两个字节按小端写作 0xAA50，即 0xAA、0x50）。
func partMagic(b []byte, off uint32) bool {
	return b[off] == 0xAA && b[off+1] == 0x50
}

var errNoAppPartition = fmt.Errorf("flash 里没有找到应用分区（分区表应在 0x%X）", partTableBase)

// findAppPartition 在分区表里定位应用分区：优先 factory，其次任意 app。
func findAppPartition(flash []byte) (uint32, error) {
	for off := partTableBase; off+partEntrySize <= partTableBase+partTableMax; off += partEntrySize {
		if off+partEntrySize > uint32(len(flash)) {
			break
		}
		if !partMagic(flash, off) {
			continue
		}
		typ, sub := flash[off+2], flash[off+3]
		addr := binary.LittleEndian.Uint32(flash[off+4:])
		if typ != partTypeApp {
			continue
		}
		if sub == partSubFactory {
			return addr, nil
		}
	}
	// 没有 factory 时退而求其次：任意 app 分区。
	for off := partTableBase; off+partEntrySize <= partTableBase+partTableMax; off += partEntrySize {
		if off+partEntrySize > uint32(len(flash)) {
			break
		}
		if partMagic(flash, off) && flash[off+2] == partTypeApp {
			return binary.LittleEndian.Uint32(flash[off+4:]), nil
		}
	}
	return 0, errNoAppPartition
}

// isXIP 判断段地址是否落在 DROM/IROM 的 cache 窗口里。
func isXIP(addr uint32) bool {
	return (addr >= perip.DROMBase && addr < perip.DROMBase+perip.XIPWindowSize) ||
		(addr >= perip.IROMBase && addr < perip.IROMBase+perip.XIPWindowSize)
}

// ensureRAM 为 RAM 段补上可写内存：SRAM 已内建，其余地址按需分配。
func ensureRAM(bus *mem.SystemBus, addr, size uint32) {
	if addr >= mem.SRAMBase && addr < mem.SRAMBase+mem.SRAMSize {
		return
	}
	if bus.RamSizeAt(addr) > 0 || bus.HasDeviceAt(addr) {
		return
	}
	if size < 1<<20 {
		size = 1 << 20
	}
	bus.AddRAM(addr, size)
}

// loadAppImage 解析 flash 里的 esp_image，返回入口地址。
func loadAppImage(flash []byte, bus *mem.SystemBus, mmu *perip.FlashMMU) (uint32, error) {
	appOff, err := findAppPartition(flash)
	if err != nil {
		return 0, err
	}
	if appOff+imageHeaderSize > uint32(len(flash)) {
		return 0, fmt.Errorf("app partition @0x%X 超出 flash 大小", appOff)
	}
	if flash[appOff] != 0xE9 {
		return 0, fmt.Errorf("app partition @0x%X 没有 ESP 镜像头（magic=0x%02X）", appOff, flash[appOff])
	}
	segCount := int(flash[appOff+1])
	entry := binary.LittleEndian.Uint32(flash[appOff+4:])
	fmt.Printf("[image] app @0x%08X segments=%d entry=0x%08X\n", appOff, segCount, entry)

	p := appOff + imageHeaderSize
	for i := 0; i < segCount; i++ {
		if p+8 > uint32(len(flash)) {
			return 0, fmt.Errorf("app image 第 %d 段头被截断", i)
		}
		loadAddr := binary.LittleEndian.Uint32(flash[p:])
		length := binary.LittleEndian.Uint32(flash[p+4:])
		dataOff := p + 8
		if dataOff+length > uint32(len(flash)) {
			return 0, fmt.Errorf("app image 第 %d 段数据被截断", i)
		}
		fmt.Printf("[image] seg%d load=0x%08X len=0x%X flash=0x%08X\n", i, loadAddr, length, dataOff)
		if isXIP(loadAddr) {
			mmu.Map(loadAddr, dataOff, length)
		} else {
			ensureRAM(bus, loadAddr, length)
			for k := uint32(0); k < length; k++ {
				bus.Write(loadAddr+k, 1, uint32(flash[dataOff+k]))
			}
		}
		p = dataOff + length
	}
	return entry, nil
}
