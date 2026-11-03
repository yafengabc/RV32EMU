package rom

// ROM 服务：模拟器没有真实 ROM 镜像，MicroPython / ESP-IDF 固件引用的
// ROM 函数（从 .map 的 rom 符号表提取）由这里用 Go 实现。
//
// 调用路径：CPU 在 ROM 区取到 0x0000（非法指令）时，chip 层把 pc 分派给
// Service.Call，服务按地址执行语义，返回值写 a0，之后 CPU 返回到 ra。
// 参数按 RISC-V psABI：a0-a7 = x10-x17，超出部分在 sp+0, sp+4, ...

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"strconv"
	"strings"

	"rv32emu/internal/cpu"
	"rv32emu/internal/mem"
	"rv32emu/internal/perip"
)

// ROM 函数表（地址见 esp-idf esp32c3.rom.ld）。
const (
	romResetReason      uint32 = 0x40000018
	romIsPrintBoot      uint32 = 0x4000003c
	romPrintf           uint32 = 0x40000040
	romInstallPutc1     uint32 = 0x40000044
	romInstallUart      uint32 = 0x40000048
	romInstallPutc2     uint32 = 0x4000004c
	romDelayUs          uint32 = 0x40000050
	romGetStackInfo     uint32 = 0x40000054
	romInstallLock      uint32 = 0x40000058
	romTxOneChar        uint32 = 0x40000068
	romTxOneChar2       uint32 = 0x4000006c
	romRxOneChar        uint32 = 0x40000070
	romRxOneCharBlock   uint32 = 0x40000074
	romTxFlush          uint32 = 0x40000080
	romTxWaitIdle       uint32 = 0x40000084
	romUartDivModify    uint32 = 0x40000088
	romSoftwareReset    uint32 = 0x40000090
	romSoftwareResetC   uint32 = 0x40000094
	romDisableWD        uint32 = 0x400000a4
	romSpiflashIdle     uint32 = 0x4000010c
	romSpiflashClk      uint32 = 0x40000150
	romCacheLineSize    uint32 = 0x400004b0
	romBootCacheInit    uint32 = 0x400004c8
	romCacheInval       uint32 = 0x400004d4
	romCacheDisable     uint32 = 0x4000051c
	romCacheEnable      uint32 = 0x40000520
	romCacheSuspend     uint32 = 0x40000524
	romCacheResume      uint32 = 0x40000528
	romCacheFreezeOn    uint32 = 0x4000052c
	romCacheFreezeOff   uint32 = 0x40000530
	romCacheIDROMSize   uint32 = 0x40000548
	romGetCpuFreq       uint32 = 0x40000584
	romSetCpuFreq       uint32 = 0x40000588
	romGetApbFreq       uint32 = 0x40000580
	romGetXtalFreq      uint32 = 0x40000598
	romGetXtalDiv       uint32 = 0x40000590
	romSetXtalDiv       uint32 = 0x40000594
	romGpioMatrixIn     uint32 = 0x400005a0
	romGpioMatrixOut    uint32 = 0x400005a4
	romGpioPadPullup    uint32 = 0x400005c4
	romGpioPadSelGpio   uint32 = 0x400005c8
	romGpioPadSetDrv    uint32 = 0x400005cc
	romIntcSetPriority  uint32 = 0x400005e0
	romIntcSetThreshold uint32 = 0x400005e4
	romIntcEnable       uint32 = 0x400005e8
	romIntcDisable      uint32 = 0x400005ec
	romIntcSetType      uint32 = 0x400005f0
	romIntrMatrixSet    uint32 = 0x400005f4
	romIntrLock         uint32 = 0x400005f8
	romIntrUnlock       uint32 = 0x400005fc
	romMD5Init          uint32 = 0x40000614
	romMD5Update        uint32 = 0x40000618
	romMD5Final         uint32 = 0x4000061c
	romCRC32Le          uint32 = 0x40000628
	romCRC32Be          uint32 = 0x4000062c
	romCRC16Le          uint32 = 0x40000630
	romCRC16Be          uint32 = 0x40000634
	romCRC8Le           uint32 = 0x40000638
	romCRC8Be           uint32 = 0x4000063c
	romEfuseSpiConfig   uint32 = 0x4000071c
	romEfuseWPPad       uint32 = 0x4000072c
	romI2CRead          uint32 = 0x40001954
	romI2CReadMask      uint32 = 0x40001958
	romI2CWrite         uint32 = 0x4000195c
	romI2CWriteMask     uint32 = 0x40001960

	// libgcc 软整数辅助（RISC-V 无硬件 64 位乘除，固件会直接引用 ROM 实现）。
	romAddDF3      uint32 = 0x4000076c
	romAddSF3      uint32 = 0x40000770
	romAshldi3     uint32 = 0x4000077c
	romAshrdi3     uint32 = 0x40000780
	romBswapdi2    uint32 = 0x40000784
	romBswapsi2    uint32 = 0x40000788
	romClzsi2      uint32 = 0x4000079c
	romCtzsi2      uint32 = 0x400007a8
	romDivDF3      uint32 = 0x400007b0
	romDivdi3      uint32 = 0x400007b4
	romDivSF3      uint32 = 0x400007bc
	romDivsi3      uint32 = 0x400007c0
	romEqDF2       uint32 = 0x400007c4
	romEqSF2       uint32 = 0x400007c8
	romExtendsfdf2 uint32 = 0x400007cc
	romFfsi2       uint32 = 0x400007d4
	romFixDFSI     uint32 = 0x400007dc
	romFixSFSI     uint32 = 0x400007e4
	romFixunsdfsi  uint32 = 0x400007e8
	romFloatDIDF   uint32 = 0x400007f4
	romFloatsidf   uint32 = 0x400007fc
	romFloatsisf   uint32 = 0x40000800
	romFloatundidf uint32 = 0x40000804
	romFloatundisf uint32 = 0x40000808
	romFloatunsidf uint32 = 0x4000080c
	romGedf2       uint32 = 0x40000818
	romGesf2       uint32 = 0x4000081c
	romGtdf2       uint32 = 0x40000820
	romGtsf2       uint32 = 0x40000824
	romLedf2       uint32 = 0x40000828
	romLesf2       uint32 = 0x4000082c
	romLshrdi3     uint32 = 0x40000830
	romLtdf2       uint32 = 0x40000834
	romLtsf2       uint32 = 0x40000838
	romModdi3      uint32 = 0x4000083c
	romModsi3      uint32 = 0x40000840
	romMulDF3      uint32 = 0x40000848
	romMuldi3      uint32 = 0x4000084c
	romMulSF3      uint32 = 0x40000854
	romMulsi3      uint32 = 0x40000858
	romNedf2       uint32 = 0x40000864
	romNegDF2      uint32 = 0x40000868
	romNegdi2      uint32 = 0x4000086c
	romNegSF2      uint32 = 0x40000870
	romNesf2       uint32 = 0x4000087c
	romSubDF3      uint32 = 0x40000894
	romSubSF3      uint32 = 0x40000898
	romTruncdfsf2  uint32 = 0x400008a4
	romUcmpdi2     uint32 = 0x400008a8
	romUdivdi3     uint32 = 0x400008ac
	romUdivmoddi   uint32 = 0x400008b0
	romUdivsi3     uint32 = 0x400008b4
	romUmoddi3     uint32 = 0x400008bc
	romUmodsi3     uint32 = 0x400008c0
	romUnorddf2    uint32 = 0x400008c4
	romUnordsf2    uint32 = 0x400008c8

	// ROM libc（firmware 先用 flash 里的 newlib，这些只是兜底）。
	romMemset  uint32 = 0x40000354
	romMemcpy  uint32 = 0x40000358
	romMemmove uint32 = 0x4000035c
	romMemcmp  uint32 = 0x40000360
	romStrcpy  uint32 = 0x40000364
	romStrncpy uint32 = 0x40000368
	romStrcmp  uint32 = 0x4000036c
	romStrncmp uint32 = 0x40000370
	romStrlen  uint32 = 0x40000374
	romStrstr  uint32 = 0x40000378
	romBzero   uint32 = 0x4000037c
	romStrcspn uint32 = 0x400003E4
	romStrspn  uint32 = 0x40000410
	romMemchr  uint32 = 0x400003c8
	romDiv     uint32 = 0x40000428 // div_t div(int numer, int denom)
	romInitMtx uint32 = 0x40000350 // esp_rom_newlib_init_common_mutexes
	romStrcat  uint32 = 0x400003d8
	romStrchr  uint32 = 0x400003e0
	romStrnlen uint32 = 0x40000404
	romStrrchr uint32 = 0x40000408
	romStrlcpy uint32 = 0x400003f0
	romQsort   uint32 = 0x40000434
	romUtoa    uint32 = 0x40000444
	romItoa    uint32 = 0x40000448
)

