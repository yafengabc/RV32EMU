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

	// TG0WDT（TIMG0 看门狗）。真机上 ROM 交出来时它是开着的，但本模拟器
	// 的 ROM 是手写桩、没有那段初始化，所以上电默认关闭；固件要按真机
	// 流程（解锁 WPROTECT → 写 CONFIG0 使能 → 定期 FEED）启用后才会计时。
	// 1 条指令按 1 个 APB 周期计（与 tick 的换算一致）。
	//
	// 寄存器语义按 C3 的 MWDT：CONFIG0 是控制/各级动作，CONFIG1 是预分频
	// （CLK_PRESCALE[31:16]），CONFIG2..5 才是 stage0..3 的超时值。
	wdtCfg0     uint32 // CONFIG0 影子，bit31 = EN
	wdtHold     uint32 // CONFIG2：stage0 超时值（MWDT 周期数）
	wdtPrescale uint32 // CONFIG1：CLK_PRESCALE，默认 1
	wdtCount    uint64
	wdtUnlock   bool // WPROTECT 是否已用解锁字打开
	wdtFires    uint64
	wdtReset    bool // 超时触发系统复位（与 RTC SW_SYS_RST 共用重装路径）
}

func NewC3Timer(c *cpu.CPU) *C3Timer {
	t := &C3Timer{
		cpu:         c,
		reload:      10000, // (80MHz/80)/100 = 10000 counts/tick，作为默认
		div:         1,
		wdtPrescale: 1,
		// WKEY 复位值就是解锁字，所以复位后处于“未上锁”状态。
		wdtUnlock: true,
		regs:      map[uint32]uint32{t0WdtProtect: t0WdtProtectKey},
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
func (t *C3Timer) Tick() { t.TickN(1) }

// TickN 一次推进 n 个周期，供 CPU 批量执行（RunHot）后补计时。
// 语义与连续调用 n 次 Tick 等价，只是把中间的分频/重载合并计算；
// 内部只用减法循环（触发次数不会很多），避免每条指令一次 64 位除法。
func (t *C3Timer) TickN(n uint64) {
	if n == 0 {
		return
	}
	if t.div > 1 {
		total := uint64(t.sub) + n
		n = total / uint64(t.div)
		t.sub = uint32(total % uint64(t.div))
		if n == 0 {
			return
		}
	}
	t.tickWDT(n)
	if t.reload == 0 { // 未设置报警值：按原语义每周期都触发
		t.fires += n
		if t.cpu != nil {
			t.cpu.SetPendingInt(c3TickCause)
		}
		return
	}
	fires := uint64(0)
	for n > 0 {
		remaining := uint64(t.reload) - t.counter
		if n < remaining {
			t.counter += n
			break
		}
		n -= remaining
		t.counter = 0
		fires++
	}
	if fires > 0 {
		t.fires += fires
		if t.cpu != nil {
			t.cpu.SetPendingInt(c3TickCause)
		}
	}
}

// tickWDT 按周期推进 TG0WDT 的 stage0 计数。超时后只记一次复位请求，
// 由主循环把整台机器重装（与 RTC_CNTL SW_SYS_RST 的处理一致）。
func (t *C3Timer) tickWDT(n uint64) {
	if t.wdtCfg0&t0WdtEn == 0 || t.wdtHold == 0 || t.wdtReset {
		return
	}
	// stage0 超时值按 MWDT 周期计：一个 MWDT 周期 = prescale 个 APB 周期。
	// 本模拟器把 1 条指令当作 1 个 APB 周期。
	limit := uint64(t.wdtHold) * uint64(t.wdtPrescale)
	if limit == 0 {
		limit = uint64(t.wdtHold)
	}
	t.wdtCount += n
	if t.wdtCount >= limit {
		t.wdtCount = limit
		t.wdtFires++
		t.wdtReset = true
	}
}

// WDTState 是供控制协议/GUI 使用的 TG0WDT 快照。
type WDTState struct {
	Enabled      bool   `json:"enabled"`
	Stage0       uint32 `json:"stage0"`
	Prescale     uint32 `json:"prescale"`
	Fires        uint64 `json:"fires"`
	ResetPending bool   `json:"resetPending"`
}

func (t *C3Timer) WDTState() WDTState {
	return WDTState{
		Enabled:      t.wdtCfg0&t0WdtEn != 0,
		Stage0:       t.wdtHold,
		Prescale:     t.wdtPrescale,
		Fires:        t.wdtFires,
		ResetPending: t.wdtReset,
	}
}

// SetWDTEnabled 供控制协议（GUI）直接开关 TG0WDT，绕过 WPROTECT 解锁流程。
// 打开时若 stage0 超时值还是 0（复位默认），补一个与 RTC WDT 默认值同量级
// 的数，否则“开关”不会有任何可见效果。
func (t *C3Timer) SetWDTEnabled(on bool) {
	if on {
		if t.wdtHold == 0 {
			t.wdtHold = 200000
		}
		t.wdtCfg0 |= t0WdtEn
		t.wdtCount = 0
	} else {
		t.wdtCfg0 &^= t0WdtEn
		t.wdtCount = 0
	}
}

// SetWDTHold 供控制协议设置 stage0 超时值（MWDT 周期数）。
func (t *C3Timer) SetWDTHold(hold uint32) { t.wdtHold = hold }

// FeedWDT 供控制协议喂狗。
func (t *C3Timer) FeedWDT() { t.wdtCount = 0 }

// WDTResetPending 报告 TG0WDT 已超时并要求系统复位。
func (t *C3Timer) WDTResetPending() bool { return t.wdtReset }

// WDTFires 返回 TG0WDT 超时次数（诊断用）。
func (t *C3Timer) WDTFires() uint64 { return t.wdtFires }

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
	t0WdtCfg0    uint32 = 0x48 // EN(bit31) + 各级动作选择
	t0WdtCfg1    uint32 = 0x4C // 预分频：CLK_PRESCALE[31:16] + DIVCNT_RST(bit0)
	t0WdtCfg2    uint32 = 0x50 // stage0 超时值（MWDT 周期数）
	t0WdtCfg3    uint32 = 0x54 // stage1
	t0WdtCfg4    uint32 = 0x58 // stage2
	t0WdtCfg5    uint32 = 0x5C // stage3
	t0WdtFeed    uint32 = 0x60
	t0WdtProtect uint32 = 0x64
	t0IntEna     uint32 = 0x70
	t0IntClr     uint32 = 0x7C
	t0RegClk     uint32 = 0xFC
)

const (
	t0En      uint32 = 1 << 31
	t0AlarmEn uint32 = 1 << 10
	t0DivS    uint32 = 13
	t0DivM    uint32 = 0xFFFF

	t0WdtEn uint32 = 1 << 31 // CONFIG0 的 WDT_EN
	// t0WdtProtectKey 是 WPROTECT 的解锁字：写入它之后 CONFIG0..5 才接受写入。
	t0WdtProtectKey uint32 = 0x50D83AA1
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
	case t0WdtCfg0:
		// CONFIG0..5 受 WPROTECT 保护，未解锁时写入被忽略（真机行为）。
		if t.wdtUnlock {
			if val&t0WdtEn != 0 && t.wdtCfg0&t0WdtEn == 0 {
				t.wdtCount = 0 // 使能时从 0 开始计时
			}
			t.regs[offset] = val
			t.wdtCfg0 = val
			if val&t0WdtEn == 0 {
				t.wdtCount = 0 // 关狗同时清计数
			}
		}
	case t0WdtCfg1:
		if t.wdtUnlock {
			t.regs[offset] = val
			// CLK_PRESCALE 为 0 时按 1 处理，避免除零/零超时。
			if p := val >> 16; p != 0 {
				t.wdtPrescale = p
			} else {
				t.wdtPrescale = 1
			}
			if val&1 != 0 { // WDT_DIVCNT_RST
				t.wdtCount = 0
			}
		}
	case t0WdtCfg2:
		if t.wdtUnlock {
			t.regs[offset] = val
			t.wdtHold = val
		}
	case t0WdtCfg3, t0WdtCfg4, t0WdtCfg5:
		if t.wdtUnlock {
			t.regs[offset] = val // stage1..3 只做影子
		}
	case t0WdtFeed:
		// FEED 不受 WPROTECT 保护：写任意值都把 stage0 计数清零。
		t.regs[offset] = val
		t.wdtCount = 0
	case t0WdtProtect:
		t.regs[offset] = val
		t.wdtUnlock = val == t0WdtProtectKey
	case t0IntEna, t0IntClr, t0RegClk:
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
	spi1CmdReg  uint32 = 0x00
	spi1AddrReg uint32 = 0x04
	spi1CmdWren uint32 = 1 << 30
	spi1CmdSe   uint32 = 1 << 24
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
