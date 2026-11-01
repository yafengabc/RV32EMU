package perip

import (
	"fmt"
	"os"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// ESP32-C3 私有外设：TIMG0 节拍源 + INTMTX 中断矩阵 + SPI1 flash 擦除。
// 这些外设不是标准 RISC-V CLINT/PLIC，而是 esp-riscv 私有实现：
//   - 没有 mie CSR，中断仅由 mstatus.MIE 门控；
//   - TG0 T0 经 INTMTX 路由到 CPU line 2，tick 上报 mcause = 0x80000002；
//   - 中断由外设在每个周期把 pendingInt 写入 CPU 来投递。

const (
	TIMG0Base uint32 = 0x6001F000
	TIMG0Size uint32 = 0x1000

	INTMTXBase uint32 = 0x600C2000
	INTMTXSize uint32 = 0x1000

	SPI1Base uint32 = 0x60002000
	SPI1Size uint32 = 0x1000

	// flash 模拟区基址：ROM 读写桩把 (FLASH_EMU_BASE + flash_addr)
	// 当作物理 flash；ramdisk 镜像放在 flash 偏移 0x100000 处。
	FlashEmuBase uint32 = 0x3F000000

	// C3 定时器上报的 mcause（IRQ | line 2）。
	c3TickCause uint32 = 0x80000002
)

// ---- TIMG0（节拍源）---------------------------------------------------

type C3Timer struct {
	cpu     *cpu.CPU
	counter uint64
	reload  uint32
	fires   uint64 // 定时器触发次数（诊断用）
	div     uint32 // 计数分频：>1 时每 div 条指令才计一次（调 tick 周期用）
	sub     uint32
	regs    map[uint32]uint32
}

func NewC3Timer(c *cpu.CPU) *C3Timer {
	t := &C3Timer{
		cpu:    c,
		reload: 10000, // (80MHz/80)/100 = 10000 counts/tick，作为默认
		div:    1,
		regs:   make(map[uint32]uint32),
	}
	return t
}

// SetDiv 设置计数分频，用于把 tick 周期拉长做时序敏感性验证。
func (t *C3Timer) SetDiv(d uint32) {
	if d == 0 {
		d = 1
	}
	t.div = d
}

func (t *C3Timer) Name() string { return "C3TIMG0" }

// SetCPU 在 CPU 创建后注入引用，使定时器能上报中断。
func (t *C3Timer) SetCPU(c *cpu.CPU) { t.cpu = c }

// Tick 每个 CPU 周期调用一次：计数达到 reload 则上报一次 tick 中断。
// 内核在 ISR 里清 INTMTX 并 re-arm T0，但我们的模型里定时器按周期
// 持续触发即可——pendingInt 每次被 CPU 取走后置 0，下一周期再置位。
func (t *C3Timer) Tick() {
	if t.div > 1 {
		t.sub++
		if t.sub < t.div {
			return
		}
		t.sub = 0
	}
	t.counter++
	if t.counter >= uint64(t.reload) {
		t.counter = 0
		t.fires++
		t.cpu.SetPendingInt(c3TickCause)
	}
}

// Fires 返回定时器累计触发次数（诊断用）。
func (t *C3Timer) Fires() uint64 { return t.fires }

// TIMG0 寄存器偏移（仅实现内核实际访问的子集）。
const (
	t0ConfigReg  uint32 = 0x00
	t0AlarmLoReg uint32 = 0x10
	t0AlarmHiReg uint32 = 0x14
	t0LoadLoReg  uint32 = 0x18
	t0LoadHiReg  uint32 = 0x1C
	t0LoadReg    uint32 = 0x20
	t0WdtCfg0    uint32 = 0x48
	t0WdtFeed    uint32 = 0x60
	t0WdtProtect uint32 = 0x64
	t0IntEna     uint32 = 0x70
	t0IntClr     uint32 = 0x7C
	t0RegClk     uint32 = 0xFC
)

const (
	t0En       uint32 = 1 << 31
	t0AlarmEn  uint32 = 1 << 10
	t0DivS     uint32 = 13
	t0DivM     uint32 = 0xFFFF
)

func (t *C3Timer) Read(offset uint32, size uint32) uint32 {
	if v, ok := t.regs[offset]; ok {
		return v
	}
	return 0
}

func (t *C3Timer) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case t0ConfigReg:
		t.regs[offset] = val
		if val&t0En != 0 {
			t.counter = 0 // 使能即重启动计数器
		}
	case t0AlarmLoReg:
		t.regs[offset] = val
		t.reload = val | (t.regs[t0AlarmHiReg] << 16)
	case t0AlarmHiReg:
		t.regs[offset] = val
		t.reload = t.regs[t0AlarmLoReg] | (val << 16)
	case t0LoadLoReg, t0LoadHiReg:
		t.regs[offset] = val
	case t0LoadReg:
		t.counter = 0 // 任意写都 reload 到 0
	case t0WdtCfg0, t0WdtFeed, t0WdtProtect, t0IntEna, t0IntClr, t0RegClk:
		t.regs[offset] = val
	default:
		fmt.Fprintf(os.Stderr, "[c3timg0] unhandled write off=0x%02X val=0x%08X\n", offset, val)
	}
}

