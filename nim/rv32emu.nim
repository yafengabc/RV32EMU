# RV32IMC emulator in Nim — performance comparison
import os, strutils, times, tables

const
  ROM_BASE: uint32 = 0x40000000
  ROM_SIZE: uint32 = 512*1024
  SRAM_BASE: uint32 = 0x3FC80000
  SRAM_SIZE: uint32 = 512*1024
  UART0_BASE: uint32 = 0x60000000

var
  rom: array[ROM_SIZE, uint8]
  sram: array[SRAM_SIZE, uint8]
  ramPages: Table[uint32, ptr array[4096, uint8]]

proc ramRead(a: uint32, size: uint32): uint32 =
  if a >= ROM_BASE and a < ROM_BASE+ROM_SIZE:
    let off = a - ROM_BASE
    case size
    of 1: result = rom[off]
    of 2: result = rom[off] or (uint32(rom[off+1]) shl 8)
    of 4: result = rom[off] or (uint32(rom[off+1]) shl 8) or (uint32(rom[off+2]) shl 16) or (uint32(rom[off+3]) shl 24)
    else: discard
  elif a >= SRAM_BASE and a < SRAM_BASE+SRAM_SIZE:
    let off = a - SRAM_BASE
    case size
    of 1: result = sram[off]
    of 2: result = sram[off] or (uint32(sram[off+1]) shl 8)
    of 4: result = sram[off] or (uint32(sram[off+1]) shl 8) or (uint32(sram[off+2]) shl 16) or (uint32(sram[off+3]) shl 24)
    else: discard
  elif a >= UART0_BASE and a < UART0_BASE+0x1000:
    result = 0
  else:
    let page = a and 0xFFFFF000'u32
    if ramPages.hasKey(page):
      let off = a - page
      let p = ramPages[page]
      case size
      of 1: result = p[off]
      of 2: result = p[off] or (uint32(p[off+1]) shl 8)
      of 4: result = p[off] or (uint32(p[off+1]) shl 8) or (uint32(p[off+2]) shl 16) or (uint32(p[off+3]) shl 24)
      else: discard
    else:
      result = 0

proc ramWrite(a: uint32, size: uint32, val: uint32) =
  if a >= ROM_BASE and a < ROM_BASE+ROM_SIZE:
    discard
  elif a >= SRAM_BASE and a < SRAM_BASE+SRAM_SIZE:
    let off = a - SRAM_BASE
    case size
    of 1: sram[off] = val.uint8
    of 2:
      sram[off] = val.uint8; sram[off+1] = (val shr 8).uint8
    of 4:
      sram[off] = val.uint8; sram[off+1] = (val shr 8).uint8
      sram[off+2] = (val shr 16).uint8; sram[off+3] = (val shr 24).uint8
    else: discard
  elif a >= UART0_BASE and a < UART0_BASE+0x1000:
    if a == UART0_BASE:
      write(stdout, char(val and 0xFF))
  else:
    let page = a and 0xFFFFF000'u32
    if not ramPages.hasKey(page):
      ramPages[page] = cast[ptr array[4096, uint8]](alloc0(4096))
    let off = a - page
    let p = ramPages[page]
    case size
    of 1: p[off] = val.uint8
    of 2: p[off] = val.uint8; p[off+1] = (val shr 8).uint8
    of 4:
      p[off] = val.uint8; p[off+1] = (val shr 8).uint8
      p[off+2] = (val shr 16).uint8; p[off+3] = (val shr 24).uint8
    else: discard

# ---- ELF loader ----
proc u32at(d: string, off: int): uint32 =
  uint32(d[off]) or (uint32(d[off+1]) shl 8) or (uint32(d[off+2]) shl 16) or (uint32(d[off+3]) shl 24)

proc u16at(d: string, off: int): uint16 =
  uint16(d[off]) or (uint16(d[off+1]) shl 8)

proc loadElf(path: string, entry: var uint32) =
  let f = open(path)
  let data = f.readAll()
  f.close()
  let e_phoff = u32at(data, 36)
  let e_phentsize = u16at(data, 42)
  let e_phnum = u16at(data, 44)
  entry = u32at(data, 24)
  for i in 0..<int(e_phnum):
    let phoff = 28 + i*int(e_phentsize)
    let p_type = u32at(data, phoff)
    if p_type != 1: continue
    let p_offset = u32at(data, phoff+4)
    let p_vaddr = u32at(data, phoff+8)
    let p_filesz = u32at(data, phoff+16)
    let p_memsz = u32at(data, phoff+20)
    for j in 0..<int(p_memsz):
      let b = if j < int(p_filesz): data[int(p_offset)+j].uint8 else: 0'u8
      ramWrite(p_vaddr + uint32(j), 1, uint32(b))

# ---- CPU ----
var
  x: array[32, uint32]
  pc: uint32
  halted: bool
  instCount: uint64

