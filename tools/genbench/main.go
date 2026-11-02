// genbench: 生成 RV32IMC 基准 ELF（手写，无浮点）。
//
//	bench_pi.elf     Leibniz 级数算 pi（Q16.16，N=10000）
//	bench_sort.elf   16 元素冒泡排序，重复 REPS 次
//	bench_sieve.elf  素数筛到 1000，重复 REPS 次
//
// 默认输出给 rv32emu（ESP32-C3 布局：0x40000000 执行、栈 0x3FCFF000、数据 0x3FC80000）：
//
//	go run ./tools/genbench -out build/benchdata
//
// QEMU virt 用参数重定位，并把收尾换成「UART 打印 a0 + 关机」：
//
//	go run ./tools/genbench -base 0x80000000 -stack 0x80100000 -data 0x80010000 -exit uart -out build/qemu
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

var code []uint32

func emit(w uint32) { code = append(code, w) }

func iR(rd, rs1 int, imm int32, f3, op uint32) uint32 {
	return (uint32(imm&0xFFF) << 20) | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | op
}
func rR(rd, rs1, rs2 int, f3, f7 uint32) uint32 {
	return f7<<25 | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | uint32(rd)<<7 | 0x33
}
func sR(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0xFFF)
	return ((u & 0xFE0) << 20) | uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | ((u & 0x1F) << 7) | 0x23
}
func bR(rs1, rs2 int, imm int32, f3 uint32) uint32 {
	u := uint32(imm & 0x1FFF)
	return ((u>>12)&1)<<31 | ((u>>11)&1)<<7 | ((u>>5)&0x3F)<<25 |
		uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | ((u>>1)&0xF)<<8 | 0x63
}
func uR(rd int, imm20 uint32) uint32 { return imm20<<12 | uint32(rd)<<7 | 0x37 }
func jR(rd int, imm int32) uint32 {
	u := uint32(imm & 0x1FFFFF)
	return ((u>>20)&1)<<31 | ((u>>1)&0x3FF)<<21 | ((u>>11)&1)<<20 |
		((u>>12)&0xFF)<<12 | uint32(rd)<<7 | 0x6F
}

// ---- label patching for the two looping programs ------------------------
// Hand-computed offsets become unusable once a program is wrapped in a repeat
// loop, so sort/sieve are written with labels and patched afterwards.

type fixup struct {
	at   int // instruction index of the placeholder
	name string
	jump bool // true: jal x0; false: branch with f3
	rs1  int
	rs2  int
	f3   uint32
}

var (
	labels map[string]int
	fixups []fixup
)

func label(name string) { labels[name] = len(code) * 4 }

func here() int { return len(code) * 4 }

func br(rs1, rs2 int, f3 uint32, target string) {
	fixups = append(fixups, fixup{at: len(code), name: target, rs1: rs1, rs2: rs2, f3: f3})
	emit(0)
}

func jp(target string) {
	fixups = append(fixups, fixup{at: len(code), name: target, jump: true})
	emit(0)
}

func resolve() {
	for _, f := range fixups {
		off := int32(labels[f.name] - f.at*4)
		if f.jump {
			code[f.at] = jR(0, off)
		} else {
			code[f.at] = bR(f.rs1, f.rs2, off, f.f3)
		}
	}
	fixups = fixups[:0]
	labels = map[string]int{}
}

var (
	outDir   = flag.String("out", ".", "output directory")
	base     = flag.Uint("base", 0x40000000, "load/entry base")
	stackTop = flag.Uint64("stack", 0x3FCFF000, "initial sp (4 KiB aligned)")
	dataBase = flag.Uint64("data", 0x3FC80000, "scratch data base (4 KiB aligned)")
	exitMode = flag.String("exit", "halt", "halt: ebreak; uart: print a0 + power off (QEMU virt); spin: print a0 + spin (ESP32-C3 QEMU)")
	uartPage = flag.Uint("uart", 0x10000, "UART base page (0x10000 = 16550 on virt, 0x60000 = ESP32-C3 UART0)")
	reps     = flag.Int("reps", 0, "repeat count for sort/sieve (0 = built-in default)")
)

