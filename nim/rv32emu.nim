# RV32IMC emulator — one-to-one translation of Go version into Nim
# Same architecture: flat memory + basic block cache + runBlock chaining

import os, strutils, times, math

# ---- Flat memory layout ----
# Cover the TinyGo ELF at 0x80000000 plus built-in regions.
# Go version uses FlatBase=0x3F000000 size=32MB; for Nim we use a wider flat
# that includes 0x80000000 so RunHot works on the benchmark directly.
const
  FlatBase: uint32 = 0x80000000'u32
  FlatSize: uint32 = 16*1024*1024  # 16 MB covers 0x80000000~0x80FFFFFF
  ROM_BASE: uint32 = 0x40000000
  UART0_BASE: uint32 = 0x60000000

var flat: array[FlatSize, uint8]

# ---- Block cache ----
const
  blockSlots = 512
  blockMaxOps = 32

type OpKind = enum
  kAddi, kSlti, kSltiu, kXori, kOri, kAndi, kSlli, kSrli, kSrai,
  kAdd, kSub, kSll, kSlt, kSltu, kXor, kSrl, kSra, kOr, kAnd,
  kMulExt, kLui, kAuipc, kJal, kJalr,
  kBeq, kBne, kBlt, kBge, kBltu, kBgeu,
  kLb, kLh, kLw, kLbu, kLhu, kSb, kSh, kSw, kNop,
  kCAddi4spn, kCLw, kCSw, kCAddi, kCJal, kCLi, kCAddi16sp, kCLui,
  kCSrli, kCSrai, kCAndi, kCSub, kCXor, kCOr, kCAnd, kCJ,
  kCBeqz, kCBnez, kCSlli, kCLwsp, kCSwsp, kCJr, kCMv, kCJalr, kCAdd

type HOp = object
  kind: OpKind
  rd, rs1, rs2, ln: uint8
  imm: int32

type HBlock = object
  start: uint32
  gen: uint32
  n: uint8
  ops: array[blockMaxOps, HOp]

var
  x: array[32, uint32]
  pc: uint32
  halted: bool
  haltMsg: string
  instCount: uint64
  blocks: array[blockSlots, HBlock]
  codeGen: array[FlatSize div 4096, uint32]
  codePage: array[FlatSize div 4096, bool]

