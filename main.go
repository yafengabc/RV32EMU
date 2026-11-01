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
)

func main() {
	elfPath := flag.String("elf", defaultELF, "ELF firmware to load (ESP-IDF .elf)")
	binPath := flag.String("bin", "", "raw .bin to load")
	entry := flag.Uint("entry", 0x40000000, "entry/load base when using -bin")
	ramdisk := flag.String("ramdisk", defaultRamdisk, "littlefs ramdisk image (flash partition)")
	max := flag.Uint64("max", 0, "max instructions (0=until halt)")
	trace := flag.Bool("trace", false, "print each executed instruction (pc + disasm)")
	traceFrom := flag.Uint64("tracefrom", 0, "only trace after this instruction index")
	traceTo := flag.Uint64("traceto", 0, "stop tracing at this instruction index (0=no limit)")
	fault := flag.Bool("fault", false, "stop at first trap and dump the last 48 instructions")
	watch := flag.Int("watch", -1, "watch a register index: print each change (pc, old->new)")
	traceIrq := flag.Int("traceirq", -1, "arm tracing at this interrupt number (1-based)")
	traceIrqN := flag.Int("traceirqn", 200, "how many instructions to trace once armed")
	rxRate := flag.Int("rxrate", 16, "console input bytes released per feed (0 = no throttling)")
	rxPeriod := flag.Uint64("rxperiod", 10000, "instructions between console input feeds (10000 = one timer tick)")
	rxDelay := flag.Uint64("rxdelay", 0, "hold console input for the first N instructions (scripting: 3000000 ≈ after boot)")
	noPMP := flag.Bool("nopmp", false, "disable PMP enforcement for user mode (A/B debugging)")
	trapLog := flag.Bool("traplog", false, "log every synchronous trap (cause, mepc, mtval)")
	breakPC := flag.Uint64("breakpc", 0, "stop when pc equals this address (0 = off)")
	tickDiv := flag.Uint("tickdiv", 1, "stretch the timer period by this factor (timing-sensitivity tests)")
	step := flag.Int("step", 0, "execute N instructions then pause & dump regs")
	dumpRAM := flag.String("dumpram", "", "write kernel RAM image (0x40380000..0x403AC000) to this file at exit")
	flag.Parse()

	bus := mem.NewSystemBus()
	uart := perip.NewUART0()
	gpio := perip.NewGPIO()
	usj := perip.NewUSJ()
	bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
	bus.Attach(mem.DeviceRange{Base: perip.GPIOBase, Size: perip.GPIOSize, Dev: gpio})
	bus.Attach(mem.DeviceRange{Base: perip.USJBase, Size: perip.USJSize, Dev: usj})
	bus.Attach(mem.DeviceRange{Base: perip.RTCBase, Size: perip.RTCSize, Dev: perip.NewRTC()})

	// flash 模拟区：ROM 桩把 (FlashEmuBase + flash_addr) 当物理 flash。
	flashSize := uint32(0x400000) // 4 MiB，与芯片一致
	bus.AddRAM(perip.FlashEmuBase, flashSize)
	if rd, err := os.ReadFile(*ramdisk); err != nil {
		fmt.Fprintf(os.Stderr, "load ramdisk: %v\n", err)
		os.Exit(1)
	} else {
		bus.CopyInto(perip.FlashEmuBase, flashOffset, rd)
		fmt.Printf("[flash] ramdisk %d bytes @0x%08X\n", len(rd), perip.FlashEmuBase+flashOffset)
	}

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

	limit := *max
	if *step > 0 {
		limit = uint64(*step)
	}

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
			fmt.Printf("[%6d] pc=0x%08X  %s\n", n, c.PC(), disasmAt(c, bus))
		} else if irqBudget > 0 {
			fmt.Printf("[%6d] pc=0x%08X  %s\n", n, c.PC(), disasmAt(c, bus))
			irqBudget--
		}
		if *fault {
			ring[n%48] = ringEnt{n, c.PC()}
		}
		if *breakPC != 0 && uint64(c.PC()) == *breakPC {
			fmt.Printf("\n=== break at pc=0x%08X (inst %d) ===\n", c.PC(), n)
			fmt.Print(c.Dump())
			if *dumpRAM != "" {
				buf := make([]byte, 0x2C000)
				bus.CopyOut(0x40380000, buf)
				_ = os.WriteFile(*dumpRAM, buf, 0o644)
				fmt.Printf("=== ram dump -> %s ===\n", *dumpRAM)
			}
			return
		}
		excBefore := c.ExcCount()
		if *watch >= 0 {
			oldv := c.Reg(*watch)
			oldpc := c.PC()
			c.Step()
			if c.Reg(*watch) != oldv {
				fmt.Printf("[%6d] pc=0x%08X  x%d: 0x%08X -> 0x%08X  %s\n",
					n, oldpc, *watch, oldv, c.Reg(*watch), disasmAtPC(oldpc, bus))
			}
		} else {
			c.Step()
		}
		c3timer.Tick()
		// 控制台输入按 tick 分批放行（见 UART.RxFeed 的注释）。
		// -rxdelay 让脚本输入等 shell 起来再喂：内核行规程只有 8 行队
		// 列，boot 期间灌进去的预输入超过 8 行就会按设计丢行。
		if *rxRate > 0 && n >= *rxDelay {
			p := *rxPeriod
			if p == 0 {
				p = 1
			}
			if n%p == 0 {
				uart.RxFeed(*rxRate)
			}
		}
		n++
		if *fault && c.ExcCount() > excBefore {
			fmt.Printf("\n=== first fault @ inst %d, mcause=0x%08X ===\n", n, c.CSR().Mcause())
			start := uint64(0)
			if n > 48 {
				start = n - 48
			}
			for k := start; k < n; k++ {
				e := ring[k%48]
				if e.pc != 0 {
					fmt.Printf("[%6d] pc=0x%08X  %s\n", e.n, e.pc, disasmAtPC(e.pc, bus))
				}
			}
			fmt.Print(c.Dump())
			return
		}
	}

	fmt.Printf("\n=== stopped after %d inst, reason=%q ===\n", n, c.HaltMsg())
	fmt.Printf("=== irq: timg0 fires=%d, cpu interrupts=%d, exceptions=%d ===\n",
		c3timer.Fires(), c.IntCount(), c.ExcCount())
	if *dumpRAM != "" {
		const dumpBase, dumpLen = 0x40380000, 0x2C000
		buf := make([]byte, dumpLen)
		bus.CopyOut(dumpBase, buf)
		if err := os.WriteFile(*dumpRAM, buf, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "dumpram: %v\n", err)
		} else {
			fmt.Printf("=== ram dump %d bytes @0x%08X -> %s ===\n", dumpLen, dumpBase, *dumpRAM)
		}
	}
	if *step > 0 || c.Halted() {
		fmt.Print(c.Dump())
	}
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
