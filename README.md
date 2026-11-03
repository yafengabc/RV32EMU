# RV32EMU — ESP32-C3 行为对齐的 RV32IMC 指令集模拟器

Go 实现，单核，严格对齐 ESP32-C3（RV32IMC，无 F/D 浮点，无 A 原子扩展）。

执行有两条路径，共用同一份语义实现：

- **`Step()`**：逐条取指 → 译码 → 执行 → 访存。`-trace` / `-step` / `-fault` 和单测走这条。
- **`RunHot(n)`**：默认路径。把直线代码段预解码成最多 32 条操作，缓存在 512 槽直接映射表里；
  以 4 KiB 代码页版本号失效，块尾必是控制转移；跑完一块直接跳后继块（块链），不返回调用方。
  另有等价性测试（`runhot_equiv_test.go`）保证两条路径结果一致。

## 目录结构

```
rv32emu/
├─ go.mod / main.go            CLI：-elf / -ramdisk / -trace / -step / -max
├─ ctl.go                      -ctl 模式：stdin/stdout 上的 JSON Lines 控制协议
├─ gui/                        图形界面（Gio）：加载镜像、UART 终端、看门狗/ADC 面板
├─ tools/benchrun/             计时器（与 rv32emu-c 的 -bench 同一口径）
├─ internal/
│  ├─ cpu/                     CPU 核心
│  │  ├─ cpu.go               状态、Step() 参考解释器、对齐检查
│  │  ├─ block.go             基本块解码缓存 + RunHot() 块链执行（默认路径）
│  │  ├─ execute.go           RV32I + M(mul/div/rem) 执行语义
│  │  ├─ compressed.go        C 扩展 16-bit 译码执行
│  │  ├─ csr.go               mstatus/mtvec/mepc/mcause + pmpcfg0..3/pmpaddr0..15
│  │  ├─ pmp.go               PMP 强制检查（U-mode 必须命中，M-mode 放行）
│  │  ├─ trap.go              异常进入、mret、vectored 派发、中断投递
│  │  └─ *_test.go            RunHot/Step 等价性、C 扩展、自修改代码、性能基准
│  ├─ mem/                     总线 + 地址映射
│  │  ├─ bus.go               Bus/Peripheral 接口
│  │  └─ memory.go            ROM/SRAM/动态 RAM/flash 模拟区，外设整字访问
│  ├─ perip/                   外设
│  │  ├─ uart.go              UART0/UART1（FIFO↔stdout/stdin，RX 中断/节流）
│  │  ├─ gpio.go              GPIO@0x60004000
│  │  ├─ c3.go                TIMG0 节拍源 + INTMTX 路由 + SPI0/SPI1 flash 控制器
│  │  ├─ c3more.go            UART1 / SPI2(GP-SPI) / I2C0 + 其余寄存器影子外设
│  │  ├─ cache.go             flash cache/XIP：EXTMEM 寄存器 + MMU 表 + DROM/IROM 窗口
│  │  ├─ systimer.go          SYSTIMER@0x60023000（计数器 + 报警）
│  │  ├─ usj.go               USJ@0x60043000（第二个控制台，黑洞模型）
│  │  ├─ rtcntl.go            RTC_CNTL@0x60008000（关看门狗用，被动）
│  │  ├─ efuse.go             eFuse@0x60008800（只读：MAC + 芯片版本）
│  │  └─ clint.go             CLINT mtimer@0x02000000（旧路径，C3 不用）
│  ├─ rom/                     ROM 服务桩（无真实 ROM 镜像）
│  │  ├─ rom.go               ROM 代码区里的 flash 桩
│  │  └─ service.go           ROM libc / printf / cache / crc 等函数按地址分派
│  ├─ soc/                     整机装配：总线 + CPU + 外设 + flash（CLI/GUI 共用）
│  ├─ session/                 上电会话：模拟循环 + 命令 + 事件（-ctl 与 GUI 共用）
│  ├─ elf/elf.go               ELF 段加载（ESP-IDF .elf 直接跑）
│  └─ debug/disasm.go          反汇编 / 单步日志
└─ firmware/test_min/          ESP-IDF 最小测试工程
```

## 内存映射（对齐 ESP32-C3 技术参考手册）

