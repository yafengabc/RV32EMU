package main

import (
	"flag"
	"fmt"
	"os"

	"rv32emu/internal/cpu"
	"rv32emu/internal/debug"
	"rv32emu/internal/elf"
	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
	"rv32emu/internal/rom"
)

// TinyOS C3 构建产物的默认位置（可用 -elf / -ramdisk 覆盖）。
const (
	defaultELF     = `C:\Users\SONG\Documents\Projects\TinyOS\build\kernel-esp32c3.elf`
	defaultRamdisk = `C:\Users\SONG\Documents\Projects\TinyOS\build\ramdisk-esp32c3.bin`
	flashOffset    = 0x100000 // littlefs 分区在 flash 偏移 0x100000
	flashSize      = 0x400000 // 4 MiB，与芯片一致
)

var (
	elfPath   = flag.String("elf", defaultELF, "ELF firmware to load (ESP-IDF .elf)")
	binPath   = flag.String("bin", "", "raw .bin to load")
	entry     = flag.Uint("entry", 0x40000000, "entry/load base when using -bin")
	ramdisk   = flag.String("ramdisk", defaultRamdisk, "littlefs ramdisk image (flash partition)")
	max       = flag.Uint64("max", 0, "max instructions (0=until halt)")
	trace     = flag.Bool("trace", false, "print each executed instruction (pc + disasm)")
	traceFrom = flag.Uint64("tracefrom", 0, "only trace after this instruction index")
	traceTo   = flag.Uint64("traceto", 0, "stop tracing at this instruction index (0=no limit)")
	fault     = flag.Bool("fault", false, "stop at first trap and dump the last 48 instructions")
	watch     = flag.Int("watch", -1, "watch a register index: print each change (pc, old->new)")
	traceIrq  = flag.Int("traceirq", -1, "arm tracing at this interrupt number (1-based)")
	traceIrqN = flag.Int("traceirqn", 200, "how many instructions to trace once armed")
	rxRate    = flag.Int("rxrate", 16, "console input bytes released per feed (0 = no throttling)")
	rxPeriod  = flag.Uint64("rxperiod", 10000, "instructions between console input feeds (10000 = one timer tick)")
	rxDelay   = flag.Uint64("rxdelay", 0, "hold console input for the first N instructions (scripting: 3000000 ≈ after boot)")
	noPMP     = flag.Bool("nopmp", false, "disable PMP enforcement for user mode (A/B debugging)")
	trapLog   = flag.Bool("traplog", false, "log every synchronous trap (cause, mepc, mtval)")
	breakPC   = flag.Uint64("breakpc", 0, "stop when pc equals this address (0 = off)")
	tickDiv   = flag.Uint("tickdiv", 1, "stretch the timer period by this factor (timing-sensitivity tests)")
	step      = flag.Int("step", 0, "execute N instructions then pause & dump regs")
	dumpRAM   = flag.String("dumpram", "", "write kernel RAM image (0x40380000..0x403AC000) to this file at exit")
	ctlMode   = flag.Bool("ctl", false, "control mode: JSON Lines protocol on stdin/stdout (for GUI frontends)")
)

func main() {
	flag.Parse()

	ramdiskImg, err := os.ReadFile(*ramdisk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load ramdisk: %v\n", err)
		os.Exit(1)
	}

	// flash 是复位后唯一留下来的东西（真机上 SPI flash 的字节不会因为
	// 复位而消失，`wfile` 写进去的文件重启后还在），所以在机器之外持有。
	flash := make([]byte, flashSize)
	copy(flash[flashOffset:], ramdiskImg)

	// 控制模式：stdout 是协议通道，人类日志只走 stderr，所以这里不能打印。
	// ELF/内存初始化等公共代码直接 fmt.Printf 到 os.Stdout，先把 os.Stdout
	// 指到 stderr，再把真正的 stdout 留给协议写入。
	if *ctlMode {
		proto := os.Stdout
		os.Stdout = os.Stderr
		runCtl(proto, flash)
		return
	}

	// UART 同理活过一次复位：它建模的是主机那一端（stdin 搬运协程、
	// 尚未送进 FIFO 的字节）。芯片侧的 FIFO 在 machine 复位时清空。
	uart := perip.NewUART0()

	fmt.Printf("[flash] ramdisk %d bytes @0x%08X\n", len(ramdiskImg), perip.FlashEmuBase+flashOffset)
	for boot := 1; ; boot++ {
		m := newMachine(flash, uart)
		if !m.run() {
			break
		}
		flash = m.flashSnapshot()
		fmt.Printf("=== rebooting (boot #%d) ===\n", boot+1)
	}
}

