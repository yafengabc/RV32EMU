// genfib: 生成递归 fib(n) 固件。sp 指向 SRAM 顶部，调用 fib(n)，结果在 a0 后 ebreak。
//
// 默认输出给 rv32emu（地址按 ESP32-C3 布局，0x40000000 起执行、栈在 0x3FCFF000）：
//
//	go run ./tools/genfib                 -> fib.elf
//
// QEMU virt（DRAM 0x80000000、16550 UART 0x10000000、SiFive test 0x100000）
// 用参数重定位，并换成「打印 a0 十六进制 + 关机」的收尾：
//
//	go run ./tools/genfib -base 0x80000000 -stack 0x80100000 -exit uart -out build/qemu
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var text []uint32

func emit(w uint32) { text = append(text, w) }

// 编码助手
func iType(rd, rs1 int, imm int32, f3 uint32, op uint32) uint32 {
	return (uint32(imm&0xFFF) << 20) | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | op
}
func rType(rd, rs1, rs2 int, f3, f7 uint32) uint32 {
	return f7<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | 0x33
}
func sType(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0xFFF)
	return (u&0xFE0)<<20 | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | (u&0x1F)<<7 | 0x23
}
func bType(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0x1FFF)
	return ((u>>12)&1)<<31 | // imm[12]
		((u>>11)&1)<<7 | // imm[11]
		((u>>5)&0x3F)<<25 | // imm[10:5]
		uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 |
		((u>>1)&0xF)<<8 | // imm[4:1]
		0x63
}
func uType(rd int, imm20 uint32) uint32 { return imm20<<12 | uint32(rd)<<7 | 0x37 }
func jType(rd int, imm int32) uint32 {
	u := uint32(imm & 0x1FFFFF)
	return ((u>>20)&1)<<31 |
		((u>>1)&0x3FF)<<21 |
		((u>>11)&1)<<20 |
		((u>>12)&0xFF)<<12 | uint32(rd)<<7 | 0x6F
}

// ---- label patching for the QEMU epilogue -------------------------------

type fixup struct {
	at   int
	name string
	jump bool
	rs1  int
	rs2  int
	f3   uint32
}

var (
	labels map[string]int
	fixups []fixup
)

func label(name string) { labels[name] = len(text) * 4 }

func here() int { return len(text) * 4 }

func br(rs1, rs2 int, f3 uint32, target string) {
	fixups = append(fixups, fixup{at: len(text), name: target, rs1: rs1, rs2: rs2, f3: f3})
	emit(0)
}

func jp(target string) {
	fixups = append(fixups, fixup{at: len(text), name: target, jump: true})
	emit(0)
}

func resolve() {
	for _, f := range fixups {
		off := int32(labels[f.name] - f.at*4)
		if f.jump {
			text[f.at] = jType(0, off)
		} else {
			text[f.at] = bType(f.rs1, f.rs2, off, f.f3)
		}
	}
	fixups = fixups[:0]
	labels = map[string]int{}
}

var epilogueJmp = -1

var (
	outPath  = flag.String("out", "fib.elf", "output ELF (a directory means fib.elf inside it)")
	base     = flag.Uint("base", 0x40000000, "load/entry base")
	stackTop = flag.Uint64("stack", 0x3FCFF000, "initial sp (must be 4 KiB aligned)")
	nArg     = flag.Int("n", 28, "fib argument")
	exitMode = flag.String("exit", "halt", "halt: ebreak; uart: print a0 + power off (QEMU virt); spin: print a0 + spin (ESP32-C3 QEMU)")
	uartPage = flag.Uint("uart", 0x10000, "UART base page (0x10000 = 16550 on virt, 0x60000 = ESP32-C3 UART0)")
)

// exitStub emits the two words at the end of the program body: ebreak+dead
// loop for rv32emu, or a jump over a dead loop into the epilogue for QEMU.
// Keeping it two words means the fib body stays at offset 20 in both modes.
func exitStub() {
	if *exitMode == "halt" {
		emit(0x00100073) // ebreak
		emit(jType(0, -4))
		return
	}
	epilogueJmp = len(text)
	emit(jType(0, 0))  // patched by epilogue()
	emit(jType(0, -4)) // dead loop, never reached
}

// epilogue appends the QEMU exit path: print a0 as 8 hex digits on the 16550
// UART at 0x10000000, then write 0x5555 to the SiFive test device at 0x100000
// to make qemu-system-riscv32 exit(0). Each byte waits for LSR.THRE first.
func epilogue() {
	if epilogueJmp < 0 {
		return
	}
	epi := uint32(len(text) * 4)
	text[epilogueJmp] = jType(0, int32(epi-uint32(epilogueJmp*4)))
	epilogueJmp = -1

	emit(uType(5, uint32(*uartPage)))
	emit(iType(6, 0, 8, 0, 0x13))
	label("epiLoop")
	if *exitMode == "uart" {
		// 16550: wait for LSR.THRE; the ESP32-C3 UART has no LSR at offset 5.
		label("epiWait")
		emit(iType(28, 5, 5, 4, 0x03))
		emit(iType(28, 28, 0x20, 7, 0x13))
		br(28, 0, 0, "epiWait")
	}
	emit(iType(28, 10, 28, 5, 0x13)) // SRLI is I-type (opcode 0x13)
	emit(iType(28, 28, 0xF, 7, 0x13))
	emit(iType(29, 0, 10, 0, 0x13))
	br(28, 29, 4, "epiDigit")
	emit(iType(28, 28, 0x37, 0, 0x13))
	jp("epiEmit")
	label("epiDigit")
	emit(iType(28, 28, 0x30, 0, 0x13))
	label("epiEmit")
	emit(sType(5, 28, 0, 0))
	emit(iType(10, 10, 4, 1, 0x13))
	emit(iType(6, 6, -1, 0, 0x13))
	br(6, 0, 1, "epiLoop")
	if *exitMode == "uart" {
		label("epiWaitNl")
		emit(iType(28, 5, 5, 4, 0x03))
		emit(iType(28, 28, 0x20, 7, 0x13))
		br(28, 0, 0, "epiWaitNl")
	}
	emit(iType(28, 0, 10, 0, 0x13))
	emit(sType(5, 28, 0, 0))
	if *exitMode == "uart" {
		emit(uType(5, 0x100)) // SiFive test device
		emit(uType(28, 0x5))
		emit(iType(28, 28, 0x555, 0, 0x13))
		emit(sType(5, 28, 0, 2))
	}
	resolve()
	emit(jType(0, 0)) // self-loop
}