| 区域 | 地址范围 |
|------|----------|
| ROM | `0x40000000 ~ 0x4007FFFF`（复位取指点，含 flash 读写桩） |
| SRAM | `0x3FC80000 ~ 0x3FCFFFFF` |
| UART0 | `0x60000000` |
| GPIO | `0x60004000` |
| SPI1 | `0x60002000`（flash 编程：硬件擦除位 + USER 模式） |
| SPI0 | `0x60003000`（cache/flash 控制器：USER 模式读 ID/读数据） |
| IO_MUX | `0x60009000`（寄存器影子） |
| RTC_CNTL | `0x60008000`（窗口到 `0x600087FF`） |
| eFuse | `0x60008800`（只读：MAC、芯片版本） |
| RTC_I2C | `0x6000E000`（寄存器影子） |
| UART1 | `0x60010000`（TX 丢弃，无 RX） |
| I2C0 | `0x60013000`（命令列表模型，无外部从机） |
| UHCI0 / RMT / LEDC | `0x60014000` / `0x60016000` / `0x60019000`（影子） |
| TIMG0 | `0x6001F000`（节拍源） |
| TIMG1 | `0x60020000`（影子） |
| SYSTIMER | `0x60023000`（2 个 52 位计数器 + 3 个比较器） |
| SPI2 | `0x60024000`（GP-SPI，无外部器件时环回） |
| SYSCON | `0x60026000`（寄存器影子） |
| TWAI / I2S0 | `0x6002B000` / `0x6002D000`（影子） |
| AES/SHA/RSA/DS/HMAC/GDMA | `0x6003A000 ~ 0x6003F000`（影子） |
| APB_SARADC / USJ | `0x60040000` / `0x60043000` |
| INTMTX | `0x600C2000`（source→CPU line 路由） |
| EXTMEM / MMU | `0x600C4000`（控制寄存器 + `0x600C5000` 起 128 项 MMU 表） |
| SYSTEM / SENSITIVE | `0x600C0000` / `0x600C1000`（SYSTEM 的 `FROM_CPU_INTR0..2` 建模为软件中断，其余影子） |
| AES_XTS / ASSIST_DEBUG / DEDICATED_GPIO / WORLD_CNTL | `0x600CC000 ~ 0x600D0000`（影子） |
| DROM / IROM | `0x3C000000` / `0x42000000`（各 8 MiB XIP 窗口） |
| flash 模拟区 | `0x3F000000 + 4 MiB`（ROM 桩把这里当物理 flash） |

WiFi/BLE 相关的 NRX（`0x6001CC00`）、BB（`0x6001D000`）与 RF 前端（FE/FE2）不在
建模范围内；对这些地址的访问仍按「读 0/写丢弃」处理并去重告警。

内核镜像由 ELF 加载器按 `0x40380000` 起点铺开（C3 的指令/数据双别名在
TinyOS 里没有用到——它是扁平链接的），ramdisk 镜像默认放到 flash 偏移
`0x80000`（512 KiB），可用 `-flashoff` 指定其他位置。

## 构建与运行

```powershell
go build -o rv32emu.exe .
go build -o rv32gui.exe .\gui     # 图形界面

# 跑 ELF（ESP-IDF 编译产物）
.\rv32emu.exe -elf build\test_min.elf

# 跑 TinyOS C3（内核 + littlefs 镜像）
.\rv32emu.exe -elf ..\TinyOS\build\kernel-esp32c3.elf -ramdisk ..\TinyOS\build\ramdisk-esp32c3.bin

# 跑官方 MicroPython（整片 flash 镜像）
.\rv32emu.exe -flashimg fw.bin

# 图形界面
.\rv32gui.exe
```

CLI 参数（常用的）：

