/*  ESP32-C3 最小测试固件：在模拟器与真实硬件上行为一致。
 *  直接操作 UART0 FIFO 寄存器（0x60000000）输出字符，
 *  模拟器把它映射到 stdout；真实 C3 上即 UART0 日志口。
 */
#include <stdint.h>

#define UART0_BASE 0x60000000
#define UART_FIFO  (*(volatile uint32_t *)(UART0_BASE + 0x00))

static void putc(char c) {
	UART_FIFO = (uint32_t)c;
}

static void puts_(const char *s) {
	while (*s) putc(*s++);
}

static void puthex(uint32_t v) {
	puts_("0x");
	for (int i = 28; i >= 0; i -= 4) {
		uint32_t n = (v >> i) & 0xF;
		putc((char)(n < 10 ? '0' + n : 'A' + n - 10));
	}
}

void app_main(void) {
	puts_("\r\n=== RV32EMU firmware on ESP32-C3 ===\r\n");

	/* 整数运算自检（M 扩展） */
	volatile int32_t a = 1234, b = 5678;
	volatile int32_t m = a * b;
	volatile int32_t q = -7 / 3;
	volatile int32_t r = -7 % 3;
	puts_("mul 1234*5678 = "); puthex((uint32_t)m); puts_("\r\n");
	puts_("div -7/3 = "); puthex((uint32_t)q); puts_("  rem = "); puthex((uint32_t)r); puts_("\r\n");

	/* 压缩指令：用 -O 编译后下面很多 C 语句会被编成 C.* 压缩指令 */
	volatile uint32_t acc = 0;
	for (uint32_t i = 0; i < 8; i++) acc += (i * i);
	puts_("sum i*i (0..7) = "); puthex(acc); puts_("\r\n");

	puts_("=== FIRMWARE DONE ===\r\n");
	while (1) { }
}
