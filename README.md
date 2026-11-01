# RV32EMU — ESP32-C3 行为对齐的 RV32IMC 指令集模拟器

Go 实现，单核，严格对齐 ESP32-C3（RV32IMC，无 F/D 浮点）。
流水线分层：**取指(IF) → 译码(ID) → 执行(EX) → 访存(MEM) → 写回(WB)**，译码表独立可扩展。

## 目录结构

```
rv32emu/
├─ go.mod / main.go            CLI：-elf / -ramdisk / -trace / -step / -max
├─ internal/
│  ├─ cpu/                     CPU 核心
│  │  ├─ cpu.go               状态、Step() 主循环、对齐检查
│  │  ├─ execute.go           RV32I + M(mul/div/rem) 执行语义
│  │  ├─ compressed.go        C 扩展 16-bit 译码执行
│  │  ├─ csr.go               mstatus/mtvec/mepc/mcause + pmpcfg0..3/pmpaddr0..15
│  │  ├─ pmp.go               PMP 强制检查（U-mode 必须命中，M-mode 放行）
│  │  └─ trap.go              异常进入、mret、vectored 派发、中断投递
│  ├─ mem/                     总线 + 地址映射
│  │  ├─ bus.go               Bus/Peripheral 接口
│  │  └─ memory.go            ROM/SRAM/动态 RAM/flash 模拟区，外设整字访问
│  ├─ perip/                   外设
│  │  ├─ uart.go              UART0@0x60000000（FIFO↔stdout/stdin，RX 节流）
│  │  ├─ gpio.go              GPIO@0x60004000
│  │  ├─ c3.go                TIMG0 节拍源 + INTMTX + SPI1 硬件擦除
│  │  ├─ usj.go               USJ@0x60043000（第二个控制台，黑洞模型）
│  │  ├─ rtcntl.go            RTC_CNTL@0x60008000（关看门狗用，被动）
│  │  └─ clint.go             CLINT mtimer@0x02000000（旧路径，C3 不用）
│  ├─ rom/rom.go              ROM 桩：esp_rom_spiflash_read/write/config_param
│  ├─ elf/elf.go              ELF 段加载（ESP-IDF .elf 直接跑）
│  └─ debug/disasm.go         反汇编 / 单步日志
└─ firmware/test_min/          ESP-IDF 最小测试工程
```

## 内存映射（对齐 ESP32-C3 技术参考手册）

| 区域 | 地址范围 |
|------|----------|
| ROM | `0x40000000 ~ 0x4007FFFF`（复位取指点，含 flash 读写桩） |
| SRAM | `0x3FC80000 ~ 0x3FCFFFFF` |
| UART0 | `0x60000000` |
| GPIO | `0x60004000` |
| SPI1 | `0x60002000`（硬件扇区擦除） |
| RTC_CNTL | `0x60008000` |
| USJ | `0x60043000` |
| TIMG0 | `0x6001F000`（节拍源） |
| INTMTX | `0x600C2000` |
| flash 模拟区 | `0x3F000000 + 4 MiB`（ROM 桩把这里当物理 flash） |

内核镜像由 ELF 加载器按 `0x40380000` 起点铺开（C3 的指令/数据双别名在
TinyOS 里没有用到——它是扁平链接的），ramdisk 镜像放到 flash 偏移 `0x100000`。

## 构建与运行

```powershell
go build -o rv32emu.exe .

# 跑 ELF（ESP-IDF 编译产物）
.\rv32emu.exe -elf build\test_min.elf

# 跑 TinyOS C3（内核 + littlefs 镜像）
.\rv32emu.exe -elf ..\TinyOS\build\kernel-esp32c3.elf -ramdisk ..\TinyOS\build\ramdisk-esp32c3.bin
```

CLI 参数（常用的）：

| 参数 | 作用 |
|------|------|
| `-elf` / `-bin` | 固件加载方式 |
| `-ramdisk` | littlefs 分区镜像，装进 flash 模拟区 `0x100000` |
| `-entry` | bin 加载基址（默认 `0x40000000`） |
| `-max N` | 最大指令数（0=跑到停机） |
| `-trace` / `-tracefrom` / `-traceto` | 逐条反汇编，可限定指令区间 |
| `-step N` | 执行 N 条后暂停并 dump 寄存器 |
| `-fault` | 停在第一次**同步异常**并回放前 48 条指令 |
| `-traplog` | 打印每次同步异常（cause/mepc/mtval），过滤掉 ecall |
| `-breakpc ADDR` | 命中该 PC 就停下 dump（配 `-dumpram` 出内存快照） |
| `-watch N` | 监视寄存器 N 的每次变化 |
| `-dumpram FILE` | 退出时导出内核 RAM 镜像（post-mortem 用） |
| `-nopmp` | 关掉 U-mode 的 PMP 强制（A/B 对照） |
| `-rxrate` / `-rxperiod` / `-rxdelay` | 控制台输入节流（见下） |
| `-tickdiv N` | 把 tick 周期拉长 N 倍，验证时序敏感性 |

### 控制台输入为什么要节流

内核的行规程只有 **8 行**队列（`kernel/tty.c`，队满即丢——读者跟不上时
更多预输入没有意义）。真人是在提示符出现后才打字的；脚本若把十几条命令
一次性灌进 stdin，boot 阶段就会被内核吃掉前 8 行。