// INTMTX 窗口里 CPU 侧寄存器的偏移（与 perip/c3.go 的 intc* 常量一致）。
const (
	intcEnaOff    uint32 = 0x104
	intcTypeOff   uint32 = 0x108
	intcPriBase   uint32 = 0x114
	intcThreshOff uint32 = 0x194
)

// Service 是 ROM 函数的 Go 实现集合。
type Service struct {
	bus  *mem.SystemBus
	uart *perip.UART

	ticksPerUs uint32
	xtalDiv    uint32
	unknown    map[uint32]bool
}

// NewService 创建 ROM 服务。uart 用于 printf / tx_one_char 的输出，
// 可为 nil（只丢弃输出）。
func NewService(bus *mem.SystemBus, uart *perip.UART) *Service {
	return &Service{bus: bus, uart: uart, ticksPerUs: 80, unknown: make(map[uint32]bool)}
}

// arg 读取第 idx 个参数（0 = a0）。前 8 个在 a0-a7，其余在 sp+0, sp+4, ...
func (s *Service) arg(c *cpu.CPU, idx int) uint32 {
	if idx < 8 {
		return c.Reg(10 + idx)
	}
	return s.bus.Read(c.Reg(2)+uint32(idx-8)*4, 4)
}

func (s *Service) ret(c *cpu.CPU, v uint32) { c.SetReg(10, v) }

// 64 位值按 psABI 放在 a0:a1（返回值）或 a2:a3 等相邻寄存器对里。
func (s *Service) getU64(c *cpu.CPU, idx int) uint64 {
	return uint64(s.arg(c, idx)) | uint64(s.arg(c, idx+1))<<32
}

func (s *Service) setU64(c *cpu.CPU, v uint64) {
	c.SetReg(10, uint32(v))
	c.SetReg(11, uint32(v>>32))
}

func (s *Service) getD(c *cpu.CPU) float64             { return math.Float64frombits(s.getU64(c, 0)) }
func (s *Service) getDArg(c *cpu.CPU, idx int) float64 { return math.Float64frombits(s.getU64(c, idx)) }
func (s *Service) setD(c *cpu.CPU, v float64)          { s.setU64(c, math.Float64bits(v)) }
func (s *Service) getF(c *cpu.CPU) float32             { return math.Float32frombits(s.arg(c, 0)) }
func (s *Service) getFArg(c *cpu.CPU, idx int) float32 { return math.Float32frombits(s.arg(c, idx)) }
func (s *Service) setF(c *cpu.CPU, v float32)          { s.ret(c, math.Float32bits(v)) }

// cmpRet 按 libgcc 约定返回比较结果：lt 类命中 -1、否则 1；
// gt 类命中 1、否则 -1（NaN 走「不命中」分支）。
func (s *Service) cmpRet(c *cpu.CPU, cond, ltStyle bool) {
	hit, miss := uint32(1), uint32(0xFFFFFFFF)
	if ltStyle {
		hit, miss = 0xFFFFFFFF, 1
	}
	if cond {
		s.ret(c, hit)
	} else {
		s.ret(c, miss)
	}
}

func boolU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func (s *Service) tx(b byte) {
	if s.uart != nil {
		s.uart.Write(0, 1, uint32(b)) // offset 0 = UART FIFO
	}
}

func (s *Service) puts(p []byte) {
	for _, b := range p {
		s.tx(b)
	}
}

// readU8 从总线读一个字节。
func (s *Service) readU8(a uint32) byte { return byte(s.bus.Read(a, 1)) }

// readBytes 从总线读 n 个字节（CRC 等按长度读取，不按 NUL 截断）。
func (s *Service) readBytes(a, n uint32) []byte {
	if a == 0 || n == 0 {
		return nil
	}
	buf := make([]byte, n)
	s.bus.CopyOut(a, buf)
	return buf
}

