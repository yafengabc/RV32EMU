package cpu_test

import (
	"testing"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
)

// 紧循环：计数器从 0 加到 limit，每次 3 条指令（addi/addi?）。
// 用它测裸解释器吞吐（MIPS）。
func buildLoopCPU(limit uint32) *cpu.CPU {
	bus := mem.NewSystemBus()
	c := cpu.New(bus)
	c.SetPC(mem.ROMBase)

	// 地址布局：
	// 0x40000000: addi t0(x5), x0, 0        ; cnt=0
	// 0x40000004: addi t1(x6), x0, LIMIT     ; limit
	// 0x40000008: addi t0, t0, 1             ; loop: cnt++
	// 0x4000000C: blt t0, t1, -8             ; if cnt<limit goto loop (pc-relative)
	// 0x40000010: ebreak
	w := []uint32{
		0x00000293,                       // addi x5,x0,0
		0x00000313 | (limit << 20),       // addi x6,x0,limit
		0x00128293,                       // addi x5,x5,1
		0xFE62CCE3,                       // blt x5,x6, -8 (回跳 loop)
		0x00100073,                       // ebreak
	}
	off := uint32(0)
	for _, x := range w {
		bus.Write(mem.ROMBase+off, 4, x)
		off += 4
	}
	return c
}

// BenchmarkALULoop 测纯 ALU 紧循环吞吐。
func BenchmarkALULoop(b *testing.B) {
	// 跑 200 万次循环迭代 ×3 条指令 ≈ 600 万指令
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c2 := buildLoopCPU(2_000_000)
		for c2.Step() {
		}
	}
	b.StopTimer()
}
