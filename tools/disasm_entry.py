#!/usr/bin/env python3
import struct, sys

ELF = r"C:\Users\SONG\Documents\Projects\TinyOS\build\kernel-esp32c3.elf"

def load():
    with open(ELF, "rb") as f:
        data = f.read()
    assert data[:4] == b"\x7fELF"
    ei_class = data[4]      # 1=32
    ei_data = data[5]       # 1=LE
    assert ei_class == 1
    end = "<" if ei_data == 1 else ">"
    (e_type, e_machine, e_version, e_entry, e_phoff, e_shoff, e_flags,
     e_ehsize, e_phentsize, e_phnum, e_shentsize, e_shnum, e_shstrndx) = struct.unpack_from(end+"HHIIIIIHHHHHH", data, 16)
    phdrs = []
    for i in range(e_phnum):
        off = e_phoff + i*e_phentsize
        p_type, p_offset, p_vaddr, p_paddr, p_filesz, p_memsz, p_flags, p_align = struct.unpack_from(end+"IIIIIIII", data, off)
        phdrs.append((p_type, p_offset, p_vaddr, p_filesz, p_memsz))
    return data, end, e_entry, phdrs

def read_vaddr(data, phdrs, vaddr, n):
    out = bytearray()
    for (pt, po, pv, pfs, pms) in phdrs:
        if pt != 1:  # PT_LOAD
            continue
        if pv <= vaddr < pv + pfs:
            take = min(n - len(out), pv + pfs - vaddr)
            out += data[po + (vaddr - pv): po + (vaddr - pv) + take]
            vaddr += take
            if len(out) >= n:
                break
    return bytes(out)

def sign(x, b):
    if x & (1 << (b-1)):
        return x - (1 << b)
    return x