// machine 是一台「通电」的 C3：总线、CPU、外设。复位 = 重装一台新的，
// 只有 flash 内容和主机侧的控制台输入从上一次带过来。
type machine struct {
	bus   *mem.SystemBus
	cpu   *cpu.CPU
	uart  *perip.UART
	rtc   *perip.RTC
	timer *perip.C3Timer
	gpio  *perip.GPIO
	adc   *perip.ADC
}

func newMachine(flash []byte, uart *perip.UART) *machine {
	bus := mem.NewSystemBus()
	gpio := perip.NewGPIO()
	usj := perip.NewUSJ()
	rtc := perip.NewRTC()
	adc := perip.NewADC()
	bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
	bus.Attach(mem.DeviceRange{Base: perip.GPIOBase, Size: perip.GPIOSize, Dev: gpio})
	bus.Attach(mem.DeviceRange{Base: perip.USJBase, Size: perip.USJSize, Dev: usj})
	bus.Attach(mem.DeviceRange{Base: perip.RTCBase, Size: perip.RTCSize, Dev: rtc})
	bus.Attach(mem.DeviceRange{Base: perip.ADCBase, Size: perip.ADCSize, Dev: adc})

	// flash 模拟区：ROM 桩把 (FlashEmuBase + flash_addr) 当物理 flash。
	bus.AddRAM(perip.FlashEmuBase, flashSize)
	bus.CopyInto(perip.FlashEmuBase, 0, flash)

	// ROM 桩：实现 esp_rom_spiflash_read/write/config_param。
	bus.LoadROM(0, rom.BuildROM())

	// C3 私有外设：TIMG0 定时器 + INTMTX + SPI1（硬件擦除）。
	c3timer := perip.NewC3Timer(nil) // cpu 在 New 后补设
	c3intc := perip.NewC3Intc()
	c3spi1 := perip.NewC3SPI1(bus, perip.FlashEmuBase)
	bus.Attach(mem.DeviceRange{Base: perip.TIMG0Base, Size: perip.TIMG0Size, Dev: c3timer})
	bus.Attach(mem.DeviceRange{Base: perip.INTMTXBase, Size: perip.INTMTXSize, Dev: c3intc})
	bus.Attach(mem.DeviceRange{Base: perip.SPI1Base, Size: perip.SPI1Size, Dev: c3spi1})

	c := cpu.New(bus)
	if *noPMP {
		c.SetPMPEnforce(false)
	}
	if *trapLog {
		c.OnTrap = func(cause, epc, tval uint32) {
			// ecall 是正常系统调用路径，只关心真正的故障。
			if cause&0x80000000 != 0 || cause == 8 || cause == 11 {
				return
			}
			fmt.Printf("[trap] cause=%d mepc=0x%08X mtval=0x%08X\n", cause, epc, tval)
		}
	}
	c3timer.SetCPU(c)
	c3timer.SetDiv(uint32(*tickDiv))

	if *elfPath != "" {
		e, err := elf.Load(*elfPath, bus)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load elf:", err)
			os.Exit(1)
		}
		c.SetPC(e)
	} else if *binPath != "" {
		if err := elf.LoadBin(*binPath, uint32(*entry), bus); err != nil {
			fmt.Fprintln(os.Stderr, "load bin:", err)
			os.Exit(1)
		}
		c.SetPC(uint32(*entry))
	} else {
		fmt.Fprintln(os.Stderr, "need -elf or -bin")
		flag.Usage()
		os.Exit(2)
	}

	return &machine{bus: bus, cpu: c, uart: uart, rtc: rtc, timer: c3timer, gpio: gpio, adc: adc}
}

