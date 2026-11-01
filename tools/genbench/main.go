// genbench: 生成 RV32IMC 基准 ELF（手写，无浮点）。
//   bench_pi.elf    Leibniz 级数算 pi（Q16.16，N=20000）
//   bench_sort.elf  冒泡排序 16 元素
//   bench_sieve.elf 素数筛到 1000
package main

import (
	"encoding/binary"
	"os"
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
	return ((u >> 12) & 1) << 31 | ((u >> 11) & 1) << 7 | ((u >> 5) & 0x3F) << 25 |
		uint32(rs2)<<20 | uint32(rs1)<<15 | f3<<12 | ((u >> 1) & 0xF) << 8 | 0x63
}
func uR(rd int, imm20 uint32) uint32 { return imm20<<12 | uint32(rd)<<7 | 0x37 }
func jR(rd int, imm int32) uint32 {
	u := uint32(imm & 0x1FFFFF)
	return ((u >> 20) & 1) << 31 | ((u >> 1) & 0x3FF) << 21 | ((u >> 11) & 1) << 20 |
		((u >> 12) & 0xFF) << 12 | uint32(rd)<<7 | 0x6F
}

func writeELF(name string) {
	base := uint32(0x40000000)
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
	p32(base)
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
	p32(base)
	p32(base)
	p32(uint32(len(tb)))
	p32(uint32(len(tb)))
	p32(7)
	p32(4)
	buf = append(buf, tb...)
	os.WriteFile(name, buf, 0644)
	code = nil
}

