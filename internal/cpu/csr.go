package cpu

// CSR 地址（machine 级，RV32）。
const (
	csrMstatus   uint32 = 0x300
	csrMisa      uint32 = 0x301
	csrMedeleg   uint32 = 0x302
	csrMideleg   uint32 = 0x303
	csrMie       uint32 = 0x304
	csrMtvec     uint32 = 0x305
	csrMscratch  uint32 = 0x340
	csrMepc      uint32 = 0x341
	csrMcause    uint32 = 0x342
	csrMtval     uint32 = 0x343
	csrMip       uint32 = 0x344
	csrMcycle    uint32 = 0xB00
	csrMinstret  uint32 = 0xB02
	csrMhartid   uint32 = 0xF14
	// PMP（RV32EMU 不强制 PMP，但内核会写；必须全部接受以免非法指令 trap）
	csrPmpcfg0  uint32 = 0x3A0
	csrPmpcfg1  uint32 = 0x3A1
	csrPmpcfg2  uint32 = 0x3A2
	csrPmpcfg3  uint32 = 0x3A3
	csrPmpaddr0 uint32 = 0x3B0
	csrPmpaddr15 uint32 = 0x3BF
)

// mstatus 位。
const (
	mstatusMIE  uint32 = 1 << 3
	mstatusMPIE uint32 = 1 << 7
	mstatusMPP  uint32 = 3 << 11 // bits 11:12：进入异常前的特权级
)

// mie / mip 位（MTIP=bit7）。
const mieMTIE uint32 = 1 << 7
const mipMTIP uint32 = 1 << 7

// CSRFile 维护机器级状态寄存器。
type CSRFile struct {
	mstatus  uint32
	misa     uint32
	mie      uint32
	mtvec    uint32
	mscratch uint32
	mepc     uint32
	mcause   uint32
	mtval    uint32
	mip      uint32
	mcycle   uint64
	minstret uint64
	pmpcfg   [4]uint32
	pmpaddr  [16]uint32
	// pmpDirty 标记配置被改写，pmpCheck 首次访问时重建解析后的区间表。
	pmpDirty bool

	// custom 保存 ESP32-C3 的自定义 CSR（0x7C0~0x7FF，如 mhint/mintthresh）。
	// 固件启动时会读写这些寄存器；精确语义不影响执行，按普通寄存器处理，
	// 至少不能因为「未知 CSR」被当成非法指令。
	custom [64]uint32
}

func newCSRFile() *CSRFile {
	// misa: XLEN='01'(bits 31:30)，base I=bit8，M=bit12，C=bit2
	misa := uint32(1<<30) | (1 << 8) | (1 << 12) | (1 << 2)
	// 复位处于 M-mode（MPP=3），MIE 关。
	return &CSRFile{misa: misa, mstatus: mstatusMPP}
}

// Mcause 返回当前 mcause（测试/调试用）。
func (c *CSRFile) Mcause() uint32 { return c.mcause }

// Mtvec 返回 mtvec 原值（含低 2 位模式，调试用）。
func (c *CSRFile) Mtvec() uint32 { return c.mtvec }

// Read 返回 CSR 值；非法/只读访问返回 (0, false)。
func (c *CSRFile) Read(addr uint32) (uint32, bool) {
	switch addr {
	case csrMstatus:
		return c.mstatus, true
	case csrMisa:
		return c.misa, true
	case csrMedeleg, csrMideleg:
		return 0, true
	case csrMie:
		return c.mie, true
	case csrMtvec:
		return c.mtvec, true
	case csrMscratch:
		return c.mscratch, true
	case csrMepc:
		return c.mepc, true
	case csrMcause:
		return c.mcause, true
	case csrMtval:
		return c.mtval, true
	case csrMip:
		return c.mip, true
	case csrMcycle:
		return uint32(c.mcycle), true
	case csrMinstret:
		return uint32(c.minstret), true
	case csrMhartid:
		return 0, true
	case csrPmpcfg0, csrPmpcfg1, csrPmpcfg2, csrPmpcfg3:
		return c.pmpcfg[addr-csrPmpcfg0], true
	}
	if addr >= csrPmpaddr0 && addr <= csrPmpaddr15 {
		return c.pmpaddr[addr-csrPmpaddr0], true
	}
	if addr >= 0x7C0 && addr <= 0x7FF {
		return c.custom[addr-0x7C0], true
	}
	return 0, false
}

// Write 写 CSR；返回是否合法。
func (c *CSRFile) Write(addr uint32, val uint32) bool {
	switch addr {
	case csrMstatus:
		// 保留已实现的位 + MPP（当前特权级），避免任何 RMW 抹掉模式位
		c.mstatus = val & (mstatusMIE | mstatusMPIE | mstatusMPP)
	case csrMie:
		c.mie = val
	case csrMtvec:
		c.mtvec = val
	case csrMscratch:
		c.mscratch = val
	case csrMepc:
		c.mepc = val
	case csrMcause:
		c.mcause = val
	case csrMtval:
		c.mtval = val
	case csrMcycle:
		c.mcycle = uint64(val)
	case csrMinstret:
		c.minstret = uint64(val)
	case csrMisa, csrMhartid, csrMedeleg, csrMideleg:
		return false // 只读
	case csrPmpcfg0, csrPmpcfg1, csrPmpcfg2, csrPmpcfg3:
		c.pmpcfg[addr-csrPmpcfg0] = val
		c.pmpDirty = true
	default:
		if addr >= csrPmpaddr0 && addr <= csrPmpaddr15 {
			c.pmpaddr[addr-csrPmpaddr0] = val
			c.pmpDirty = true
			return true
		}
		if addr >= 0x7C0 && addr <= 0x7FF {
			c.custom[addr-0x7C0] = val
			return true
		}
		return false
	}
	return true
}
