import time, struct
from unicorn import *
from unicorn.riscv_const import *

BASE = 0x40000000
code = open('fib.elf','rb').read()
# text 段在 file offset 84, vaddr 0x40000000
text = code[84:84+0x64]

uc = Uc(UC_ARCH_RISCV, UC_MODE_RISCV32)
# ROM 区
uc.mem_map(0x40000000, 1024*1024)
# SRAM 区
uc.mem_map(0x3FC80000, 512*1024)
uc.mem_write(0x40000000, text)

t0 = time.perf_counter()
uc.emu_start(0x40000000, 0x4000000C, timeout=0, count=0)
dt = time.perf_counter() - t0

a0 = uc.reg_read(UC_RISCV_REG_X10)
print(f"Unicorn fib(28): {dt*1000:.1f} ms, a0={a0} (expect 317811)")
