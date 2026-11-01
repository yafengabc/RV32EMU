.section .text.startup
.global _start
.type _start, @function

_start:
	la	sp, _stack_top

	/* 拷贝 .data：从 LMA(_sidata) 到 VMA(_sdata.._edata) */
	la	a0, _sdata
	la	a1, _edata
	la	a2, _sidata
1:
	beq	a0, a1, 2f
	lw	a3, (a2)
	sw	a3, (a0)
	addi	a0, a0, 4
	addi	a2, a2, 4
	j	1b
2:
	/* 清零 .bss */
	la	a0, _sbss
	la	a1, _ebss
3:
	beq	a0, a1, 4f
	sw	zero, (a0)
	addi	a0, a0, 4
	j	3b
4:
	call	main

	/* main 返回后停机 */
	ebreak
5:
	j	5b