proc signExtend(v: uint32, bits: int): uint32 =
  if (v and (1'u32 shl (bits-1))) != 0:
    result = v or (not(uint32(0)) shl bits)
  else:
    result = v

proc step(): bool =
  if halted: return false
  result = true
  let curPC = pc
  let low = ramRead(pc, 2)
  var instr: uint32
  var compressed: bool

  case low and 3
  of 0, 1, 2:
    instr = low
    compressed = true
    pc = pc + 2
  else:
    let high = ramRead(pc+2, 2)
    instr = low or (high shl 16)
    compressed = false
    pc = pc + 4

  if compressed:
    let ci = uint16(low)
    let q = ci and 3'u16
    let f3 = (ci shr 13) and 7'u16
    case q
    of 0:  # CA/CIW
      case f3
      of 0:  # C.ADDI4SPN
        let nzuimm = ((ci shr 5) and 0xf'u16) or ((ci shr 7) and 0x18'u16) shl 1
        let rd = 8 + int((ci shr 2) and 7'u16)
        x[rd] = x[2] + uint32(nzuimm)
      of 2:  # C.LW
        let uimm = ((ci shr 7) and 0x7'u16) shl 2 or ((ci shr 10) and 0x3'u16) shl 6
        let rs1 = 8 + int((ci shr 4) and 7'u16)
        let rd = 8 + int((ci shr 2) and 7'u16)
        x[rd] = ramRead(x[rs1]+uint32(uimm), 4)
      of 6:  # C.SW
        let uimm = ((ci shr 7) and 0x7'u16) shl 2 or ((ci shr 10) and 0x3'u16) shl 6
        let rs1 = 8 + int((ci shr 4) and 7'u16)
        let rs2 = 8 + int((ci shr 2) and 7'u16)
        ramWrite(x[rs1]+uint32(uimm), 4, x[rs2])
      else:
        halted = true; echo "illegal C 0x", ci.toHex(4)
    of 1:  # CI/CJ
      case f3
      of 0:  # C.NOP/C.ADDI
        let imm = signExtend(uint32((ci shr 2) and 0x1f'u16), 5)
        let rd = int((ci shr 7) and 0x1f'u16)
        if rd != 0: x[rd] = x[rd] + imm
      of 1:  # C.JAL
        let imm = signExtend(((ci shr 2) and 0x1) shl 5 or ((ci shr 3) and 0x3f) shl 1 or
                             ((ci shr 11) and 1) shl 7 or ((ci shr 7) and 0x1) shl 6 or
                             ((ci shr 9) and 0xf) shl 3 or ((ci shr 8) and 1) shl 8, 11)
        x[1] = curPC + 2
        pc = curPC + imm
      of 2:  # C.LI
        let imm = signExtend(uint32((ci shr 2) and 0x1f'u16), 5)
        let rd = int((ci shr 7) and 0x1f'u16)
        if rd != 0: x[rd] = imm
      of 3:
        if ((ci shr 7) and 0x1f'u16) == 2:  # C.ADDI16SP
          let imm = signExtend(((ci shr 2) and 1) shl 5 or ((ci shr 3) and 0x7) shl 1 or
                               ((ci shr 5) and 1) shl 7 or ((ci shr 6) and 1) shl 4, 9)
          x[2] = x[2] + imm
        else:  # C.LUI
          let imm = signExtend(uint32((ci shr 2) and 0x1f'u16), 5)
          let rd = int((ci shr 7) and 0x1f'u16)
          if rd != 0: x[rd] = imm shl 12
      of 8, 9:  # SRLI/SRAI
        let sh = (ci shr 2) and 0x1f'u16
        let rd = 8 + int((ci shr 7) and 7'u16)
        if f3 == 8: x[rd] = x[rd] shr sh
        else: x[rd] = (int32(x[rd]) shr sh).uint32
      of 12:  # C.ANDI
        let imm = signExtend(uint32((ci shr 2) and 0x1f'u16), 5)
        let rs1 = 8 + int((ci shr 7) and 7'u16)
        x[rs1] = x[rs1] and imm
      of 13:  # CA: C.SUB/XOR/OR/AND
        let rd = 8 + int((ci shr 7) and 7'u16)
        let rs2 = 8 + int((ci shr 2) and 7'u16)
        case (ci shr 5) and 3'u16
        of 0: x[rd] = x[rd] - x[rs2]
        of 1: x[rd] = x[rd] xor x[rs2]
        of 2: x[rd] = x[rd] or x[rs2]
        of 3: x[rd] = x[rd] and x[rs2]
        else: discard
      of 5:  # C.J
        let imm = signExtend(((ci shr 2) and 0x1) shl 5 or ((ci shr 3) and 0x3f) shl 1 or
                             ((ci shr 11) and 1) shl 7 or ((ci shr 7) and 0x1) shl 6 or
                             ((ci shr 9) and 0xf) shl 3 or ((ci shr 8) and 1) shl 8, 11)
        pc = curPC + imm
      else:
        halted = true; echo "illegal C 1x 0x", ci.toHex(4)
    of 2:  # CR/CI
      case f3
      of 8:  # C.JR/C.MV
        let rs1 = int((ci shr 7) and 0x1f'u16)
        let rs2 = int((ci shr 2) and 0x1f'u16)
        if rs2 == 0:
          pc = x[rs1] and 0xfffffffe'u32
        else:
          x[rs1] = x[rs2]
      of 9:  # C.JALR/C.ADD
        let rs1 = int((ci shr 7) and 0x1f'u16)
        let rs2 = int((ci shr 2) and 0x1f'u16)
        if rs2 == 0:
          x[1] = curPC + 2
          pc = x[rs1] and 0xfffffffe'u32
        else:
          x[rs1] = x[rs1] + x[rs2]
      of 0, 4:  # C.SLLI
        let sh = (ci shr 2) and 0x1f'u16
        let rd = int((ci shr 7) and 0x1f'u16)
        if rd != 0: x[rd] = x[rd] shl sh
      of 6, 7:  # C.LWSP/C.SWSP
        if f3 == 6:  # C.LWSP
          let uimm = ((ci shr 2) and 0x3'u16) shl 2 or ((ci shr 4) and 0xf'u16) shl 3 or ((ci shr 12) and 1) shl 5
          let rd = int((ci shr 7) and 0x1f'u16)
          if rd != 0: x[rd] = ramRead(x[2]+uint32(uimm), 4)
        else:  # C.SWSP
          let uimm = ((ci shr 2) and 0x3'u16) shl 2 or ((ci shr 9) and 0xf'u16) shl 3
          let rs2 = int((ci shr 7) and 0x1f'u16)
          ramWrite(x[2]+uint32(uimm), 4, x[rs2])
      else:
        halted = true; echo "illegal C 2x 0x", ci.toHex(4)
    of 3:  # CB
      case f3
      of 6:  # C.BEQZ
        let imm = signExtend(((ci shr 3) and 0x7'u16) shl 1 or ((ci shr 10) and 0x3'u16) shl 3 or
                             ((ci shr 2) and 1) shl 5 or ((ci shr 5) and 0x3'u16) shl 6, 8)
        let rs1 = 8 + int((ci shr 7) and 7'u16)
        if x[rs1] == 0: pc = curPC + imm
      of 7:  # C.BNEZ
        let imm = signExtend(((ci shr 3) and 0x7'u16) shl 1 or ((ci shr 10) and 0x3'u16) shl 3 or
                             ((ci shr 2) and 1) shl 5 or ((ci shr 5) and 0x3'u16) shl 6, 8)
        let rs1 = 8 + int((ci shr 7) and 7'u16)
        if x[rs1] != 0: pc = curPC + imm
      else:
        halted = true; echo "illegal C 3x 0x", ci.toHex(4)
    else:
      discard
  else:
    let op = instr and 0x7f
    let rd = (instr shr 7) and 0x1f
    let f3 = (instr shr 12) and 0x7
    let r1 = (instr shr 15) and 0x1f
    let r2 = (instr shr 20) and 0x1f

    case op
    of 0x37:
      if rd != 0: x[rd] = instr and 0xfffff000'u32
    of 0x17:
      if rd != 0: x[rd] = curPC + (instr and 0xfffff000'u32)
    of 0x6f:
      if rd != 0: x[rd] = curPC + 4
      let imm = signExtend(((instr shr 31) and 1) shl 20 or ((instr shr 12) and 0xff) shl 12 or
                           ((instr shr 20) and 1) shl 11 or ((instr shr 21) and 0x3ff) shl 1, 21)
      pc = curPC + imm
    of 0x67:
      let t = (x[r1] + signExtend(instr shr 20, 12)) and 0xfffffffe'u32
      if rd != 0: x[rd] = curPC + 4
      pc = t
    of 0x63:
      let a = x[r1]
      let b = x[r2]
      var taken = false
      case f3
      of 0: taken = a == b
      of 1: taken = a != b
      of 4: taken = int32(a) < int32(b)
      of 5: taken = int32(a) >= int32(b)
      of 6: taken = a < b
      of 7: taken = a >= b
      else: discard
      if taken:
        let imm = signExtend(((instr shr 31) and 1) shl 12 or ((instr shr 7) and 1) shl 11 or
                             ((instr shr 25) and 0x3f) shl 5 or ((instr shr 8) and 0xf) shl 1, 13)
        pc = curPC + imm
    of 0x03:
      let imm = signExtend(instr shr 20, 12)
      let ea = x[r1] + imm
      var v: uint32
      case f3
      of 0: v = signExtend(ramRead(ea, 1), 8)
      of 1: v = signExtend(ramRead(ea, 2), 16)
      of 2: v = ramRead(ea, 4)
      of 4: v = ramRead(ea, 1)
      of 5: v = ramRead(ea, 2)
      else: discard
      if rd != 0: x[rd] = v
    of 0x23:
      let imm = signExtend(((instr shr 25) shl 5) or ((instr shr 7) and 0x1f), 12)
      let ea = x[r1] + imm
      case f3
      of 0: ramWrite(ea, 1, x[r2])
      of 1: ramWrite(ea, 2, x[r2])
      of 2: ramWrite(ea, 4, x[r2])
      else: discard
    of 0x13:
      let imm = signExtend(instr shr 20, 12)
      case f3
      of 0:
        if rd != 0: x[rd] = x[r1] + imm
      of 2:
        if rd != 0: x[rd] = (int32(x[r1]) < int32(imm)).uint32
      of 3:
        if rd != 0: x[rd] = (x[r1] < imm).uint32
      of 4:
        if rd != 0: x[rd] = x[r1] xor imm
      of 6:
        if rd != 0: x[rd] = x[r1] or imm
      of 7:
        if rd != 0: x[rd] = x[r1] and imm
      of 1:
        let sh = (instr shr 20) and 0x1f
        if rd != 0: x[rd] = x[r1] shl sh
      of 5:
        let sh = (instr shr 20) and 0x1f
        if ((instr shr 30) and 1) != 0:
          if rd != 0: x[rd] = (int32(x[r1]) shr sh).uint32
        else:
          if rd != 0: x[rd] = x[r1] shr sh
      else: discard
    of 0x33:
      let f7 = instr shr 25
      case f3
      of 0:
        if f7 == 1:
          if rd != 0: x[rd] = x[r1] * x[r2]
        elif f7 == 0:
          if rd != 0: x[rd] = x[r1] + x[r2]
        else:
          if rd != 0: x[rd] = x[r1] - x[r2]
      of 1:
        if rd != 0: x[rd] = x[r1] shl (r2 and 0x1f)
      of 2:
        if rd != 0: x[rd] = (int32(x[r1]) < int32(x[r2])).uint32
      of 3:
        if rd != 0: x[rd] = (x[r1] < x[r2]).uint32
      of 4:
        if f7 == 1:
          if x[r2] == 0: x[rd] = 0xffffffff'u32
          else: x[rd] = (int32(x[r1]) div int32(x[r2])).uint32
        else:
          if rd != 0: x[rd] = x[r1] xor x[r2]
      of 5:
        if f7 == 1:
          if x[r2] == 0: x[rd] = 0xffffffff'u32
          else: x[rd] = x[r1] div x[r2]
        elif f7 == 0:
          if rd != 0: x[rd] = x[r1] shr (r2 and 0x1f)
        else:
          if rd != 0: x[rd] = (int32(x[r1]) shr (r2 and 0x1f)).uint32
      of 6:
        if f7 == 1:
          if x[r2] == 0: x[rd] = x[r1]
          else: x[rd] = (int32(x[r1]) mod int32(x[r2])).uint32
        else:
          if rd != 0: x[rd] = x[r1] or x[r2]
      of 7:
        if f7 == 1:
          if x[r2] == 0: x[rd] = x[r1]
          else: x[rd] = x[r1] mod x[r2]
        else:
          if rd != 0: x[rd] = x[r1] and x[r2]
      else: discard
    of 0x73:
      let csr = (instr shr 20) and 0xfff
      if csr == 0:
        halted = true
        echo "ECALL at pc=0x", curPC.toHex(8)
      elif csr == 1:
        halted = true
        echo "EBREAK at pc=0x", curPC.toHex(8)
      else:
        if rd != 0: x[rd] = 0
    else:
      halted = true
      echo "illegal opcode 0x", op.toHex(2), " at pc=0x", curPC.toHex(8)
  instCount.inc()

when isMainModule:
  let args = commandLineParams()
  if args.len < 1:
    echo "usage: rv32emu_nim <elf> [max_inst]"
    quit(1)
  let elfPath = args[0]
  var maxInst = if args.len >= 2: parseInt(args[1]).uint64 else: 1000000'u64
  loadElf(elfPath, pc)
  echo "entry=0x", pc.toHex(8)
  let t0 = cpuTime()
  while not halted and instCount < maxInst:
    discard step()
  let t1 = cpuTime()
  let dt = t1 - t0
  echo "=== stopped after ", instCount, " inst ==="
  echo "time: ", formatFloat(dt, ffDecimal, 3), " s"
  if dt > 0:
    let mips = float64(instCount) / dt / 1e6
    echo "MIPS: ", formatFloat(mips, ffDecimal, 1)
