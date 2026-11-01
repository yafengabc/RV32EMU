package mem

import (
	"encoding/binary"
	"fmt"
)

// ESP32-C3 地址映射（摘自技术参考手册 Memory Map 章节）。
const (
	ROMBase  uint32 = 0x40000000
	ROMSize  uint32 = 512 * 1024 // 0x40000000~0x4007FFFF
	SRAMBase uint32 = 0x3FC80000
	SRAMSize uint32 = 512 * 1024 // 0x3FC80000~0x3FCFFFFF

	// FlatBase/FlatSize 是「一次性边界检查」用的连续平铺区：
	// 把 flash 模拟区、SRAM、ROM、IRAM/内核 RAM 装进同一块切片，
	// 地址 → 索引只是一次减法。CPU 取指和访存因此省掉
	// fastSlice 的区间串比较和函数调用间接跳转。
	//
	// 覆盖范围 0x3F000000~0x40FFFFFF（32MiB，零填充页由 OS 惰性分配），
	// 含 flash 模拟区(4MiB)、SRAM(512KiB)、ROM(512KiB)、
	// IRAM/内核 RAM(0x4037C000 起) 以及 ELF 加载器 1MiB 最小分配余量。
	// 所有 MMIO 外设都在 0x60000000 以上，不会落在平铺区里。
	FlatBase uint32 = 0x3F000000
	FlatSize uint32 = 32 * 1024 * 1024
)

// SystemBus 实现 Bus：ROM + SRAM + 挂载的 MMIO 外设 + 动态 RAM 区。
type SystemBus struct {
	// flat 是上述平铺区；rom/sram 以及落在区内的动态 RAM 都是它的视图，
	// 保证「直访切片」和「走总线」读到的是同一份字节。
	flat []byte
	rom  []byte
	sram []byte
	devs []DeviceRange
	rams []ramRegion

	// codePage/codeGen 是平铺区按 4KiB 页的代码版本表，供 CPU 的解码缓存
	// 判断「这一页的指令字节有没有被改写」（自修改代码、SPI1 擦 flash）。
	// codePage[p] 为真时，对页 p 的写才需要把 codeGen[p]+1。
	codePage []bool
	codeGen  []uint32
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
	b := &SystemBus{
		flat:      make([]byte, FlatSize),
		codePage:  make([]bool, FlatSize>>12),
		codeGen:   make([]uint32, FlatSize>>12),
		unmappedR: make(map[uint32]bool),
		unmappedW: make(map[uint32]bool),
	}
	b.rom = b.flat[ROMBase-FlatBase : ROMBase-FlatBase+ROMSize]
	b.sram = b.flat[SRAMBase-FlatBase : SRAMBase-FlatBase+SRAMSize]
	return b
}

// FlatSlice 返回平铺区，供 CPU 做「地址减基址」式直访。
func (b *SystemBus) FlatSlice() []byte { return b.flat }

// CodePages 返回代码页标记与页版本号，供 CPU 的解码缓存校验。
// 两个切片与总线共享底层数组，CPU 直写平铺区时也按同一规则记账。
func (b *SystemBus) CodePages() ([]bool, []uint32) { return b.codePage, b.codeGen }

// touchCode 记录一次对平铺区的写：该页曾被当作代码页时，把页版本号 +1，
// 让解码缓存里这一页的块失效。数据页不受影响（codePage 为假时零开销）。
func (b *SystemBus) touchCode(addr uint32) {
	if off := addr - FlatBase; off < FlatSize {
		if p := off >> 12; b.codePage[p] {
			b.codeGen[p]++
		}
	}
}

// touchCodeRange 使 [base, base+n) 覆盖的所有代码页失效（装载镜像用）。
func (b *SystemBus) touchCodeRange(base, n uint32) {
	if n == 0 {
		return
	}
	lo := base - FlatBase
	hi := lo + n - 1
	if lo >= FlatSize {
		return
	}
	for p := lo >> 12; p <= hi>>12 && p < uint32(len(b.codeGen)); p++ {
		if b.codePage[p] {
			b.codeGen[p]++
		}
	}
}

