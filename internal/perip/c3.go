package perip

import (
	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// ESP32-C3 私有外设：TIMG0 节拍源 + INTMTX 中断矩阵 + SPI0/SPI1 flash 控制器。
// 这些外设不是标准 RISC-V CLINT/PLIC，而是 esp-riscv 私有实现：
//   - 没有 mie CSR，中断仅由 mstatus.MIE 门控；
//   - 各中断源经 INTMTX 路由到 CPU line，mcause = 0x80000000 | line
//     （未配置 INTMTX 的旧测试程序仍按 TG0 T0 → line 2 直接投递）；
//   - 中断由外设在每个周期把 pendingInt 写入 CPU 来投递。

const (
	TIMG0Base uint32 = 0x6001F000
	TIMG0Size uint32 = 0x1000

	INTMTXBase uint32 = 0x600C2000
	INTMTXSize uint32 = 0x1000

	SPI0Base uint32 = 0x60003000 // cache / flash 控制器
	SPI1Base uint32 = 0x60002000 // flash 编程控制器
	SPISize  uint32 = 0x1000

	// flash 模拟区基址：ROM 读写桩把 (FLASH_EMU_BASE + flash_addr)
	// 当作物理 flash；ramdisk 镜像默认放在 flash 偏移 0x80000 处。
	FlashEmuBase uint32 = 0x3F000000

	// C3 定时器上报的 mcause（IRQ | line 2）。
	c3TickCause uint32 = 0x80000002

	// TIMG0 T0 的 INTMTX 中断源编号（ETS_TG0_T0_LEVEL_INTR_SOURCE）。
	timerSrc uint32 = 27
)

// ---- TIMG0（节拍源）---------------------------------------------------

type C3Timer struct {
	cpu     *cpu.CPU
	intc    *C3Intc
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

	// TIMG0 RTC 校准（0x68/0x6C/0x80）。固件用它对慢时钟做频率校准，
	// 校准完成后读 VALUE 计算 RTC 时间；不建模会让启动卡在轮询 RDY。
	rtcCaliCfg  uint32
	rtcCaliVal  uint32
	rtcCaliCfg2 uint32

	// tickPending 是 T0 报警的原生态电平：到点置位，固件写 INT_CLR 才撤除。
	tickPending bool
}

func NewC3Timer(c *cpu.CPU) *C3Timer {
	t := &C3Timer{
		cpu:         c,
		reload:      10000, // (80MHz/80)/100 = 10000 counts/tick，作为默认
		div:         1,
		wdtPrescale: 1,
		// WKEY 复位值就是解锁字，所以复位后处于“未上锁”状态。
		wdtUnlock:   true,
		rtcCaliCfg:  t0RtcCaliCfgDefault,
		rtcCaliVal:  1 << 7, // MAX=1 时约 1 个 XTAL 周期量级，够启动用
		rtcCaliCfg2: 3 << 3, // TIMEOUT_RST_CNT 默认 3
		regs:        map[uint32]uint32{t0WdtProtect: t0WdtProtectKey},
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

// SetIntc 注入中断矩阵：配置过映射后，T0 中断按 INTMTX 的使能/优先级/阈值
// 投递，而不是绕过矩阵直接写 CPU line 2。
func (t *C3Timer) SetIntc(i *C3Intc) { t.intc = i }

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
	// T0 只有被固件真正布防（CONFIG0 的 EN + ALARM_EN 都置位）才会产生
	// 报警中断。真机复位后 T0 是关的；ESP-IDF 在 C3 上用 SYSTIMER 做
	// FreeRTOS tick，根本不碰 TIMG0 T0，所以这里不能凭空造 tick——
	// 否则会持续投递一个固件从未使能、也永远不会清的中断源。
	if t.regs[t0ConfigReg]&t0En == 0 || t.regs[t0ConfigReg]&t0AlarmEn == 0 {
		return
	}
	if t.reload == 0 { // 未设置报警值：按原语义每周期都触发
		t.fires += n
		t.raiseTick()
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
		t.raiseTick()
	}
}

// raiseTick 上报一次 T0 定时中断。真机上 TIMG0 T0 是 INTMTX 的一个中断源
// （source 27），能否送到 CPU 取决于映射、line 使能、优先级和阈值——固件
// 在 ISR 里把阈值抬到 pri+1 屏蔽同级中断，这个约束必须遵守，否则会凭空
// 嵌套出嵌套「不存在的」中断，打乱 FreeRTOS 的嵌套计数。没配置 INTMTX 的
// 旧测试程序保持原样，直接按 CPU line 2 投递。
func (t *C3Timer) raiseTick() {
	if t.cpu == nil {
		return
	}
	t.tickPending = true
	t.syncIntc()
}

// syncIntc 把 T0 的「原生态」（tickPending 且时钟中断使能）同步到 INTMTX。
// 真机上这是电平：定时器到点后源一直保持，直到 ISR 写 INT_CLR 撤除；
// 若此刻被阈值挡住，等阈值恢复后会重新投递，而不是丢掉这一拍。
func (t *C3Timer) syncIntc() {
	if t.cpu == nil {
		return
	}
	if t.intc != nil && t.intc.MappedLine(timerSrc) >= 0 {
		if t.tickPending {
			t.intc.Raise(timerSrc)
		} else {
			t.intc.ClearSource(timerSrc)
		}
		t.intc.Deliver()
		return
	}
	if t.tickPending {
		t.cpu.SetPendingInt(c3TickCause)
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
	t0LoReg      uint32 = 0x04 // 计数器低 32 位（只读）
	t0HiReg      uint32 = 0x08 // 计数器高 32 位（只读）
	t0UpdateReg  uint32 = 0x0C // 写 1 把 LO/HI 锁存进计数器，自清
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
	t0RtcCaliCfg uint32 = 0x68 // RTC 校准：START(31) / MAX[30:16] / RDY(15) / CLK_SEL[14:13] / CYCLING(12)
	t0RtcCali1   uint32 = 0x6C // VALUE[31:7]（用 XTAL 周期数表示）
	t0RtcCali2   uint32 = 0x80 // TIMEOUT(0) / TIMEOUT_RST_CNT[6:3] / TIMEOUT_THRES[31:7]
	t0IntEna     uint32 = 0x70
	t0IntClr     uint32 = 0x7C
	t0RegClk     uint32 = 0xFC
)

const (
	t0RtcCaliStart   uint32 = 1 << 31
	t0RtcCaliRdy     uint32 = 1 << 15
	t0RtcCaliClkSelS uint32 = 13
	t0RtcCaliMaxS    uint32 = 16
	t0RtcCaliMaxM    uint32 = 0x7FFF
	// 复位默认：CYCLING=1、CLK_SEL=1、MAX=1（见 timer_group_reg.h），
	// RDY 也置 1——固件启动时先等「上一次循环校准」结束，真机上早已完成。
	t0RtcCaliCfgDefault uint32 = 0x0001B000
	// 模拟器固定 XTAL 40MHz；校准的慢时钟频率按 CLK_SEL 选。
	c3XtalHz uint32 = 40 * 1000 * 1000
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

// startRtcCali 立即完成一次 RTC 校准：按 CLK_SEL 选慢时钟频率，把
// slowclk_cycles 个慢时钟周期折算成 XTAL 周期数写入 RTCCALICFG1。
// 真机需要等几十微秒，这里直接给出结果（固件随后轮询 RDY 一次就过）。
func (t *C3Timer) startRtcCali() {
	max := (t.rtcCaliCfg >> t0RtcCaliMaxS) & t0RtcCaliMaxM
	if max == 0 {
		max = 1
	}
	sel := (t.rtcCaliCfg >> t0RtcCaliClkSelS) & 3
	var slowHz uint32
	switch sel {
	case 0:
		slowHz = 150 * 1000 // RC_SLOW
	case 1:
		slowHz = 8 * 1000 * 1000 / 256 // RC_FAST 8M 分频 256
	case 2, 3:
		slowHz = 32768 // 外部 32K XTAL
	}
	cycles := uint64(max) * uint64(c3XtalHz) / uint64(slowHz)
	t.rtcCaliVal = uint32(cycles&0x1FFFFFF) << 7
}

func (t *C3Timer) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case t0LoReg:
		return uint32(t.counter)
	case t0HiReg:
		return uint32(t.counter >> 32)
	case t0UpdateReg:
		return 0 // 只写触发位
	case t0RtcCaliCfg:
		return t.rtcCaliCfg
	case t0RtcCali1:
		return t.rtcCaliVal
	case t0RtcCali2:
		return t.rtcCaliCfg2
	}
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
	case t0UpdateReg:
		// 真机是「把 LO/HI 锁存值搬进计数器」，本模型按指令数自由运行，
		// 没有独立的锁存寄存器，这里只接受写入（自清）。
	case t0LoadReg:
		t.counter = 0 // 任意写都 reload 到 0
	case t0RtcCaliCfg:
		// RDY 是只读的：保留当前值，其余位按写入更新。
		t.rtcCaliCfg = (val &^ t0RtcCaliRdy) | (t.rtcCaliCfg & t0RtcCaliRdy)
		if val&t0RtcCaliStart != 0 {
			t.startRtcCali()
			t.rtcCaliCfg &^= t0RtcCaliStart
			t.rtcCaliCfg |= t0RtcCaliRdy
		}
	case t0RtcCali2:
		// TIMEOUT 是只读位，保持 0（校准总能立刻完成）。
		t.rtcCaliCfg2 = val &^ 1
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
	case t0IntEna:
		t.regs[offset] = val
		t.syncIntc()
	case t0IntClr:
		t.regs[offset] = val
		// 写 1 撤除对应中断电平（bit0 = T0 报警）。
		if val&1 != 0 {
			t.tickPending = false
		}
		t.syncIntc()
	case t0RegClk:
		t.regs[offset] = val
	}
}

// ---- INTMTX（中断矩阵）------------------------------------------------

// INTMTX 寄存器窗口里，source n 的映射寄存器位于 base + 4*n（低 5 位是
// CPU line）；0x100 之后是 CPU 侧的控制/状态寄存器：
//
//	0x104 CPU_INT_ENABLE   每 line 一位，置 1 才允许投递
//	0x114 + 4*line  CPU_INT_PRI_n
//	0x194 CPU_INT_THRESH
//	0x110 CPU_INT_EIP_STATUS（只读，当前正在请求的 line）
const (
	intcEna     uint32 = 0x104
	intcType    uint32 = 0x108
	intcClear   uint32 = 0x10C
	intcEip     uint32 = 0x110
	intcPriBase uint32 = 0x114
	intcThresh  uint32 = 0x194
)

type C3Intc struct {
	regs map[uint32]uint32
	cpu  *cpu.CPU
	// pending 是各外设 source 的电平状态：外设把「RAW & ENA」同步进来，
	// Deliver 时再按映射/使能/优先级折算成一条 CPU line。
	pending map[uint32]bool
}

func NewC3Intc() *C3Intc {
	return &C3Intc{
		regs:    make(map[uint32]uint32),
		pending: make(map[uint32]bool),
	}
}

func (i *C3Intc) Name() string { return "C3INTMTX" }

// SetCPU 在 CPU 创建后注入引用，用于投递 mcause = 0x80000000 | line。
func (i *C3Intc) SetCPU(c *cpu.CPU) { i.cpu = c }

// Raise/ClearSource 由外设调用，标记某个中断 source 的电平。
func (i *C3Intc) Raise(src uint32) {
	i.pending[src] = true
}
func (i *C3Intc) ClearSource(src uint32) { delete(i.pending, src) }

// MappedLine 返回 source 当前映射到的 CPU line；映射寄存器为 0（line 0
// 不是合法目标）表示还没配置，返回 -1。
func (i *C3Intc) MappedLine(src uint32) int {
	v := i.regs[src*4]
	if v == 0 {
		return -1
	}
	return int(v & 0x1F)
}

// Deliver 选出当前应投递的 CPU line（优先级最高者），写入 CPU 的挂起中断。
// 没有配置过映射/使能的 source 不会产生中断——与真机复位状态一致。
func (i *C3Intc) Deliver() {
	if i.cpu == nil {
		return
	}
	ena := i.regs[intcEna]
	thresh := i.regs[intcThresh] & 0xF
	best, bestPri := -1, -1
	for line := 0; line < 32; line++ {
		if ena&(1<<uint(line)) == 0 || line == 1 || line == 6 {
			continue
		}
		// line 1（WiFi）和 6 在 C3 上不可用，跳过。
		pri := int(i.regs[intcPriBase+uint32(4*line)] & 0xF)
		if pri < int(thresh) {
			continue
		}
		hit := false
		for src := range i.pending {
			if int(i.regs[src*4]&0x1F) == line {
				hit = true
				break
			}
		}
		if hit && pri > bestPri {
			best, bestPri = line, pri
		}
	}
	if best >= 0 {
		i.cpu.SetPendingInt(0x80000000 | uint32(best))
	}
}

// eipStatus 返回当前在请求的 line（EIP_STATUS 的只读视图）。
func (i *C3Intc) eipStatus() uint32 {
	ena := i.regs[intcEna]
	thresh := i.regs[intcThresh] & 0xF
	var st uint32
	for src := range i.pending {
		line := i.regs[src*4] & 0x1F
		if line >= 32 || ena&(1<<line) == 0 || line == 1 || line == 6 {
			continue
		}
		if i.regs[intcPriBase+4*line]&0xF < thresh {
			continue
		}
		st |= 1 << line
	}
	return st
}

func (i *C3Intc) Read(offset uint32, size uint32) uint32 {
	if offset == intcEip {
		return i.eipStatus()
	}
	if v, ok := i.regs[offset]; ok {
		return v
	}
	return 0
}

func (i *C3Intc) Write(offset uint32, size uint32, val uint32) {
	i.regs[offset] = val
	// 映射/使能/优先级/阈值的变化可能让已有的 pending 立刻生效。
	if offset < 0x100 || offset == intcEna || offset == intcThresh ||
		offset == intcType || offset == intcClear || offset == intcEip {
		i.Deliver()
	}
}

// ---- SPI_MEM（SPI0/SPI1 flash 控制器）---------------------------------

// SPI0（0x60003000）和 SPI1（0x60002000）共用同一套 SPI_MEM 寄存器布局：
// 既能用硬件命令位（WREN/SE/BE/RDID…）直接发起操作，也能用 USER 模式
// 发任意命令。内核只用 SPI1 的擦除位，ROM/IDF 则走 USER 模式。
type C3SPI1 struct {
	bus       *mem.SystemBus
	flashBase uint32
	name      string
	addr      uint32
	cmd       uint32
	status    uint32 // flash 状态寄存器（bit0=WIP，bit1=WEL）
	regs      map[uint32]uint32
}

func NewC3SPI1(b *mem.SystemBus, flashBase uint32) *C3SPI1 {
	return newSPICtl(b, flashBase, "C3SPI1")
}

func NewC3SPI0(b *mem.SystemBus, flashBase uint32) *C3SPI1 {
	return newSPICtl(b, flashBase, "C3SPI0")
}

func newSPICtl(b *mem.SystemBus, flashBase uint32, name string) *C3SPI1 {
	s := &C3SPI1{bus: b, flashBase: flashBase, name: name, regs: make(map[uint32]uint32)}
	// SPI0 复位后 cache 处于关闭状态，配置寄存器给个合理初值。
	s.regs[spiRdStatusReg] = 0
	return s
}

func (s *C3SPI1) Name() string { return s.name }

const (
	spiCmdReg      uint32 = 0x00
	spiAddrReg     uint32 = 0x04
	spiUserReg     uint32 = 0x18
	spiUser1Reg    uint32 = 0x1C
	spiUser2Reg    uint32 = 0x20
	spiMosiDlenReg uint32 = 0x24
	spiMisoDlenReg uint32 = 0x28
	spiRdStatusReg uint32 = 0x2C
	spiW0Reg       uint32 = 0x58
	spiW15Reg      uint32 = 0x94

	spiCmdFlashPE   uint32 = 1 << 17
	spiCmdUsr       uint32 = 1 << 18
	spiCmdFlashCE   uint32 = 1 << 22
	spiCmdFlashBE   uint32 = 1 << 23
	spiCmdFlashSE   uint32 = 1 << 24
	spiCmdFlashPP   uint32 = 1 << 25
	spiCmdFlashRSR  uint32 = 1 << 27
	spiCmdFlashID   uint32 = 1 << 28
	spiCmdFlashWRDI uint32 = 1 << 29
	spiCmdFlashWR   uint32 = 1 << 30
	spiCmdFlashRd   uint32 = 1 << 31

	// 状态寄存器位：WIP=忙，WEL=写使能锁存（WREN 置位，编程/擦除后自动清零）。
	stWIP uint32 = 1 << 0
	stWEL uint32 = 1 << 1
)

// 一片 4 MiB GD25Q32 的 JEDEC ID：C8 40 16。
const c3FlashJEDECID uint32 = 0x1640C8

func (s *C3SPI1) Read(offset uint32, size uint32) uint32 {
	switch offset {
	case spiCmdReg:
		return s.cmd // 已完成的命令位读回 0
	case spiAddrReg:
		return s.addr
	}
	// W0..W15 是 32 位寄存器，但硬件允许按字节寻址：固件读完 flash 后会
	// 直接 memcpy((void *)SPI_MEM_W0, buf, len)，即按连续字节地址取值。
	if offset >= spiW0Reg && offset <= spiW15Reg+3 {
		base := offset &^ 3
		v := s.regs[base] >> ((offset & 3) * 8)
		if size < 4 {
			v &= 1<<(8*size) - 1
		}
		return v
	}
	if v, ok := s.regs[offset]; ok {
		return v
	}
	return 0
}

func (s *C3SPI1) Write(offset uint32, size uint32, val uint32) {
	switch offset {
	case spiAddrReg:
		s.addr = val
	case spiCmdReg:
		s.fireCommands(val)
	default:
		// 与读对称：支持按字节/半字写入 W0..W15（固件用 memcpy 填缓冲区）。
		if offset >= spiW0Reg && offset <= spiW15Reg+3 {
			base := offset &^ 3
			shift := (offset & 3) * 8
			var mask uint32 = 0xFFFFFFFF
			if size < 4 {
				mask = 1<<(8*size) - 1
			}
			s.regs[base] = (s.regs[base] &^ (mask << shift)) | ((val & mask) << shift)
			return
		}
		s.regs[offset] = val
	}
}

// fireCommands 处理一次 CMD 寄存器写入：硬件命令位和 USER 模式都可能是
// 一次操作，执行完把对应位清掉，供软件轮询。
func (s *C3SPI1) fireCommands(val uint32) {
	done := uint32(0)
	if val&spiCmdFlashWR != 0 {
		s.status |= stWEL // WREN: 置写使能锁存
		done |= spiCmdFlashWR
	}
	if val&spiCmdFlashWRDI != 0 {
		s.status &^= stWEL // WRDI: 清写使能锁存
		done |= spiCmdFlashWRDI
	}
	if val&spiCmdFlashID != 0 {
		s.regs[spiRdStatusReg] = c3FlashJEDECID
		done |= spiCmdFlashID
	}
	if val&spiCmdFlashRSR != 0 {
		s.regs[spiRdStatusReg] = s.status
		done |= spiCmdFlashRSR
	}
	if val&spiCmdFlashSE != 0 {
		s.eraseSector(s.addr)
		s.status &^= stWEL
		done |= spiCmdFlashSE
	}
	if val&spiCmdFlashBE != 0 {
		s.eraseBlock(s.addr, 0x8000)
		s.status &^= stWEL
		done |= spiCmdFlashBE
	}
	if val&spiCmdFlashCE != 0 {
		s.eraseBlock(0, 0x400000)
		s.status &^= stWEL
		done |= spiCmdFlashCE
	}
	if val&spiCmdFlashPP != 0 {
		s.programPage(s.addr)
		s.status &^= stWEL
		done |= spiCmdFlashPP
	}
	if val&spiCmdFlashRd != 0 {
		s.readFlash(s.addr, lenBits(s.regs[spiMisoDlenReg]))
		done |= spiCmdFlashRd
	}
	if val&spiCmdUsr != 0 {
		s.userCommand()
		done |= spiCmdUsr | spiCmdFlashPE
	}
	s.cmd = val &^ done
}

// ---- USER 模式命令引擎 -------------------------------------------------

// userCommand 支持 flash 最常用的一小组命令：读 ID/状态、读数据、页编程、
// 扇区/块/整片擦除、写使能。数据按 memcpy 语义放在 W0..W15 中
// （第 i 字节 → W[i/4] 的第 (i%4) 字节）。
func (s *C3SPI1) userCommand() {
	cmd := s.regs[spiUser2Reg] & 0xFFFF
	misoLen := lenBits(s.regs[spiMisoDlenReg])
	addr := s.addr

	switch cmd {
	case 0x9F: // RDID
		s.regs[spiRdStatusReg] = c3FlashJEDECID
		s.setWBytes([]byte{0xC8, 0x40, 0x16})
	case 0x05: // RDSR
		s.regs[spiRdStatusReg] = s.status
		s.setWBytes([]byte{byte(s.status)})
	case 0x35: // RDSR2
		s.regs[spiRdStatusReg] = 0
		s.setWBytes([]byte{0})
	case 0x03, 0x0B, 0x3B, 0xBB: // READ / FAST READ
		s.readFlash(addr, misoLen)
	case 0x02, 0x32, 0x38: // PP / QPP
		s.programPage(addr)
		s.status &^= stWEL
	case 0x06: // WREN
		s.status |= stWEL
	case 0x04: // WRDI
		s.status &^= stWEL
	case 0x20: // SE
		s.eraseSector(addr)
		s.status &^= stWEL
	case 0xD8: // BE (64 KiB)
		s.eraseBlock(addr, 0x10000)
		s.status &^= stWEL
	case 0xC7, 0x60: // CE
		s.eraseBlock(0, 0x400000)
		s.status &^= stWEL
	case 0x01, 0x31: // WRSR
		// 只保留保护位；WIP/WEL 是只读状态位。
		sr := uint32(s.wBytes(1)[0])
		s.status = (s.status & (stWIP | stWEL)) | (sr &^ (stWIP | stWEL))
	default:
	}
}

// lenBits 把 DLEN 寄存器（bit_len-1）换算成字节数，最多 64 字节。
func lenBits(v uint32) int {
	n := int((v&0x3FF)+1) / 8
	if n > 64 {
		n = 64
	}
	return n
}

func (s *C3SPI1) setWBytes(data []byte) {
	for i := range data {
		reg := spiW0Reg + uint32(i/4)*4
		shift := uint(8 * (i % 4))
		s.regs[reg] &^= 0xFF << shift
		s.regs[reg] |= uint32(data[i]) << shift
	}
}

func (s *C3SPI1) wBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		reg := spiW0Reg + uint32(i/4)*4
		shift := uint(8 * (i % 4))
		out[i] = byte(s.regs[reg] >> shift)
	}
	return out
}

