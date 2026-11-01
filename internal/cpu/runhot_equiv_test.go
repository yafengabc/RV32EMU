package cpu

import (
	"math/rand"
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

// TestBlockDecodeMatchesStep 随机差分测试：对大量随机编码的单条指令，
// 各自用 Step 与 RunHot(max=1) 执行，比较寄存器、pc、停机状态与 trap
// 现场。它专门覆盖基本块解码与 execI/execC 的位域解释是否一致。
func TestBlockDecodeMatchesStep(t *testing.T) {
	rng := rand.New(rand.NewSource(20261102))
	opcodes := []uint32{0x13, 0x33, 0x63, 0x03, 0x23, 0x37, 0x17, 0x6f, 0x67, 0x0f, 0x73}

	for it := 0; it < 30000; it++ {
		var code []byte
		if rng.Intn(3) == 0 {
			c16 := uint16(rng.Uint32()) &^ 0x3                   // 随机 16-bit 压缩指令
			code = []byte{byte(c16), byte(c16 >> 8), 0x02, 0x90} // 后面跟 C.EBREAK
		} else {
			instr := (rng.Uint32() &^ 0x7f) | opcodes[rng.Intn(len(opcodes))]
			code = append(words(instr), 0x73, 0x00, 0x10, 0x00) // 后面跟 EBREAK
		}

		var regs [32]uint32
		for i := 1; i < 32; i++ {
			regs[i] = rng.Uint32()
		}
		rams := make([]uint32, 64)
		for i := range rams {
			rams[i] = rng.Uint32()
		}

		mk := func() (*CPU, *mem.SystemBus) {
			bus := mem.NewSystemBus()
			for i, v := range rams {
				bus.Write(mem.SRAMBase+uint32(i)*4, 4, v)
			}
			bus.LoadROM(0, code)
			c := New(bus)
			for i := 1; i < 32; i++ {
				c.SetReg(i, regs[i])
			}
			c.SetPC(mem.ROMBase)
			return c, bus
		}

		a, _ := mk()
		b, _ := mk()
		a.Step()
		b.RunHot(1)

		for i := 1; i < 32; i++ {
			if a.Reg(i) != b.Reg(i) {
				t.Fatalf("iter %d code=% X x%d: Step=0x%08X RunHot=0x%08X",
					it, code[:4], i, a.Reg(i), b.Reg(i))
			}
		}
		if a.PC() != b.PC() || a.Halted() != b.Halted() || a.HaltMsg() != b.HaltMsg() {
			t.Fatalf("iter %d code=% X: Step(pc=0x%08X halt=%v/%q) RunHot(pc=0x%08X halt=%v/%q)",
				it, code[:4], a.PC(), a.Halted(), a.HaltMsg(),
				b.PC(), b.Halted(), b.HaltMsg())
		}
		if a.ExcCount() != b.ExcCount() {
			t.Fatalf("iter %d code=% X: exceptions Step=%d RunHot=%d",
				it, code[:4], a.ExcCount(), b.ExcCount())
		}
		for _, id := range []uint32{csrMcause, csrMepc, csrMtval} {
			va, _ := a.CSR().Read(id)
			vb, _ := b.CSR().Read(id)
			if va != vb {
				t.Fatalf("iter %d code=% X csr 0x%03X: Step=0x%08X RunHot=0x%08X",
					it, code[:4], id, va, vb)
			}
		}
	}
}

// TestBlockCacheSeesSelfModifyingCode 验证总线写代码页后，已解码块会失效。
func TestBlockCacheSeesSelfModifyingCode(t *testing.T) {
	bus := mem.NewSystemBus()
	c := New(bus)
	code := append(words(encI(1, 0, 0, 3, 0x13)), 0x73, 0x00, 0x10, 0x00) // addi x3,x0,1; ebreak
	bus.LoadROM(0, code)
	c.SetPC(mem.ROMBase)
	if n := c.RunHot(1); n != 1 || c.Reg(3) != 1 {
		t.Fatalf("first run: n=%d x3=%d", n, c.Reg(3))
	}
	// 改写同一页的第一条指令：页版本号应使旧块失效。
	bus.Write(mem.ROMBase, 4, encI(7, 0, 0, 3, 0x13))
	c.SetPC(mem.ROMBase)
	if n := c.RunHot(1); n != 1 || c.Reg(3) != 7 {
		t.Fatalf("after modify: n=%d x3=%d (stale block?)", n, c.Reg(3))
	}
}

// TestBlockCacheSelfModifyFromCPU 验证块内的 store 直写平铺区同样会让
// 该页的块失效（CPU 直写路径绕过了总线的 FastWrite）。
func TestBlockCacheSelfModifyFromCPU(t *testing.T) {
	bus := mem.NewSystemBus()
	c := New(bus)
	code := words(
		encU(0x40000, 5, 0x37),     // lui  x5, 0x40000
		encI(20, 5, 0, 5, 0x13),    // addi x5, x5, 20   -> &target(0x40000014)
		encU(0x00700, 7, 0x37),     // lui  x7, 0x00700
		encI(0x193, 7, 0, 7, 0x13), // addi x7, x7, 0x193 -> addi x3,x0,7
		encS(0, 7, 5, 2, 0x23),     // 0x40000010: sw x7, 0(x5)
		encI(1, 0, 0, 3, 0x13),     // 0x40000014 target: addi x3,x0,1
	)
	bus.LoadROM(0, code)
	// 先把 target 所在块解码并缓存。
	c.SetPC(mem.ROMBase + 20)
	if n := c.RunHot(1); n != 1 || c.Reg(3) != 1 {
		t.Fatalf("first run: n=%d x3=%d", n, c.Reg(3))
	}
	// 再执行含 store 的块，由 CPU 直写路径改写 target。
	c.SetPC(mem.ROMBase)
	if n := c.RunHot(5); n != 5 || c.Reg(3) != 1 {
		t.Fatalf("store run: n=%d x3=%d", n, c.Reg(3))
	}
	// 重新进入 target：旧块必须因页版本号变化而失效。
	c.SetPC(mem.ROMBase + 20)
	if n := c.RunHot(1); n != 1 || c.Reg(3) != 7 {
		t.Fatalf("after in-block store: n=%d x3=%d (stale block?)", n, c.Reg(3))
	}
}
