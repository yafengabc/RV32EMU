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
)

func main() {
	elfPath := flag.String("elf", "", "ELF firmware to load (ESP-IDF .elf)")
	binPath := flag.String("bin", "", "raw .bin to load")
	entry := flag.Uint("entry", 0x40000000, "entry/load base when using -bin")
	max := flag.Uint64("max", 0, "max instructions (0=until halt)")
	trace := flag.Bool("trace", false, "print each executed instruction (pc + disasm)")
	step := flag.Int("step", 0, "execute N instructions then pause & dump regs")
	flag.Parse()

	bus := mem.NewSystemBus()
	uart := perip.NewUART0()
	gpio := perip.NewGPIO()
	clint := perip.NewCLINT()
	bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
	bus.Attach(mem.DeviceRange{Base: perip.GPIOBase, Size: perip.GPIOSize, Dev: gpio})
	bus.Attach(mem.DeviceRange{Base: perip.CLINTBase, Size: perip.CLINTSize, Dev: clint})

	c := cpu.New(bus)

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
	for !c.Halted() && (limit == 0 || n < limit) {
		if *trace {
			fmt.Printf("[%6d] pc=0x%08X  %s\n", n, c.PC(), disasmAt(c, bus))
		}
		c.Step()
		clint.Tick()
		c.SetTimerPending(clint.MTIP())
		n++
	}

	fmt.Printf("\n=== stopped after %d inst, reason=%q ===\n", n, c.HaltMsg())
	if *step > 0 || c.Halted() {
		fmt.Print(c.Dump())
	}
}

// disasmAt 读当前 pc 处指令并反汇编。
func disasmAt(c *cpu.CPU, bus mem.Bus) string {
	p := c.PC()
	low := bus.Read(p, 2)
	if low&0x3 != 0x3 {
		return debug.DisasmC(uint16(low))
	}
	hi := bus.Read(p+2, 2)
	return debug.DisasmI(p, low|hi<<16)
}
