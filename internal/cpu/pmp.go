package cpu

// PMP（物理内存保护）强制检查。
//
// ESP32-C3 的 U-mode 只能通过 PMP 访问内存：没有匹配条目时 M-mode 放行、
// U-mode 拒绝（真机实测空 PMP 下 U-mode 第一条取指就 fault）。TinyOS 的
// arch/rv32/platform.c 正是靠这套机制给每个任务切四段窗口：
//
//	0  off(win_base)                    —— 只提供下界，不参与匹配
//	1  TOR RX [win_base, wx)            —— 代码段可读可执行
//	2  TOR RW [wx, brk)                 —— 数据段 + 堆
//	3  NAPOT RW [stack_low, top)        —— 栈（必须 NAPOT，TOR 的下界会
//	                                       把堆栈之间的空洞填上）
//
// 内核从不设置 L（锁定）位，所以 M-mode 一律放行；这也是「单一平坦地址
// 空间 + 只有用户侧受约束」这个模型能成立的前提。

// 访问类型位（与 pmpcfg 的 R/W/X 位一一对应）。
const (
	pmpRead  uint32 = 1 << 0
	pmpWrite uint32 = 1 << 1
	pmpExec  uint32 = 1 << 2
)

// pmpEntry 是一条已解析生效的 PMP 区间。
type pmpEntry struct {
	perm uint32 // R/W/X 位
	base uint32
	top  uint32 // 开区间上界
	all  bool   // NAPOT 全 1：覆盖整个地址空间
}

// pmpRebuild 在内核改写 pmpcfg/pmpaddr 后重建紧凑区间表。
//
// 直接每次访存扫 16 条配置会让 U-mode 的每条 load/store 多花一轮循环，
// 而这正是用户程序跑的主路径。解析后通常只剩 3~4 条，且天然保持了
// 「编号最小者优先」的语义（TOR 的下界来自前一条地址）。
func (c *CPU) pmpRebuild() {
	c.csr.pmpDirty = false
	c.pmpEnt = c.pmpEnt[:0]
	for i := 0; i < 16; i++ {
		cfg := (c.csr.pmpcfg[i/4] >> (8 * uint(i%4))) & 0xFF
		a := (cfg >> 3) & 3
		if a == 0 { // OFF：不参与匹配
			continue
		}
		e := pmpEntry{perm: cfg & 0x7}
		switch a {
		case 1: // TOR：区间 [pmpaddr[i-1]<<2, pmpaddr[i]<<2)
			if i > 0 {
				e.base = c.csr.pmpaddr[i-1] << 2
			}
			e.top = c.csr.pmpaddr[i] << 2
		case 2: // NA4：4 字节
			e.base = c.csr.pmpaddr[i] << 2
			e.top = e.base + 4
		case 3: // NAPOT：地址尾部连续 1 的个数决定区间大小
			v := c.csr.pmpaddr[i]
			t := 0
			for t < 32 && (v>>uint(t))&1 == 1 {
				t++
			}
			if t == 32 { // 全 1 = 覆盖全空间（规范的边角条款）
				e.all = true
			} else if t > 0 {
				e.base = (v &^ (uint32(1)<<uint(t) - 1)) << 2
				e.top = e.base + (uint32(1) << uint(t+3))
			}
			// t == 0 表示编码非法，等同 OFF
		}
		if !e.all && e.top <= e.base {
			continue // 空区间
		}
		c.pmpEnt = append(c.pmpEnt, e)
	}
}

// pmpCheck 判断一次访存是否允许。M-mode 直接放行（无锁定项）。
func (c *CPU) pmpCheck(addr, size uint32, access uint32) bool {
	if !c.pmpEnforce || c.priv != 0 {
		return true
	}
	if c.csr.pmpDirty {
		c.pmpRebuild()
	}
	for _, e := range c.pmpEnt {
		if !e.all && !(addr >= e.base && addr+size > addr && addr+size <= e.top) {
			continue
		}
		// 编号最小的匹配条目生效：权限不足就是拒绝，不再继续找。
		return access&e.perm == access
	}
	return false // U-mode 无匹配 = 拒绝
}