func main() {
	flag.Parse()
	labels = map[string]int{}
	const (
		sp = 2
		ra = 1
		a0 = 10
		s0 = 9
		t0 = 5
		t1 = 6
	)
	n := int32(*nArg)

	// 0: lui sp, top of SRAM
	emit(uType(sp, uint32(*stackTop>>12)))
	// 4: addi a0, x0, n
	emit(iType(a0, 0, n, 0, 0x13))
	// 8: jal ra, fib   (fib 在字节 20)
	emit(jType(ra, 20-8))
	// 12/16: exit sequence (ebreak+dead loop, or jump to the UART epilogue)
	exitStub()

	// fib @20
	emit(iType(sp, sp, -16, 0, 0x13)) // 20: addi sp,sp,-16
	emit(sType(sp, ra, 0, 2))         // 24: sw ra,0(sp)
	emit(sType(sp, s0, 4, 2))         // 28: sw s0,4(sp)
	emit(iType(s0, a0, 0, 0, 0x13))   // 32: addi s0,a0,0
	emit(iType(t0, 0, 2, 0, 0x13))    // 36: addi t0,x0,2
	// 40: blt a0,t0, base   base 在后面
	var bltOff int32 = 0
	bltIdx := len(text)
	emit(bType(a0, t0, bltOff, 4)) // 40
	// 44: addi a0,a0,-1
	emit(iType(a0, a0, -1, 0, 0x13))
	// 48: jal ra, fib   (从 48 到 fib@20)
	emit(jType(ra, 20-48))
	// 52: sw a0,8(sp)
	emit(sType(sp, a0, 8, 2))
	// 56: addi a0,s0,-2
	emit(iType(a0, s0, -2, 0, 0x13))
	// 60: jal ra, fib
	emit(jType(ra, 20-60))
	// 64: addi t1,a0,0
	emit(iType(t1, a0, 0, 0, 0x13))
	// 68: lw t0,8(sp)
	emit(iType(t0, sp, 8, 2, 0x03))
	// 72: add a0,t0,t1
	emit(rType(a0, t0, t1, 0, 0))
	// 76: j ret   (jal x0, ret@84)
	emit(jType(0, 84-76))
	// base @80: addi a0,s0,0
	emit(iType(a0, s0, 0, 0, 0x13))
	// ret @84: lw ra,0(sp)
	emit(iType(ra, sp, 0, 2, 0x03))
	// 88: lw s0,4(sp)
	emit(iType(s0, sp, 4, 2, 0x03))
	// 92: addi sp,sp,16
	emit(iType(sp, sp, 16, 0, 0x13))
	// 96: ret = jalr x0,ra,0
	emit(iType(0, ra, 0, 0, 0x67))

	// 回填 blt offset：从 40 到 base@80
	bltOff = int32(80 - 40)
	text[bltIdx] = bType(a0, t0, bltOff, 4)

	// QEMU 模式下把「打印结果 + 关机」的尾段接在 fib 之后
	epilogue()

	// 打包 ELF（ET_EXEC, entry = -base）
	entry := uint32(*base)
	textBytes := make([]byte, 0, len(text)*4)
	for _, w := range text {
		textBytes = binary.LittleEndian.AppendUint32(textBytes, w)
	}
	buf := make([]byte, 0, 52+32+len(textBytes))
	w16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); buf = append(buf, b[:]...) }
	buf = append(buf, []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}...)
	w16(2)
	w16(243)
	var b4 [4]byte
	put32 := func(v uint32) { binary.LittleEndian.PutUint32(b4[:], v); buf = append(buf, b4[:]...) }
	put32(1)
	put32(entry)
	put32(52)
	put32(0)
	put32(0)
	w16(52)
	w16(32)
	w16(1)
	w16(0)
	w16(0)
	w16(0)
	put32(1) // PT_LOAD
	put32(84)
	put32(entry)
	put32(entry)
	put32(uint32(len(textBytes)))
	put32(uint32(len(textBytes)))
	put32(7)
	put32(4)
	buf = append(buf, textBytes...)

	out := *outPath
	if strings.HasSuffix(out, string(os.PathSeparator)) || isDir(out) {
		_ = os.MkdirAll(out, 0o755)
		out = filepath.Join(out, "fib.elf")
	}
	if d := filepath.Dir(out); d != "" && d != "." {
		_ = os.MkdirAll(d, 0o755)
	}
	if err := os.WriteFile(out, buf, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genfib:", err)
		os.Exit(1)
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
