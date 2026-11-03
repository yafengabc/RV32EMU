package rom

import "rv32emu/internal/mem"

// FlashBase 是 flash 模拟区在总线上的基址。ROM 读写桩把
// (FlashBase + flash_addr) 当作物理 flash。ramdisk 镜像放在总线
// FlashBase + flash 偏移处（默认 0x80000，见 soc.DefaultFlashOffset）。
// 与 perip.FlashEmuBase 保持一致。
const FlashBase uint32 = 0x3F000000

// ROM 入口点（来自 esp-idf esp32c3.rom.ld）：
//   esp_rom_spiflash_write  = 0x4000012c
//   esp_rom_spiflash_read   = 0x40000130
//   esp_rom_spiflash_config_param = 0x40000134
const (
	entryWrite uint32 = 0x12c
	entryRead  uint32 = 0x130
	entryConf  uint32 = 0x134
)

// 实现例程地址（放在 ROM 高地址空闲区）。
// 不能用 0x400/0x440：ROM 符号表里 0x40000400 起是 strndup/atoi 等
// libc 入口，桩代码会把它们覆盖掉，固件调用这些函数时会执行到错误指令。
const (
	implRead  uint32 = 0x7F000
	implWrite uint32 = 0x7F100
)

// ---- 指令编码（RV32I）-------------------------------------------------

func encLui(rd, imm20 uint32) uint32 {
	return (imm20 << 12) | (rd << 7) | 0x37
}
func encAddi(rd, rs1, imm uint32) uint32 {
	return ((imm & 0xFFF) << 20) | (rs1 << 15) | (rd << 7) | 0x13
}
func encAdd(rd, rs1, rs2 uint32) uint32 {
	return (rs2 << 20) | (rs1 << 15) | (rd << 7) | 0x33
}
func encLb(rd, rs1, imm uint32) uint32 {
	return ((imm & 0xFFF) << 20) | (rs1 << 15) | (rd << 7) | 0x03
}
func encSb(rs2, rs1, imm uint32) uint32 {
	imm &= 0xFFF
	hi := (imm >> 5) & 0x7F
	lo := imm & 0x1F
	return (hi << 25) | (rs2 << 20) | (rs1 << 15) | (lo << 7) | 0x23
}
func encBeq(rs1, rs2 uint32, imm int32) uint32 {
	immU := uint32(imm)
	i12 := (immU >> 12) & 1
	i10_5 := (immU >> 5) & 0x3F
	i4_1 := (immU >> 1) & 0xF
	i11 := (immU >> 11) & 1
	field := (i12 << 31) | (i10_5 << 25) | (i4_1 << 8) | (i11 << 7)
	return field | (rs2 << 20) | (rs1 << 15) | 0x63
}
func encJal(rd uint32, imm int32) uint32 {
	immU := uint32(imm)
	i20 := (immU >> 20) & 1
	i10_1 := (immU >> 1) & 0x3FF
	i11 := (immU >> 11) & 1
	i19_12 := (immU >> 12) & 0xFF
	field := (i20 << 31) | (i10_1 << 21) | (i11 << 20) | (i19_12 << 12)
	return field | (rd << 7) | 0x6F
}
func encJalr(rd, rs1, imm uint32) uint32 {
	return ((imm & 0xFFF) << 20) | (rs1 << 15) | (rd << 7) | 0x67
}

// 寄存器编号。
const (
	xA0, xA1, xA2 uint32 = 10, 11, 12
	xT0, xT1, xT2, xT3   uint32 = 5, 6, 7, 28
	xRA                   uint32 = 1
	xZERO                 uint32 = 0
)

func putW(rom []byte, off uint32, w uint32) {
	rom[off] = byte(w)
	rom[off+1] = byte(w >> 8)
	rom[off+2] = byte(w >> 16)
	rom[off+3] = byte(w >> 24)
}

// BuildROM 生成一份 512KB ROM 镜像，含三个 ROM flash 桩。
func BuildROM() []byte {
	rom := make([]byte, mem.ROMSize)

	// 蹦床：jal zero,impl —— rd=0 故不破坏 ra，impl 用原 ra 直接返回内核。
	putW(rom, entryWrite, encJal(xZERO, int32(implWrite-entryWrite)))
	putW(rom, entryRead, encJal(xZERO, int32(implRead-entryRead)))
	// config_param：直接 li a0,0 ; ret（返回成功）。
	putW(rom, entryConf, encAddi(xA0, xZERO, 0))
	putW(rom, entryConf+4, encJalr(xZERO, xRA, 0))

	// read 桩：src = FlashBase + a0, dst = a1, len = a2
	hi := FlashBase >> 12
	off := implRead
	putW(rom, off, encLui(xT0, hi)); off += 4            // t0 = FlashBase
	putW(rom, off, encAdd(xT0, xT0, xA0)); off += 4      // t0 += a0
	putW(rom, off, encAddi(xT1, xA1, 0)); off += 4       // t1 = a1 (dst)
	putW(rom, off, encAddi(xT2, xA2, 0)); off += 4       // t2 = a2 (len)
	loopR := off
	putW(rom, off, encBeq(xT2, xZERO, int32(implRead+0x2C-loopR))); off += 4 // beqz t2, done
	putW(rom, off, encLb(xT3, xT0, 0)); off += 4         // lb t3,0(t0)
	putW(rom, off, encSb(xT3, xT1, 0)); off += 4         // sb t3,0(t1)
	putW(rom, off, encAddi(xT0, xT0, 1)); off += 4
	putW(rom, off, encAddi(xT1, xT1, 1)); off += 4
	putW(rom, off, encAddi(xT2, xT2, 0xFFFFFFFF)); off += 4
	putW(rom, off, encJal(xZERO, int32(loopR-off))); off += 4 // jal zero, loopR
	putW(rom, off, encAddi(xA0, xZERO, 0)); off += 4     // done: li a0,0
	putW(rom, off, encJalr(xZERO, xRA, 0)); off += 4     // ret

	// write 桩：dst = FlashBase + a0, src = a1, len = a2
	off = implWrite
	putW(rom, off, encLui(xT0, hi)); off += 4            // t0 = FlashBase
	putW(rom, off, encAdd(xT0, xT0, xA0)); off += 4      // t0 += a0
	putW(rom, off, encAddi(xT1, xA1, 0)); off += 4       // t1 = a1 (src)
	putW(rom, off, encAddi(xT2, xA2, 0)); off += 4       // t2 = a2 (len)
	loopW := off
	putW(rom, off, encBeq(xT2, xZERO, int32(implWrite+0x2C-loopW))); off += 4
	putW(rom, off, encLb(xT3, xT1, 0)); off += 4         // lb t3,0(t1)  src
	putW(rom, off, encSb(xT3, xT0, 0)); off += 4         // sb t3,0(t0)  dst
	putW(rom, off, encAddi(xT0, xT0, 1)); off += 4
	putW(rom, off, encAddi(xT1, xT1, 1)); off += 4
	putW(rom, off, encAddi(xT2, xT2, 0xFFFFFFFF)); off += 4
	putW(rom, off, encJal(xZERO, int32(loopW-off))); off += 4
	putW(rom, off, encAddi(xA0, xZERO, 0)); off += 4
	putW(rom, off, encJalr(xZERO, xRA, 0)); off += 4

	return rom
}
