import time, struct, subprocess
from unicorn import *
from unicorn.riscv_const import *

elf = 'firmware/bench/bench_qemu.elf'
N = 5_000_000

# Go emulator
t0 = time.perf_counter()
r = subprocess.run(['./rv32emu.exe', '-elf', elf, '-max', str(N)],
                   capture_output=True, text=True, timeout=60)
dt_go = time.perf_counter() - t0
print(f"Go emulator:  {dt_go*1000:8.1f} ms for {N} inst  =  {N/dt_go/1e6:6.1f} MIPS")

# Unicorn
data = open(elf, 'rb').read()
e_entry = struct.unpack_from('<I', data, 24)[0]
e_phoff = struct.unpack_from('<I', data, 28)[0]
e_phnum = struct.unpack_from('<H', data, 44)[0]

uc = Uc(UC_ARCH_RISCV, UC_MODE_RISCV32)
BASE = 0x80000000
SIZE = 16*1024*1024
uc.mem_map(BASE, SIZE)

for i in range(e_phnum):
    off = e_phoff + i*32
    p_type, p_offset, p_vaddr, p_paddr, p_filesz, p_memsz, p_flags, p_align = struct.unpack_from('<IIIIIIII', data, off)
    if p_type != 1:
        continue
    seg_data = data[p_offset:p_offset+p_filesz]
    if len(seg_data):
        uc.mem_write(p_paddr, seg_data)

uc.reg_write(UC_RISCV_REG_SP, BASE + SIZE - 16)

# Auto-map pages on unmapped access
def hook_mem(uc, access, addr, size, value, user):
    page = addr & ~0xFFF
    try:
        uc.mem_map(page, 0x1000)
    except:
        pass
    return True

uc.hook_add(UC_HOOK_MEM_READ_UNMAPPED | UC_HOOK_MEM_WRITE_UNMAPPED | UC_HOOK_MEM_FETCH_UNMAPPED, hook_mem)

t0 = time.perf_counter()
uc.emu_start(e_entry, 0, count=N)
dt_uc = time.perf_counter() - t0
print(f"Unicorn:      {dt_uc*1000:8.1f} ms for {N} inst  =  {N/dt_uc/1e6:6.1f} MIPS")

print(f"\nRatio: Unicorn is {dt_go/dt_uc:.1f}x faster than Go")