# ---- Immediate helpers ----
proc signExtend(v: uint32, bits: int): uint32 =
  if (v and (1'u32 shl (bits-1))) != 0:
    result = v or (not(uint32(0)) shl bits)
  else:
    result = v

proc immI(instr: uint32): uint32 = signExtend(instr shr 20, 12)
proc immS(instr: uint32): uint32 =
  signExtend(((instr shr 25) shl 5) or ((instr shr 7) and 0x1f), 12)
proc immB(instr: uint32): uint32 =
  signExtend(((instr shr 31) and 1) shl 12 or ((instr shr 7) and 1) shl 11 or
             ((instr shr 25) and 0x3f) shl 5 or ((instr shr 8) and 0xf) shl 1, 13)
proc immU(instr: uint32): uint32 = instr and 0xfffff000'u32
proc immJ(instr: uint32): uint32 =
  signExtend(((instr shr 31) and 1) shl 20 or ((instr shr 12) and 0xff) shl 12 or
             ((instr shr 20) and 1) shl 11 or ((instr shr 21) and 0x3ff) shl 1, 21)

# ---- Compressed immediate helpers ----
proc cReg(b: uint32): int = (int(b and 7) + 8)
proc cAddi4spnImm(inst: uint32): uint32 =
  ((inst shr 7) and 0xF) shl 6 or ((inst shr 11) and 3) shl 4 or
  ((inst shr 6) and 1) shl 2 or ((inst shr 5) and 1) shl 3
proc cLWOff(inst: uint32): uint32 =
  ((inst shr 5) and 1) shl 6 or ((inst shr 12) and 1) shl 5 or
  ((inst shr 11) and 1) shl 4 or ((inst shr 10) and 1) shl 3 or
  ((inst shr 6) and 1) shl 2
proc cAddi16spImm(inst: uint32): uint32 =
  signExtend(((inst shr 12) and 1) shl 9 or ((inst shr 4) and 1) shl 8 or
             ((inst shr 3) and 1) shl 7 or ((inst shr 5) and 1) shl 6 or
             ((inst shr 2) and 1) shl 5 or ((inst shr 6) and 1) shl 4, 10)
proc cLWSPOff(inst: uint32): uint32 =
  ((inst shr 12) and 1) shl 5 or ((inst shr 6) and 1) shl 4 or
  ((inst shr 5) and 1) shl 3 or ((inst shr 4) and 1) shl 2 or
  ((inst shr 3) and 1) shl 7 or ((inst shr 2) and 1) shl 6
proc cSWSPOff(inst: uint32): uint32 =
  ((inst shr 2) and 3) shl 2 or ((inst shr 9) and 0xf) shl 3
proc cJOffset(inst: uint32): uint32 =
  signExtend(((inst shr 2) and 1) shl 5 or ((inst shr 3) and 0x3f) shl 1 or
             ((inst shr 11) and 1) shl 7 or ((inst shr 7) and 1) shl 6 or
             ((inst shr 9) and 0xf) shl 3 or ((inst shr 8) and 1) shl 8, 12)
proc cBOffset(inst: uint32): uint32 =
  signExtend(((inst shr 3) and 7) shl 1 or ((inst shr 10) and 3) shl 3 or
             ((inst shr 2) and 1) shl 5 or ((inst shr 5) and 3) shl 6, 9)

# ---- Decoders ----
proc decodeI32(instr: uint32): (HOp, bool, bool) =
  var o: HOp
  o.rd = uint8((instr shr 7) and 0x1f)
  o.rs1 = uint8((instr shr 15) and 0x1f)
  o.rs2 = uint8((instr shr 20) and 0x1f)
  let f3 = (instr shr 12) and 7
  case instr and 0x7f
  of 0x13:  # OP-IMM
    o.imm = cast[int32](immI(instr))
    case f3
    of 0: o.kind = kAddi
    of 2: o.kind = kSlti
    of 3: o.kind = kSltiu
    of 4: o.kind = kXori
    of 6: o.kind = kOri
    of 7: o.kind = kAndi
    of 1: o.kind = kSlli
    of 5:
      if (o.imm and 0x400) != 0: o.kind = kSrai
      else: o.kind = kSrli
    else: discard
    return (o, true, false)
  of 0x33:  # OP / M
    let f7 = instr shr 25
    if f7 == 1:
      o.kind = kMulExt
      o.imm = int32(f3)
      return (o, true, false)
    case f3
    of 0: o.kind = if (f7 and 0x20) != 0: kSub else: kAdd
    of 1: o.kind = kSll
    of 2: o.kind = kSlt
    of 3: o.kind = kSltu
    of 4: o.kind = kXor
    of 5: o.kind = if (f7 and 0x20) != 0: kSra else: kSrl
    of 6: o.kind = kOr
    of 7: o.kind = kAnd
    else: discard
    return (o, true, false)
  of 0x63:  # BRANCH
    o.imm = int32(immB(instr))
    case f3
    of 0: o.kind = kBeq
    of 1: o.kind = kBne
    of 4: o.kind = kBlt
    of 5: o.kind = kBge
    of 6: o.kind = kBltu
    of 7: o.kind = kBgeu
    else: return (o, false, true)
    return (o, true, true)
  of 0x03:  # LOAD
    o.imm = int32(immI(instr))
    if o.rd == 0: return (o, false, true)
    case f3
    of 0: o.kind = kLb
    of 1: o.kind = kLh
    of 2: o.kind = kLw
    of 4: o.kind = kLbu
    of 5: o.kind = kLhu
    else: return (o, false, true)
    return (o, true, false)
  of 0x23:  # STORE
    o.imm = int32(immS(instr))
    case f3
    of 0: o.kind = kSb
    of 1: o.kind = kSh
    of 2: o.kind = kSw
    else: return (o, false, true)
    return (o, true, false)
  of 0x37: o.kind = kLui; o.imm = int32(instr and 0xfffff000'u32); return (o, true, false)
  of 0x17: o.kind = kAuipc; o.imm = int32(instr and 0xfffff000'u32); return (o, true, false)
  of 0x6f: o.kind = kJal; o.imm = int32(immJ(instr)); return (o, true, true)
  of 0x67: o.kind = kJalr; o.imm = int32(immI(instr)); return (o, true, true)
  of 0x0f: o.kind = kNop; return (o, true, false)
  else: return (o, false, true)

proc decodeI16(inst: uint32): (HOp, bool, bool) =
  var o: HOp
  case inst and 3
  of 0:
    case (inst shr 13) and 7
    of 0:
      let imm = cAddi4spnImm(inst)
      if imm == 0: return (o, false, true)
      o.kind = kCAddi4spn; o.rd = uint8(cReg((inst shr 2) and 7)); o.imm = int32(imm)
      return (o, true, false)
    of 2:
      o.kind = kCLw; o.rd = uint8(cReg((inst shr 2) and 7)); o.rs1 = uint8(cReg((inst shr 7) and 7)); o.imm = int32(cLWOff(inst))
      return (o, true, false)
    of 6:
      o.kind = kCSw; o.rs2 = uint8(cReg((inst shr 2) and 7)); o.rs1 = uint8(cReg((inst shr 7) and 7)); o.imm = int32(cLWOff(inst))
      return (o, true, false)
    else: discard
  of 1:
    let rd5 = (inst shr 7) and 0x1f
    let cd = uint8(cReg((inst shr 7) and 7))
    let simm = int32(signExtend(((inst shr 12) and 1) shl 5 or ((inst shr 2) and 0x1f), 6))
    case (inst shr 13) and 7
    of 0:
      if rd5 == 0: return (o, false, true)
      o.kind = kCAddi; o.rd = uint8(rd5); o.rs1 = uint8(rd5); o.imm = simm
      return (o, true, false)
    of 1: o.kind = kCJal; o.imm = int32(cJOffset(inst)); return (o, true, true)
    of 2:
      if rd5 == 0: return (o, false, true)
      o.kind = kCLi; o.rd = uint8(rd5); o.imm = simm
      return (o, true, false)
    of 3:
      if rd5 == 2: o.kind = kCAddi16sp; o.imm = int32(cAddi16spImm(inst)); return (o, true, false)
      if rd5 == 0: return (o, false, true)
      o.kind = kCLui; o.rd = uint8(rd5); o.imm = simm
      return (o, true, false)
    of 4:
      case (inst shr 10) and 3
      of 0: o.kind = kCSrli; o.rd = cd; o.rs1 = cd; o.imm = int32((inst shr 2) and 0x1f); return (o, true, false)
      of 1: o.kind = kCSrai; o.rd = cd; o.rs1 = cd; o.imm = int32((inst shr 2) and 0x1f); return (o, true, false)
      of 2: o.kind = kCAndi; o.rd = cd; o.rs1 = cd; o.imm = simm; return (o, true, false)
      else:
        o.rd = cd; o.rs1 = cd; o.rs2 = uint8(cReg((inst shr 2) and 7))
        case (inst shr 5) and 3
        of 0: o.kind = kCSub
        of 1: o.kind = kCXor
        of 2: o.kind = kCOr
        of 3: o.kind = kCAnd
        else: discard
        return (o, true, false)
    of 5: o.kind = kCJ; o.imm = int32(cJOffset(inst)); return (o, true, true)
    of 6: o.kind = kCBeqz; o.rs1 = uint8(cReg((inst shr 7) and 7)); o.imm = int32(cBOffset(inst)); return (o, true, true)
    of 7: o.kind = kCBnez; o.rs1 = uint8(cReg((inst shr 7) and 7)); o.imm = int32(cBOffset(inst)); return (o, true, true)
    else: discard
  of 2:
    let rd5 = (inst shr 7) and 0x1f
    let shamt = int32((inst shr 2) and 0x1f)
    case (inst shr 13) and 7
    of 0:
      if rd5 == 0: return (o, false, true)
      o.kind = kCSlli; o.rd = uint8(rd5); o.rs1 = uint8(rd5); o.imm = shamt
      return (o, true, false)
    of 2:
      if rd5 == 0: return (o, false, true)
      o.kind = kCLwsp; o.rd = uint8(rd5); o.rs1 = 2; o.imm = int32(cLWSPOff(inst))
      return (o, true, false)
    of 6:
      o.kind = kCSwsp; o.rs2 = uint8((inst shr 2) and 0x1f); o.rs1 = 2; o.imm = int32(cSWSPOff(inst))
      return (o, true, false)
    of 4:
      let rs2 = (inst shr 2) and 0x1f
      if ((inst shr 12) and 1) == 0:
        if rs2 == 0:
          if rd5 == 0: return (o, false, true)
          o.kind = kCJr; o.rd = uint8(rd5); o.rs1 = uint8(rd5); return (o, true, true)
        if rd5 == 0: return (o, false, true)
        o.kind = kCMv; o.rd = uint8(rd5); o.rs2 = uint8(rs2); return (o, true, false)
      if rs2 == 0:
        if rd5 == 0: return (o, false, true)
        o.kind = kCJalr; o.rd = uint8(rd5); o.rs1 = uint8(rd5); return (o, true, true)
      if rd5 == 0: return (o, false, true)
      o.kind = kCAdd; o.rd = uint8(rd5); o.rs1 = uint8(rd5); o.rs2 = uint8(rs2)
      return (o, true, false)
    else: discard
  else: discard
  return (o, false, true)

# ---- Block decode ----
proc decodeBlock(blk: var HBlock, pc, off: uint32): bool =
  let page = off shr 12
  var lim: uint32 = FlatSize
  let pageEnd = (page + 1) shl 12
  if pageEnd < lim: lim = pageEnd
  var cur = off
  var curPC = pc
  var n = 0
  while n < blockMaxOps:
    if cur + 2 > lim: break
    let low = uint32(flat[cur]) or (uint32(flat[cur+1]) shl 8)
    var o: HOp
    var ok, term: bool
    if (low and 3) == 3:
      if cur + 4 > lim: break
      let instr = low or ((uint32(flat[cur+2]) or (uint32(flat[cur+3]) shl 8)) shl 16)
      (o, ok, term) = decodeI32(instr)
      o.ln = 4
    else:
      (o, ok, term) = decodeI16(low)
      o.ln = 2
    if not ok: break
    blk.ops[n] = o
    inc n
    cur += uint32(o.ln)
    curPC += uint32(o.ln)
    if term: break
  if n == 0: return false
  blk.start = pc
  blk.gen = codeGen[page]
  blk.n = uint8(n)
  codePage[page] = true
  result = true

proc blockFor(pc, off: uint32): ptr HBlock =
  let idx = int(((pc shr 1) xor (pc shr 10)) and uint32(blockSlots - 1))
  result = addr(blocks[idx])
  if result.start == pc and result.gen == codeGen[off shr 12]:
    return result
  if not decodeBlock(result[], pc, off):
    return nil
  return result

# ---- M extension ----
proc execM(f3: uint32, rd: int, rs1, rs2: uint32) =
  case f3
  of 0:
    if rd != 0: x[rd] = rs1 * rs2
  of 1:
    if rd != 0: x[rd] = (int32(rs1) shl (rs2 and 0x1f)).uint32
  of 4:  # DIV
    if rs2 == 0: x[rd] = 0xffffffff'u32
    else: x[rd] = (int32(rs1) div int32(rs2)).uint32
  of 5:  # DIVU
    if rs2 == 0: x[rd] = 0xffffffff'u32
    else: x[rd] = rs1 div rs2
  of 6:  # REM
    if rs2 == 0: x[rd] = rs1
    else: x[rd] = (int32(rs1) mod int32(rs2)).uint32
  of 7:  # REMU
    if rs2 == 0: x[rd] = rs1
    else: x[rd] = rs1 mod rs2
  else: discard

# ---- touchCode ----
proc touchCode(off: uint32) =
  let p = off shr 12
  if codePage[p]:
    inc codeGen[p]

# ---- runBlock: chain blocks together ----
proc runBlock(blk: ptr HBlock, budget: int): (int, bool) =
  var blk = blk
  var curPC = blk.start
  var done = 0
  while true:
    var n = int(blk.n)
    let remain = budget - done
    if n > remain: n = remain
    for i in 0..<n:
      let o = blk.ops[i]
      let next = curPC + uint32(o.ln)
      case o.kind
      of kAddi:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] + uint32(o.imm)
        curPC = next
      of kSlti:
        if o.rd != 0: x[o.rd and 31] = (int32(x[o.rs1 and 31]) < o.imm).uint32
        curPC = next
      of kSltiu:
        if o.rd != 0: x[o.rd and 31] = (x[o.rs1 and 31] < uint32(o.imm)).uint32
        curPC = next
      of kXori:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] xor uint32(o.imm)
        curPC = next
      of kOri:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] or uint32(o.imm)
        curPC = next
      of kAndi:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] and uint32(o.imm)
        curPC = next
      of kSlli:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] shl (uint32(o.imm) and 0x1f)
        curPC = next
      of kSrli:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] shr (uint32(o.imm) and 0x1f)
        curPC = next
      of kSrai:
        if o.rd != 0: x[o.rd and 31] = (int32(x[o.rs1 and 31]) shr (uint32(o.imm) and 0x1f)).uint32
        curPC = next
      of kAdd:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] + x[o.rs2 and 31]
        curPC = next
      of kSub:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] - x[o.rs2 and 31]
        curPC = next
      of kSll:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] shl (x[o.rs2 and 31] and 0x1f)
        curPC = next
      of kSlt:
        if o.rd != 0: x[o.rd and 31] = (int32(x[o.rs1 and 31]) < int32(x[o.rs2 and 31])).uint32
        curPC = next
      of kSltu:
        if o.rd != 0: x[o.rd and 31] = (x[o.rs1 and 31] < x[o.rs2 and 31]).uint32
        curPC = next
      of kXor:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] xor x[o.rs2 and 31]
        curPC = next
      of kSrl:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] shr (x[o.rs2 and 31] and 0x1f)
        curPC = next
      of kSra:
        if o.rd != 0: x[o.rd and 31] = (int32(x[o.rs1 and 31]) shr (x[o.rs2 and 31] and 0x1f)).uint32
        curPC = next
      of kOr:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] or x[o.rs2 and 31]
        curPC = next
      of kAnd:
        if o.rd != 0: x[o.rd and 31] = x[o.rs1 and 31] and x[o.rs2 and 31]
        curPC = next
      of kMulExt:
        execM(uint32(o.imm), int(o.rd and 31), x[o.rs1 and 31], x[o.rs2 and 31])
        curPC = next
      of kLui:
        if o.rd != 0: x[o.rd and 31] = uint32(o.imm)
        curPC = next
      of kAuipc:
        if o.rd != 0: x[o.rd and 31] = curPC + uint32(o.imm)
        curPC = next
      of kJal:
        if o.rd != 0: x[o.rd and 31] = curPC + 4
        curPC += uint32(o.imm)
      of kJalr:
        let t = (x[o.rs1 and 31] + uint32(o.imm)) and 0xfffffffe'u32
        if o.rd != 0: x[o.rd and 31] = curPC + 4
        curPC = t
      of kBeq:
        if x[o.rs1 and 31] == x[o.rs2 and 31]: curPC += uint32(o.imm)
        else: curPC = next
      of kBne:
        if x[o.rs1 and 31] != x[o.rs2 and 31]: curPC += uint32(o.imm)
        else: curPC = next
      of kBlt:
        if int32(x[o.rs1 and 31]) < int32(x[o.rs2 and 31]): curPC += uint32(o.imm)
        else: curPC = next
      of kBge:
        if int32(x[o.rs1 and 31]) >= int32(x[o.rs2 and 31]): curPC += uint32(o.imm)
        else: curPC = next
      of kBltu:
        if x[o.rs1 and 31] < x[o.rs2 and 31]: curPC += uint32(o.imm)
        else: curPC = next
      of kBgeu:
        if x[o.rs1 and 31] >= x[o.rs2 and 31]: curPC += uint32(o.imm)
        else: curPC = next
      of kLw:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          x[o.rd and 31] = uint32(flat[doff]) or (uint32(flat[doff+1]) shl 8) or
                           (uint32(flat[doff+2]) shl 16) or (uint32(flat[doff+3]) shl 24)
          curPC = next
        else:
          return (done + i, true)
      of kLb:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff < FlatSize:
          x[o.rd and 31] = (int8(flat[doff])).uint32
          curPC = next
        else: return (done + i, true)
      of kLh:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 1 < FlatSize:
          x[o.rd and 31] = (int16(uint16(flat[doff]) or (uint16(flat[doff+1]) shl 8))).uint32
          curPC = next
        else: return (done + i, true)
      of kLbu:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff < FlatSize:
          x[o.rd and 31] = uint32(flat[doff])
          curPC = next
        else: return (done + i, true)
      of kLhu:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 1 < FlatSize:
          x[o.rd and 31] = uint32(flat[doff]) or (uint32(flat[doff+1]) shl 8)
          curPC = next
        else: return (done + i, true)
      of kSw:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          let v = x[o.rs2 and 31]
          flat[doff] = v.uint8; flat[doff+1] = (v shr 8).uint8
          flat[doff+2] = (v shr 16).uint8; flat[doff+3] = (v shr 24).uint8
          touchCode(doff)
          curPC = next
        else: return (done + i, true)
      of kSb:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff < FlatSize:
          flat[doff] = x[o.rs2 and 31].uint8
          touchCode(doff)
          curPC = next
        else: return (done + i, true)
      of kSh:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 1 < FlatSize:
          let v = x[o.rs2 and 31]
          flat[doff] = v.uint8; flat[doff+1] = (v shr 8).uint8
          touchCode(doff)
          curPC = next
        else: return (done + i, true)
      of kNop: curPC = next
      of kCAddi4spn: x[o.rd and 31] = x[2] + uint32(o.imm); curPC = next
      of kCLw:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          x[o.rd and 31] = uint32(flat[doff]) or (uint32(flat[doff+1]) shl 8) or
                           (uint32(flat[doff+2]) shl 16) or (uint32(flat[doff+3]) shl 24)
          curPC = next
        else: return (done + i, true)
      of kCSw:
        let ea = x[o.rs1 and 31] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          let v = x[o.rs2 and 31]
          flat[doff] = v.uint8; flat[doff+1] = (v shr 8).uint8
          flat[doff+2] = (v shr 16).uint8; flat[doff+3] = (v shr 24).uint8
          touchCode(doff)
          curPC = next
        else: return (done + i, true)
      of kCAddi: x[o.rd and 31] = x[o.rs1 and 31] + uint32(o.imm); curPC = next
      of kCJal: x[1] = curPC + 2; curPC += uint32(o.imm)
      of kCLi: x[o.rd and 31] = uint32(o.imm); curPC = next
      of kCAddi16sp: x[2] = x[2] + uint32(o.imm); curPC = next
      of kCLui: x[o.rd and 31] = uint32(o.imm) shl 12; curPC = next
      of kCSrli: x[o.rd and 31] = x[o.rs1 and 31] shr (uint32(o.imm) and 0x1f); curPC = next
      of kCSrai: x[o.rd and 31] = (int32(x[o.rs1 and 31]) shr (uint32(o.imm) and 0x1f)).uint32; curPC = next
      of kCAndi: x[o.rd and 31] = x[o.rs1 and 31] and uint32(o.imm); curPC = next
      of kCSub: x[o.rd and 31] = x[o.rs1 and 31] - x[o.rs2 and 31]; curPC = next
      of kCXor: x[o.rd and 31] = x[o.rs1 and 31] xor x[o.rs2 and 31]; curPC = next
      of kCOr: x[o.rd and 31] = x[o.rs1 and 31] or x[o.rs2 and 31]; curPC = next
      of kCAnd: x[o.rd and 31] = x[o.rs1 and 31] and x[o.rs2 and 31]; curPC = next
      of kCJ: curPC += uint32(o.imm)
      of kCBeqz:
        if x[o.rs1 and 31] == 0: curPC += uint32(o.imm)
        else: curPC = next
      of kCBnez:
        if x[o.rs1 and 31] != 0: curPC += uint32(o.imm)
        else: curPC = next
      of kCSlli: x[o.rd and 31] = x[o.rs1 and 31] shl (uint32(o.imm) and 0x1f); curPC = next
      of kCLwsp:
        let ea = x[2] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          x[o.rd and 31] = uint32(flat[doff]) or (uint32(flat[doff+1]) shl 8) or
                           (uint32(flat[doff+2]) shl 16) or (uint32(flat[doff+3]) shl 24)
          curPC = next
        else: return (done + i, true)
      of kCSwsp:
        let ea = x[2] + uint32(o.imm)
        let doff = ea - FlatBase
        if doff + 3 < FlatSize:
          let v = x[o.rs2 and 31]
          flat[doff] = v.uint8; flat[doff+1] = (v shr 8).uint8
          flat[doff+2] = (v shr 16).uint8; flat[doff+3] = (v shr 24).uint8
          touchCode(doff)
          curPC = next
        else: return (done + i, true)
      of kCJr: curPC = x[o.rd and 31] and 0xfffffffe'u32
      of kCMv: x[o.rd and 31] = x[o.rs2 and 31]; curPC = next
      of kCJalr: x[1] = curPC + 2; curPC = x[o.rs1 and 31] and 0xfffffffe'u32
      of kCAdd: x[o.rd and 31] = x[o.rs1 and 31] + x[o.rs2 and 31]; curPC = next
    discard
    done += n
    if done >= budget:
      pc = curPC
      return (done, false)
    let off = curPC - FlatBase
    if off >= FlatSize - 3:
      pc = curPC
      return (done, false)
    let succIdx = int(((curPC shr 1) xor (curPC shr 10)) and uint32(blockSlots - 1))
    if blocks[succIdx].start == curPC and blocks[succIdx].gen == codeGen[off shr 12]:
      blk = addr(blocks[succIdx])
      continue
    blk = blockFor(curPC, off)
    if blk == nil:
      pc = curPC
      return (done, false)