// exitStub ends a program: ebreak+dead loop for rv32emu, or a jump over a dead
// loop into the epilogue appended by epilogue() for QEMU.
var epilogueJmp = -1

func exitStub() {
	if *exitMode == "halt" {
		emit(0x00100073)
		emit(jR(0, -4))
		return
	}
	epilogueJmp = len(code)
	emit(jR(0, 0))  // patched by epilogue()
	emit(jR(0, -4)) // dead loop, never reached
}

// epilogue emits the QEMU exit path: print a0 as 8 hex digits on the 16550
// UART at 0x10000000, then write 0x5555 to the SiFive test device at 0x100000
// so qemu-system-riscv32 exits 0. Each byte waits for LSR.THRE first: writing
// back-to-back without polling loses characters in QEMU's UART FIFO.
func epilogue() {
	if epilogueJmp < 0 {
		return
	}
	epi := uint32(len(code) * 4)
	code[epilogueJmp] = jR(0, int32(epi-uint32(epilogueJmp*4)))
	epilogueJmp = -1

	emit(uR(5, uint32(*uartPage))) // t0 = UART base
	emit(iR(6, 0, 8, 0, 0x13))     // t1 = 8 digits
	label("epiLoop")
	if *exitMode == "uart" {
		// 16550: wait for LSR.THRE before each byte, otherwise the write is
		// lost. The ESP32-C3 UART has no LSR at offset 5, so spin mode writes
		// straight into the TX FIFO.
		label("epiWait")
		emit(iR(28, 5, 5, 4, 0x03))
		emit(iR(28, 28, 0x20, 7, 0x13))
		br(28, 0, 0, "epiWait") // while !(LSR & THRE)
	}
	emit(iR(28, 10, 28, 5, 0x13)) // t3 = a0 >> 28 (SRLI is I-type, opcode 0x13)
	emit(iR(28, 28, 0xF, 7, 0x13))
	emit(iR(29, 0, 10, 0, 0x13)) // t4 = 10
	br(28, 29, 4, "epiDigit")    // blt t3, 10, digit
	emit(iR(28, 28, 0x37, 0, 0x13))
	jp("epiEmit")
	label("epiDigit")
	emit(iR(28, 28, 0x30, 0, 0x13))
	label("epiEmit")
	emit(sR(5, 28, 0, 0))        // sb t3, 0(t0)
	emit(iR(10, 10, 4, 1, 0x13)) // a0 <<= 4
	emit(iR(6, 6, -1, 0, 0x13))
	br(6, 0, 1, "epiLoop")

	if *exitMode == "uart" {
		label("epiWaitNl")
		emit(iR(28, 5, 5, 4, 0x03))
		emit(iR(28, 28, 0x20, 7, 0x13))
		br(28, 0, 0, "epiWaitNl")
	}
	emit(iR(28, 0, 10, 0, 0x13))
	emit(sR(5, 28, 0, 0)) // '\n'

	if *exitMode == "uart" {
		emit(uR(5, 0x100))               // SiFive test device
		emit(uR(28, 0x5))                // 0x5000
		emit(iR(28, 28, 0x555, 0, 0x13)) // 0x5555
		emit(sR(5, 28, 0, 2))            // sw -> poweroff
	}
	resolve()
	emit(jR(0, 0)) // self-loop
}

// li emits `rd = v` for any 32-bit value (lui+addi pair when needed).
// addi alone only reaches 12 signed bits, which silently truncates things like
// a repeat count of 16000.
func li(rd int, v int32) {
	uv := uint32(v)
	if v >= -2048 && v <= 2047 {
		emit(iR(rd, 0, v, 0, 0x13))
		return
	}
	hi := int32((uv + 0x800) >> 12)
	lo := int32(uv) - (hi << 12)
	emit(uR(rd, uint32(hi)&0xFFFFF))
	if lo != 0 {
		emit(iR(rd, rd, lo, 0, 0x13))
	}
}