# RV32/RVC disassembler (sufficient for _start + trap).
# w is the full 32-bit instruction when bits[1:0]==3, else the 16-bit half.
def disasm(pc, w):
    if w & 0x3 == 0x3:
        ins = w
        op = ins & 0x7f
        rd = (ins >> 7) & 0x1f
        f3 = (ins >> 12) & 0x7
        rs1 = (ins >> 15) & 0x1f
        rs2 = (ins >> 20) & 0x1f
        imm_i = sign(ins >> 20, 12)
        if op == 0x37: return "lui x%d,0x%x" % (rd, ins & 0xfffff000)
        if op == 0x17: return "auipc x%d,0x%x" % (rd, ins & 0xfffff000)
        if op == 0x6f:
            imm = sign(((ins>>31)&1)<<20 | ((ins>>12)&0xff)<<12 | ((ins>>20)&1)<<11 | ((ins>>21)&0x3ff)<<1, 21)
            return "jal x%d,0x%x" % (rd, pc + imm)
        if op == 0x67:
            return "jalr x%d,x%d,%d" % (rd, rs1, imm_i)
        if op == 0x63:
            imm = sign(((ins>>31)&1)<<12 | ((ins>>7)&1)<<11 | ((ins>>25)&0x3f)<<5 | ((ins>>8)&0xf)<<1, 13)
            names = {0:"beq",1:"bne",4:"blt",5:"bge",6:"bltu",7:"bgeu"}
            return "%s x%d,x%d,0x%x" % (names.get(f3,'?'), rs1, rs2, pc+imm)
        if op == 0x03:
            return "lw x%d,%d(x%d)" % (rd, imm_i, rs1)
        if op == 0x23:
            imm = sign(((ins>>25)&0x7f)<<5 | ((ins>>7)&0x1f), 12)
            return "sw x%d,%d(x%d)" % (rs2, imm, rs1)
        if op == 0x13:
            if f3 == 0: return "addi x%d,x%d,%d" % (rd, rs1, imm_i)
            if f3 == 4: return "xori x%d,x%d,%d" % (rd, rs1, imm_i)
            if f3 == 6: return "ori x%d,x%d,%d" % (rd, rs1, imm_i)
            if f3 == 7: return "andi x%d,x%d,%d" % (rd, rs1, imm_i)
            if f3 == 1: return "slli x%d,x%d,%d" % (rd, rs1, imm_i & 0x1f)
            if f3 == 5:
                if imm_i & 0x400: return "srai x%d,x%d,%d" % (rd, rs1, imm_i&0x1f)
                return "srli x%d,x%d,%d" % (rd, rs1, imm_i&0x1f)
        if op == 0x33:
            if (ins>>25)&0x7f == 1: return "mul/div x%d (M)" % rd
            fn = (ins>>25)&0x7f
            names={0:"add" if not fn&0x20 else "sub",1:"sll",2:"slt",3:"sltu",4:"xor",5:"srl" if not fn&0x20 else "sra",6:"or",7:"and"}
            return "%s x%d,x%d,x%d" % (names.get(f3,'?'), rd, rs1, rs2)
        if op == 0x73:
            csr = (ins>>20)&0xfff
            fr = (ins>>12)&0x7
            if fr==0:
                if csr==0: return "ecall"
                if csr==1: return "ebreak"
                if csr==0x302: return "mret"
                if csr==0x105: return "wfi"
                return "csr-sys csr=0x%x" % csr
            return "csr x%d,0x%x" % (rd, csr)
        if op == 0x0f: return "fence"
        return "??32 op=0x%x" % op
    else:
        # compressed
        def creg(b): return (b & 0x7) + 8
        op = w & 0x3
        f3 = (w >> 13) & 0x7
        if op == 0:
            if f3 == 0:
                rdp = creg((w>>2)&7)
                imm = ((w>>7)&0xF)<<6 | ((w>>11)&3)<<4 | ((w>>6)&1)<<2 | ((w>>5)&1)<<3
                return "c.addi4spn x%d,%d" % (rdp, imm)
            if f3 == 2: return "c.lw x%d,..(x%d)" % (creg((w>>4)&7), creg((w>>7)&7))
            if f3 == 6: return "c.sw x%d,..(x%d)" % (creg((w>>2)&7), creg((w>>7)&7))
            return "c0 f3=%d" % f3
        if op == 1:
            rd = (w>>7)&0x1f
            cd = creg((w>>7)&7)
            imm5 = (w>>12)&1
            imm40 = (w>>2)&0x1f
            simm = sign((imm5<<5)|imm40, 6)
            if f3 == 0: return "c.addi x%d,%d" % (rd, simm)
            if f3 == 1:
                # C.JAL: imm[11]=w12 imm[4]=w11 imm[9:8]=w10:9 imm[10]=w8 imm[6]=w7 imm[7]=w6 imm[3:1]=w5:3 imm[5]=w2
                imm = sign(((w>>12)&1)<<11 | ((w>>11)&1)<<4 | ((w>>10)&1)<<9 | ((w>>9)&1)<<8 |
                          ((w>>8)&1)<<10 | ((w>>7)&1)<<6 | ((w>>6)&1)<<7 |
                          ((w>>5)&1)<<3 | ((w>>4)&1)<<2 | ((w>>3)&1)<<1 | ((w>>2)&1)<<5, 12)
                return "c.jal ra,0x%x (off=%d)" % (pc+imm, imm)
            if f3 == 2: return "c.li x%d,%d" % (cd, simm)
            if f3 == 3:
                if rd==2: return "c.addi16sp %d" % simm
                return "c.lui x%d,0x%x" % (rd, simm)
            if f3 == 4:
                sub=(w>>10)&3; sh=(w>>2)&0x1f
                if sub==0: return "c.srli x%d,%d" % (cd, sh)
                if sub==1: return "c.srai x%d,%d" % (cd, sh)
                if sub==2: return "c.andi x%d,%d" % (cd, simm)
                if sub==3:
                    rs2=creg((w>>2)&7); f2=(w>>5)&3
                    n={0:"sub",1:"xor",2:"or",3:"and"}[f2]
                    return "c.%s x%d,x%d" % (n, cd, rs2)
            if f3 == 5:
                # C.J
                imm = sign(((w>>12)&1)<<11 | ((w>>11)&1)<<4 | ((w>>10)&1)<<9 | ((w>>9)&1)<<8 |
                          ((w>>8)&1)<<10 | ((w>>7)&1)<<6 | ((w>>6)&1)<<7 |
                          ((w>>5)&1)<<3 | ((w>>4)&1)<<2 | ((w>>3)&1)<<1 | ((w>>2)&1)<<5, 12)
                return "c.j 0x%x (off=%d)" % (pc+imm, imm)
            if f3 == 6:
                rs1=creg((w>>7)&7)
                imm = sign(((w>>12)&1)<<8 | ((w>>6)&1)<<7 | ((w>>5)&1)<<6 | ((w>>2)&1)<<5 |
                          ((w>>11)&1)<<4 | ((w>>10)&1)<<3 | ((w>>4)&1)<<2 | ((w>>3)&1)<<1, 9)
                return "c.beqz x%d,0x%x (off=%d)" % (rs1, pc+imm, imm)
            if f3 == 7:
                rs1=creg((w>>7)&7)
                imm = sign(((w>>12)&1)<<8 | ((w>>6)&1)<<7 | ((w>>5)&1)<<6 | ((w>>2)&1)<<5 |
                          ((w>>11)&1)<<4 | ((w>>10)&1)<<3 | ((w>>4)&1)<<2 | ((w>>3)&1)<<1, 9)
                return "c.bnez x%d,0x%x (off=%d)" % (rs1, pc+imm, imm)
        if op == 2:
            rd=(w>>7)&0x1f
            if f3==0: return "c.slli x%d,%d" % (rd, (w>>2)&0x1f)
            if f3==2: return "c.lwsp x%d" % rd
            if f3==6: return "c.swsp x%d" % ((w>>2)&0x1f)
            if f3==4:
                b12=(w>>12)&1; rs2=(w>>2)&0x1f
                if b12==0:
                    if rs2==0: return "c.jr x%d" % rd
                    return "c.mv x%d,x%d" % (rd, rs2)
                else:
                    if rd==0 and rs2==0: return "c.ebreak"
                    if rs2==0: return "c.jalr x%d" % rd
                    return "c.add x%d,x%d" % (rd, rs2)
        return "c? op=%d f3=%d" % (op, f3)

def main():
    data, end, entry, phdrs = load()
    print("entry = 0x%08X" % entry)
    start = 0x40380000
    n = 0x70
    if len(sys.argv) >= 2:
        start = int(sys.argv[1], 0)
    if len(sys.argv) >= 3:
        n = int(sys.argv[2], 0)
    raw = read_vaddr(data, phdrs, start, n)
    pc = start
    i = 0
    while i < len(raw) - 1:
        w0 = raw[i] | (raw[i+1] << 8)
        if (w0 & 0x3) == 0x3:
            w1 = raw[i+2] | (raw[i+3] << 8)
            ins = w0 | (w1 << 16)
            mnem = disasm(pc, ins)
            print("0x%08X:  %04X %04X   %s" % (pc, w0, w1, mnem))
            pc += 4; i += 4
        else:
            mnem = disasm(pc, w0)
            print("0x%08X:  %04X       %s" % (pc, w0, mnem))
            pc += 2; i += 2

main()
