$ErrorActionPreference = "Stop"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $dir

$clang = "D:\msys64\ucrt64\bin\clang.exe"
$llvmBin = "D:\msys64\ucrt64\bin"
$flags = @("--target=riscv32", "-march=rv32imc", "-mabi=ilp32",
           "-ffreestanding", "-fno-pic", "-mno-relax", "-O2",
           "-Wall", "-nostdlib")

& $clang @flags -c startup.s -o startup.o
if ($LASTEXITCODE) { throw "startup.s compile failed" }
& $clang @flags -c main.c -o main.o
if ($LASTEXITCODE) { throw "main.c compile failed" }

& ld.lld -T linker.ld startup.o main.o -o c3demo.elf
if ($LASTEXITCODE) { throw "link failed" }

Write-Host "=== symbols ==="
& "$llvmBin\llvm-nm.exe" c3demo.elf 2>$null | Select-String "_start|_sdata|_ebss|_stack_top|main"
Write-Host "=== sections ==="
& "$llvmBin\llvm-objdump.exe" -h c3demo.elf 2>$null | Select-String "text|data|bss"
Write-Host "built: $dir\c3demo.elf"
