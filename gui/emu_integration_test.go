package main

// 集成测试：进程内启动真实模拟会话，验证 hello/uart/halt 事件链路。
// 依赖仓库根目录的 c3demo.elf 与 demo.bin。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rv32emu/internal/soc"
)

func TestEmuInProcess(t *testing.T) {
	elf := filepath.Join("..", "c3demo.elf")
	ram := filepath.Join("..", "demo.bin")
	if _, err := os.Stat(elf); err != nil {
		t.Skip("c3demo.elf not found")
	}
	ramdisk, err := os.ReadFile(ram)
	if err != nil {
		t.Skipf("demo.bin not found: %v", err)
	}

	e := &Emu{}
	events := make(chan Event, 256)
	e.OnEvent = func(ev Event) {
		select {
		case events <- ev:
		default:
		}
	}

	if err := e.Start(elf, ramdisk, soc.DefaultFlashOffset); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer e.Stop()

	wait := func(name string, match func(Event) bool) Event {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case ev := <-events:
				if match(ev) {
					return ev
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s", name)
			}
		}
	}

	hello := wait("hello", func(ev Event) bool { return ev.Event == "hello" })
	if hello.Pins != 22 || hello.ADCChannels != 5 {
		t.Fatalf("hello = %+v", hello)
	}
	wait("state", func(ev Event) bool { return ev.Event == "state" })

	if err := e.Step(1000); err != nil {
		t.Fatalf("step: %v", err)
	}
	uart := wait("uart", func(ev Event) bool { return ev.Event == "uart" })
	if !strings.Contains(uart.Data, "C3-OK") {
		t.Fatalf("uart data = %q", uart.Data)
	}
	wait("halt", func(ev Event) bool { return ev.Event == "halt" })
}