// readCString 读取以 NUL 结尾的字符串（最多 max 字节）。
func (s *Service) readCString(a uint32, max int) []byte {
	if a == 0 {
		return nil
	}
	out := make([]byte, 0, 32)
	for len(out) < max {
		b := s.readU8(a + uint32(len(out)))
		if b == 0 {
			break
		}
		out = append(out, b)
	}
	return out
}

// Call 按地址分派一个 ROM 调用。未知地址返回 0 并在首次出现时告警。
func (s *Service) Call(c *cpu.CPU, addr uint32) {
	switch addr {
	case romResetReason:
		s.ret(c, 1) // POWERON_RESET
	case romIsPrintBoot:
		s.ret(c, 0)
	case romPrintf:
		s.printf(c)
	case romInstallPutc1, romInstallUart, romInstallPutc2, romInstallLock:
		s.ret(c, 0)
	case romDelayUs:
		s.ret(c, 0)
	case romGetStackInfo:
		s.ret(c, 0)
	case romTxOneChar, romTxOneChar2:
		s.tx(byte(s.arg(c, 0)))
		s.ret(c, 0)
	case romRxOneChar:
		// int uart_rx_one_char(uint8_t *buf)：返回 0=读到，1=无数据。
		// 模拟器里 ROM 这条路径不搬输入，统一报「无数据」。
		s.ret(c, 1)
	case romRxOneCharBlock:
		s.ret(c, 0)
	case romTxFlush, romTxWaitIdle, romUartDivModify:
		s.ret(c, 0)
	case romSoftwareReset, romSoftwareResetC:
		// RTC_CNTL_OPTIONS0 bit31 = SW_SYS_RST；主循环据此重装机器。
		s.bus.Write(perip.RTCBase, 4, 1<<31)
		s.ret(c, 0)
	case romDisableWD:
		s.ret(c, 0)
	case romSpiflashIdle, romSpiflashClk:
		s.ret(c, 0)
	case romCacheLineSize:
		s.ret(c, 32)
	case romBootCacheInit, romCacheInval, romCacheDisable, romCacheEnable,
		romCacheSuspend, romCacheResume, romCacheFreezeOn, romCacheFreezeOff,
		romCacheIDROMSize:
		s.ret(c, 0)
	case romGetCpuFreq:
		s.ret(c, s.ticksPerUs)
	case romSetCpuFreq:
		s.ticksPerUs = s.arg(c, 0)
		s.ret(c, 0)
	case romGetApbFreq:
		s.ret(c, s.ticksPerUs)
	case romGetXtalFreq:
		s.ret(c, 40)
	case romGetXtalDiv:
		if s.xtalDiv == 0 {
			s.xtalDiv = 1
		}
		s.ret(c, s.xtalDiv)
	case romSetXtalDiv:
		s.xtalDiv = s.arg(c, 0)
		s.ret(c, 0)
	case romGpioMatrixIn, romGpioMatrixOut, romGpioPadPullup, romGpioPadSelGpio,
		romGpioPadSetDrv:
		s.ret(c, 0)
	case romIntrMatrixSet:
		// intr_matrix_set(cpu_no, source, line)：写 INTMTX 映射寄存器。
		src := s.arg(c, 1)
		line := s.arg(c, 2)
		s.bus.Write(perip.INTMTXBase+src*4, 4, line)
		s.ret(c, 0)
	case romIntcSetPriority:
		// esprv_intc_int_set_priority(line, pri)：CPU_INT_PRI_n（0x114+4n）。
		s.bus.Write(perip.INTMTXBase+intcPriBase+s.arg(c, 0)*4, 4, s.arg(c, 1))
		s.ret(c, 0)
	case romIntcSetThreshold:
		s.bus.Write(perip.INTMTXBase+intcThreshOff, 4, s.arg(c, 0))
		s.ret(c, 0)
	case romIntcEnable, romIntcDisable:
		// esprv_intc_int_enable/disable(mask)：位掩码按 line 置/清 CPU_INT_ENABLE。
		v := s.bus.Read(perip.INTMTXBase+intcEnaOff, 4)
		if addr == romIntcEnable {
			v |= s.arg(c, 0)
		} else {
			v &^= s.arg(c, 0)
		}
		s.bus.Write(perip.INTMTXBase+intcEnaOff, 4, v)
		s.ret(c, 0)
	case romIntcSetType:
		// esprv_intc_int_set_type(line, type)：CPU_INT_TYPE 位（1=边沿）。
		v := s.bus.Read(perip.INTMTXBase+intcTypeOff, 4)
		bit := uint32(1) << (s.arg(c, 0) & 31)
		if s.arg(c, 1) != 0 {
			v |= bit
		} else {
			v &^= bit
		}
		s.bus.Write(perip.INTMTXBase+intcTypeOff, 4, v)
		s.ret(c, 0)
	case romIntrLock, romIntrUnlock:
		s.ret(c, 0)
	case romMD5Init:
		// void esp_rom_md5_init(esp_rom_md5_context *ctx)
		s.md5Init(s.arg(c, 0))
		s.ret(c, 0)
	case romMD5Update:
		// void esp_rom_md5_update(ctx, const void *buf, uint32_t len)
		s.md5Update(s.arg(c, 0), s.arg(c, 1), s.arg(c, 2))
		s.ret(c, 0)
	case romMD5Final:
		// void esp_rom_md5_final(uint8_t *digest, esp_rom_md5_context *ctx)
		s.md5Final(s.arg(c, 1), s.arg(c, 0))
		s.ret(c, 0)
	case romCRC32Le:
		s.ret(c, crc32(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), true))
	case romCRC32Be:
		s.ret(c, crc32(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), false))
	case romCRC16Le:
		s.ret(c, crc16(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), true))
	case romCRC16Be:
		s.ret(c, crc16(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), false))
	case romCRC8Le:
		s.ret(c, crc8(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), true))
	case romCRC8Be:
		s.ret(c, crc8(s.arg(c, 0), s.readBytes(s.arg(c, 1), s.arg(c, 2)), false))
	case romEfuseSpiConfig:
		s.ret(c, 0)
	case romEfuseWPPad:
		s.ret(c, 0xFFFFFFFF)
	case romI2CRead, romI2CReadMask, romI2CWrite, romI2CWriteMask:
		s.ret(c, 0)

	// ---- libgcc 软整数 / 软浮点 --------------------------------------
	case romAshldi3:
		s.setU64(c, s.getU64(c, 0)<<(s.arg(c, 2)&63))
	case romAshrdi3:
		sh := s.arg(c, 2) & 63
		s.setU64(c, uint64(int64(s.getU64(c, 0))>>sh))
	case romBswapsi2:
		// __bswapsi2：32 位字节序翻转。LittleFS 的 tag 存取依赖它，
		// 缺失时返回 0 会让元数据 tag 全为 0（mount 报 CORRUPT）。
		s.ret(c, bswap32(s.arg(c, 0)))
	case romBswapdi2:
		v := s.getU64(c, 0)
		s.setU64(c, uint64(bswap32(uint32(v>>32)))|uint64(bswap32(uint32(v)))<<32)
	case romLshrdi3:
		s.setU64(c, s.getU64(c, 0)>>(s.arg(c, 2)&63))
	case romClzsi2:
		v := s.arg(c, 0)
		n := uint32(32)
		for v != 0 {
			n--
			v >>= 1
		}
		s.ret(c, n)
	case romCtzsi2:
		v := s.arg(c, 0)
		n := uint32(32)
		if v != 0 {
			n = 0
			for v&1 == 0 {
				n++
				v >>= 1
			}
		}
		s.ret(c, n)
	case romFfsi2:
		// __ffssi2(int a)：返回最低置位位序号（1 基），a==0 返回 0。
		// tlsf_malloc 用它的 log2 结果算 size class。
		v := s.arg(c, 0)
		if v == 0 {
			s.ret(c, 0)
		} else {
			s.ret(c, uint32(bits.TrailingZeros32(v))+1)
		}
	case romMulsi3:
		s.ret(c, s.arg(c, 0)*s.arg(c, 1))
	case romMuldi3:
		s.setU64(c, s.getU64(c, 0)*s.getU64(c, 2))
	case romNegdi2:
		s.setU64(c, -s.getU64(c, 0))
	case romUcmpdi2:
		a, b := s.getU64(c, 0), s.getU64(c, 2)
		switch {
		case a < b:
			s.ret(c, 0)
		case a == b:
			s.ret(c, 1)
		default:
			s.ret(c, 2)
		}
	case romUdivdi3:
		a, b := s.getU64(c, 0), s.getU64(c, 2)
		if b == 0 {
			s.setU64(c, 0)
		} else {
			s.setU64(c, a/b)
		}
	case romUmoddi3:
		a, b := s.getU64(c, 0), s.getU64(c, 2)
		if b == 0 {
			s.setU64(c, 0)
		} else {
			s.setU64(c, a%b)
		}
	case romUdivmoddi:
		a, b := s.getU64(c, 0), s.getU64(c, 2)
		if b == 0 {
			s.setU64(c, 0)
			s.bus.Write(s.arg(c, 4), 4, 0)
			s.bus.Write(s.arg(c, 4)+4, 4, 0)
		} else {
			s.setU64(c, a/b)
			s.bus.Write(s.arg(c, 4), 4, uint32(a%b))
			s.bus.Write(s.arg(c, 4)+4, 4, uint32((a%b)>>32))
		}
	case romDivdi3:
		a, b := int64(s.getU64(c, 0)), int64(s.getU64(c, 2))
		if b == 0 {
			s.setU64(c, 0)
		} else {
			s.setU64(c, uint64(a/b))
		}
	case romModdi3:
		a, b := int64(s.getU64(c, 0)), int64(s.getU64(c, 2))
		if b == 0 {
			s.setU64(c, 0)
		} else {
			s.setU64(c, uint64(a%b))
		}
	case romUdivsi3:
		a, b := s.arg(c, 0), s.arg(c, 1)
		if b == 0 {
			s.ret(c, 0)
		} else {
			s.ret(c, a/b)
		}
	case romUmodsi3:
		a, b := s.arg(c, 0), s.arg(c, 1)
		if b == 0 {
			s.ret(c, 0)
		} else {
			s.ret(c, a%b)
		}
	case romDivsi3:
		a, b := int32(s.arg(c, 0)), int32(s.arg(c, 1))
		if b == 0 {
			s.ret(c, 0)
		} else {
			s.ret(c, uint32(a/b))
		}
	case romModsi3:
		a, b := int32(s.arg(c, 0)), int32(s.arg(c, 1))
		if b == 0 {
			s.ret(c, 0)
		} else {
			s.ret(c, uint32(a%b))
		}

	// 软浮点：a0:a1（double）或 a0（float）传参，返回值同 ABI。
	case romAddDF3:
		s.setD(c, s.getD(c)+s.getDArg(c, 2))
	case romSubDF3:
		s.setD(c, s.getD(c)-s.getDArg(c, 2))
	case romMulDF3:
		s.setD(c, s.getD(c)*s.getDArg(c, 2))
	case romDivDF3:
		s.setD(c, s.getD(c)/s.getDArg(c, 2))
	case romNegDF2:
		s.setD(c, -s.getD(c))
	case romAddSF3:
		s.setF(c, s.getF(c)+s.getFArg(c, 1))
	case romSubSF3:
		s.setF(c, s.getF(c)-s.getFArg(c, 1))
	case romMulSF3:
		s.setF(c, s.getF(c)*s.getFArg(c, 1))
	case romDivSF3:
		s.setF(c, s.getF(c)/s.getFArg(c, 1))
	case romNegSF2:
		s.setF(c, -s.getF(c))
	case romEqDF2:
		if s.getD(c) == s.getDArg(c, 2) {
			s.ret(c, 0)
		} else {
			s.ret(c, 1)
		}
	case romEqSF2:
		if s.getF(c) == s.getFArg(c, 1) {
			s.ret(c, 0)
		} else {
			s.ret(c, 1)
		}
	case romNedf2:
		if s.getD(c) != s.getDArg(c, 2) {
			s.ret(c, 1)
		} else {
			s.ret(c, 0)
		}
	case romNesf2:
		if s.getF(c) != s.getFArg(c, 1) {
			s.ret(c, 1)
		} else {
			s.ret(c, 0)
		}
	case romLtdf2:
		s.cmpRet(c, s.getD(c) < s.getDArg(c, 2), true)
	case romLedf2:
		s.cmpRet(c, s.getD(c) <= s.getDArg(c, 2), true)
	case romGtdf2:
		s.cmpRet(c, s.getD(c) > s.getDArg(c, 2), false)
	case romGedf2:
		s.cmpRet(c, s.getD(c) >= s.getDArg(c, 2), false)
	case romLtsf2:
		s.cmpRet(c, s.getF(c) < s.getFArg(c, 1), true)
	case romLesf2:
		s.cmpRet(c, s.getF(c) <= s.getFArg(c, 1), true)
	case romGtsf2:
		s.cmpRet(c, s.getF(c) > s.getFArg(c, 1), false)
	case romGesf2:
		s.cmpRet(c, s.getF(c) >= s.getFArg(c, 1), false)
	case romUnorddf2:
		d := s.getD(c)
		s.ret(c, boolU32(d != d))
	case romUnordsf2:
		f := s.getF(c)
		s.ret(c, boolU32(f != f))
	case romTruncdfsf2:
		s.setF(c, float32(s.getD(c)))
	case romExtendsfdf2:
		s.setD(c, float64(s.getF(c)))
	case romFixDFSI:
		s.ret(c, uint32(int32(s.getD(c))))
	case romFixSFSI:
		s.ret(c, uint32(int32(s.getF(c))))
	case romFixunsdfsi:
		v := s.getD(c)
		if v < 0 || v != v {
			s.ret(c, 0)
		} else if v >= 4294967296 {
			s.ret(c, 0xFFFFFFFF)
		} else {
			s.ret(c, uint32(v))
		}
	case romFloatsidf:
		s.setD(c, float64(int32(s.arg(c, 0))))
	case romFloatsisf:
		s.setF(c, float32(int32(s.arg(c, 0))))
	case romFloatDIDF:
		s.setD(c, float64(int64(s.getU64(c, 0))))
	case romFloatunsidf:
		s.setD(c, float64(s.arg(c, 0)))
	case romFloatundidf:
		s.setD(c, float64(s.getU64(c, 0)))
	case romFloatundisf:
		s.setF(c, float32(s.getU64(c, 0)))

	// ---- 兜底 libc ---------------------------------------------------
	case romInitMtx:
		// esp_rom_newlib_init_common_mutexes(void)：无锁环境下的空操作。
		s.ret(c, 0)
	case romDiv:
		// div_t div(int numer, int denom)：商在 a0、余数在 a1。
		a, b := int32(s.arg(c, 0)), int32(s.arg(c, 1))
		if b == 0 {
			s.ret(c, 0)
			c.SetReg(11, 0)
		} else {
			c.SetReg(10, uint32(a/b))
			c.SetReg(11, uint32(a%b))
		}
	case romMemset:
		s.memset(c)
	case romMemcpy:
		s.memcpy(c, false)
	case romMemmove:
		s.memcpy(c, true)
	case romMemcmp:
		s.memcmp(c)
	case romStrcpy:
		s.strcpy(c)
	case romStrncpy:
		s.strncpy(c)
	case romStrcmp:
		s.strcmp(c, false)
	case romStrncmp:
		s.strcmp(c, true)
	case romStrlen:
		s.ret(c, uint32(len(s.readCString(s.arg(c, 0), 1<<20))))
	case romStrspn:
		// size_t strspn(s, accept)：s 开头连续属于 accept 的字符数。
		s.strspan(c, false)
	case romStrcspn:
		// size_t strcspn(s, reject)：s 开头连续不属于 reject 的字符数。
		// LittleFS 用它切分路径组件，返回 0 会让路径永远停在原地。
		s.strspan(c, true)
	case romStrnlen:
		n := s.arg(c, 1)
		s.ret(c, uint32(len(s.readCString(s.arg(c, 0), int(n)))))
	case romStrlcpy:
		// size_t strlcpy(char *dst, const char *src, size_t size)：拷贝至多
		// size-1 字节并补 NUL，返回源串全长。
		src := s.readCString(s.arg(c, 1), 1<<20)
		size := s.arg(c, 2)
		if size > 0 {
			n := uint32(len(src))
			if n > size-1 {
				n = size - 1
			}
			for i := uint32(0); i < n; i++ {
				s.bus.Write(s.arg(c, 0)+i, 1, uint32(src[i]))
			}
			s.bus.Write(s.arg(c, 0)+n, 1, 0)
		}
		s.ret(c, uint32(len(src)))
	case romStrstr:
		s.strstr(c)
	case romBzero:
		s.fill(s.arg(c, 0), 0, s.arg(c, 1))
		s.ret(c, 0)
	case romMemchr:
		s.memchr(c)
	case romStrchr:
		s.strchr(c)
	case romStrrchr:
		s.strrchr(c)
	case romStrcat:
		s.strcat(c)
	case romQsort:
		s.qsort(c)
	case romItoa, romUtoa:
		s.itoa(c, addr == romUtoa)

	default:
		if !s.unknown[addr] {
			s.unknown[addr] = true
			fmt.Fprintf(os.Stderr, "[rom] unimplemented 0x%08X a0=0x%08X a1=0x%08X a2=0x%08X ra=0x%08X\n",
				addr, s.arg(c, 0), s.arg(c, 1), s.arg(c, 2), c.Reg(1))
		}
		s.ret(c, 0)
	}
}