func main() {
	// ===== bench_pi: Leibniz Q16.16, N=20000 =====
	// x5=sum, x6=k, x7=sign, x28=N, x29=denom, x30=term, x31=tmp
	emit(uR(2, 0x3FCFF))              // 0:  lui sp
	emit(iR(5, 0, 0, 0, 0x13))        // 4:  sum=0
	emit(iR(6, 0, 0, 0, 0x13))        // 8:  k=0
	emit(uR(7, 0x10))                  // 12: sign=0x10000
	emit(uR(28, 0x2))                  // 16: x28=0x2000
	emit(iR(28, 28, 0x710, 0, 0x13))   // 20: N=0x2710=10000
	// loop @24
	emit(iR(29, 6, 1, 1, 0x13))       // 24: slli x29,x6,1 (denom=2k)
	emit(iR(29, 29, 1, 0, 0x13))      // 28: addi x29,x29,1
	emit(uR(30, 0x10))                 // 32: x30=0x10000
	emit(rR(30, 30, 29, 5, 0x01))     // 36: divu x30,x30,x29
	emit(rR(31, 7, 30, 0, 0x02))     // 40: mul x31,x7,x30
	emit(rR(5, 5, 31, 0, 0x00))      // 44: add sum
	emit(iR(6, 6, 1, 0, 0x13))       // 48: k++
	emit(rR(7, 0, 7, 0, 0x20))       // 52: sub sign=-sign
	emit(bR(6, 28, -32, 4))          // 56: blt k,N,loop (24-56=-32)
	emit(iR(10, 5, 2, 1, 0x13))      // 60: slli a0,x5,2 (pi=sum*4)
	emit(0x00100073)                   // 64: ebreak
	emit(jR(0, -4))                    // 68: j $
	writeELF("bench_pi.elf")

	// ===== bench_sort: 冒泡排序 16 字节数组（数组在 SRAM 0x3FC80000）=====
	// x8=base, x5=i, x6=j, x7=N-1=15, x9=limit
	emit(uR(2, 0x3FCFF))              // 0
	emit(uR(8, 0x3FC8))                // 4: x8=0x3FC80000
	vals := []int32{5, 3, 8, 1, 9, 2, 7, 4, 0, 6, 15, 11, 13, 10, 14, 12}
	for i, v := range vals {
		emit(iR(9, 0, v, 0, 0x13))    // li x9, v
		emit(sR(8, 9, int32(i), 0))   // sb x9, i(x8)
	}
	// outer @72: i=0
	emit(iR(5, 0, 0, 0, 0x13))        // 72: i=0
	// outer_loop @76
	emit(iR(7, 0, 15, 0, 0x13))       // 76: x7=15
	emit(rR(9, 7, 5, 0, 0x20))        // 80: x9=15-i (limit)
	// inner_loop @84: j=0
	emit(iR(6, 0, 0, 0, 0x13))        // 84: j=0
	// load a=arr[j], b=arr[j+1]
	emit(iR(10, 8, 0, 4, 0x03))       // 88: lbu a0,0(x8+j)
	emit(iR(11, 8, 1, 4, 0x03))       // 92: lbu x11,1(x8+j)
	// if a<=b skip swap -> offset to noswap
	emit(bR(10, 11, 12, 5))           // 96: bge a,b,noswap (108-96=12)
	// swap: arr[j]=b, arr[j+1]=a
	emit(sR(8, 11, 0, 0))             // 100: sb x11,0(x8+j)
	emit(sR(8, 10, 1, 0))             // 104: sb x10,1(x8+j)
	// noswap @108
	emit(iR(6, 6, 1, 0, 0x13))        // 108: j++
	emit(bR(6, 9, -32, 4))           // 112: blt j,limit,inner (84-112=-28? recompute)
	// end inner @116
	emit(iR(5, 5, 1, 0, 0x13))        // 116: i++
	emit(bR(5, 7, -44, 6))            // 120: bge i,15,end (to 128)
	// not done -> outer_loop @76
	emit(jR(0, 76-124))               // 124: j outer_loop
	// end @128: result=arr[0]
	emit(iR(10, 8, 0, 4, 0x03))       // 128: lbu a0,0(x8)
	emit(0x00100073)                   // 132: ebreak
	emit(jR(0, -4))                    // 136
	writeELF("bench_sort.elf")

	// ===== bench_sieve: 素数筛到 1000 =====
	// sieve 数组在 SRAM 0x3FC80000，byte per number
	emit(uR(2, 0x3FCFF))              // 0
	emit(uR(8, 0x3FC8))                // 4: base
	// 初始化 sieve[i]=1 for i=0..1000
	emit(iR(9, 0, 0, 0, 0x13))        // 8: i=0
	// init_loop @12
	emit(iR(10, 0, 1, 0, 0x13))       // 12: x10=1
	emit(sR(8, 10, 0, 0))             // 16: sb x10,0(x8+i)
	emit(iR(9, 9, 1, 0, 0x13))        // 20: i++
	emit(iR(11, 0, 1001, 0, 0x13))    // 24: N=1001
	emit(bR(9, 11, -16, 4))           // 28: blt i,1001,init_loop (12-28=-16)
	// sieve loop: p=2
	emit(iR(9, 0, 2, 0, 0x13))        // 32: p=2
	// outer @36: if p*p>1000 done
	emit(rR(12, 9, 9, 2, 0x02))       // 36: mul x12,p,p
	emit(iR(11, 0, 1001, 0, 0x13))    // 40: limit=1001
	emit(bR(12, 11, 40, 5))           // 44: bge p*p,1001,done (to 84)
	// if sieve[p]==0 skip
	emit(iR(10, 8, 0, 4, 0x03))       // 48: lbu x10,0(x8+p)
	emit(bR(10, 0, 16, 5))            // 52: bge x10,1,mark (to 68? recompute)
	// skip: p++
	emit(iR(9, 9, 1, 0, 0x13))        // 56: p++
	emit(jR(0, 36-60))                 // 60: j outer
	// mark @64: mark multiples of p
	emit(iR(13, 9, 0, 0, 0x13))       // 64: j=p
	emit(rR(13, 9, 9, 0, 0x00))       // 68: j=p*2? no, start j=p*p
	// start j=p*p
	emit(rR(13, 12, 0, 0, 0x00))      // 72: j=p*p
	// mark_loop @76: sieve[j]=0
	emit(sR(8, 0, 0, 0))              // 76: sb x0,0(x8+j)
	emit(iR(13, 13, 0, 0, 0x13))      // 80: j+=p
	emit(rR(13, 13, 9, 0, 0x00))      // 84: add j,p
	emit(iR(11, 0, 1001, 0, 0x13))    // 88: limit
	emit(bR(13, 11, -16, 4))          // 92: blt j,1001,mark_loop (76-92=-16)
	// p++
	emit(iR(9, 9, 1, 0, 0x13))        // 96: p++
	emit(jR(0, 36-100))               // 100: j outer
	// done @104: count primes
	emit(iR(5, 0, 0, 0, 0x13))        // 104: count=0
	emit(iR(6, 0, 2, 0, 0x13))        // 108: i=2
	// count_loop @112
	emit(iR(10, 8, 0, 4, 0x03))       // 112: lbu x10,0(x8+i)
	emit(rR(5, 5, 10, 0, 0x00))      // 116: count += x10
	emit(iR(6, 6, 1, 0, 0x13))        // 120: i++
	emit(iR(11, 0, 1001, 0, 0x13))    // 124: limit
	emit(bR(6, 11, -16, 4))           // 128: blt i,1001,count_loop (112-128=-16)
	// result=count in a0
	emit(rR(10, 5, 0, 0, 0x00))      // 132: a0=count
	emit(0x00100073)                   // 136: ebreak
	emit(jR(0, -4))                    // 140
	writeELF("bench_sieve.elf")
}
