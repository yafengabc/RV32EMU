package perip

import "rv32emu/internal/mem"

// ESP32-C3 的 flash cache（XIP）模型。
//
// 真实硬件有两级映射：EXTMEM 控制寄存器（0x600C4000）+ 128 项 MMU 表
// （0x600C5000），每项 64 KiB，把 flash 页映射到 0x3C000000（DROM）
// 或 0x42000000（IROM）窗口。本模型：
//   - MMU 表就是普通内存，软件（bootloader/内核）写什么就按什么翻译；
//   - 复位默认是线性映射（窗口偏移 == flash 偏移），方便裸机固件直接取指；
//   - 通过 ELF 加载写进窗口的字节保存在一份按页的写回缓存里（overlay），
//     读取优先命中 overlay，因此 ELF 的 IROM/DROM 段不需要真写 flash；
//   - EXT MEM 控制寄存器只做影子存储，不按 ENABLE/SHUT 位真正开关 cache
//     （否则没有 bootloader 的固件在初始化前取指就会失败）。
//
// 已知限制：没有真正的 cache 失效/一致性行为；SPI 擦写 flash 后，已写入
// overlay 的页不会自动失效。

const (
	CacheRegBase  uint32 = 0x600C4000
	CacheRegSize  uint32 = 0x1200 // 0x000..0xFFF 控制寄存器 + 0x1000 起的 MMU 表
	MMUTableBase  uint32 = 0x600C5000
	MMUTableSize  uint32 = 0x200
	DROMBase      uint32 = 0x3C000000
	IROMBase      uint32 = 0x42000000
	XIPWindowSize uint32 = 0x800000 // 8 MiB / 128 个 64 KiB 页

	cachePageShift = 16
	cachePageSize  = 1 << cachePageShift
	mmuEntryNum    = 128
	mmuInvalidBit  = 1 << 8
)

// ---- MMU / XIP 窗口 ---------------------------------------------------

type FlashMMU struct {
	bus       *mem.SystemBus
	flashBase uint32
	flashSize uint32
	entries   [mmuEntryNum]uint32
	overlay   map[uint32][]byte // 虚拟页基址 → 64 KiB 数据
}

func NewFlashMMU(b *mem.SystemBus, flashBase, flashSize uint32) *FlashMMU {
	m := &FlashMMU{
		bus:       b,
		flashBase: flashBase,
		flashSize: flashSize,
		overlay:   make(map[uint32][]byte),
	}
	// 复位默认线性映射：页 i ↔ flash 页 i；超出 flash 的页标记为无效。
	for i := 0; i < mmuEntryNum; i++ {
		if uint32(i)<<cachePageShift < flashSize {
			m.entries[i] = uint32(i)
		} else {
			m.entries[i] = mmuInvalidBit
		}
	}
	return m
}

func (m *FlashMMU) phys(vaddr uint32) (uint32, bool) {
	e := m.entries[(vaddr>>cachePageShift)&(mmuEntryNum-1)]
	if e&mmuInvalidBit != 0 {
		return 0, false
	}
	page := e & 0xFF
	if page<<cachePageShift >= m.flashSize {
		return 0, false
	}
	return page<<cachePageShift | (vaddr & (cachePageSize - 1)), true
}

func (m *FlashMMU) read(addr uint32, size uint32) uint32 {
	var v uint32
	for i := uint32(0); i < size; i++ {
		v |= uint32(m.readByte(addr+i)) << (8 * i)
	}
	return v
}

func (m *FlashMMU) readByte(addr uint32) byte {
	if line, ok := m.overlay[addr&^uint32(cachePageSize-1)]; ok {
		return line[addr&(cachePageSize-1)]
	}
	phys, ok := m.phys(addr)
	if !ok {
		return 0xFF // 未映射的 cache 访问在真机上会报 cache fault，这里给空值继续跑
	}
	return byte(m.bus.Read(m.flashBase+phys, 1))
}