| 参数 | 作用 |
|------|------|
| `-elf` / `-bin` | 固件加载方式 |
| `-ramdisk` | littlefs 分区镜像，装进 flash 模拟区 |
| `-flashoff` | ramdisk 在 flash 中的偏移（默认 `0x80000`，支持十进制/`0x` 十六进制） |
| `-flashimg` | 整片 SPI flash 镜像（如 `fw.bin`）：按镜像内分区表找应用并启动 |
| `-entry` | bin 加载基址（默认 `0x40000000`） |
| `-max N` | 最大指令数（0=跑到停机） |
| `-trace` / `-tracefrom` / `-traceto` | 逐条反汇编，可限定指令区间 |
| `-traceirq N` / `-traceirqn M` | 第 N 次中断后开始逐条跟踪 M 条（看 trap 入口） |
| `-step N` | 执行 N 条后暂停并 dump 寄存器 |
| `-fault` | 停在第一次**同步异常**并回放前 48 条指令 |
| `-traplog` | 打印每次同步异常（cause/mepc/mtval），过滤掉 ecall |
| `-breakpc ADDR` | 命中该 PC 就停下 dump（配 `-dumpram` 出内存快照） |
| `-watch N` | 监视寄存器 N 的每次变化 |
| `-dumpram FILE` | 退出时导出内核 RAM 镜像（post-mortem 用） |
| `-nopmp` | 关掉 U-mode 的 PMP 强制（A/B 对照） |
| `-rxrate` / `-rxperiod` / `-rxdelay` | 控制台输入节流（见下） |
| `-tickdiv N` | 把 tick 周期拉长 N 倍，验证时序敏感性 |
| `-ctl` | 控制模式：stdin/stdout 走 JSON Lines，供 GUI/脚本驱动 |

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
- **软件复位**：内核 `reboot` 写 `RTC_CNTL_OPTIONS0` 的 `SW_SYS_RST`(bit31) 时，
  模拟器把整台机器重装一遍——SRAM、外设寄存器、CPU 全部回到复位状态，只有
  SPI flash 的内容（`wfile` 写进去的东西）和主机侧尚未送达的控制台输入留下。
  这跟真机一致：C3 复位后 ROM 报 `rst:0x3 RTC_SW_SYS_RST`，随后从 flash 重新启动。
  脚本里连着喂 `reboot` 之后的命令会丢一部分字节（复位瞬间内核行规程里预输入
  的那些行跟着机器一起没了），这是真机行为，不是模拟器的毛病。

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
- `reboot`：软件复位 → 第二次 boot 到 `tos>`，flash 里的文件还在

## 自测

```powershell
go test ./...
```

覆盖：UART0 字符输出与 RX 中断（FULL/TOUT/INT_CLR）、UART1、SPI2 环回与
TRANS_DONE、I2C0 命令列表（DONE/FIFO 计数/中断）、寄存器影子读回、M 扩展乘除
（含有符号 `-7/3`）、C 压缩指令（`c.addi`、`c.lwsp/c.swsp`、分支/跳转偏移）、
非法指令异常 `mcause=2`。

另外 `runhot_equiv_test.go` 专门盯两条执行路径的一致性：整段程序 `RunHot` 与逐条 `Step`
的寄存器/PC 必须相同，解码出的块逐条重放也要相同；还有两条自修改代码用例，保证块缓存在
代码被改写后不会命中过期块。`perf_test.go` / `cpu_bench_test.go` 是性能基准（`go test -bench .` 出 ns/op，
换算成每条指令的耗时即可和上面的 MIPS 对照）。

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

## 跑官方 MicroPython 固件

官方 ESP32-C3 固件（IDF 构建的 `ESP32_GENERIC_C3`）可以整片 flash 镜像直接启动：

```powershell
.\rv32emu.exe -flashimg fw.bin
```

- 镜像里的 bootloader / 分区表 / 应用由 Go 侧 `loadAppImage` 解析：按分区表找到
  app，建立 IROM/DROM 的 MMU 映射，再把 RAM 段拷到目标地址，PC 直接落在 app entry。
- 分区表校验通过：ROM 里的 `esp_rom_md5_init/update/final` 按 RFC 1321 实现
  （此前是返回 0 的空桩，摘要恒为零导致 `Partition table MD5 mismatch` 并回退到
  编译期默认分区表）。stderr 不再出现 `Partition table MD5 mismatch` /
  `load_partitions returned 0x103`。
- flash 未写入区域按擦除态 `0xFF` 初始化，LittleFS 分区因此被判定为空，首次启动
  打印 `Performing initial setup`（格式化）。
