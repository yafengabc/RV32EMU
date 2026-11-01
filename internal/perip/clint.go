package perip

import "encoding/binary"

// CLINT 基址（标准 RISC-V CLINT 位置，作为 ESP32-C3 定时器中断来源的近似）。
const (
	CLINTBase uint32 = 0x02000000
	CLINTSize uint32 = 0x10000
)

// 偏移。
const (
	mtimecmpLo uint32 = 0x4000
	mtimecmpHi uint32 = 0x4004
	mtimeLo    uint32 = 0xBFF8
	mtimeHi    uint32 = 0xBFFC
)

// CLINT：提供 mtimer，比较命中后置 MTIP（machine timer interrupt pending）。
type CLINT struct {
	mtime    uint64
	mtimecmp uint64
	mtip     bool
}

func NewCLINT() *CLINT { return &CLINT{mtimecmp: ^uint64(0)} }

func (c *CLINT) Name() string { return "CLINT" }

// Tick 每个 CPU 周期调用一次，推进 64-bit 计数器。
func (c *CLINT) Tick() {
	c.mtime++
	c.mtip = c.mtime >= c.mtimecmp
}

// MTIP 返回当前定时器中断是否挂起。
func (c *CLINT) MTIP() bool { return c.mtip }

func (c *CLINT) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case mtimecmpLo:
		return uint32(c.mtimecmp)
	case mtimecmpHi:
		return uint32(c.mtimecmp >> 32)
	case mtimeLo:
		return uint32(c.mtime)
	case mtimeHi:
		return uint32(c.mtime >> 32)
	}
	return 0
}

func (c *CLINT) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case mtimecmpLo:
		lo := uint64(val)
		c.mtimecmp = (c.mtimecmp & 0xFFFFFFFF00000000) | lo
		c.mtip = c.mtime >= c.mtimecmp
	case mtimecmpHi:
		hi := uint64(val) << 32
		c.mtimecmp = (c.mtimecmp & 0x00000000FFFFFFFF) | hi
		c.mtip = c.mtime >= c.mtimecmp
	case mtimeLo:
		c.mtime = (c.mtime & 0xFFFFFFFF00000000) | uint64(val)
	case mtimeHi:
		c.mtime = (c.mtime & 0x00000000FFFFFFFF) | (uint64(val) << 32)
	}
}

// must be imported; keep binary.LittleEndian used conceptually for future.
var _ = binary.LittleEndian
