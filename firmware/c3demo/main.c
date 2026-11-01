/* ESP32-C3 真实固件（freestanding，直接编译为 RV32IMC）。
 * 通过 UART0 FIFO 寄存器（0x60000000）输出，模拟器映射到 stdout。
 */
typedef unsigned int  uint32_t;
typedef          int  int32_t;
typedef unsigned char uint8_t;

#define UART0_BASE 0x60000000
#define UART_FIFO  (*(volatile uint32_t *)(UART0_BASE + 0x00))

/* 全局变量：验证 .data 拷贝与 .bss 清零 */
uint32_t data_var = 0x12345678;
uint32_t bss_var;

static void putc(char c) { UART_FIFO = (uint32_t)(uint8_t)c; }

static void puts_(const char *s) { while (*s) putc(*s++); }

static void puthex(uint32_t v) {
	const char *h = "0123456789ABCDEF";
	putc('0'); putc('x');
	for (int i = 7; i >= 0; i--) putc(h[(v >> (i * 4)) & 0xF]);
}

int main(void) {
	puts_("\r\n=== ESP32-C3 firmware booted ===\r\n");

	/* 验证启动拷贝 */
	puts_("data_var = "); puthex(data_var); puts_("\r\n");
	puts_("bss_var  = "); puthex(bss_var);  puts_("\r\n");

	/* M 扩展乘除 */
	int32_t a = 1234, b = 5678;
	puts_("1234*5678 = "); puthex((uint32_t)(a * b)); puts_("\r\n");
	puts_("-7/3="); puthex((uint32_t)(-7 / 3));
	puts_(" rem=");    puthex((uint32_t)(-7 % 3)); puts_("\r\n");

	/* 求和（编译器会插入大量 C 压缩指令） */
	uint32_t acc = 0;
	for (uint32_t i = 0; i < 16; i++) acc += i * i;
	puts_("sum i*i 0..15 = "); puthex(acc); puts_("\r\n");

	puts_("=== FIRMWARE DONE ===\r\n");
	return 0;
}