// readFlash 从 flash 读 n 字节放进 W0..W15。
func (s *C3SPI1) readFlash(addr uint32, n int) {
	if n <= 0 {
		return
	}
	for i := 0; i < 16; i++ {
		s.regs[spiW0Reg+uint32(i)*4] = 0
	}
	buf := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		buf = append(buf, byte(s.bus.Read(s.flashBase+(addr+uint32(i))&0x3FFFFF, 1)))
	}
	s.setWBytes(buf)
}

// programPage 把 W0..W15 中的数据写进 flash；长度取 MOSI_DLEN。
func (s *C3SPI1) programPage(addr uint32) {
	n := lenBits(s.regs[spiMosiDlenReg])
	if n <= 0 {
		return
	}
	data := s.wBytes(n)
	for i := 0; i < n; i++ {
		s.bus.Write(s.flashBase+(addr+uint32(i))&0x3FFFFF, 1, uint32(data[i]))
	}
}

// eraseSector 把 flash 中 4 KiB 扇区（按地址对齐）填 0xFF，模拟 NOR 擦除。
func (s *C3SPI1) eraseSector(flashAddr uint32) {
	s.eraseBlock(flashAddr&^0xFFF, 0x1000)
}

func (s *C3SPI1) eraseBlock(flashAddr uint32, size uint32) {
	base := flashAddr &^ (size - 1)
	for i := uint32(0); i < size; i++ {
		s.bus.Write(s.flashBase+(base+i)&0x3FFFFF, 1, 0xFF)
	}
}