// ---- printf ---------------------------------------------------------

// printf 实现 ets_printf/esp_rom_printf 的格式化子集：%d %i %u %x %X %o
// %p %c %s %%，支持 0/ - 标志、宽度与 .精度、l/ll/h/hh 长度修饰。
func (s *Service) printf(c *cpu.CPU) {
	fmtAddr := s.arg(c, 0)
	if fmtAddr == 0 {
		return
	}
	next := 1
	getArg := func() uint32 {
		v := s.arg(c, next)
		next++
		return v
	}

	var out []byte
	for i := uint32(0); ; {
		ch := s.readU8(fmtAddr + i)
		i++
		if ch == 0 {
			break
		}
		if ch != '%' {
			out = append(out, ch)
			continue
		}

		var zero, minus, plus bool
		var width int
	parFlags:
		for {
			switch s.readU8(fmtAddr + i) {
			case '0':
				zero = true
				i++
			case '-':
				minus = true
				i++
			case '+':
				plus = true
				i++
			case ' ', '#':
				i++
			default:
				break parFlags
			}
		}
		for d := s.readU8(fmtAddr + i); d >= '0' && d <= '9'; d = s.readU8(fmtAddr + i) {
			width = width*10 + int(d-'0')
			i++
		}
		prec := -1
		if s.readU8(fmtAddr+i) == '.' {
			i++
			prec = 0
			for d := s.readU8(fmtAddr + i); d >= '0' && d <= '9'; d = s.readU8(fmtAddr + i) {
				prec = prec*10 + int(d-'0')
				i++
			}
		}
		for {
			switch s.readU8(fmtAddr + i) {
			case 'l', 'h', 'z', 'j', 't':
				i++
			default:
				goto lenDone
			}
		}
	lenDone:
		spec := s.readU8(fmtAddr + i)
		i++

		var text string
		numeric := false
		switch spec {
		case 'd', 'i':
			numeric = true
			v := int32(getArg())
			if v < 0 {
				text = "-" + strconv.FormatUint(uint64(-int64(v)), 10)
			} else {
				text = strconv.FormatUint(uint64(v), 10)
				if plus {
					text = "+" + text
				}
			}
		case 'u':
			numeric = true
			text = strconv.FormatUint(uint64(getArg()), 10)
		case 'x':
			numeric = true
			text = strconv.FormatUint(uint64(getArg()), 16)
		case 'X':
			numeric = true
			text = strings.ToUpper(strconv.FormatUint(uint64(getArg()), 16))
		case 'o':
			numeric = true
			text = strconv.FormatUint(uint64(getArg()), 8)
		case 'p':
			numeric = true
			text = fmt.Sprintf("%08x", getArg())
		case 'c':
			text = string(rune(getArg()))
		case 's':
			text = string(s.readCString(getArg(), 1<<20))
			if prec >= 0 && len(text) > prec {
				text = text[:prec]
			}
		case '%':
			text = "%"
		default:
			text = "%" + string(spec)
		}

		if width > len(text) {
			pad := width - len(text)
			if zero && numeric {
				sign := ""
				if len(text) > 0 && (text[0] == '-' || text[0] == '+') {
					sign, text = text[:1], text[1:]
				}
				text = sign + strings.Repeat("0", pad) + text
			} else if minus {
				text += strings.Repeat(" ", pad)
			} else {
				text = strings.Repeat(" ", pad) + text
			}
		}
		out = append(out, text...)
	}
	s.puts(out)
}

