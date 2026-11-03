package elf

import (
	"debug/elf"
	"fmt"
	"io"
	"os"

	"rv32emu/internal/mem"
)

// Load 打开 ELF，把所有 PT_LOAD 段按 p_paddr 写入总线，返回入口地址。
// 对未映射的段地址自动分配 RAM。
func Load(path string, bus *mem.SystemBus) (uint32, error) {
	f, err := elf.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if f.Machine != elf.EM_RISCV {
		return 0, fmt.Errorf("not RISC-V ELF (machine=%d)", f.Machine)
	}
	if f.Class != elf.ELFCLASS32 {
		return 0, fmt.Errorf("not RV32 (class=%v)", f.Class)
	}

	// 第一遍：收集所有 PT_LOAD 段，为未映射区域一次性分配 RAM
	type segInfo struct {
		base uint32
		end  uint32
	}
	var segs []segInfo
	for _, seg := range f.Progs {
		if seg.Type != elf.PT_LOAD {
			continue
		}
		base := uint32(seg.Paddr)
		end := base + uint32(seg.Memsz)
		segs = append(segs, segInfo{base, end})
	}

	// 为未映射地址分配 RAM：按 base 分组，覆盖连续范围
	for _, s := range segs {
		if inBuiltin(s.base) {
			continue
		}
		if bus.RamSizeAt(s.base) > 0 {
			continue // 已有 RAM
		}
		if bus.HasDeviceAt(s.base) {
			continue // 已挂外设（如 IROM/DROM 的 XIP 窗口）
		}
		// 找连续段范围
		lo, hi := s.base, s.end
		for _, s2 := range segs {
			if s2.base >= lo && s2.base <= hi && !inBuiltin(s2.base) && !bus.HasDeviceAt(s2.base) {
				if s2.end > hi {
					hi = s2.end
				}
			}
		}
		size := (hi - lo + 0xFFF) &^ 0xFFF
		// 至少分配 1MB，留足栈/堆空间
		if size < 1024*1024 {
			size = 1024 * 1024
		}
		bus.AddRAM(lo, size)
	}

	// 第二遍：加载段数据
	for _, seg := range f.Progs {
		if seg.Type != elf.PT_LOAD {
			continue
		}
		r := seg.Open()
		buf, err := io.ReadAll(r)
		if err != nil {
			return 0, fmt.Errorf("read segment @0x%x: %w", seg.Paddr, err)
		}
		loadSegQuiet(bus, uint32(seg.Paddr), buf)
		fmt.Printf("[elf] load vaddr=0x%08X paddr=0x%08X filesz=0x%x memsz=0x%x\n",
			seg.Vaddr, seg.Paddr, seg.Filesz, seg.Memsz)
	}

	entry := uint32(f.Entry)
	fmt.Printf("[elf] entry=0x%08X\n", entry)
	return entry, nil
}

func inBuiltin(addr uint32) bool {
	return (addr >= 0x40000000 && addr < 0x40080000) || // ROM
		(addr >= 0x3FC80000 && addr < 0x3FC80000+0x80000) // SRAM
}

// loadSegQuiet 拷数据但不打印 unmapped 告警（加载阶段自动分配 RAM 后不会触发）。
func loadSegQuiet(bus *mem.SystemBus, addr uint32, data []byte) {
	for i, b := range data {
		bus.Write(addr+uint32(i), 1, uint32(b))
	}
}

// loadSeg 把字节流拷到总线目标地址（逐字节写，对齐外设/内存译码）。
func loadSeg(bus mem.Bus, addr uint32, data []byte) {
	for i, b := range data {
		bus.Write(addr+uint32(i), 1, uint32(b))
	}
}

// LoadBin 加载纯 .bin 镜像到指定基址。
func LoadBin(path string, base uint32, bus *mem.SystemBus) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	loadSeg(bus, base, b)
	fmt.Printf("[bin] load base=0x%08X size=0x%x\n", base, len(b))
	return nil
}
