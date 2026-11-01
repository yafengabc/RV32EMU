import time
from unicorn import *
from unicorn.riscv_const import *

BASE = 0x100000
# 与 Go 模拟器相同的循环：cnt 0 -> 2,000,000，每条迭代 3 条指令
code = bytes.fromhex(
    "93020000"   # 0x00: addi x5,x0,0
    "13034000"   # 0x04: addi x6,x0,2000000  (2000000=0x1E8480)
    "93821200"   # 0x08: addi x5,x5,1
    "e3cc62fe"   # 0x0C: blt x5,x6, -8
    "73001000"   # 0x10: ebreak
)
# 修正 addi x6 立即数：2000000 = 0x1E8480
code = bytes.fromhex(
    "93020000"
    "1303841E"   # 占位，下面直接构造
    "93821200"
    "e3cc62fe"
    "73001000"
)

def i_op(rd, rs1, imm, funct3=0, op=0x13):
    return ((imm & 0xFFF) << 20) | (rs1 << 15) | (funct3 << 12) | (rd << 7) | op

def le32(w):
    return w.to_bytes(4, "little")

blob  = le32(i_op(5, 0, 0))
blob += le32(i_op(6, 0, 2000000 & 0xFFF))  # 12-bit imm, 2000000 装不下，改用小值
LIMIT = 2000000
# 用 lui 装大立即数不现实；这里 LIMIT 用 12 位范围，靠重复轮次补
LIMIT = 2047
blob  = le32(i_op(5, 0, 0))
blob += le32(i_op(6, 0, LIMIT))
blob += le32(0x00128293)  # addi x5,x5,1
blob += le32(0xFE62CCE3)  # blt x5,x6,-8
blob += le32(0x00100073)  # ebreak

uc = Uc(UC_ARCH_RISCV, UC_MODE_RISCV32)
uc.mem_map(BASE, 2*1024*1024)
uc.mem_write(BASE, blob)

ROUNDS = 1000  # 每轮 2047 次迭代 ×3 指令
t0 = time.perf_counter()
for _ in range(ROUNDS):
    uc.reg_write(UC_RISCV_REG_X5, 0)
    uc.emu_start(BASE+8, BASE+0x10, timeout=0, count=0)
dt = time.perf_counter() - t0

instr = ROUNDS * LIMIT * 3
print(f"unicorn: {instr} instr in {dt*1000:.1f} ms = {instr/dt/1e6:.1f} MIPS")