// ---- libc 兜底 -------------------------------------------------------

func (s *Service) fill(dst, val, n uint32) {
	for i := uint32(0); i < n; i++ {
		s.bus.Write(dst+i, 1, val&0xFF)
	}
}

func (s *Service) memset(c *cpu.CPU) {
	s.fill(s.arg(c, 0), s.arg(c, 1), s.arg(c, 2))
	s.ret(c, s.arg(c, 0))
}

func (s *Service) memcpy(c *cpu.CPU, overlap bool) {
	dst := s.arg(c, 0)
	src := s.arg(c, 1)
	n := s.arg(c, 2)
	if overlap {
		buf := make([]byte, n)
		s.bus.CopyOut(src, buf)
		for i := uint32(0); i < n; i++ {
			s.bus.Write(dst+i, 1, uint32(buf[i]))
		}
	} else {
		for i := uint32(0); i < n; i++ {
			s.bus.Write(dst+i, 1, s.bus.Read(src+i, 1))
		}
	}
	s.ret(c, dst)
}

// bswap32 翻转 32 位整数的字节序（对应 ROM 的 __bswapsi2）。
func bswap32(v uint32) uint32 {
	return v<<24 | (v&0xFF00)<<8 | (v>>8)&0xFF00 | v>>24
}

// strspan 实现 strspn / strcspn：complement=false 统计开头属于 set 的字符数，
// complement=true 统计开头不属于 set 的字符数。
func (s *Service) strspan(c *cpu.CPU, complement bool) {
	str := string(s.readCString(s.arg(c, 0), 1<<20))
	set := string(s.readCString(s.arg(c, 1), 1<<20))
	n := 0
	for n < len(str) {
		if (strings.IndexByte(set, str[n]) >= 0) == complement {
			break
		}
		n++
	}
	s.ret(c, uint32(n))
}