- 当前状态：首次启动的 `inisetup.py` 完整走通（LittleFS 格式化 → 挂载 → 创建
  `boot.py`），随后进入 MicroPython `>>> REPL`，可交互执行代码（`import os`、`print`、
  `os.stat('/')` 均正常）。FreeRTOS tick 由 SYSTIMER TARGET0 经 INTMTX 投递，
  `vTaskDelay` 不再阻塞启动。WiFi/BLE 没有建模。
  开发中定位过的两个坑：flash 状态寄存器缺 WEL 语义会让 LittleFS 报
  `ESP_ERR_NOT_FOUND (-261)`；ROM 桩缺 `__bswapsi2` / `strspn` / `strcspn`
  会让元数据 tag 全为 0 并报 `OSError: 84`（`LFS2_ERR_CORRUPT`）。

## 性能

### 口径

只计模拟执行本身：`RunHot(1000)` 批次 + TIMG0 tick，不含进程启动、ELF 加载和终端输出；
每个负载跑 7 次取中位数。Go 侧用 `tools/benchrun`（与 `rv32emu-c -bench` 是同一套批次循环），
C 侧用 `-bench`。主机：AMD Ryzen 7 255（8C/16T），Windows 11，Go 1.27.3，gcc 16.2.0，
C 侧编译选项 `-O3 -DNDEBUG -flto`（不含 `-march=native`）。

自终止程序（fib、排序、筛法）跑到停机；死循环负载（pi、ALU、内存读写）给 2M 指令预算，
连跑三次取最好的一次。短窗口对主机状态很敏感，这类数字按区间看。

### 当前吞吐

| 负载 | 指令数 | MIPS |
|---|---:|---:|
| fib(28) 递归 | 15,426,855 | ~381 |
| pi（Leibniz，含 M 扩展除法） | 2,000,000 | ~379 |
| ALU 紧循环 | 2,000,000 | ~429 |
| 内存读写循环 | 2,000,000 | ~441 |
| 冒泡排序（19,950,006 条，自然结束） | 19,950,006 | ~412 |
| 素数筛（19,931,166 条，自然结束） | 19,931,166 | ~465 |
| TinyOS 启动到 shell | 5,000,000 | ~345 |

提速主要来自 `RunHot` 的基本块缓存和块链：之前逐条 `Step()` 的版本在 i9-11950H 上
fib(28) 约 59.6 MIPS，现在同一负载在本机约 383 MIPS（主机不同，只看量级）。

### 和 C 移植的对比

