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
)

// mstatus 位。
const (
	mstatusMIE   uint32 = 1 << 3
	mstatusMPIE  uint32 = 1 << 7
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
}

func newCSRFile() *CSRFile {
	// misa: XLEN=2('01' at bits 31:30)，base I=bit8，M=bit12，C=bit2
	misa := uint32(1<<30) | (1 << 8) | (1 << 12) | (1 << 2)
	return &CSRFile{misa: misa}
}

// Read 返回 CSR 值；非法/只读访问返回非法指令信号（由调用方转成 trap）。
func (c *CSRFile) Mcause() uint32 { return c.mcause }
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
	}
	return 0, false
}

// Write 写 CSR；返回是否合法。
func (c *CSRFile) Write(addr uint32, val uint32) bool {
	switch addr {
	case csrMstatus:
		// 只保留实现的位
		c.mstatus = val & (mstatusMIE | mstatusMPIE)
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
	default:
		return false
	}
	return true
}