// flashSnapshot 取回 flash 模拟区的当前内容，供复位后灌进新总线。
func (m *machine) flashSnapshot() []byte {
	buf := make([]byte, flashSize)
	m.bus.CopyOut(perip.FlashEmuBase, buf)
	return buf
}

// run 执行到停机、指令上限或一次软件复位。返回 true 表示内核请求了复位
// （RTC_CNTL SW_SYS_RST），调用方应当重装机器。
func (m *machine) run() bool {
	c := m.cpu

	limit := *max
	if *step > 0 {
		limit = uint64(*step)
	}

	// bulk：没有任何逐条插桩时，用 RunHot 批量执行（快路径）。
	// trace/watch/fault/breakpc/traceirq 都依赖每条指令前的 PC 与状态，只能单步。
	bulk := !*trace && *traceFrom == 0 && *step == 0 && *watch < 0 &&
		!*fault && *breakPC == 0 && *traceIrq < 0
	const hotBatch = 1000 // 与默认 tick(rxperiod) 同量级，减少批边界相对误差

	fmt.Printf("=== RV32EMU start pc=0x%08X ===\n", c.PC())
	n := uint64(0)
	type ringEnt struct {
		n  uint64
		pc uint32
	}
	ring := make([]ringEnt, 48)
	irqBudget := 0
	irqArmed := false
	for !c.Halted() && (limit == 0 || n < limit) {
		// -traceirq：以 PC 进入 trap 向量为触发条件（中断投递瞬间），
		// 追踪接下来 N 条指令，观察 向量 → trap_entry → trap_handler
		// → plat_timer_tick 的真实路径。
		if *traceIrq >= 0 && !irqArmed {
			vec := c.CSR().Mtvec() &^ 3
			// 中断向量的第一条（j trap_entry）与投递在同一条 Step 里执行完，
			// 所以这里看到 pc 已落在 vec+0x80（trap_entry）附近；同步异常
			// 则在 Step 末尾才改 pc，看到的是 vec 本身。两种都覆盖。
			if p := c.PC(); p >= vec && p < vec+0x100 &&
				c.CSR().Mcause()&0x80000000 != 0 {
				irqBudget = *traceIrqN
				irqArmed = true
				fmt.Printf("--- trap vector entry at inst %d, pc=0x%08X, mcause=0x%08X, irq#%d ---\n",
					n, p, c.CSR().Mcause(), c.IntCount())
			}
		}
		if *trace || (*traceFrom > 0 && n >= *traceFrom && (*traceTo == 0 || n < *traceTo)) {
			fmt.Printf("[%6d] pc=0x%08X  %s\n", n, c.PC(), disasmAt(c, m.bus))
		} else if irqBudget > 0 {
			fmt.Printf("[%6d] pc=0x%08X  %s\n", n, c.PC(), disasmAt(c, m.bus))
			irqBudget--
		}
		if *fault {
			ring[n%48] = ringEnt{n, c.PC()}
		}
		if *breakPC != 0 && uint64(c.PC()) == *breakPC {
			fmt.Printf("\n=== break at pc=0x%08X (inst %d) ===\n", c.PC(), n)
			fmt.Print(c.Dump())
			m.writeRAMDump()
			return false
		}
		excBefore := c.ExcCount()
		var ran uint64
		switch {
		case bulk:
			b := uint64(hotBatch)
			if limit != 0 && limit-n < b {
				b = limit - n
			}
			ran = c.RunHot(b)
		case *watch >= 0:
			oldv := c.Reg(*watch)
			oldpc := c.PC()
			c.Step()
			if c.Reg(*watch) != oldv {
				fmt.Printf("[%6d] pc=0x%08X  x%d: 0x%08X -> 0x%08X  %s\n",
					n, oldpc, *watch, oldv, c.Reg(*watch), disasmAtPC(oldpc, m.bus))
			}
			ran = 1
		default:
			c.Step()
			ran = 1
		}
		if ran == 0 {
			break
		}
		m.timer.TickN(ran)
		m.rtc.Tick(ran)
		// 控制台输入按 tick 分批放行（见 UART.RxFeed 的注释）。
		// -rxdelay 让脚本输入等 shell 起来再喂：内核行规程只有 8 行队
		// 列，boot 期间灌进去的预输入超过 8 行就会按设计丢行。
		// 说明：批量执行时一次补齐本批跨过的所有放行点。
		if *rxRate > 0 && n >= *rxDelay {
			p := *rxPeriod
			if p == 0 {
				p = 1
			}
			// 原本是"指令序号 n 为 p 的倍数时放行一次"；批量执行时
			// 把本批覆盖到的所有放行点一次补齐。
			for k := (n + p - 1) / p; k <= (n+ran-1)/p; k++ {
				m.uart.RxFeed(*rxRate)
			}
		}
		n += ran
		// 看门狗超时 / 软件复位：真机上都是整颗芯片复位（SRAM、外设、
		// CPU 全部重来），模拟器这边对应的做法就是重装一台机器。
		if m.timer.WDTResetPending() {
			fmt.Printf("\n=== watchdog reset after %d inst (TIMG0 TG0WDT timeout) ===\n", n)
			m.uart.Reset()
			return true
		}
		if m.rtc.WDTResetPending() {
			fmt.Printf("\n=== watchdog reset after %d inst (RTC WDT timeout) ===\n", n)
			m.uart.Reset()
			return true
		}
		// 软件复位：内核写完 RTC_CNTL 就会一直自旋等到复位生效，模拟器
		// 这边没有比「重装一台机器」更准确的做法。批量执行只影响
		// 这一步晚几条指令被发现，内核在自旋里等，无所谓。
		if m.rtc.ResetPending() {
			fmt.Printf("\n=== software reset after %d inst (RTC_CNTL SW_SYS_RST) ===\n", n)
			m.uart.Reset()
			return true
		}
		if *fault && c.ExcCount() > excBefore {
			fmt.Printf("\n=== first fault @ inst %d, mcause=0x%08X ===\n", n, c.CSR().Mcause())
			start := uint64(0)
			if n > 48 {
				start = n - 48
			}
			for k := start; k < n; k++ {
				e := ring[k%48]
				if e.pc != 0 {
					fmt.Printf("[%6d] pc=0x%08X  %s\n", e.n, e.pc, disasmAtPC(e.pc, m.bus))
				}
			}
			fmt.Print(c.Dump())
			return false
		}
	}

	fmt.Printf("\n=== stopped after %d inst, reason=%q ===\n", n, c.HaltMsg())
	fmt.Printf("=== irq: timg0 fires=%d, cpu interrupts=%d, exceptions=%d ===\n",
		m.timer.Fires(), c.IntCount(), c.ExcCount())
	m.writeRAMDump()
	if *step > 0 || c.Halted() {
		fmt.Print(c.Dump())
	}
	return false
}

// writeRAMDump 在 -dumpram 下把内核 RAM 落盘（调试用）。
func (m *machine) writeRAMDump() {
	if *dumpRAM == "" {
		return
	}
	const dumpBase, dumpLen = 0x40380000, 0x2C000
	buf := make([]byte, dumpLen)
	m.bus.CopyOut(dumpBase, buf)
	if err := os.WriteFile(*dumpRAM, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "dumpram: %v\n", err)
		return
	}
	fmt.Printf("=== ram dump %d bytes @0x%08X -> %s ===\n", dumpLen, dumpBase, *dumpRAM)
}

// disasmAt 读当前 pc 处指令并反汇编。
func disasmAt(c *cpu.CPU, bus mem.Bus) string {
	return disasmAtPC(c.PC(), bus)
}

// disasmAtPC 读 pc 处指令并反汇编。
func disasmAtPC(p uint32, bus mem.Bus) string {
	low := bus.Read(p, 2)
	if low&0x3 != 0x3 {
		return debug.DisasmC(uint16(low))
	}
	hi := bus.Read(p+2, 2)
	return debug.DisasmI(p, low|hi<<16)
}