func (s *Service) memcmp(c *cpu.CPU) {
	p0, p1, n := s.arg(c, 0), s.arg(c, 1), s.arg(c, 2)
	var ret uint32
	for i := uint32(0); i < n; i++ {
		a := s.readU8(p0 + i)
		b := s.readU8(p1 + i)
		if a != b {
			if a < b {
				ret = 0xFFFFFFFF
			} else {
				ret = 1
			}
			break
		}
	}
	s.ret(c, ret)
}

func (s *Service) strcpy(c *cpu.CPU) {
	dst := s.arg(c, 0)
	text := s.readCString(s.arg(c, 1), 1<<20)
	for i, b := range text {
		s.bus.Write(dst+uint32(i), 1, uint32(b))
	}
	s.bus.Write(dst+uint32(len(text)), 1, 0)
	s.ret(c, dst)
}

func (s *Service) strncpy(c *cpu.CPU) {
	dst, src, n := s.arg(c, 0), s.arg(c, 1), s.arg(c, 2)
	text := s.readCString(src, int(n))
	var i uint32
	for ; i < uint32(len(text)); i++ {
		s.bus.Write(dst+i, 1, uint32(text[i]))
	}
	for ; i < n; i++ {
		s.bus.Write(dst+i, 1, 0)
	}
	s.ret(c, dst)
}

// strcat 把 src 追加到 dst 末尾，返回 dst。
func (s *Service) strcat(c *cpu.CPU) {
	dst, src := s.arg(c, 0), s.arg(c, 1)
	n := uint32(0)
	for ; n < 1<<20; n++ {
		if s.readU8(dst+n) == 0 {
			break
		}
	}
	text := s.readCString(src, 1<<20)
	for i, b := range text {
		s.bus.Write(dst+n+uint32(i), 1, uint32(b))
	}
	s.bus.Write(dst+n+uint32(len(text)), 1, 0)
	s.ret(c, dst)
}