# ---- ELF loader ----
proc u32at(d: string, off: int): uint32 =
  uint32(d[off]) or (uint32(d[off+1]) shl 8) or (uint32(d[off+2]) shl 16) or (uint32(d[off+3]) shl 24)
proc u16at(d: string, off: int): uint16 =
  uint16(d[off]) or (uint16(d[off+1]) shl 8)

proc loadElf(path: string) =
  let f = open(path)
  let data = f.readAll()
  f.close()
  let e_phoff = u32at(data, 28)
  let e_phentsize = u16at(data, 42)
  let e_phnum = u16at(data, 44)
  pc = u32at(data, 24)
  for i in 0..<int(e_phnum):
    let phoff = int(e_phoff) + i*int(e_phentsize)
    let p_type = u32at(data, phoff)
    if p_type != 1: continue
    let p_offset = u32at(data, phoff+4)
    let p_vaddr = u32at(data, phoff+8)
    let p_filesz = u32at(data, phoff+16)
    let p_memsz = u32at(data, phoff+20)
    for j in 0..<int(p_memsz):
      let b = if j < int(p_filesz): data[int(p_offset)+j].uint8 else: 0'u8
      let doff = p_vaddr + uint32(j) - FlatBase
      if doff < FlatSize:
        flat[doff] = b
  echo "entry=0x", pc.toHex(8)

