import time, subprocess, os, sys

# 对每个 bench ELF，在 Go 模拟器和 Unicorn 上分别计时
elfs = ['bench_pi.elf', 'bench_sort.elf', 'bench_sieve.elf']

# Go emulator
for elf in elfs:
    t0 = time.perf_counter()
    r = subprocess.run(['./rv32emu.exe', '-elf', elf, '-max', '5000000'],
                       capture_output=True, text=True, timeout=30)
    dt = time.perf_counter() - t0
    # parse instruction count
    inst = 0
    for line in r.stdout.splitlines():
        if 'stopped after' in line:
            inst = int(line.split('after')[1].split('inst')[0].strip())
    print(f"Go  {elf:20s}  {dt*1000:8.1f} ms  {inst:>10d} inst  {inst/dt/1e6:6.1f} MIPS")
