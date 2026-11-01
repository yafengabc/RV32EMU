package mem

import (
	"encoding/binary"
	"fmt"
)

// ESP32-C3 地址映射（摘自技术参考手册 Memory Map 章节）。
const (
	ROMBase    uint32 = 0x40000000
	ROMSize    uint32 = 512 * 1024 // 0x40000000~0x4007FFFF
	SRAMBase   uint32 = 0x3FC80000
	SRAMSize   uint32 = 512 * 1024 // 0x3FC80000~0x3FCFFFFF
)

// SystemBus 实现 Bus：ROM + SRAM + 挂载的 MMIO 外设 + 动态 RAM 区。
type SystemBus struct {
	rom   []byte
	sram  []byte
	devs  []DeviceRange
	rams  []ramRegion
	// 未映射访问按地址去重上报：只报一次全局会把后面真正的外设缺口
	// 全部吞掉（曾因此漏掉 RTC_CNTL），逐个地址报又会被循环刷屏。
	unmappedR map[uint32]bool
	unmappedW map[uint32]bool
}

// unmappedWarnLimit 每个方向最多报这么多个不同地址。
const unmappedWarnLimit = 12

type ramRegion struct {
	base uint32
	data []byte
}

func NewSystemBus() *SystemBus {
	return &SystemBus{
		rom:       make([]byte, ROMSize),
		sram:      make([]byte, SRAMSize),
		unmappedR: make(map[uint32]bool),
		unmappedW: make(map[uint32]bool),
	}
}

// AddRAM 动态挂一段 RAM（供 ELF 加载器为未映射段自动分配）。
func (b *SystemBus) AddRAM(base uint32, size uint32) {
	b.rams = append(b.rams, ramRegion{base: base, data: make([]byte, size)})
}

// ExtendRAM 扩展已有的动态 RAM 区到至少 need 字节。
func (b *SystemBus) ExtendRAM(base uint32, need uint32) {
	for i := range b.rams {
		if b.rams[i].base == base {
			if uint32(len(b.rams[i].data)) < need {
				newData := make([]byte, need)
				copy(newData, b.rams[i].data)
				b.rams[i].data = newData
			}
			return
		}
	}
	// 没找到就新加
	b.AddRAM(base, need)
}

// MapSize 返回某地址所在动态 RAM 区的大小，未映射返回 0。
func (b *SystemBus) RamSizeAt(addr uint32) uint32 {
	for _, r := range b.rams {
		if addr >= r.base && addr < r.base+uint32(len(r.data)) {
			return uint32(len(r.data))
		}
	}
	return 0
}

// Attach 挂载一个外设到总线上。
func (b *SystemBus) Attach(r DeviceRange) {
	b.devs = append(b.devs, r)
}

func (b *SystemBus) readRaw(addr uint32) uint8 {
	switch {
	case in(addr, ROMBase, ROMSize):
		return b.rom[addr-ROMBase]
	case in(addr, SRAMBase, SRAMSize):
		return b.sram[addr-SRAMBase]
	default:
		for _, r := range b.rams {
			if in(addr, r.base, uint32(len(r.data))) {
				return r.data[addr-r.base]
			}
		}
		for _, d := range b.devs {
			if in(addr, d.Base, d.Size) {
				return uint8(d.Dev.Read(addr-d.Base, 1))
			}
		}
	}
	// 未映射地址读为 0
	if !b.unmappedR[addr] && len(b.unmappedR) < unmappedWarnLimit {
		b.unmappedR[addr] = true
		fmt.Printf("[bus] unmapped read @0x%08X\n", addr)
	}
	return 0
}

func (b *SystemBus) writeRaw(addr uint32, v uint8) {
	switch {
	case in(addr, ROMBase, ROMSize):
		b.rom[addr-ROMBase] = v // ROM 也允许写（便于把固件段拷进去）
	case in(addr, SRAMBase, SRAMSize):
		b.sram[addr-SRAMBase] = v
	default:
		for _, r := range b.rams {
			if in(addr, r.base, uint32(len(r.data))) {
				r.data[addr-r.base] = v
				return
			}
		}
		for _, d := range b.devs {
			if in(addr, d.Base, d.Size) {
				d.Dev.Write(addr-d.Base, 1, uint32(v))
				return
			}
		}
		if !b.unmappedW[addr] && len(b.unmappedW) < unmappedWarnLimit {
			b.unmappedW[addr] = true
			fmt.Printf("[bus] unmapped write @0x%08X = 0x%02X\n", addr, v)
		}
	}
}