# ---- RunHot main loop ----
proc runHot(max: uint64) =
  var count: uint64 = 0
  while max == 0 or count < max:
    if halted: break
    let off = pc - FlatBase
    if off >= FlatSize - 3:
      # outside flat — fallback (not expected for benchmark)
      break
    let blk = blockFor(pc, off)
    if blk == nil:
      # step fallback for system instructions
      let low = uint32(flat[off]) or (uint32(flat[off+1]) shl 8)
      if (low and 3) == 3: pc += 4 else: pc += 2
      count += 1
      continue
    var k = 0x7fffffff
    if max != 0 and (max - count) < uint64(k):
      k = int(max - count)
    let (n, stepped) = runBlock(blk, k)
    count += uint64(n)
    if stepped: count += 1
  instCount += count

when isMainModule:
  let args = commandLineParams()
  if args.len < 1:
    echo "usage: rv32emu_nim <elf> [max]"
    quit(1)
  let maxInst = if args.len >= 2: parseInt(args[1]).uint64 else: 5000000'u64
  loadElf(args[0])
  echo "=== start ==="
  let t0 = cpuTime()
  runHot(maxInst)
  let t1 = cpuTime()
  let dt = t1 - t0
  echo "=== stopped after ", instCount, " inst ==="
  echo "time: ", formatFloat(dt, ffDecimal, 3), " s"
  if dt > 0:
    echo "MIPS: ", formatFloat(float64(instCount) / dt / 1e6, ffDecimal, 1)