func spPage() uint32   { return uint32(*stackTop >> 12) }
func dataPage() uint32 { return uint32(*dataBase >> 12) }

func writeELF(name string) {
	entry := uint32(*base)
	tb := make([]byte, 0, len(code)*4)
	for _, w := range code {
		tb = binary.LittleEndian.AppendUint32(tb, w)
	}
	buf := make([]byte, 0, 52+32+len(tb))
	w16 := func(v uint16) { var b [2]byte; binary.LittleEndian.PutUint16(b[:], v); buf = append(buf, b[:]...) }
	var b4 [4]byte
	p32 := func(v uint32) { binary.LittleEndian.PutUint32(b4[:], v); buf = append(buf, b4[:]...) }
	buf = append(buf, []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}...)
	w16(2)
	w16(243)
	p32(1)
	p32(entry)
	p32(52)
	p32(0)
	p32(0)
	w16(52)
	w16(32)
	w16(1)
	w16(0)
	w16(0)
	w16(0)
	p32(1)
	p32(84)
	p32(entry)
	p32(entry)
	p32(uint32(len(tb)))
	p32(uint32(len(tb)))
	p32(7)
	p32(4)
	buf = append(buf, tb...)
	path := filepath.Join(*outDir, name)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genbench:", err)
		os.Exit(1)
	}
	code = nil
}

// benchPi is the original Leibniz loop; its hand-computed offsets are kept so
// the default output stays byte-identical to earlier releases.
func benchPi() {
	emit(uR(2, spPage()))            // 0:  lui sp
	emit(iR(5, 0, 0, 0, 0x13))       // 4:  sum=0
	emit(iR(6, 0, 0, 0, 0x13))       // 8:  k=0
	emit(iR(7, 0, 1, 0, 0x13))       // 12: sign=+1 (multiplying by 0x10000 overflows)
	emit(uR(28, 0x2))                // 16: x28=0x2000
	emit(iR(28, 28, 0x710, 0, 0x13)) // 20: N=0x2710=10000
	emit(iR(29, 6, 1, 1, 0x13))      // 24: slli x29,x6,1
	emit(iR(29, 29, 1, 0, 0x13))     // 28: addi x29,x29,1
	emit(uR(30, 0x10))               // 32: x30=0x10000
	emit(rR(30, 30, 29, 5, 0x01))    // 36: divu
	emit(rR(31, 7, 30, 0, 0x01))     // 40: mul (MULDIV: f7=0x01, f3=0)
	emit(rR(5, 5, 31, 0, 0x00))      // 44: add sum
	emit(iR(6, 6, 1, 0, 0x13))       // 48: k++
	emit(rR(7, 0, 7, 0, 0x20))       // 52: sub
	emit(bR(6, 28, -32, 4))          // 56: blt k,N,loop
	emit(iR(10, 5, 2, 1, 0x13))      // 60: slli a0,x5,2
	exitStub()
	epilogue()
	writeELF("bench_pi.elf")
}

// benchSort sorts a 16-byte array with bubble sort, re-initialising the array
// each pass, repsCount times. a0 = arr[0] after the last pass (0 for this input).
func benchSort(repsCount int) {
	vals := []int32{5, 3, 8, 1, 9, 2, 7, 4, 0, 6, 15, 11, 13, 10, 14, 12}

	emit(uR(2, spPage()))
	emit(uR(8, dataPage()))  // x8 = array base
	li(20, int32(repsCount)) // x20 = repeats

	label("rep")
	for i, v := range vals {
		emit(iR(9, 0, v, 0, 0x13))  // li x9, v
		emit(sR(8, 9, int32(i), 0)) // sb x9, i(x8)
	}

	emit(iR(5, 0, 0, 0, 0x13)) // i = 0
	label("outer")
	li(7, 15)                  // x7 = 15
	br(5, 7, 5, "done")        // bge i, 15, done
	emit(rR(9, 7, 5, 0, 0x20)) // limit = 15 - i
	emit(iR(6, 0, 0, 0, 0x13)) // j = 0
	label("inner")
	br(6, 9, 5, "nextI")         // bge j, limit, nextI
	emit(rR(12, 8, 6, 0, 0x00))  // x12 = &arr[j]
	emit(iR(10, 12, 0, 4, 0x03)) // a = arr[j]
	emit(iR(11, 12, 1, 4, 0x03)) // b = arr[j+1]
	br(11, 10, 5, "noSwap")      // bge b, a, noSwap
	emit(sR(12, 11, 0, 0))       // arr[j] = b
	emit(sR(12, 10, 1, 0))       // arr[j+1] = a
	label("noSwap")
	emit(iR(6, 6, 1, 0, 0x13)) // j++
	jp("inner")
	label("nextI")
	emit(iR(5, 5, 1, 0, 0x13)) // i++
	jp("outer")
	label("done")
	emit(iR(20, 20, -1, 0, 0x13)) // --
	br(20, 0, 1, "rep")           // bne x20, x0, rep

	emit(iR(10, 8, 0, 4, 0x03)) // a0 = arr[0]
	resolve()
	exitStub()
	epilogue()
	writeELF("bench_sort.elf")
}