所以 RX 默认按 tick 分批放行（`-rxrate` 字节 / `-rxperiod` 指令），并且
`-rxdelay` 可以让脚本等 shell 起来再喂：

```powershell
# 脚本化跑一串命令
.\rv32emu.exe ... -rxdelay 3000000 -rxrate 1 -rxperiod 60000 < cmds.txt
```

## 异常 / 中断

- 实现：非法指令(2)、load/store 地址不对齐(4/6)、load/store 访问故障(5/7)、
  ecall(8=U-mode / 11=M-mode)、ebreak(3)、中断(`mcause` 带 bit31)。
- `trap` 进入时 `mepc` 指向**故障指令本身**；中断的 `mepc` 指向被抢占的指令。
- mtvec **强制按 vectored 处理**（C3 硬件只支持 vectored，且基址 256 字节对齐）：
  同步异常跳 base，中断跳 `base + 4*cause`。
- 中断模型对齐 esp-riscv：**没有 `mie` CSR**，中断仅由 `mstatus.MIE` 门控；
  TG0 经 INTMTX 路由到 CPU line 2，tick 上报 `mcause = 0x80000002`。
- PMP：U-mode 必须命中允许的条目，否则 load/store access fault；M-mode 放行
  （内核从不设锁定位）。区间在 `pmpcfg/pmpaddr` 写入后惰性重建。
- ebreak 在本模拟器约定为**停机**（作为固件退出）。

## 跑 TinyOS 的验收结果

`kernel-esp32c3.elf` + `ramdisk-esp32c3.bin` 下已验证：

- 启动横幅、`kernel heap: 32 KiB arena`、littlefs 14 个文件的列表
- `tick self-check`、`starting /bin/sh`、`tos>` 提示符
- 内置命令 `help` / `ls` / `ps` / `uptime`
- 用户态程序：`hello`、`hello_printf`、`cat`、`mem`、`spin`
- `wfile a b` 写 flash → `cat a` 读回（ROM 桩 + SPI1 擦除 + littlefs 全链路）
- `crash`：非法指令只杀该进程，shell 存活
- `run spin &`：后台任务按 50 ms 时间片被抢占，前台 shell 仍可用
- `prot rodata|heap|stack|null`：四条越界全部按真机语义 fault 并杀掉任务

## 自测

```powershell
go test ./...
```

覆盖：UART0 字符输出、M 扩展乘除（含有符号 `-7/3`）、C 压缩指令 `c.addi`、非法指令异常 mcause=2。

## ESP-IDF 测试固件

`firmware/test_min/`：
```powershell
cd firmware\test_min
idf.py set-target esp32c3
idf.py build
# 产物 build\test_min.elf 直接喂给模拟器：
..\..\rv32emu.exe -elf build\test_min.elf
```
固件直接写 UART0 FIFO 寄存器打印结果，模拟器映射到 stdout，行为与真实 C3 一致。

## 性能对比（i9-11950H，Windows）

| 基准 | Go 模拟器 | Unicorn 2.1.4 | 比值 |
|------|-----------|---------------|------|
| 紧循环 ALU（6M 指令） | ~58 MIPS | ~800 MIPS | 13.8x |
| fib(28) 递归（15.4M 指令） | 59.6 MIPS | 120.5 MIPS | 2.0x |
| TinyGo 运行时（5M 指令，含 pi/排序/素数筛） | 31.7 MIPS | 67.4 MIPS | 2.1x |

真实算法负载（TinyGo 运行时）下 Unicorn 仅快 2 倍——JIT 优势被内存访存和分支稀释。

## TinyGo 固件验证

使用 TinyGo 交叉编译为 riscv-qemu 裸机目标：

```powershell
cd firmware/bench
tinygo build -target=riscv-qemu -o bench.elf .
../../rv32emu.exe -elf bench.elf -max 5000000
```

模拟器成功跑通 TinyGo 运行时（启动 + BSS 清零 + 算法计算 + GC），证明取指→译码→执行→访存→异常链路正确。

## 当前边界 / 后续扩展

- 外设覆盖 TinyOS 实际用到的部分：UART0、GPIO 影子、TIMG0 节拍、INTMTX
  （被动影子）、SPI1 扇区擦除、USJ（黑洞）、RTC_CNTL（被动）、ROM flash 桩。
  未建模的寄存器一律返回 0 并按地址去重告警，便于发现新缺口。
- ROM 没有真机 dump：`esp_rom_spiflash_read/write/config_param` 用三条手写
  RV32I 蹦床桩顶替（`internal/rom`），行为等价于「按字节 memcpy 到 flash 模拟区」。
- 未做精确外设时序：tick 按「每 N 条指令」计，而不是墙钟。
- **已知的内核侧时序问题**（不是模拟器缺陷）：tick 恰好落在 `sched_pick` 扫描
  中途时，`process_tick` 会把等待输入的任务置为 `ST_READY`，而 `sched_pick`
  已经扫过该槽位，本轮返回 NULL；此时 `proc_waiting()` 已为假，于是走到
  `panic("no runnable process")`。现象是 shell 偶发 panic，把 tick 周期拉长
  （`-tickdiv 2`）即消失。QEMU 因时序不同不易命中。最小复现：

  ```
  run wfile a xxxxxxxx...（180 字节左右）
  run hello
  ```

  修法在 TinyOS 侧：`sched_switch` 在 panic 前重新扫一次，或在扫描期间关中断。
