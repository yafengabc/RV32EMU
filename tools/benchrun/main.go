// benchrun measures pure emulation throughput for the Go emulator.
//
// It uses the same batch loop as main.go (RunHot(1000) + TIMG0 TickN) but
// excludes ELF loading, process startup and console I/O from the timed region,
// so the number is directly comparable with rv32emu-c -bench.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"rv32emu/internal/cpu"
	"rv32emu/internal/elf"
	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
	"rv32emu/internal/rom"
)

func main() {
	elfPath := flag.String("elf", "", "ELF firmware")
	ramdisk := flag.String("ramdisk", "", "littlefs image (optional)")
	max := flag.Uint64("max", 0, "instruction budget per run (0=until halt)")
	reps := flag.Int("reps", 5, "repetitions")
	flag.Parse()
	if *elfPath == "" {
		fmt.Fprintln(os.Stderr, "need -elf")
		os.Exit(2)
	}

	flash := make([]byte, 0x400000)
	if *ramdisk != "" {
		img, err := os.ReadFile(*ramdisk)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ramdisk:", err)
			os.Exit(1)
		}
		copy(flash[0x100000:], img)
	}

	run := func() (uint64, float64) {
		bus := mem.NewSystemBus()
		uart := perip.NewUART0()
		gpio := perip.NewGPIO()
		usj := perip.NewUSJ()
		rtc := perip.NewRTC()
		bus.Attach(mem.DeviceRange{Base: perip.UART0Base, Size: perip.UARTSize, Dev: uart})
		bus.Attach(mem.DeviceRange{Base: perip.GPIOBase, Size: perip.GPIOSize, Dev: gpio})
		bus.Attach(mem.DeviceRange{Base: perip.USJBase, Size: perip.USJSize, Dev: usj})
		bus.Attach(mem.DeviceRange{Base: perip.RTCBase, Size: perip.RTCSize, Dev: rtc})
		bus.AddRAM(perip.FlashEmuBase, 0x400000)
		bus.CopyInto(perip.FlashEmuBase, 0, flash)
		bus.LoadROM(0, rom.BuildROM())

		timer := perip.NewC3Timer(nil)
		intc := perip.NewC3Intc()
		spi1 := perip.NewC3SPI1(bus, perip.FlashEmuBase)
		bus.Attach(mem.DeviceRange{Base: perip.TIMG0Base, Size: perip.TIMG0Size, Dev: timer})
		bus.Attach(mem.DeviceRange{Base: perip.INTMTXBase, Size: perip.INTMTXSize, Dev: intc})
		bus.Attach(mem.DeviceRange{Base: perip.SPI1Base, Size: perip.SPI1Size, Dev: spi1})

		c := cpu.New(bus)
		timer.SetCPU(c)
		entry, err := elf.Load(*elfPath, bus)
		if err != nil {
			fmt.Fprintln(os.Stderr, "elf:", err)
			os.Exit(1)
		}
		c.SetPC(entry)

		start := time.Now()
		var n uint64
		for !c.Halted() && (*max == 0 || n < *max) {
			b := uint64(1000)
			if *max != 0 && *max-n < b {
				b = *max - n
			}
			ran := c.RunHot(b)
			if ran == 0 {
				break
			}
			timer.TickN(ran)
			n += ran
			if rtc.ResetPending() {
				break
			}
		}
		dt := time.Since(start).Seconds()
		return n, dt
	}

	times := make([]float64, 0, *reps)
	var inst uint64
	for i := 0; i < *reps; i++ {
		var dt float64
		inst, dt = run()
		times = append(times, dt)
		fmt.Printf("[bench] rep=%d sec=%.6f mips=%.2f\n", i+1, dt, float64(inst)/dt/1e6)
	}
	sort.Float64s(times)
	med := times[len(times)/2]
	fmt.Printf("[bench] median sec=%.6f inst=%d mips=%.2f\n", med, inst, float64(inst)/med/1e6)
}