// devAt 返回 addr 所在外设，未命中返回 nil。
// 外设必须整体访问（一次 Read/Write 带 size），不能按字节拆：
// 拆开会让 sw 变成 4 次字节写，寄存器语义全错（TIMG0 就踩过这个坑），
// 4 字节读 UART FIFO 还会一次吃掉 4 个字符。
func (b *SystemBus) devAt(addr uint32) *DeviceRange {
	for i := range b.devs {
		if in(addr, b.devs[i].Base, b.devs[i].Size) {
			return &b.devs[i]
		}
	}
	return nil
}

// Read 按 size(1/2/4) 小端读。
func (b *SystemBus) Read(addr uint32, size uint32) uint32 {
	if d := b.devAt(addr); d != nil {
		return d.Dev.Read(addr-d.Base, size)
	}
	switch size {
	case 1:
		return uint32(b.readRaw(addr))
	case 2:
		lo := b.readRaw(addr)
		hi := b.readRaw(addr + 1)
		return uint32(lo) | uint32(hi)<<8
	case 4:
		var buf [4]byte
		for i := 0; i < 4; i++ {
			buf[i] = b.readRaw(addr + uint32(i))
		}
		return binary.LittleEndian.Uint32(buf[:])
	}
	panic("bad read size")
}

// Write 按 size(1/2/4) 小端写。
func (b *SystemBus) Write(addr uint32, size uint32, val uint32) {
	if d := b.devAt(addr); d != nil {
		d.Dev.Write(addr-d.Base, size, val)
		return
	}
	switch size {
	case 1:
		b.writeRaw(addr, uint8(val))
	case 2:
		b.writeRaw(addr, uint8(val))
		b.writeRaw(addr+1, uint8(val>>8))
	case 4:
		for i := 0; i < 4; i++ {
			b.writeRaw(addr+uint32(i), uint8(val>>(8*i)))
		}
	default:
		panic("bad write size")
	}
}

// LoadROM 直接把镜像写入 ROM 区（供 ELF/测试用）。
func (b *SystemBus) LoadROM(off uint32, data []byte) {
	copy(b.rom[off:], data)
}

// CopyInto 把 data 拷入基址为 base 的已挂载动态 RAM 区，从区内的 off 偏移开始。
// 用于把 ramdisk 镜像放进 flash 模拟区（如 base=FlashEmuBase, off=0x100000）。
func (b *SystemBus) CopyInto(base, off uint32, data []byte) {
	for _, r := range b.rams {
		if r.base == base {
			copy(r.data[off:], data)
			return
		}
	}
}

// CopyOut 把 [base, base+len(dst)) 的内容拷出到 dst（调试用）。
func (b *SystemBus) CopyOut(base uint32, dst []byte) {
	for _, r := range b.rams {
		if base >= r.base && base+uint32(len(dst)) <= r.base+uint32(len(r.data)) {
			copy(dst, r.data[base-r.base:])
			return
		}
	}
	for i := range dst {
		dst[i] = b.readRaw(base + uint32(i))
	}
}

// 在 RAM/ROM 区间内返回切片（快速路径），否则返回 nil。
func (b *SystemBus) fastSlice(addr uint32) []byte {
	switch {
	case in(addr, ROMBase, ROMSize):
		return b.rom[addr-ROMBase:]
	case in(addr, SRAMBase, SRAMSize):
		return b.sram[addr-SRAMBase:]
	}
	for _, r := range b.rams {
		if in(addr, r.base, uint32(len(r.data))) {
			return r.data[addr-r.base:]
		}
	}
	return nil
}

// FastRead 热路径读：ROM/SRAM 直接索引，外设回落到通用 Read。
func (b *SystemBus) FastRead(addr uint32, size uint32) uint32 {
	s := b.fastSlice(addr)
	if s == nil {
		return b.Read(addr, size)
	}
	switch size {
	case 1:
		return uint32(s[0])
	case 2:
		return uint32(s[0]) | uint32(s[1])<<8
	case 4:
		return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
	}
	panic("bad size")
}

// FastWrite 热路径写。
func (b *SystemBus) FastWrite(addr uint32, size uint32, val uint32) {
	s := b.fastSlice(addr)
	if s == nil {
		b.Write(addr, size, val)
		return
	}
	switch size {
	case 1:
		s[0] = uint8(val)
	case 2:
		s[0] = uint8(val)
		s[1] = uint8(val >> 8)
	case 4:
		s[0] = uint8(val)
		s[1] = uint8(val >> 8)
		s[2] = uint8(val >> 16)
		s[3] = uint8(val >> 24)
	}
}

func in(addr, base, size uint32) bool {
	return addr >= base && addr < base+size
}