// itoa/utoa：ROM 版本是 itoa(int value, char *str, int base)，返回 str。
// abort() 用它把 PC 与 core id 写进 panic 消息，所以必须实现。
func (s *Service) itoa(c *cpu.CPU, unsigned bool) {
	v := s.arg(c, 0)
	dst := s.arg(c, 1)
	base := s.arg(c, 2)
	if base < 2 || base > 36 {
		base = 10
	}
	var digits []byte
	if !unsigned && int32(v) < 0 {
		digits = append(digits, '-')
		v = uint32(-int32(v))
	}
	for {
		d := v % base
		if d < 10 {
			digits = append(digits, byte('0'+d))
		} else {
			digits = append(digits, byte('a'+d-10))
		}
		v /= base
		if v == 0 {
			break
		}
	}
	// digits 里低位的在前，需要反转（负号留在最前）。
	start := 0
	if digits[0] == '-' {
		start = 1
	}
	for a, b := start, len(digits)-1; a < b; a, b = a+1, b-1 {
		digits[a], digits[b] = digits[b], digits[a]
	}
	for k, b := range digits {
		s.bus.Write(dst+uint32(k), 1, uint32(b))
	}
	s.bus.Write(dst+uint32(len(digits)), 1, 0)
	s.ret(c, dst)
}

// callSentinel 是 ROM 服务回调 guest 代码时用的返回地址。它落在 ROM 区
// 尾部，不会真的被执行：嵌套执行循环一看到 PC 到达它就算调用结束。
const callSentinel uint32 = 0x4007FFF0

// callGuest 在 ROM 服务内部同步调用一段 guest 代码（典型场景是 qsort
// 的比较函数），返回后从 a0 取结果。调用前后保存/恢复全部寄存器与 PC，
// 对调用者（正在执行的那条 ROM 指令）完全透明。
func (s *Service) callGuest(c *cpu.CPU, fn, a0, a1 uint32) uint32 {
	var saved [32]uint32
	for i := range saved {
		saved[i] = c.Reg(i)
	}
	pc := c.PC()
	c.SetReg(10, a0)
	c.SetReg(11, a1)
	c.SetReg(1, callSentinel)
	c.SetPC(fn)
	for n := 0; n < 200000; n++ {
		if c.Halted() || c.PC() == callSentinel {
			break
		}
		c.Step()
	}
	ret := c.Reg(10)
	for i := range saved {
		c.SetReg(i, saved[i])
	}
	c.SetPC(pc)
	return ret
}

// qsort 是 ROM 的新库排序。这里用插入排序，通过 callGuest 回调 guest 的
// 比较函数。启动阶段的排序规模很小（内存区域表最多几十项）。
func (s *Service) qsort(c *cpu.CPU) {
	base, n, size, cmp := s.arg(c, 0), s.arg(c, 1), s.arg(c, 2), s.arg(c, 3)
	if n < 2 || size == 0 || size > 256 || n > 4096 || cmp == 0 {
		return
	}
	tmp := make([]byte, size)
	for i := uint32(1); i < n; i++ {
		for j := i; j > 0; j-- {
			a := base + (j-1)*size
			b := base + j*size
			if int32(s.callGuest(c, cmp, a, b)) <= 0 {
				break
			}
			for k := uint32(0); k < size; k++ {
				tmp[k] = byte(s.readU8(a + k))
			}
			for k := uint32(0); k < size; k++ {
				s.bus.Write(a+k, 1, uint32(s.readU8(b+k)))
			}
			for k := uint32(0); k < size; k++ {
				s.bus.Write(b+k, 1, uint32(tmp[k]))
			}
		}
	}
}

func (s *Service) strcmp(c *cpu.CPU, bounded bool) {
	a := s.readCString(s.arg(c, 0), 1<<20)
	b := s.readCString(s.arg(c, 1), 1<<20)
	if bounded {
		n := int(s.arg(c, 2))
		if len(a) > n {
			a = a[:n]
		}
		if len(b) > n {
			b = b[:n]
		}
	}
	s.ret(c, uint32(int32(compareBytes(a, b))))
}