// AddRAM 动态挂一段 RAM（供 ELF 加载器为未映射段自动分配）。
// 落在平铺区内的段直接切平铺区的视图，避免出现「总线写一份、
// CPU 直访读到另一份」的分裂。
func (b *SystemBus) AddRAM(base uint32, size uint32) {
	if off := base - FlatBase; off < FlatSize && size <= FlatSize-off {
		b.rams = append(b.rams, ramRegion{base: base, data: b.flat[off : off+size]})
		return
	}
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
	b.touchCode(addr)
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
	b.touchCodeRange(ROMBase+off, uint32(len(data)))
}

// CopyInto 把 data 拷入基址为 base 的已挂载动态 RAM 区，从区内的 off 偏移开始。
// 用于把 ramdisk 镜像放进 flash 模拟区（如 base=FlashEmuBase, off=0x100000）。
func (b *SystemBus) CopyInto(base, off uint32, data []byte) {
	for _, r := range b.rams {
		if r.base == base {
			copy(r.data[off:], data)
			b.touchCodeRange(base+off, uint32(len(data)))
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

// ramAt 返回 addr 所在动态 RAM 区及其区内偏移，未命中 ok=false。
// 只查平铺区之外的区：区内 RAM 已由平铺区覆盖，不会走到这里。
func (b *SystemBus) ramAt(addr uint32, size uint32) (*ramRegion, uint32, bool) {
	for i := range b.rams {
		r := &b.rams[i]
		if off := addr - r.base; off < uint32(len(r.data)) && size <= uint32(len(r.data))-off {
			return r, off, true
		}
	}
	return nil, 0, false
}

// FastRead 热路径读：平铺区内一次边界检查直接索引；
// 区外的动态 RAM（如 ELF 加载器为 0x80000000 这类非 C3 地址分配的段）
// 仍按区直接索引；其余（MMIO 外设、未映射）回落通用 Read。
func (b *SystemBus) FastRead(addr uint32, size uint32) uint32 {
	if off := addr - FlatBase; off < FlatSize-size+1 {
		switch size {
		case 1:
			return uint32(b.flat[off])
		case 2:
			return uint32(b.flat[off]) | uint32(b.flat[off+1])<<8
		case 4:
			return uint32(b.flat[off]) | uint32(b.flat[off+1])<<8 |
				uint32(b.flat[off+2])<<16 | uint32(b.flat[off+3])<<24
		}
		panic("bad size")
	}
	if r, off, ok := b.ramAt(addr, size); ok {
		switch size {
		case 1:
			return uint32(r.data[off])
		case 2:
			return uint32(r.data[off]) | uint32(r.data[off+1])<<8
		case 4:
			return uint32(r.data[off]) | uint32(r.data[off+1])<<8 |
				uint32(r.data[off+2])<<16 | uint32(r.data[off+3])<<24
		}
		panic("bad size")
	}
	return b.Read(addr, size)
}

// FastWrite 热路径写。
func (b *SystemBus) FastWrite(addr uint32, size uint32, val uint32) {
	if off := addr - FlatBase; off < FlatSize-size+1 {
		b.touchCode(addr)
		switch size {
		case 1:
			b.flat[off] = uint8(val)
		case 2:
			b.flat[off] = uint8(val)
			b.flat[off+1] = uint8(val >> 8)
		case 4:
			b.flat[off] = uint8(val)
			b.flat[off+1] = uint8(val >> 8)
			b.flat[off+2] = uint8(val >> 16)
			b.flat[off+3] = uint8(val >> 24)
		default:
			panic("bad size")
		}
		return
	}
	if r, off, ok := b.ramAt(addr, size); ok {
		switch size {
		case 1:
			r.data[off] = uint8(val)
		case 2:
			r.data[off] = uint8(val)
			r.data[off+1] = uint8(val >> 8)
		case 4:
			r.data[off] = uint8(val)
			r.data[off+1] = uint8(val >> 8)
			r.data[off+2] = uint8(val >> 16)
			r.data[off+3] = uint8(val >> 24)
		default:
			panic("bad size")
		}
		return
	}
	b.Write(addr, size, val)
}

func in(addr, base, size uint32) bool {
	return addr >= base && addr < base+size
}