// benchSieve counts primes below 1001 with a byte sieve, repsCount times.
// a0 = the count of the last pass (168).
func benchSieve(repsCount int) {
	emit(uR(2, spPage()))
	emit(uR(8, dataPage()))  // x8 = sieve base
	li(15, 1001)             // x15 = 1001
	li(20, int32(repsCount)) // x20 = repeats

	label("rep")
	emit(iR(9, 15, 0, 0, 0x13)) // n = 1001
	emit(iR(10, 0, 1, 0, 0x13)) // one = 1
	emit(rR(12, 8, 0, 0, 0x00)) // ptr = base
	label("init")
	emit(sR(12, 10, 0, 0)) // *ptr = 1
	emit(iR(12, 12, 1, 0, 0x13))
	emit(iR(9, 9, -1, 0, 0x13))
	br(9, 0, 1, "init") // bne n, x0, init

	emit(iR(13, 0, 2, 0, 0x13)) // p = 2
	label("outer")
	emit(rR(14, 13, 13, 0, 0x01)) // p*p (mul)
	br(14, 15, 5, "count")        // bge p*p, 1001, count
	emit(rR(16, 8, 13, 0, 0x00))  // &sieve[p]
	emit(iR(17, 16, 0, 4, 0x03))  // sieve[p]
	br(17, 0, 0, "nextP")         // beq sieve[p], 0, nextP
	emit(rR(18, 14, 0, 0, 0x00))  // j = p*p
	label("mark")
	emit(rR(19, 8, 18, 0, 0x00)) // &sieve[j]
	emit(sR(19, 0, 0, 0))        // sieve[j] = 0
	emit(rR(18, 18, 13, 0, 0x00))
	br(18, 15, 4, "mark") // blt j, 1001, mark
	label("nextP")
	emit(iR(13, 13, 1, 0, 0x13)) // p++
	jp("outer")

	label("count")
	emit(iR(5, 0, 0, 0, 0x13)) // count = 0
	emit(iR(6, 0, 2, 0, 0x13)) // i = 2
	label("countLoop")
	emit(rR(12, 8, 6, 0, 0x00))
	emit(iR(10, 12, 0, 4, 0x03))
	emit(rR(5, 5, 10, 0, 0x00))
	emit(iR(6, 6, 1, 0, 0x13))
	br(6, 15, 4, "countLoop") // blt i, 1001, countLoop

	emit(iR(20, 20, -1, 0, 0x13))
	br(20, 0, 1, "rep") // bne x20, x0, rep

	emit(rR(10, 5, 0, 0, 0x00)) // a0 = count
	resolve()
	exitStub()
	epilogue()
	writeELF("bench_sieve.elf")
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "genbench:", err)
		os.Exit(1)
	}
	labels = map[string]int{}

	sortReps, sieveReps := 19000, 1340
	if *reps > 0 {
		sortReps, sieveReps = *reps, *reps
	}

	benchPi()
	benchSort(sortReps)
	benchSieve(sieveReps)
}