`rv32emu-c` 是同一份设计的 C11 移植，独立仓库
[yafengabc/RV32EMU-C](https://github.com/yafengabc/RV32EMU-C)。同一台机器、同一批镜像、
同一套批次循环下：

| 负载 | Go MIPS | C MIPS | C/Go |
|---|---:|---:|---:|
| fib(28) 递归 | ~381 | ~441 | 1.16x |
| pi（Leibniz） | ~379 | ~432 | 1.14x |
| ALU 紧循环 | ~429 | ~441 | 1.03x |
| 内存读写循环 | ~441 | ~529 | 1.20x |
| 冒泡排序 | ~412 | ~465 | 1.13x |
| 素数筛 | ~465 | ~503 | 1.08x |
| TinyOS 启动到 shell | ~345 | ~406 | 1.18x |

- 总体 **1.02x ~ 1.22x**：访存/分支密集的负载差距最大，纯 ALU 循环几乎持平。
- 纯 ALU 和除法循环已基本追平：位域解析被块缓存摊掉之后，一个块里边界检查和接口调用
  所剩无几；C 的余量主要来自 load/store 路径（`priv`/PMP/平铺区判断）和更紧凑的代码。
- 两侧都会随主机状态波动（Go 的 pi 中位数见过 368~430 MIPS、TinyOS 285~357），
  比值按区间看，不要当成固定倍率。
- **QEMU 交叉验证**：`rv32emu-c/bench/qemu_check.py` 把同样四个程序跑在 QEMU 上
  （乐鑫 `esp32c3` 机器按真机 C3 映射，或上游 `virt` 重链接），输出的 a0 与 Go/C
  以及独立的 Python 模型一致。QEMU 是 TCG/JIT，同一台机器上约 1.1~6.5 GIPS
  （递归 fib ~1115、pi ~6490、排序 ~2640、筛法 ~3550 MIPS），比 Go/C 解释器快
  2.5~15 倍，循环越紧差距越大；详见该仓库 README 的「在 QEMU 上验证」。
- 一键复现（需要 RV32EMU-C 仓库）：

```powershell
cd rv32emu-c
.\bench\compare.ps1 -Reps 7 -IncludeTinyOS
```

### 为什么自己写模拟器

这个项目起因于 QEMU：TinyOS 在 QEMU 上跑基准时频繁超时，最后定位到 `memcpy` 这类
循环上性能劣化 100 多倍。根因是 **store 命中已翻译成 TB 的代码页**：每次写都让包含
该循环的 TB 失效、下一轮重新翻译，JIT 结果活不过一次写。

C 版 README 里的实测可以复现这个现象：同一份 `bench`（256 KiB 拷贝）在 rv32emu 上
25 ms，在乐鑫 QEMU 的 esp32c3 机器上 9.1 s，约 360 倍，而且随数据量非线性恶化。

用 `-d in_asm` 能看到 8192 次迭代里同一段循环的 5 个 TB 各被重译了 8198~8201 次。

`RunHot` 的块缓存没这个问题：失效粒度是 4 KiB 页的版本号，数据区与代码页分离，
写数据不会触发重译；即使自修改代码，重译成本也就是最多 32 条指令。
所以 TinyOS 的速度数据用自家解释器 / C 移植来测，QEMU 只做功能对照。

### 与 Unicorn 2.1.4：旧表已作废

早期 README 里那张 Unicorn 对比表（紧循环 13.8x、fib 2.0x、TinyGo 2.1x）是在另一台机器、
另一版 Unicorn 上测的，用本仓库依赖的 MSYS2 包**复现不出来**：
`mingw-w64-ucrt-x86_64-unicorn`（QEMU TCG，2.1.4）的 RISC-V 前端跑不了这些镜像——

- 长 TB 里被跳转的分支会把**跳转目标**当成分支执行，随后在 TB 末尾抛 `UC_ERR_EXCEPTION`；
- `EBREAK` 不被解码（`UC_ERR_INSN_INVALID`），`ECALL` 能进 `UC_HOOK_INTR`；
- 越界 load/store 会悄悄落到相邻 4 KiB 页；
- `uc_emu_start(..., count=N)` 在 TB 边界裁掉尾部分支，循环镜像直接跳出循环。

能稳定跑的只有单页直线代码：16,000 条 `nop` 在 Python 绑定下约 0.018 s（≈0.9 MIPS），
加 `UC_HOOK_CODE`（与 Go/C 一样逐条记账）掉到 0.74 MIPS；同一段代码 C 侧约 0.00003 s。
也就是说在「每条指令都要记账」的口径下，JIT 并不占便宜。

结论：那张表不再是有效参考。要么换成能正确执行 RISC-V 的 Unicorn/QEMU 版本重测，
要么就以 Go vs C 的对比为准；复现方法和结论在 `rv32emu-c/bench/README.md`。

## TinyGo 固件验证

使用 TinyGo 交叉编译为 riscv-qemu 裸机目标：

```powershell
cd firmware/bench
tinygo build -target=riscv-qemu -o bench.elf .
../../rv32emu.exe -elf bench.elf -max 5000000
```

模拟器成功跑通 TinyGo 运行时（启动 + BSS 清零 + 算法计算 + GC），证明取指→译码→执行→访存→异常链路正确。

## C 移植版

同一套 RV32IMC/ESP32-C3 模拟器的 C11 移植在独立仓库
[yafengabc/RV32EMU-C](https://github.com/yafengabc/RV32EMU-C)：平铺总线、基本块缓存、
块链执行、PMP/trap 语义和全部外设模型一一对应。逐项速度对比见上面的
「和 C 移植的对比」，构建与运行见该仓库的 `README.md`。

## 当前边界

### 指令集 / CPU

- **RV32IMC**：I（`fence` 视为 nop）、M、C。没有 F/D，没有 A（原子指令按非法指令 trap），
  也没有自定义扩展；C3 本身也没有这些。
- 单核单 hart（`mhartid` 恒为 0）；只实现 M-mode + U-mode，没有 S-mode。
- PMP：U-mode 必须命中允许条目，M-mode 直接放行（对齐真机，内核不设锁定位）；区间在
  `pmpcfg/pmpaddr` 写入后惰性重建。
- `mtvec` 强制按 vectored 处理（C3 硬件只支持这一种）：同步异常跳 base，中断跳
  `base + 4*cause`。
- CSR 读写指令（`csrrw/csrrs/csrrc`）按规范先取 `rs1` 值再写 `rd`，因此
  `csrrs a5, mstatus, a5`（rd == rs1）能正确置位 `MIE`——FreeRTOS 端口用它退出临界区。
- 中断源：TIMG0 T0 直接上报 CPU line 2（`mcause = 0x80000002`）；SYSTIMER 的
  TARGET0..2 经 INTMTX 路由（source 37..39）；UART0 RX（FIFO 满 / 接收超时）经
  INTMTX 路由（source 21）；SYSTEM 的 `FROM_CPU_INTR0`（写 1 触发、写 0 清除，
  即 esp-riscv 的软件中断 / `vPortYield`）路由到 INTMTX source 50。都按固件写入的
  映射/优先级/阈值投递。没有 `mie` CSR，仅由 `mstatus.MIE` 门控。
- `ebreak` 约定为停机，作为固件退出。

### 内存 / 取指

- 既支持**扁平链接**的固件（从 `0x40000000` 起执行），也支持 ESP-IDF 风格的
  IROM `0x42000000` / DROM `0x3C000000` 虚地址段（经 flash cache 窗口读取）。
- XIP 窗口的映射由 128 项 MMU 表决定，复位默认线性映射；ELF 段写进窗口时落进
  overlay，不会改 flash 镜像。未建模地址仍按读 0/写丢弃处理并去重告警。
- 基本块缓存以 4 KiB 页为失效粒度，单块最多 32 条指令、512 个槽；写平铺区会 bump 所在页
  的版本号，所以自修改代码和 SPI1 擦写 flash 都不会读到过期块。

### 外设

- 覆盖 C3 除 WiFi/BLE 外的标准外设：UART0（含 RX 中断）、UART1（复用 UART 模型，
  TX 丢弃、无 RX）、GPIO、TIMG0（T0 节拍 + TG0WDT）、TIMG1（影子）、SYSTIMER
  （计数 + TARGET 报警）、INTMTX（source→CPU line 路由）、SPI1/SPI0（flash 擦写与
  USER 模式命令）、SPI2（GP-SPI，USER 传输 + TRANS_DONE 中断）、I2C0（命令列表
  传输，COMD DONE/TRANS_COMPLETE）、APB_SARADC（通道值注入）、flash cache/XIP（MMU +
  DROM/IROM 窗口）、eFuse（只读 MAC/版本）、USJ（黑洞）、RTC_CNTL（被动）；
  CLINT mtimer 是旧路径，C3 不用。
- **寄存器影子外设**：SENSITIVE、IO_MUX、SYSCON、RTC_I2C、UHCI0、RMT、
  LEDC、TWAI、I2S0、GDMA、AES/SHA/RSA/DS/HMAC/AES_XTS、ASSIST_DEBUG、
  DEDICATED_GPIO、WORLD_CNTL 只保存写入值（读回一致），不产生中断、不驱动引脚，
  也不模拟触发/完成时序。固件做初始化/读回配置能正常通过；若有固件真的轮询这些
  模块的「完成位」，仍可能停住（目前没有固件在使用它们）。
- **SPI2 是环回模型**：没有接外部器件，同时开 MOSI/MISO 时 W0..W15 的发送数据
  原样作为接收数据；只开 MISO 时接收清零。`USR`/`UPDATE` 写 1 自清，
  `DMA_INT_RAW.TRANS_DONE` 置位并可经 INTMTX source 19 投递。
- **I2C0 是命令列表模型**：写 `CTR.TRANS_START` 后按 `COMD0..7` 执行，WRITE 消费
  TX FIFO、READ 往 RX FIFO 填 0，每条命令置 `DONE`；同时置 TRANS_START /
  TRANS_COMPLETE / END_DETECT 中断（source 29）。没有外部从机，因此不产生 NACK。
- **UART1** 与 UART0 共用寄存器语义（中断 source 22），但没有接收通路，TX 字节
  直接丢弃。
- **flash cache/XIP**：MMU 表（128×64 KiB）按软件写入的页号翻译，复位默认线性映射
  （窗口偏移 == flash 偏移）；ELF 的 IROM/DROM 段写进按虚拟页懒分配的 overlay，不污染
  flash 本体。EXTMEM 控制寄存器只做影子，不按 ENABLE/SHUT 真正门控 cache——没有
  bootloader 的裸机固件在初始化前也要能取指。
- **cache 一致性是简化的**：SPI 擦写 flash 不会让已写入 overlay 的页失效；XIP 页不在
  平铺区里，擦写 flash 也不会让基本块缓存失效。IROM/DROM 段只在加载时写入，正常流程不受影响。
- **SPI0/SPI1 共用一套 SPI_MEM 模型**：硬件命令位（RDID/RDSR/SE/BE/CE/PP/READ）和
  USER 模式都支持；USER 数据按 memcpy 语义放在 W0..W15（第 i 字节 → `W[i/4]` 的第
  `i%4` 字节），与 IDF 实际的字节排列可能不同。flash 容量固定 4 MiB，JEDEC ID
  `C84016`。
- **UART0 接收中断**：模型 `INT_RAW/INT_ST/INT_ENA/INT_CLR`，`RXFIFO_FULL` 按
  `CONF1` 阈值做电平，`RXFIFO_TOUT` 是锁存（新字节置位、写 `INT_CLR` 或读空 FIFO 清零），
  再经 INTMTX source 21 投递。没有按波特率建模超时延迟；内核仍保留 tick 轮询兜底。
  其余配置寄存器（`CONF0`/`HWFC`/`SWFC`/`CLK_CONF` 等）做寄存器影子，写入可读回。
- **eFuse 只读模型**：BLK1 提供固定 MAC `AC:67:B2:12:34:56` 与晶圆版本 v0.4，
  BLK0 的 `DIS_*` 选项位全 0（cache/JTAG/下载模式都保持可用）；编程寄存器写入丢弃。
  固定值只影响打印和 WiFi/BT 的 MAC，不影响启动流程。
- 因此**真机上「粘贴长命令丢字节」那类问题复现不出来**：管道没有 115200 的物理上限，
  tick 之间不会堆满 128 字节接收 FIFO。
- 没有精确外设时序：tick 是「每 N 条指令」而不是墙钟，`-tickdiv` 用来验证时序敏感性。
- **看门狗只建模了 TG0WDT（TIMG0）**，语义按真机：`WPROTECT` 写入解锁字 `0x50D83AA1`
  之后 `CONFIG0..5` 才接受写入；`CONFIG1` 是 stage0 超时值（按指令数计，1 条指令 ≈
  1 个 APB 周期），`FEED` 写任意值清零计数；超时按整机复位处理（和 `reboot` 共用
  重装路径，打印 `=== watchdog reset ... ===`）。上电默认关闭——本模拟器的 ROM 是
  手写桩，没有真 ROM 打开看门狗的那段初始化。RTC WDT / super WDT 仍只是寄存器影子
  （接受写入与读回，不参与复位）。deep sleep/电源管理、USB-Serial-JTAG 协议没有建模。
- ROM 没有真机 dump：`esp_rom_spiflash_read/write/config_param` 是三条手写 RV32I 蹦床桩
  （`internal/rom`），等价于「按字节 memcpy 到 flash 模拟区」。

### 调试 / 工具

- 没有 GDB stub。调试手段是快照式的：`-trace` / `-step` / `-traplog` / `-fault` /
  `-breakpc` / `-watch` / `-traceirq` / `-dumpram`。
- `-dumpram` 固定导出 `0x40380000..0x403AC000`（TinyOS 内核 RAM 窗口），换固件要改常量。
- 性能数字只在同一台机器、同一份代码上可比；跨机器的绝对值不要直接对比。

### 与真机的已知差异（时序类）

- 中断延迟按指令数而不是周期数，单条指令的「耗时」是均匀的。
- 没有总线竞争、flash 等待周期，cache 也不建模命中/失效延迟，所以对时序敏感的竞态在
  模拟器里更「干净」。
- 反过来，这种确定性可以用来复现竞态：TinyOS 的 `sched_switch` 丢任务 bug 就是靠
  `-tickdiv 1` 必现 + `-traplog` + `-breakpc` + `-dumpram` 定位的（TinyOS 侧已修，
  commit `dbe0e90`：把「选择任务」和「探测是否还有人等」放进同一个关中断区间）。
