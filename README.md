# RV32EMU — ESP32-C3 行为对齐的 RV32IMC 指令集模拟器

Go 实现，单核，严格对齐 ESP32-C3（RV32IMC，无 F/D 浮点）。
流水线分层：**取指(IF) → 译码(ID) → 执行(EX) → 访存(MEM) → 写回(WB)**，译码表独立可扩展。

## 目录结构

```
rv32emu/
├─ go.mod / main.go            CLI：-elf / -bin / -trace / -step / -max
├─ internal/
│  ├─ cpu/                     CPU 核心
│  │  ├─ cpu.go               状态、Step() 主循环、对齐检查
│  │  ├─ execute.go           RV32I + M(mul/div/rem) 执行语义
│  │  ├─ compressed.go        C 扩展 16-bit 译码执行
│  │  ├─ csr.go               mstatus/mtvec/mepc/mcause/mie/mip...
│  │  └─ trap.go              异常进入、mret、中断采样
│  ├─ mem/                     总线 + 地址映射
│  │  ├─ bus.go               Bus/Peripheral 接口
│  │  └─ memory.go            ROM@0x40000000 / SRAM@0x3FC80000
│  ├─ perip/                   外设最小子集
│  │  ├─ uart.go             UART0@0x60000000（FIFO↔stdout/stdin）
│  │  ├─ gpio.go              GPIO@0x60004000
│  │  └─ clint.go             CLINT mtimer@0x02000000（MTIP）
│  ├─ elf/elf.go              ELF 段加载（ESP-IDF .elf 直接跑）
│  └─ debug/disasm.go         反汇编 / 单步日志
└─ firmware/test_min/          ESP-IDF 最小测试工程
```

## 内存映射（对齐 ESP32-C3 技术参考手册）

| 区域 | 地址范围 |
|------|----------|
| ROM | `0x40000000 ~ 0x4007FFFF`（复位取指点） |
| SRAM | `0x3FC80000 ~ 0x3FCFFFFF` |
| UART0 | `0x60000000` |
| GPIO | `0x60004000` |
| CLINT mtimer | `0x02000000` |

## 构建与运行

```powershell
go build -o rv32emu.exe .

# 跑 ELF（ESP-IDF 编译产物）
.\rv32emu.exe -elf build\test_min.elf

# 跑裸 bin / 带单步反汇编
.\rv32emu.exe -bin demo.bin -entry 0x40000000 -trace
```

CLI 参数：
- `-elf` / `-bin`：固件加载方式
- `-entry`：bin 加载基址（默认 `0x40000000`）
- `-trace`：每条指令打印 pc + 反汇编
- `-step N`：执行 N 条后暂停并 dump 寄存器
- `-max N`：最大指令数（0=跑到停机）

## 异常 / 中断

- 实现：非法指令(2)、load/store 地址不对齐(4/6)、访问故障、ecall(11)、ebreak(3)。
- mtimer 比较命中 → MTIP；`mstatus.MIE` 开且 `mie.MTIE` 允许时进入 machine timer interrupt。
- `mret` 正确恢复 MIE/MPIE 并跳 `mepc`。
- ebreak 在本模拟器约定为**停机**（作为固件退出）。

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

- 外设为最小子集：UART0 字符 IO、GPIO 影子寄存器、CLINT mtimer。PLIC/INTC 中断矩阵为近似（C3 实际走 INTC 路由，后续可补 `0x600C0000` 中断矩阵）。
- C 扩展实现常用指令；未实现的压缩指令触发非法指令 trap，便于发现缺口。
- 时序为周期级计数，未做精确外设波特率分频。
