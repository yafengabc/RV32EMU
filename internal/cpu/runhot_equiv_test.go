package cpu

import (
	"testing"

	"rv32emu/internal/mem"
)

// TestRunHotMatchesStep 差分测试：同一段程序分别用 Step 逐条执行与
// RunHot 批量执行，最终的寄存器、pc 与 SRAM 内容必须逐字一致。
// 覆盖算术/访存/分支/M 扩展，以及压缩指令（C.LI/C.ADDI/C.BNEZ）。
func TestRunHotMatchesStep(t *testing.T) {
	code := words(
		encU(0x3FC80, 8, 0x37),          // lui  x8, 0x3FC80
		encI(0, 0, 0, 9, 0x13),          // addi x9, x0, 0
		encI(32, 0, 0, 10, 0x13),        // addi x10, x0, 32
		encI(0, 8, 2, 11, 0x03),         // lw   x11, 0(x8)
		encR(0, 11, 9, 0, 9, 0x33),      // add  x9, x9, x11
		encS(0, 9, 8, 2, 0x23),          // sw   x9, 0(x8)
		encI(4, 8, 0, 8, 0x13),          // addi x8, x8, 4
		encI(-1, 10, 0, 10, 0x13),       // addi x10, x10, -1
		encB(-20, 0, 10, 1, 0x63),       // bne  x10, x0, loop
		encR(1, 9, 9, 0, 12, 0x33),      // mul  x12, x9, x9
		encR(1, 9, 12, 5, 13, 0x33),     // divu x13, x12, x9
		encI(3, 12, 1, 15, 0x13),        // slli x15, x12, 3
		encR(0x20, 13, 15, 0, 16, 0x33), // sub  x16, x15, x13
	)
	code = append(code,
		0x15, 0x47, // c.li x14, 5
		0x7D, 0x17, // c.addi x14, -1
		0x7D, 0xF9, // c.bnez x14, -2
		0x73, 0x00, 0x10, 0x00, // ebreak
	)

	run := func(bulk bool) (*CPU, *mem.SystemBus) {
		bus := mem.NewSystemBus()
		c := New(bus)
		for i := uint32(0); i < 32; i++ {
			bus.Write(mem.SRAMBase+i*4, 4, i*3+1)
		}
		bus.LoadROM(0, code)
		c.SetPC(mem.ROMBase)
		c.SetReg(2, mem.SRAMBase+0x200)
		c.SetReg(8, mem.SRAMBase)
		if bulk {
			for !c.Halted() {
				if c.RunHot(1000) == 0 {
					break
				}
			}
		} else {
			for c.Step() {
			}
		}
		return c, bus
	}

	a, abus := run(false)
	b, bbus := run(true)

	for i := 1; i < 32; i++ {
		if a.Reg(i) != b.Reg(i) {
			t.Fatalf("x%d: Step=0x%08X RunHot=0x%08X", i, a.Reg(i), b.Reg(i))
		}
	}
	if a.PC() != b.PC() {
		t.Fatalf("pc: Step=0x%08X RunHot=0x%08X", a.PC(), b.PC())
	}
	if a.Halted() != b.Halted() || a.HaltMsg() != b.HaltMsg() {
		t.Fatalf("halt: Step=(%v,%q) RunHot=(%v,%q)",
			a.Halted(), a.HaltMsg(), b.Halted(), b.HaltMsg())
	}
	for off := uint32(0); off < 0x100; off += 4 {
		if va, vb := abus.Read(mem.SRAMBase+off, 4), bbus.Read(mem.SRAMBase+off, 4); va != vb {
			t.Fatalf("SRAM+0x%02X: Step=0x%08X RunHot=0x%08X", off, va, vb)
		}
	}
}