// ---- INTMTX（中断矩阵，被动）-----------------------------------------

type C3Intc struct {
	regs map[uint32]uint32
}

func NewC3Intc() *C3Intc {
	return &C3Intc{regs: make(map[uint32]uint32)}
}

func (i *C3Intc) Name() string { return "C3INTMTX" }

func (i *C3Intc) Read(offset uint32, size uint32) uint32 {
	if v, ok := i.regs[offset]; ok {
		return v
	}
	return 0
}

func (i *C3Intc) Write(offset uint32, size uint32, val uint32) {
	// 路由/优先级/使能位只做影子存储；实际的 line 2 tick 由 TIMG0 驱动。
	i.regs[offset] = val
}

// ---- SPI1（flash 控制器，仅擦除被内核走硬件）-------------------------

type C3SPI1 struct {
	bus       *mem.SystemBus
	flashBase uint32
	addr      uint32
	cmd       uint32
}

func NewC3SPI1(b *mem.SystemBus, flashBase uint32) *C3SPI1 {
	return &C3SPI1{bus: b, flashBase: flashBase}
}

func (s *C3SPI1) Name() string { return "C3SPI1" }

const (
	spi1CmdReg     uint32 = 0x00
	spi1AddrReg    uint32 = 0x04
	spi1CmdWren    uint32 = 1 << 30
	spi1CmdSe      uint32 = 1 << 24
)

func (s *C3SPI1) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case spi1CmdReg:
		return s.cmd // 已清除的位会读回 0
	case spi1AddrReg:
		return s.addr
	}
	return 0
}

func (s *C3SPI1) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case spi1AddrReg:
		s.addr = val
	case spi1CmdReg:
		// 逐位处理并清除（内核写后轮询直到位清 0）。
		if val&spi1CmdWren != 0 {
			// write-enable 在模拟里无需动作
			val &^= spi1CmdWren
		}
		if val&spi1CmdSe != 0 {
			s.eraseSector(s.addr)
			val &^= spi1CmdSe
		}
		s.cmd = val
	default:
		fmt.Fprintf(os.Stderr, "[c3spi1] unhandled write off=0x%02X val=0x%08X\n", offset, val)
	}
}

// eraseSector 把 flash 中 4 KiB 扇区（按地址对齐）填 0xFF，模拟 NOR 擦除。
func (s *C3SPI1) eraseSector(flashAddr uint32) {
	off := flashAddr &^ 0xFFF
	for i := uint32(0); i < 4096; i++ {
		s.bus.Write(s.flashBase+off+i, 1, 0xFF)
	}
}
