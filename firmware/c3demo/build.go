//go:build ignore

// build.go - build the esp32c3 demo firmware (replaces build.ps1).
//
//   go run firmware/c3demo/build.go
//
// Needs a clang that can target riscv32 (MSYS2 UCRT64 or an LLVM install) and
// ld.lld.  CC and LD override the lookup; TOS_BIN-style extra paths can be
// listed in EXTRA_BIN.  The demo has no dependency on the TinyOS build tool.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

var flags = []string{
	"--target=riscv32", "-march=rv32imc", "-mabi=ilp32",
	"-ffreestanding", "-fno-pic", "-mno-relax", "-O2",
	"-Wall", "-nostdlib",
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func find(tool string, envName string, extra ...string) string {
	if p := os.Getenv(envName); p != "" {
		return p
	}
	var dirs []string
	if runtime.GOOS == "windows" {
		dirs = append(dirs, `C:\msys64\ucrt64\bin`, `C:\msys64\clang64\bin`, `D:\msys64\ucrt64\bin`)
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("EXTRA_BIN"))...)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if p := filepath.Join(d, exeName(tool)); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath(tool); err == nil {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	dir, _ := os.Getwd()
	// Allow running from the repo root or from the demo directory.
	if fileExists(filepath.Join(dir, "firmware", "c3demo", "main.c")) {
		dir = filepath.Join(dir, "firmware", "c3demo")
	}
	if !fileExists(filepath.Join(dir, "main.c")) {
		fail("run this from the repo root or firmware/c3demo")
	}
	if err := os.Chdir(dir); err != nil {
		fail(err.Error())
	}

	clang := find("clang", "CC")
	if clang == "" {
		clang = find("clang", "")
	}
	if clang == "" {
		fail("no clang found (set CC)")
	}
	ld := find("ld.lld", "LD")
	if ld == "" {
		fail("ld.lld not found (set LD)")
	}
	fmt.Println("CC =", clang)
	fmt.Println("LD =", ld)

	if err := run(clang, append(flags, "-c", "startup.s", "-o", "startup.o")...); err != nil {
		fail("startup.s compile failed")
	}
	if err := run(clang, append(flags, "-c", "main.c", "-o", "main.o")...); err != nil {
		fail("main.c compile failed")
	}
	if err := run(ld, "-m", "elf32lriscv", "-T", "linker.ld", "startup.o", "main.o", "-o", "c3demo.elf"); err != nil {
		fail("link failed")
	}

	fmt.Println("=== symbols ===")
	if nm := find("llvm-nm", ""); nm != "" {
		_ = run(nm, "c3demo.elf")
	}
	fmt.Println("=== sections ===")
	if od := find("llvm-objdump", ""); od != "" {
		_ = run(od, "-h", "c3demo.elf")
	}
	fmt.Println("built:", filepath.Join(dir, "c3demo.elf"))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "build failed:", msg)
	os.Exit(1)
}