func compareBytes(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func (s *Service) strstr(c *cpu.CPU) {
	hay := string(s.readCString(s.arg(c, 0), 1<<20))
	needle := string(s.readCString(s.arg(c, 1), 1<<20))
	idx := strings.Index(hay, needle)
	if idx < 0 {
		s.ret(c, 0)
		return
	}
	s.ret(c, s.arg(c, 0)+uint32(idx))
}

func (s *Service) memchr(c *cpu.CPU) {
	p := s.arg(c, 0)
	ch := byte(s.arg(c, 1))
	n := s.arg(c, 2)
	for i := uint32(0); i < n; i++ {
		if s.readU8(p+i) == ch {
			s.ret(c, p+i)
			return
		}
	}
	s.ret(c, 0)
}

func (s *Service) strchr(c *cpu.CPU) {
	p := s.arg(c, 0)
	ch := byte(s.arg(c, 1))
	for i := uint32(0); ; i++ {
		b := s.readU8(p + i)
		if b == ch {
			s.ret(c, p+i)
			return
		}
		if b == 0 {
			s.ret(c, 0)
			return
		}
	}
}

func (s *Service) strrchr(c *cpu.CPU) {
	p := s.arg(c, 0)
	ch := byte(s.arg(c, 1))
	last := uint32(0)
	found := false
	for i := uint32(0); ; i++ {
		b := s.readU8(p + i)
		if b == ch {
			last = p + i
			found = true
		}
		if b == 0 {
			if !found {
				s.ret(c, 0)
			} else {
				s.ret(c, last)
			}
			return
		}
	}
}

// ---- CRC -------------------------------------------------------------

// crc32 实现 ROM 的 crc32_le/crc32_be：crc 作为初值，不做最终异或。
func crc32(crc uint32, data []byte, reflect bool) uint32 {
	if reflect {
		for _, b := range data {
			crc ^= uint32(b)
			for i := 0; i < 8; i++ {
				if crc&1 != 0 {
					crc = (crc >> 1) ^ 0xEDB88320
				} else {
					crc >>= 1
				}
			}
		}
		return crc
	}
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func crc16(crc uint32, data []byte, reflect bool) uint32 {
	crc &= 0xFFFF
	if reflect {
		for _, b := range data {
			crc ^= uint32(b)
			for i := 0; i < 8; i++ {
				if crc&1 != 0 {
					crc = (crc >> 1) ^ 0x8408
				} else {
					crc >>= 1
				}
			}
		}
		return crc & 0xFFFF
	}
	for _, b := range data {
		crc ^= uint32(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc & 0xFFFF
}

func crc8(crc uint32, data []byte, reflect bool) uint32 {
	crc &= 0xFF
	if reflect {
		for _, b := range data {
			crc ^= uint32(b)
			for i := 0; i < 8; i++ {
				if crc&1 != 0 {
					crc = (crc >> 1) ^ 0x8C
				} else {
					crc >>= 1
				}
			}
		}
		return crc & 0xFF
	}
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc & 0xFF
}

// ---- MD5 -------------------------------------------------------------

// esp_rom_md5_context 布局（88 字节）：
//
//	state[4] @0x00，count[2] @0x10（单位：位），buffer[64] @0x18。
const (
	md5CtxSize  = 88
	md5StateOff = 0
	md5CountOff = 16
	md5BufOff   = 24
)

// md5LoadCtx 从 guest 内存读回 MD5 上下文。
func (s *Service) md5LoadCtx(addr uint32) ([4]uint32, [2]uint32, []byte) {
	raw := s.readBytes(addr, md5CtxSize)
	if raw == nil {
		raw = make([]byte, md5CtxSize)
	}
	var st [4]uint32
	var cnt [2]uint32
	for i := 0; i < 4; i++ {
		st[i] = binary.LittleEndian.Uint32(raw[md5StateOff+i*4:])
	}
	cnt[0] = binary.LittleEndian.Uint32(raw[md5CountOff:])
	cnt[1] = binary.LittleEndian.Uint32(raw[md5CountOff+4:])
	buf := make([]byte, 64)
	copy(buf, raw[md5BufOff:md5CtxSize])
	return st, cnt, buf
}

func (s *Service) md5StoreCtx(addr uint32, st [4]uint32, cnt [2]uint32, buf []byte) {
	for i := 0; i < 4; i++ {
		s.bus.Write(addr+uint32(md5StateOff+i*4), 4, st[i])
	}
	s.bus.Write(addr+md5CountOff, 4, cnt[0])
	s.bus.Write(addr+md5CountOff+4, 4, cnt[1])
	for i := 0; i < 64; i++ {
		s.bus.Write(addr+uint32(md5BufOff+i), 1, uint32(buf[i]))
	}
}

// md5Init 对应 esp_rom_md5_init：复位摘要状态与长度计数。
func (s *Service) md5Init(addr uint32) {
	st := [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}
	s.md5StoreCtx(addr, st, [2]uint32{}, make([]byte, 64))
}

func (s *Service) md5Update(addr, bufAddr, length uint32) {
	st, cnt, buf := s.md5LoadCtx(addr)
	md5LocalUpdate(&st, &cnt, buf, s.readBytes(bufAddr, length))
	s.md5StoreCtx(addr, st, cnt, buf)
}

// md5Final 对应 esp_rom_md5_final(digest, ctx)：补齐 0x80 + 长度，输出 16 字节摘要。
func (s *Service) md5Final(ctxAddr, digestAddr uint32) {
	st, cnt, buf := s.md5LoadCtx(ctxAddr)
	// 先记下最终长度：padding 更新本身会改变 count。
	var enc [8]byte
	binary.LittleEndian.PutUint32(enc[0:], cnt[0])
	binary.LittleEndian.PutUint32(enc[4:], cnt[1])
	idx := int((cnt[0] >> 3) & 0x3F)
	pad := 56 - idx
	if pad <= 0 {
		pad += 64
	}
	var padBuf [64]byte
	padBuf[0] = 0x80
	md5LocalUpdate(&st, &cnt, buf, padBuf[:pad])
	md5LocalUpdate(&st, &cnt, buf, enc[:])
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint32(enc[0:], st[i])
		for j := 0; j < 4; j++ {
			s.bus.Write(digestAddr+uint32(i*4+j), 1, uint32(enc[j]))
		}
	}
}

// md5LocalUpdate 在本地上下文中累积数据（RFC 1321 的 MD5Update）。
func md5LocalUpdate(st *[4]uint32, cnt *[2]uint32, buf []byte, data []byte) {
	idx := int((cnt[0] >> 3) & 0x3F)
	add := uint32(len(data))
	cnt[0] += add << 3
	if cnt[0] < add<<3 {
		cnt[1]++
	}
	cnt[1] += add >> 29
	if idx+len(data) < 64 {
		copy(buf[idx:], data)
		return
	}
	part := 64 - idx
	copy(buf[idx:], data[:part])
	md5Block(st, buf)
	data = data[part:]
	for len(data) >= 64 {
		md5Block(st, data[:64])
		data = data[64:]
	}
	copy(buf, data)
}

var md5K = [64]uint32{
	0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee,
	0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
	0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
	0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
	0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa,
	0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
	0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
	0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
	0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
	0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
	0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05,
	0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
	0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039,
	0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
	0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
	0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391,
}

var md5Shift = [64]int{
	7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
	5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
	4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
	6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
}

func md5Block(st *[4]uint32, p []byte) {
	var m [16]uint32
	for i := 0; i < 16; i++ {
		m[i] = binary.LittleEndian.Uint32(p[i*4:])
	}
	a, b, c, d := st[0], st[1], st[2], st[3]
	for i := 0; i < 64; i++ {
		var f uint32
		var g int
		switch {
		case i < 16:
			f = (b & c) | (^b & d)
			g = i
		case i < 32:
			f = (d & b) | (^d & c)
			g = (5*i + 1) & 15
		case i < 48:
			f = b ^ c ^ d
			g = (3*i + 5) & 15
		default:
			f = c ^ (b | ^d)
			g = (7 * i) & 15
		}
		f += a + md5K[i] + m[g]
		a = d
		d = c
		c = b
		b += (f << md5Shift[i]) | (f >> (32 - md5Shift[i]))
	}
	st[0] += a
	st[1] += b
	st[2] += c
	st[3] += d
}