func (m *FlashMMU) write(addr uint32, size uint32, val uint32) {
	page := addr &^ uint32(cachePageSize-1)
	line, ok := m.overlay[page]
	if !ok {
		line = make([]byte, cachePageSize)
		m.overlay[page] = line
	}
	for i := uint32(0); i < size; i++ {
		line[(addr+i)&(cachePageSize-1)] = byte(val >> (8 * i))
	}
}

// Map 把 flash 的 [flashOff, flashOff+size) 映射到从 vaddr 开始的窗口页。
// 用于「按 esp_image 加载应用」：Bootloader 在真机上就是用 MMU 把应用
// 的 DROM/IROM 段映射进两个 XIP 窗口的。映射后这些虚拟页以 flash 为准，
// 已有的 overlay 页被清掉。
func (m *FlashMMU) Map(vaddr, flashOff, size uint32) {
	first := (vaddr >> cachePageShift) & (mmuEntryNum - 1)
	fpage := (flashOff >> cachePageShift) & 0xFF
	pages := (size + cachePageSize - 1) >> cachePageShift
	for i := uint32(0); i < pages; i++ {
		idx := (first + i) & (mmuEntryNum - 1)
		m.entries[idx] = (fpage + i) & 0xFF
		delete(m.overlay, (vaddr+i*cachePageSize)&^uint32(cachePageSize-1))
	}
}

// XIPWindow 是挂在 DROM/IROM 地址段上的 cache 窗口。
type XIPWindow struct {
	mmu  *FlashMMU
	base uint32
	name string
}

func NewXIPWindow(m *FlashMMU, base uint32, name string) *XIPWindow {
	return &XIPWindow{mmu: m, base: base, name: name}
}

func (w *XIPWindow) Name() string { return w.name }

func (w *XIPWindow) Read(offset uint32, size uint32) uint32 {
	return w.mmu.read(w.base+offset, size)
}

func (w *XIPWindow) Write(offset uint32, size uint32, val uint32) {
	w.mmu.write(w.base+offset, size, val)
}

// ---- EXTMEM 控制寄存器 ------------------------------------------------

type CacheCtrl struct {
	mmu  *FlashMMU
	regs map[uint32]uint32
}

func NewCacheCtrl(m *FlashMMU) *CacheCtrl {
	c := &CacheCtrl{mmu: m, regs: make(map[uint32]uint32)}
	c.regs[0x004] = 0x3 // ICACHE_CTRL1: 复位默认 IRAM/DRAM cache 关闭
	c.regs[0x054] = IROMBase
	c.regs[0x058] = IROMBase + XIPWindowSize - 1
	c.regs[0x05C] = DROMBase
	c.regs[0x060] = DROMBase + XIPWindowSize - 1
	return c
}

func (c *CacheCtrl) Name() string { return "C3CACHE" }

// 这些寄存器里的使能位是自清的（lock/sync/preload/计数清零等），
// 软件会写 1 后轮询读回 0；模型按写完即完成处理。
func cacheTrigger(offset uint32) bool {
	switch offset {
	case 0x01C, 0x028, 0x034, 0x064, 0x07C, 0x088:
		return true
	}
	return false
}

func (c *CacheCtrl) Read(offset uint32, size uint32) uint32 {
	if offset >= MMUTableBase-CacheRegBase {
		idx := (offset - (MMUTableBase - CacheRegBase)) / 4
		if idx < mmuEntryNum {
			return c.mmu.entries[idx]
		}
		return 0
	}
	if cacheTrigger(offset) {
		return 0
	}
	return c.regs[offset]
}

func (c *CacheCtrl) Write(offset uint32, size uint32, val uint32) {
	if offset >= MMUTableBase-CacheRegBase {
		idx := (offset - (MMUTableBase - CacheRegBase)) / 4
		if idx < mmuEntryNum {
			c.mmu.entries[idx] = val
		}
		return
	}
	if cacheTrigger(offset) {
		return
	}
	c.regs[offset] = val
}
