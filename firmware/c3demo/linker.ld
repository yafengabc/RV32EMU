ENTRY(_start)

MEMORY {
  CODE (rx) : ORIGIN = 0x40000000, LENGTH = 128K   /* IRAM/ROM 区 */
  DATA (rw) : ORIGIN = 0x3FC80000, LENGTH = 128K   /* SRAM */
}

SECTIONS {
  .text : {
    *(.text.startup)
    *(.text .text.*)
  } > CODE

  .rodata : { *(.rodata .rodata.*) } > CODE

  .data : {
    _sdata = .;
    *(.data .data.*)
    _edata = .;
  } > DATA AT > CODE
  _sidata = LOADADDR(.data);

  .bss : {
    _sbss = .;
    *(.bss .bss.*)
    *(COMMON)
    _ebss = .;
  } > DATA

  _stack_top = ORIGIN(DATA) + LENGTH(DATA);

  /DISCARD/ : { *(.comment) *(.note*) *(.riscv.attributes) }
}
