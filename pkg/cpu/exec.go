package cpu

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"math/bits"
	"time"
)

// ALU operation kinds (x86 group encodings 0-7)
const (
	ALUAdd = iota
	ALUOr
	ALUAdc
	ALUSbb
	ALUAnd
	ALUSub
	ALUXor
	ALUCmp
)

func sext(v uint64, fromBytes int) uint64 {
	if fromBytes >= 8 {
		return v
	}
	s := uint(64 - fromBytes*8)
	return uint64(int64(v<<s) >> s)
}

func maskW(v uint64, w int) uint64 {
	switch w {
	case 1:
		return v & 0xFF
	case 2:
		return v & 0xFFFF
	case 4:
		return v & 0xFFFFFFFF
	}
	return v
}

func boolU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func (c *CPUState) setPF(res uint64) {
	if bits.OnesCount8(uint8(res))&1 == 0 {
		c.RFLAGS |= FLAG_PF
	} else {
		c.RFLAGS &^= FLAG_PF
	}
}

// ALU computes kind(a, b) at operand width w, sets flags, returns masked result.
func ALU(c *CPUState, kind int, a, b uint64, w int) uint64 {
	var m, msb uint64
	if w >= 8 {
		m = ^uint64(0)
		msb = 1 << 63
	} else {
		bitsN := uint(w * 8)
		m = 1<<bitsN - 1
		msb = 1 << (bitsN - 1)
	}
	a &= m
	b &= m
	var res, eb uint64
	var cf, of bool

	switch kind {
	case ALUAdd, ALUAdc:
		var carry uint64
		if kind == ALUAdc && c.RFLAGS&FLAG_CF != 0 {
			carry = 1
		}
		if w >= 8 {
			var c1, c2 uint64
			res, c1 = bits.Add64(a, b, 0)
			res, c2 = bits.Add64(res, carry, 0)
			cf = c1 != 0 || c2 != 0
		} else {
			full := a + b + carry
			res = full & m
			cf = full > m
		}
		eb = b + carry
		of = ((^(a ^ b)) & (a ^ res) & msb) != 0
	case ALUSub, ALUCmp, ALUSbb:
		var borrow uint64
		if kind == ALUSbb && c.RFLAGS&FLAG_CF != 0 {
			borrow = 1
		}
		if w >= 8 {
			var b1, b2 uint64
			res, b1 = bits.Sub64(a, b, 0)
			res, b2 = bits.Sub64(res, borrow, 0)
			cf = b1 != 0 || b2 != 0
		} else {
			eb = b + borrow
			res = (a - eb) & m
			cf = eb > a
		}
		eb = (b + borrow) & m
		of = ((a ^ eb) & (a ^ res) & msb) != 0
	default:
		switch kind {
		case ALUAnd:
			res = a & b
		case ALUOr:
			res = a | b
		default:
			res = a ^ b
		}
		cf, of = false, false
	}

	res &= m
	c.SetZF(res == 0)
	c.SetSF(res&msb != 0)
	c.setPF(res)
	if kind == ALUAnd || kind == ALUOr || kind == ALUXor {
		c.RFLAGS &^= FLAG_CF
		c.RFLAGS &^= FLAG_OF
	} else {
		if cf {
			c.RFLAGS |= FLAG_CF
		} else {
			c.RFLAGS &^= FLAG_CF
		}
		if of {
			c.RFLAGS |= FLAG_OF
		} else {
			c.RFLAGS &^= FLAG_OF
		}
	}
	return res
}

// evalCC evaluates condition code cc (0-15) against current flags.
func evalCC(c *CPUState, cc uint8) bool {
	cf := c.RFLAGS&FLAG_CF != 0
	zf := c.RFLAGS&FLAG_ZF != 0
	sf := c.RFLAGS&FLAG_SF != 0
	of := c.RFLAGS&FLAG_OF != 0
	pf := c.RFLAGS&FLAG_PF != 0
	switch cc & 0xF {
	case 0x0:
		return of
	case 0x1:
		return !of
	case 0x2:
		return cf
	case 0x3:
		return !cf
	case 0x4:
		return zf
	case 0x5:
		return !zf
	case 0x6:
		return cf || zf
	case 0x7:
		return !cf && !zf
	case 0x8:
		return sf
	case 0x9:
		return !sf
	case 0xA:
		return pf
	case 0xB:
		return !pf
	case 0xC:
		return sf != of
	case 0xD:
		return sf == of
	case 0xE:
		return zf || sf != of
	default:
		return !zf && sf == of
	}
}

// shiftAlu implements group-2 shifts/rotates (kind 0..7) with x86 count masking.
func shiftAlu(c *CPUState, kind int, a uint64, count uint64, w int) uint64 {
	var m, msb uint64
	var bitsN uint
	if w >= 8 {
		m = ^uint64(0)
		msb = 1 << 63
		bitsN = 64
	} else {
		bitsN = uint(w * 8)
		m = 1<<bitsN - 1
		msb = 1 << (bitsN - 1)
	}
	limit := uint64(31)
	if w >= 8 {
		limit = 63
	}
	count &= limit
	if count == 0 {
		return a & m
	}
	a &= m
	orig := a
	var cf bool
	for i := uint64(0); i < count; i++ {
		switch kind {
		case 0: // ROL
			msbSet := a&msb != 0
			a = ((a << 1) | boolU64(msbSet)) & m
			cf = msbSet
		case 1: // ROR
			lsb := a & 1
			a = (a >> 1) | (lsb * msb)
			cf = lsb != 0
		case 2: // RCL
			in := uint64(0)
			if c.RFLAGS&FLAG_CF != 0 {
				in = 1
			}
			msbSet := a&msb != 0
			a = ((a << 1) | in) & m
			cf = msbSet
		case 3: // RCR
			in := uint64(0)
			if c.RFLAGS&FLAG_CF != 0 {
				in = msb
			}
			lsb := a & 1
			a = (a >> 1) | in
			cf = lsb != 0
		case 4, 6: // SHL / SAL
			cf = a&msb != 0
			a = (a << 1) & m
		case 5: // SHR
			cf = a&1 != 0
			a = a >> 1
		case 7: // SAR
			cf = a&1 != 0
			a = (a >> 1) | (a & msb)
		}
	}
	c.SetZF(a == 0)
	c.SetSF(a&msb != 0)
	c.setPF(a)
	if cf {
		c.RFLAGS |= FLAG_CF
	} else {
		c.RFLAGS &^= FLAG_CF
	}
	if count == 1 {
		var of bool
		switch kind {
		case 0, 2, 4, 6: // OF = MSB(result) XOR CF
			of = (a&msb != 0) != cf
		case 1: // ROR: XOR of two top result bits
			of = ((a>>(bitsN-1))&1) != ((a>>(bitsN-2))&1)
		case 3: // RCR
			of = (a&msb != 0) != cf
		case 5: // SHR: original MSB
			of = orig&msb != 0
		case 7: // SAR: always clear
			of = false
		}
		if of {
			c.RFLAGS |= FLAG_OF
		} else {
			c.RFLAGS &^= FLAG_OF
		}
	}
	return a
}

// mulPair returns low/high w-bit halves of a*b.
func mulPair(a, b uint64, w int, signed bool) (uint64, uint64) {
	if signed {
		a6, b6 := sext(a, w), sext(b, w)
		full, fullHi := bits.Mul64(a6, b6)
		if int64(a6) < 0 {
			fullHi -= b6
		}
		if int64(b6) < 0 {
			fullHi -= a6
		}
		if w >= 8 {
			return full, fullHi
		}
		return maskW(full, w), maskW(full>>uint(w*8), w)
	}
	full, fullHi := bits.Mul64(maskW(a, w), maskW(b, w))
	if w >= 8 {
		return full, fullHi
	}
	return maskW(full, w), maskW(full>>uint(w*8), w)
}

// ---------- decoder ----------

type decoder struct {
	c     *CPUState
	mem   Memory
	io    IOBus
	rip   uint64
	start uint64

	rex    bool
	rexW   bool
	rexR   bool
	rexX   bool
	rexB   bool
	op66   bool
	opF2   bool
	opF3   bool
}

func (d *decoder) fetch8() (uint8, error) {
	v, err := d.mem.Read8(d.rip)
	if err != nil {
		return 0, err
	}
	d.rip++
	return v, nil
}

func (d *decoder) fetchN(n int) (uint64, error) {
	var v uint64
	for i := 0; i < n; i++ {
		b, err := d.mem.Read8(d.rip)
		if err != nil {
			return 0, err
		}
		d.rip++
		v |= uint64(b) << (8 * i)
	}
	return v, nil
}

// fetchImm reads immBytes little-endian, sign-extended when signExt.
func (d *decoder) fetchImm(immBytes int, signExt bool) (uint64, error) {
	v, err := d.fetchN(immBytes)
	if err != nil {
		return 0, err
	}
	if signExt {
		v = sext(v, immBytes)
	}
	return v, nil
}

// width returns default operand size for current prefixes.
func (d *decoder) width() int {
	if d.rexW {
		return 8
	}
	if d.op66 {
		return 2
	}
	return 4
}

// pushW returns default stack slot size (64-bit unless 0x66).
func (d *decoder) pushW() int {
	if d.op66 {
		return 2
	}
	return 8
}

type rmRef struct {
	isReg  bool
	idx    int
	hiByte bool // byte 1 of Regs[idx] (AH/CH/DH/BH)
	addr   uint64
	ripRel bool
	disp   int64
}

// parseRM decodes the ModRM byte (+SIB+displacement) at current rip.
func (d *decoder) parseRM(w int) (byte, int, rmRef, rmRef, error) {
	modrm, err := d.fetch8()
	if err != nil {
		return 0, 0, rmRef{}, rmRef{}, err
	}
	mod := modrm >> 6
	regRaw := int((modrm >> 3) & 7)
	rmRaw := int(modrm & 7)

	regRef := rmRef{isReg: true, idx: regRaw}
	if d.rexR {
		regRef.idx += 8
	} else if w == 1 && !d.rex && regRaw >= 4 {
		regRef.idx = regRaw - 4
		regRef.hiByte = true
	}

	if mod == 3 {
		rm := rmRef{isReg: true, idx: rmRaw}
		if d.rexB {
			rm.idx += 8
		} else if w == 1 && !d.rex && rmRaw >= 4 {
			rm.idx = rmRaw - 4
			rm.hiByte = true
		}
		return modrm, regRaw, regRef, rm, nil
	}

	var r rmRef
	var base uint64
	var hasBase bool
	var index uint64
	var disp int64

	if rmRaw == 4 { // SIB
		sib, err := d.fetch8()
		if err != nil {
			return 0, 0, rmRef{}, rmRef{}, err
		}
		scale := 1 << (sib >> 6)
		idxF := int((sib >> 3) & 7)
		baseF := int(sib & 7)

		if !(idxF == 4 && !d.rexX) {
			if d.rexX {
				idxF += 8
			}
			index = d.c.Regs[idxF] * uint64(scale)
		}
		if baseF == 5 && mod == 0 {
			v, err := d.fetchN(4)
			if err != nil {
				return 0, 0, rmRef{}, rmRef{}, err
			}
			disp = int64(int32(uint32(v)))
		} else {
			if d.rexB {
				baseF += 8
			}
			base = d.c.Regs[baseF]
			hasBase = true
		}
	} else if rmRaw == 5 && mod == 0 && !d.rexB {
		// RIP-relative
		v, err := d.fetchN(4)
		if err != nil {
			return 0, 0, rmRef{}, rmRef{}, err
		}
		r.ripRel = true
		r.disp = int64(int32(uint32(v)))
	} else {
		if d.rexB {
			rmRaw += 8
		}
		base = d.c.Regs[rmRaw]
		hasBase = true
	}

	if mod == 1 {
		v, err := d.fetchN(1)
		if err != nil {
			return 0, 0, rmRef{}, rmRef{}, err
		}
		disp = int64(int8(v))
	} else if mod == 2 {
		v, err := d.fetchN(4)
		if err != nil {
			return 0, 0, rmRef{}, rmRef{}, err
		}
		disp = int64(int32(uint32(v)))
	}
	_ = hasBase
	if !r.ripRel {
		r.addr = base + index + uint64(disp)
	}
	return modrm, regRaw, regRef, r, nil
}

// finish resolves RIP-relative addressing after the whole instruction is fetched.
func (d *decoder) finish(r *rmRef) {
	if r.ripRel {
		r.addr = d.rip + uint64(r.disp)
	}
}

func (d *decoder) readRM(r *rmRef, w int) (uint64, error) {
	if r.isReg {
		v := d.c.Regs[r.idx]
		if r.hiByte {
			return (v >> 8) & 0xFF, nil
		}
		return maskW(v, w), nil
	}
	switch w {
	case 1:
		v, err := d.mem.Read8(r.addr)
		return uint64(v), err
	case 2:
		v, err := d.mem.Read16(r.addr)
		return uint64(v), err
	case 4:
		v, err := d.mem.Read32(r.addr)
		return uint64(v), err
	default:
		return d.mem.Read64(r.addr)
	}
}

func (d *decoder) writeRM(r *rmRef, w int, val uint64) error {
	if r.isReg {
		if r.hiByte {
			d.c.Regs[r.idx] = (d.c.Regs[r.idx] &^ 0xFF00) | ((val & 0xFF) << 8)
			return nil
		}
		switch w {
		case 1:
			d.c.Regs[r.idx] = (d.c.Regs[r.idx] &^ 0xFF) | (val & 0xFF)
		case 2:
			d.c.Regs[r.idx] = (d.c.Regs[r.idx] &^ 0xFFFF) | (val & 0xFFFF)
		case 4:
			d.c.Regs[r.idx] = val & 0xFFFFFFFF
		default:
			d.c.Regs[r.idx] = val
		}
		return nil
	}
	switch w {
	case 1:
		return d.mem.Write8(r.addr, uint8(val))
	case 2:
		return d.mem.Write16(r.addr, uint16(val))
	case 4:
		return d.mem.Write32(r.addr, uint32(val))
	default:
		return d.mem.Write64(r.addr, val)
	}
}

func (d *decoder) readBytes(addr uint64, n int) ([]byte, error) {
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		v, err := d.mem.Read8(addr + uint64(i))
		if err != nil {
			return nil, err
		}
		buf[i] = v
	}
	return buf, nil
}

func (d *decoder) writeBytes(addr uint64, b []byte) error {
	for i, v := range b {
		if err := d.mem.Write8(addr+uint64(i), v); err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) push(v uint64, w int) error {
	sp := d.c.Regs[RSP] - uint64(w)
	tmp := rmRef{addr: sp}
	if err := d.writeRM(&tmp, w, maskW(v, w)); err != nil {
		return err
	}
	d.c.Regs[RSP] = sp
	return nil
}

func (d *decoder) pop(w int) (uint64, error) {
	tmp := rmRef{addr: d.c.Regs[RSP]}
	v, err := d.readRM(&tmp, w)
	if err != nil {
		return 0, err
	}
	d.c.Regs[RSP] += uint64(w)
	return v, nil
}

// ---------- string operations ----------

func (d *decoder) countRCX() uint64 {
	if d.op66 {
		return d.c.Regs[RCX] & 0xFFFF
	}
	return d.c.Regs[RCX]
}

func (d *decoder) decRCX() {
	if d.op66 {
		cx := uint16(d.c.Regs[RCX]) - 1
		d.c.Regs[RCX] = (d.c.Regs[RCX] &^ 0xFFFF) | uint64(cx)
	} else {
		d.c.Regs[RCX]--
	}
}

func (d *decoder) execString(op byte) error {
	w := d.width()
	if op == 0x6C || op == 0x6E { // INS/OUTS byte forms
		w = 1
	}
	step := uint64(w)
	if d.c.RFLAGS&FLAG_DF != 0 {
		step = -step
	}
	repeat := d.opF2 || d.opF3
	cond := op == 0xA6 || op == 0xA7 || op == 0xAE || op == 0xAF

	for {
		if repeat && d.countRCX() == 0 {
			return nil
		}
		if err := d.stringIter(op, w, step); err != nil {
			return err
		}
		if !repeat {
			return nil
		}
		d.decRCX()
		if d.countRCX() == 0 {
			return nil
		}
		if cond {
			zf := d.c.GetZF()
			if d.opF3 && !zf {
				return nil
			}
			if d.opF2 && zf {
				return nil
			}
		}
	}
}

func (d *decoder) stringIter(op byte, w int, step uint64) error {
	c := d.c

	switch op {
	case 0xA4, 0xA5: // MOVS
		src := rmRef{addr: c.Regs[RSI]}
		v, err := d.readRM(&src, w)
		if err != nil {
			return err
		}
		dst := rmRef{addr: c.Regs[RDI]}
		if err := d.writeRM(&dst, w, v); err != nil {
			return err
		}
		c.Regs[RSI] += step
		c.Regs[RDI] += step
	case 0xA6, 0xA7: // CMPS
		s := rmRef{addr: c.Regs[RSI]}
		a, err := d.readRM(&s, w)
		if err != nil {
			return err
		}
		dst := rmRef{addr: c.Regs[RDI]}
		b, err := d.readRM(&dst, w)
		if err != nil {
			return err
		}
		ALU(c, ALUCmp, a, b, w)
		c.Regs[RSI] += step
		c.Regs[RDI] += step
	case 0xAA, 0xAB: // STOS
		dst := rmRef{addr: c.Regs[RDI]}
		if err := d.writeRM(&dst, w, maskW(c.Regs[RAX], w)); err != nil {
			return err
		}
		c.Regs[RDI] += step
	case 0xAC, 0xAD: // LODS
		src := rmRef{addr: c.Regs[RSI]}
		v, err := d.readRM(&src, w)
		if err != nil {
			return err
		}
		raxRef := rmRef{isReg: true, idx: RAX}
		if err := d.writeRM(&raxRef, w, v); err != nil {
			return err
		}
		c.Regs[RSI] += step
	case 0x6C, 0x6D: // INS
		port := uint16(c.Regs[RDX] & 0xFFFF)
		v, err := d.io.In(port, w)
		if err != nil {
			return err
		}
		dst := rmRef{addr: c.Regs[RDI]}
		if err := d.writeRM(&dst, w, uint64(v)); err != nil {
			return err
		}
		c.Regs[RDI] += step
	case 0x6E, 0x6F: // OUTS
		src := rmRef{addr: c.Regs[RSI]}
		v, err := d.readRM(&src, w)
		if err != nil {
			return err
		}
		if err := d.io.Out(uint16(c.Regs[RDX]&0xFFFF), uint32(v), w); err != nil {
			return err
		}
		c.Regs[RSI] += step
	case 0xAE, 0xAF: // SCAS
		a := maskW(c.Regs[RAX], w)
		dst := rmRef{addr: c.Regs[RDI]}
		b, err := d.readRM(&dst, w)
		if err != nil {
			return err
		}
		ALU(c, ALUCmp, a, b, w)
		c.Regs[RDI] += step
	}
	return nil
}

// ---------- CPUID / timers / MSRs ----------

func execCPUID(c *CPUState) {
	leaf := uint32(c.Regs[RAX])
	var eax, ebx, ecx, edx uint32
	switch leaf {
	case 0:
		eax = 1
		ebx = 0x756E6547 // "Genu"
		edx = 0x49656E69 // "ineI"
		ecx = 0x6C65746E // "ntel"
	case 1:
		eax = 0x000306C3
		ecx = 1<<0 | 1<<9 | 1<<13 | 1<<23 // SSE3, SSSE3, CX16, POPCNT
		edx = 0x078BABFF                  // FPU TSC MSR PAE MCE APIC CX8 SEP PGE CMOV PAT PSE36 CLFSH MMX FXSR SSE SSE2
	case 7:
		eax, ebx, ecx, edx = 0, 0, 0, 0
	case 0x80000000:
		eax = 0x80000001
	case 0x80000001:
		ecx = 1 // LAHF/SAHF
		edx = 1<<20 | 1<<27 | 1<<29 // NX, RDTSCP, LM
	case 0x80000008:
		ecx = 48<<8 | 36
	default:
		eax, ebx, ecx, edx = 0, 0, 0, 0
	}
	c.Regs[RAX] = uint64(eax)
	c.Regs[RBX] = uint64(ebx)
	c.Regs[RCX] = uint64(ecx)
	c.Regs[RDX] = uint64(edx)
}

func execRDTSC(c *CPUState) {
	ns := uint64(time.Now().UnixNano())
	c.Regs[RAX] = ns
	c.Regs[RDX] = ns >> 32
}

func execRDMSR(c *CPUState) {
	var eax, edx uint32
	switch uint32(c.Regs[RCX]) {
	case 0x1B: // IA32_APIC_BASE: 0xFEE00000 | BSP | enabled
		eax = 0xFEE00900
	}
	c.Regs[RAX] = uint64(eax)
	c.Regs[RDX] = uint64(edx)
}

// ---------- main entry ----------

// Step decodes and executes one guest instruction.
func (c *CPUState) Step(mem Memory, io IOBus) error {
	if c.Halted {
		return nil
	}
	d := &decoder{c: c, mem: mem, io: io, rip: c.RIP, start: c.RIP}
	if err := d.decodeExec(); err != nil {
		return err
	}
	c.RIP = d.rip
	return nil
}

func (d *decoder) decodeExec() error {
	for {
		b, err := d.fetch8()
		if err != nil {
			return err
		}
		switch {
		case b == 0x66:
			d.op66 = true
		case b == 0x67:
			// ponytail: address-size override ignored (64-bit assumed); upgrade: 32-bit addressing mode
		case b == 0xF0: // LOCK
		case b == 0xF2:
			d.opF2 = true
		case b == 0xF3:
			d.opF3 = true
		case b == 0x26 || b == 0x2E || b == 0x36 || b == 0x3E || b == 0x64 || b == 0x65:
			// segment override prefixes consumed; FS/GS bases not applied (ceiling)
		case b >= 0x40 && b <= 0x4F:
			d.rex = true
			d.rexW = b&0x08 != 0
			d.rexR = b&0x04 != 0
			d.rexX = b&0x02 != 0
			d.rexB = b&0x01 != 0
		default:
			return d.execOp(b)
		}
	}
}

func (d *decoder) execOp(op byte) error {
	// ALU encodings 0x00-0x3D: op&7 <= 5 (n selects operand form, op>>3 selects kind)
	if op < 0x40 && (op&7) <= 5 {
		return d.execALUPlain(op)
	}

	switch {
	case op == 0x0F:
		return d.execTwoByte()

	// PUSH/POP r64
	case op >= 0x50 && op <= 0x57:
		idx := int(op&7) + boolI(d.rexB)*8
		return d.push(d.c.Regs[idx], d.pushW())
	case op >= 0x58 && op <= 0x5F:
		idx := int(op&7) + boolI(d.rexB)*8
		v, err := d.pop(d.pushW())
		if err != nil {
			return err
		}
		ref := rmRef{isReg: true, idx: idx}
		return d.writeRM(&ref, d.pushW(), v)

	// MOVSXD r64, r/m32
	case op == 0x63:
		w := d.width()
		_, _, regRef, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		v, err := d.readRM(&r, 4)
		if err != nil {
			return err
		}
		return d.writeRM(&regRef, w, sext(v, 4))

	// PUSH/IMUL immediates
	case op == 0x68:
		w := d.pushW()
		ib := 4
		if w == 2 {
			ib = 2
		}
		imm, err := d.fetchImm(ib, w == 8)
		if err != nil {
			return err
		}
		return d.push(imm, w)
	case op == 0x6A:
		imm, err := d.fetchImm(1, true)
		if err != nil {
			return err
		}
		return d.push(imm, d.pushW())
	case op == 0x69 || op == 0x6B:
		w := d.width()
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		ib := 4
		if w == 2 {
			ib = 2
		}
		if op == 0x6B {
			ib = 1
		}
		imm, err := d.fetchImm(ib, ib == 1 || w == 8)
		if err != nil {
			return err
		}
		d.finish(&r)
		b, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.writeRM(&regRef, w, d.execIMulOp(b, imm, w))

	// Jcc rel8
	case op >= 0x70 && op <= 0x7F:
		rel, err := d.fetch8()
		if err != nil {
			return err
		}
		if evalCC(d.c, op&0xF) {
			d.rip = uint64(int64(d.rip) + int64(int8(rel)))
		}
		return nil

	// Group1 r/m, imm
	case op == 0x80 || op == 0x81 || op == 0x83:
		return d.execGroup1(op)

	case op == 0x84 || op == 0x85: // TEST r/m, r
		w := 1
		if op == 0x85 {
			w = d.width()
		}
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		b, err := d.readRM(&regRef, w)
		if err != nil {
			return err
		}
		ALU(d.c, ALUAnd, a, b, w)
		return nil

	case op == 0x86 || op == 0x87: // XCHG r/m, r
		w := 1
		if op == 0x87 {
			w = d.width()
		}
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		b, err := d.readRM(&regRef, w)
		if err != nil {
			return err
		}
		if err := d.writeRM(&r, w, b); err != nil {
			return err
		}
		return d.writeRM(&regRef, w, a)

	// MOV r/m <-> r
	case op == 0x88 || op == 0x89 || op == 0x8A || op == 0x8B:
		w := 1
		if op == 0x89 || op == 0x8B {
			w = d.width()
		}
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		if op == 0x88 || op == 0x89 { // store reg -> rm
			v, err := d.readRM(&regRef, w)
			if err != nil {
				return err
			}
			return d.writeRM(&r, w, v)
		}
		v, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.writeRM(&regRef, w, v)

	case op == 0x8D: // LEA
		w := d.width()
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		if r.isReg {
			return fmt.Errorf("invalid LEA operand at rip=0x%x", d.start)
		}
		d.finish(&r)
		return d.writeRM(&regRef, w, r.addr)

	case op == 0x8F: // POP r/m
		w := d.pushW()
		_, _, _, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		v, err := d.pop(w)
		if err != nil {
			return err
		}
		return d.writeRM(&r, w, v)

	case op == 0x90: // NOP
		return nil
	case op >= 0x91 && op <= 0x97: // XCHG rAX, r
		w := d.width()
		idx := int(op&7) + boolI(d.rexB)*8
		aRef := rmRef{isReg: true, idx: RAX}
		bRef := rmRef{isReg: true, idx: idx}
		a, err := d.readRM(&aRef, w)
		if err != nil {
			return err
		}
		b, err := d.readRM(&bRef, w)
		if err != nil {
			return err
		}
		if err := d.writeRM(&aRef, w, b); err != nil {
			return err
		}
		return d.writeRM(&bRef, w, a)

	case op == 0x98: // CBW/CWDE/CDQE
		w := d.width()
		raxRef := rmRef{isReg: true, idx: RAX}
		switch w {
		case 2:
			return d.writeRM(&raxRef, 2, sext(d.c.Regs[RAX]&0xFF, 1))
		case 4:
			return d.writeRM(&raxRef, 4, sext(d.c.Regs[RAX]&0xFFFF, 2))
		default:
			d.c.Regs[RAX] = sext(d.c.Regs[RAX], 4)
			return nil
		}

	case op == 0x99: // CDQ/CQO sign-extend EAX/RAX into EDX/RDX
		w := d.width()
		rdxRef := rmRef{isReg: true, idx: RDX}
		switch w {
		case 2:
			v := uint64(0)
			if d.c.Regs[RAX]&0x8000 != 0 {
				v = 0xFFFF
			}
			return d.writeRM(&rdxRef, 2, v)
		case 4:
			v := uint64(0)
			if d.c.Regs[RAX]&0x80000000 != 0 {
				v = 0xFFFFFFFF
			}
			d.c.Regs[RDX] = v
			return nil
		default:
			if d.c.Regs[RAX]&(1<<63) != 0 {
				d.c.Regs[RDX] = ^uint64(0)
			} else {
				d.c.Regs[RDX] = 0
			}
			return nil
		}

	case op == 0x9C: // PUSHF
		return d.push(d.c.RFLAGS, d.pushW())
	case op == 0x9D: // POPF
		v, err := d.pop(d.pushW())
		if err != nil {
			return err
		}
		d.c.RFLAGS = v | 0x2
		return nil

	case op == 0x9B: // FWAIT
		return nil
	case op == 0x9E: // SAHF
		d.c.RFLAGS = d.c.RFLAGS&^uint64(0xD5) | ((d.c.Regs[RAX] >> 8) & 0xD5)
		return nil
	case op == 0x9F: // LAHF
		ah := byte(d.c.RFLAGS&0xD5) | 0x02
		d.c.Regs[RAX] = d.c.Regs[RAX]&^0xFF00 | uint64(ah)<<8
		return nil

	case op == 0xA8: // TEST AL, imm8
		imm, err := d.fetchImm(1, true)
		if err != nil {
			return err
		}
		ALU(d.c, ALUAnd, d.c.Regs[RAX]&0xFF, imm, 1)
		return nil
	case op == 0xA9: // TEST eAX, imm
		w := d.width()
		ib := 4
		if w == 2 {
			ib = 2
		}
		imm, err := d.fetchImm(ib, w == 8)
		if err != nil {
			return err
		}
		ALU(d.c, ALUAnd, maskW(d.c.Regs[RAX], w), imm, w)
		return nil

	// MOVS moffs (absolute address; segment/addr-size overrides ignored)
	case op == 0xA0 || op == 0xA1 || op == 0xA2 || op == 0xA3:
		return d.execMoffs(op)

	// String I/O: INS/OUTS
	case op == 0x6C || op == 0x6D || op == 0x6E || op == 0x6F:
		return d.execString(op)

	// String operations
	case op == 0xA4 || op == 0xA5 || op == 0xA6 || op == 0xA7 ||
		op == 0xAA || op == 0xAB || op == 0xAC || op == 0xAD ||
		op == 0xAE || op == 0xAF:
		return d.execString(op)

	// MOV r8, imm8
	case op >= 0xB0 && op <= 0xB7:
		idx := int(op & 7)
		hi := false
		if d.rexB {
			idx += 8
		} else if !d.rex && idx >= 4 {
			idx -= 4
			hi = true
		}
		imm, err := d.fetchImm(1, false)
		if err != nil {
			return err
		}
		ref := rmRef{isReg: true, idx: idx, hiByte: hi}
		return d.writeRM(&ref, 1, imm)

	// MOV r, imm
	case op >= 0xB8 && op <= 0xBF:
		idx := int(op&7) + boolI(d.rexB)*8
		w, ib := 4, 4
		if d.rexW {
			w, ib = 8, 8
		} else if d.op66 {
			w, ib = 2, 2
		}
		imm, err := d.fetchImm(ib, false)
		if err != nil {
			return err
		}
		ref := rmRef{isReg: true, idx: idx}
		return d.writeRM(&ref, w, imm)

	// Group2 shifts by imm / 1 / CL
	case op == 0xC0 || op == 0xC1 || op == 0xD0 || op == 0xD1 || op == 0xD2 || op == 0xD3:
		return d.execGroup2(op)

	case op == 0xC2: // RET imm16
		imm, err := d.fetchN(2)
		if err != nil {
			return err
		}
		v, err := d.pop(8)
		if err != nil {
			return err
		}
		d.c.Regs[RSP] += imm
		d.rip = v
		return nil
	case op == 0xC3: // RET
		v, err := d.pop(8)
		if err != nil {
			return err
		}
		d.rip = v
		return nil

	// MOV r/m, imm
	case op == 0xC6 || op == 0xC7:
		w := 1
		if op == 0xC7 {
			w = d.width()
		}
		_, _, _, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		ib := 4
		if w == 2 {
			ib = 2
		}
		imm, err := d.fetchImm(ib, w == 8)
		if err != nil {
			return err
		}
		d.finish(&r)
		return d.writeRM(&r, w, imm)

	case op == 0xC9: // LEAVE
		d.c.Regs[RSP] = d.c.Regs[RBP]
		v, err := d.pop(8)
		if err != nil {
			return err
		}
		d.c.Regs[RBP] = v
		return nil

	// LOOPNE/LOOPE/LOOP/JRCXZ
	case op == 0xE0 || op == 0xE1 || op == 0xE2:
		rel, err := d.fetch8()
		if err != nil {
			return err
		}
		d.decRCX()
		zf := d.c.GetZF()
		taken := d.countRCX() != 0
		if op == 0xE0 && zf {
			taken = false
		}
		if op == 0xE1 && !zf {
			taken = false
		}
		if taken {
			d.rip = uint64(int64(d.rip) + int64(int8(rel)))
		}
		return nil
	case op == 0xE3: // JRCXZ
		rel, err := d.fetch8()
		if err != nil {
			return err
		}
		if d.countRCX() == 0 {
			d.rip = uint64(int64(d.rip) + int64(int8(rel)))
		}
		return nil

	// CALL/JMP rel
	case op == 0xE8:
		rel, err := d.fetchImm(4, true)
		if err != nil {
			return err
		}
		if err := d.push(d.rip, 8); err != nil {
			return err
		}
		d.rip = uint64(int64(d.rip) + int64(rel))
		return nil
	case op == 0xE9:
		rel, err := d.fetchImm(4, true)
		if err != nil {
			return err
		}
		d.rip = uint64(int64(d.rip) + int64(rel))
		return nil
	case op == 0xEB:
		rel, err := d.fetch8()
		if err != nil {
			return err
		}
		d.rip = uint64(int64(d.rip) + int64(int8(rel)))
		return nil

	// IN/OUT imm and DX forms
	case op == 0xE4 || op == 0xE5:
		port, err := d.fetch8()
		if err != nil {
			return err
		}
		sz := 1
		if op == 0xE5 {
			sz = 4
		}
		v, err := d.io.In(uint16(port), sz)
		if err != nil {
			return err
		}
		if sz == 1 {
			d.c.Regs[RAX] = (d.c.Regs[RAX] &^ 0xFF) | uint64(v&0xFF)
		} else {
			d.c.Regs[RAX] = uint64(v)
		}
		return nil
	case op == 0xE6 || op == 0xE7:
		port, err := d.fetch8()
		if err != nil {
			return err
		}
		sz := 1
		if op == 0xE7 {
			sz = 4
		}
		return d.io.Out(uint16(port), uint32(d.c.Regs[RAX]), sz)
	case op == 0xEC:
		v, err := d.io.In(uint16(d.c.Regs[RDX]&0xFFFF), 1)
		if err != nil {
			return err
		}
		d.c.Regs[RAX] = (d.c.Regs[RAX] &^ 0xFF) | uint64(v&0xFF)
		return nil
	case op == 0xED:
		v, err := d.io.In(uint16(d.c.Regs[RDX]&0xFFFF), 4)
		if err != nil {
			return err
		}
		d.c.Regs[RAX] = uint64(v)
		return nil
	case op == 0xEE:
		return d.io.Out(uint16(d.c.Regs[RDX]&0xFFFF), uint32(d.c.Regs[RAX]&0xFF), 1)
	case op == 0xEF:
		return d.io.Out(uint16(d.c.Regs[RDX]&0xFFFF), uint32(d.c.Regs[RAX]), 4)

	// Flags / halt
	case op == 0xF4: // HLT
		d.c.Halted = true
		return nil
	case op == 0xF5: // CMC
		d.c.RFLAGS ^= FLAG_CF
		return nil
	case op == 0xF8: // CLC
		d.c.RFLAGS &^= FLAG_CF
		return nil
	case op == 0xF9: // STC
		d.c.RFLAGS |= FLAG_CF
		return nil
	case op == 0xFA: // CLI
		d.c.RFLAGS &^= FLAG_IF
		return nil
	case op == 0xFB: // STI
		d.c.RFLAGS |= FLAG_IF
		return nil
	case op == 0xFC: // CLD
		d.c.RFLAGS &^= FLAG_DF
		return nil
	case op == 0xFD: // STD
		d.c.RFLAGS |= FLAG_DF
		return nil

	// Group3: TEST/NOT/NEG/MUL/IMUL/DIV/IDIV
	case op == 0xF6 || op == 0xF7:
		return d.execGroup3(op)

	// Group4/5: INC/DEC/CALL/JMP/PUSH
	case op == 0xFE || op == 0xFF:
		return d.execGroup5(op)

	case op == 0x8C: // MOV r/m16, Sreg — segments not modeled, writes 0
		_, _, _, r, err := d.parseRM(2)
		if err != nil {
			return err
		}
		d.finish(&r)
		return d.writeRM(&r, 2, 0)
	case op == 0x8E: // MOV Sreg, r/m16 — selector ignored
		_, _, _, r, err := d.parseRM(2)
		if err != nil {
			return err
		}
		d.finish(&r)
		return nil

	case op == 0x62: // EVEX prefix (AVX-512)
		return fmt.Errorf("EVEX prefix (AVX-512) not implemented at rip=0x%x", d.start)
	case op == 0xC4 || op == 0xC5: // VEX prefix (AVX)
		return fmt.Errorf("VEX prefix (AVX) not implemented at rip=0x%x", d.start)

	case op == 0xC8: // ENTER imm16, imm8
		sz, err := d.fetchN(2)
		if err != nil {
			return err
		}
		if _, err := d.fetch8(); err != nil {
			return err
		}
		if err := d.push(d.c.Regs[RBP], 8); err != nil {
			return err
		}
		d.c.Regs[RBP] = d.c.Regs[RSP]
		d.c.Regs[RSP] -= sz
		// ponytail: nesting level >0 (frame chaining) not implemented; add when guest frames require it
		return nil
	case op == 0xD6: // SALC (undocumented)
		if d.c.RFLAGS&FLAG_CF != 0 {
			d.c.Regs[RAX] |= 0xFF
		} else {
			d.c.Regs[RAX] &^= 0xFF
		}
		return nil
	case op == 0xD7: // XLATB: AL = [RBX+AL]
		b, err := d.readBytes(d.c.Regs[RBX]+(d.c.Regs[RAX]&0xFF), 1)
		if err != nil {
			return err
		}
		d.c.Regs[RAX] = d.c.Regs[RAX]&^0xFF | uint64(b[0])
		return nil
	case op >= 0xD8 && op <= 0xDF: // x87 FPU
		return d.execX87(op)

	case op == 0xCD: // INT imm8 — no BIOS/IDT services implemented
		imm, err := d.fetch8()
		if err != nil {
			return err
		}
		return fmt.Errorf("software interrupt INT 0x%02X not implemented at rip=0x%x", imm, d.start)
	case op == 0xCC:
		return fmt.Errorf("INT3 breakpoint at rip=0x%x", d.start)
	}

	return fmt.Errorf("unsupported opcode: 0x%02X at rip=0x%x", op, d.start)
}

func boolI(b bool) int {
	if b {
		return 1
	}
	return 0
}

// execALUPlain handles 0x00-0x3D ALU forms (n = op&7 selects form).
func (d *decoder) execALUPlain(op byte) error {
	kind := int(op >> 3)
	w := 1
	switch op & 7 {
	case 1, 3, 5:
		w = d.width()
	}

	switch n := op & 7; n {
	case 0, 1: // r/m <- r/m OP r
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		b, err := d.readRM(&regRef, w)
		if err != nil {
			return err
		}
		res := ALU(d.c, kind, a, b, w)
		if kind != ALUCmp {
			return d.writeRM(&r, w, res)
		}
		return nil
	case 2, 3: // r <- r OP r/m
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		a, err := d.readRM(&regRef, w)
		if err != nil {
			return err
		}
		b, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		res := ALU(d.c, kind, a, b, w)
		if kind != ALUCmp {
			return d.writeRM(&regRef, w, res)
		}
		return nil
	case 4: // AL, imm8
		imm, err := d.fetchImm(1, true)
		if err != nil {
			return err
		}
		res := ALU(d.c, kind, d.c.Regs[RAX]&0xFF, imm, 1)
		if kind != ALUCmp {
			ref := rmRef{isReg: true, idx: RAX}
			return d.writeRM(&ref, 1, res)
		}
		return nil
	default: // 5: eAX, imm
		ib := 4
		if w == 2 {
			ib = 2
		}
		imm, err := d.fetchImm(ib, w == 8)
		if err != nil {
			return err
		}
		res := ALU(d.c, kind, maskW(d.c.Regs[RAX], w), imm, w)
		if kind != ALUCmp {
			ref := rmRef{isReg: true, idx: RAX}
			return d.writeRM(&ref, w, res)
		}
		return nil
	}
}

// execGroup1 handles 0x80/0x81/0x83 r/m, imm with kind in ModRM.reg.
func (d *decoder) execGroup1(op byte) error {
	w := 1
	if op != 0x80 {
		w = d.width()
	}
	modrm, kind, _, r, err := d.parseRM(w)
	_ = modrm
	if err != nil {
		return err
	}
	ib := 4
	if w == 2 {
		ib = 2
	}
	if op == 0x80 || op == 0x83 {
		ib = 1
	}
	imm, err := d.fetchImm(ib, op == 0x83 || w == 8 || op == 0x80)
	if err != nil {
		return err
	}
	d.finish(&r)
	a, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	res := ALU(d.c, kind, a, imm, w)
	if kind != ALUCmp {
		return d.writeRM(&r, w, res)
	}
	return nil
}

// execGroup2 handles C0/C1/D0/D1/D2/D3 shifts.
func (d *decoder) execGroup2(op byte) error {
	w := 1
	switch op {
	case 0xC1, 0xD1, 0xD3:
		w = d.width()
	}
	_, kind, _, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	var count uint64
	switch op {
	case 0xC0, 0xC1:
		count, err = d.fetchImm(1, false)
		if err != nil {
			return err
		}
	case 0xD0, 0xD1:
		count = 1
	default: // D2/D3: CL
		count = d.c.Regs[RCX] & 0xFF
	}
	if err != nil {
		return err
	}
	d.finish(&r)
	a, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	res := shiftAlu(d.c, kind, a, count, w)
	return d.writeRM(&r, w, res)
}

// execIMulOp computes two-operand IMUL (reg = a * imm) with flags, returns result.
func (d *decoder) execIMulOp(a, b uint64, w int) uint64 {
	lo, hi := mulPair(a, b, w, true)
	expected := uint64(0)
	if lo&(1<<(uint(w*8)-1)) != 0 {
		if w >= 8 {
			expected = ^uint64(0)
		} else {
			expected = (1 << (uint(w * 8))) - 1
		}
	}
	ovf := hi != expected
	if ovf {
		d.c.RFLAGS |= FLAG_CF | FLAG_OF
	} else {
		d.c.RFLAGS &^= FLAG_CF
		d.c.RFLAGS &^= FLAG_OF
	}
	return lo
}

// execGroup3 handles F6/F7.
func (d *decoder) execGroup3(op byte) error {
	w := 1
	if op == 0xF7 {
		w = d.width()
	}
	modrm, sub, _, r, err := d.parseRM(w)
	_ = modrm
	if err != nil {
		return err
	}

	switch sub {
	case 0, 1: // TEST r/m, imm
		ib := 4
		if w == 2 {
			ib = 2
		}
		if op == 0xF6 {
			ib = 1
		}
		imm, err := d.fetchImm(ib, w == 8 || op == 0xF6)
		if err != nil {
			return err
		}
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		ALU(d.c, ALUAnd, a, imm, w)
		return nil
	case 2: // NOT
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.writeRM(&r, w, ^a)
	case 3: // NEG
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		res := ALU(d.c, ALUSub, 0, a, w)
		return d.writeRM(&r, w, res)
	case 4, 5: // MUL / IMUL
		d.finish(&r)
		src, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		signed := sub == 5
		lo, hi := mulPair(d.c.Regs[RAX], src, w, signed)
		var ovf bool
		if signed {
			expected := uint64(0)
			if lo&(1<<(uint(w*8)-1)) != 0 {
				if w >= 8 {
					expected = ^uint64(0)
				} else {
					expected = (1 << (uint(w * 8))) - 1
				}
			}
			ovf = hi != expected
		} else {
			ovf = hi != 0
		}
		if ovf {
			d.c.RFLAGS |= FLAG_CF | FLAG_OF
		} else {
			d.c.RFLAGS &^= FLAG_CF
			d.c.RFLAGS &^= FLAG_OF
		}
		raxRef := rmRef{isReg: true, idx: RAX}
		rdxRef := rmRef{isReg: true, idx: RDX}
		if w == 1 {
			// AX = 16-bit product
			return d.writeRM(&raxRef, 2, lo|(hi<<8))
		}
		if err := d.writeRM(&raxRef, w, lo); err != nil {
			return err
		}
		return d.writeRM(&rdxRef, w, hi)
	case 6: // DIV
		d.finish(&r)
		src, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.execDiv(src, w, false)
	case 7: // IDIV
		d.finish(&r)
		src, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.execDiv(src, w, true)
	}
	return fmt.Errorf("invalid F6/F7 group at rip=0x%x", d.start)
}

func (d *decoder) execDiv(src uint64, w int, signed bool) error {
	if maskW(src, w) == 0 {
		return fmt.Errorf("divide by zero at rip=0x%x", d.start)
	}
	c := d.c
	raxRef := rmRef{isReg: true, idx: RAX}
	rdxRef := rmRef{isReg: true, idx: RDX}

	if !signed {
		if w == 1 {
			num := c.Regs[RAX] & 0xFFFF
			q := num / (src & 0xFF)
			r := num % (src & 0xFF)
			if q > 0xFF {
				return fmt.Errorf("divide overflow at rip=0x%x", d.start)
			}
			return d.writeRM(&raxRef, 2, (r<<8)|q)
		}
		var num, den uint64
		if w == 2 {
			num = (c.Regs[RDX] << 16) | (c.Regs[RAX] & 0xFFFF)
			den = src & 0xFFFF
		} else if w == 4 {
			num = (c.Regs[RDX] << 32) | (c.Regs[RAX] & 0xFFFFFFFF)
			den = src & 0xFFFFFFFF
		} else {
			if c.Regs[RDX] >= src {
				return fmt.Errorf("divide overflow at rip=0x%x", d.start)
			}
			quo, rem := bits.Div64(c.Regs[RDX], c.Regs[RAX], src)
			c.Regs[RAX] = quo
			c.Regs[RDX] = rem
			return nil
		}
		q := num / den
		r := num % den
		if w == 4 && q > 0xFFFFFFFF {
			return fmt.Errorf("divide overflow at rip=0x%x", d.start)
		}
		if w == 2 && q > 0xFFFF {
			return fmt.Errorf("divide overflow at rip=0x%x", d.start)
		}
		if err := d.writeRM(&raxRef, w, q); err != nil {
			return err
		}
		return d.writeRM(&rdxRef, w, r)
	}

	// signed division via math/big (truncated like x86)
	bitsN := uint(w * 8)
	var num big.Int
	if w >= 8 {
		num.SetUint64(c.Regs[RDX])
		num.Lsh(&num, 64)
		num.Or(&num, new(big.Int).SetUint64(c.Regs[RAX]))
		if c.Regs[RDX]&(1<<63) != 0 {
			two := new(big.Int).Lsh(big.NewInt(1), 128)
			num.Sub(&num, two)
		}
	} else {
		raw := (c.Regs[RDX] << bitsN) | maskW(c.Regs[RAX], w)
		raw &= (1 << (2 * bitsN)) - 1
		num.SetUint64(raw)
		if raw&(1<<(2*bitsN-1)) != 0 {
			two := new(big.Int).Lsh(big.NewInt(1), 2*bitsN)
			num.Sub(&num, two)
		}
	}
	var den big.Int
	s := sext(src, w)
	den.SetUint64(s)
	if int64(s) < 0 {
		two := new(big.Int).Lsh(big.NewInt(1), 64)
		den.Sub(&den, two)
	}
	q := new(big.Int).Quo(&num, &den)
	r := new(big.Int).Rem(&num, &den)

	min := new(big.Int).Lsh(big.NewInt(1), bitsN-1)
	max := new(big.Int).Sub(new(big.Int).Set(min), big.NewInt(1))
	min.Neg(min)
	if q.Cmp(min) < 0 || q.Cmp(max) > 0 {
		return fmt.Errorf("divide overflow at rip=0x%x", d.start)
	}
	qv := q.Uint64()
	rv := r.Uint64()
	if w == 1 {
		return d.writeRM(&raxRef, 2, ((rv & 0xFF) << 8) | (qv & 0xFF))
	}
	if err := d.writeRM(&raxRef, w, qv); err != nil {
		return err
	}
	return d.writeRM(&rdxRef, w, rv)
}

// execGroup5 handles FE/FF: INC/DEC/CALL/JMP/PUSH.
func (d *decoder) execGroup5(op byte) error {
	w := 1
	if op == 0xFF {
		w = d.width()
	}
	_, sub, _, r, err := d.parseRM(w)
	if err != nil {
		return err
	}

	switch sub {
	case 0, 1: // INC / DEC
		d.finish(&r)
		a, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		var m, msb uint64
		if w >= 8 {
			m = ^uint64(0)
			msb = 1 << 63
		} else {
			bitsN := uint(w * 8)
			m = 1<<bitsN - 1
			msb = 1 << (bitsN - 1)
		}
		a &= m
		var res uint64
		if sub == 0 {
			res = (a + 1) & m
			d.c.SetZF(res == 0)
			d.c.SetSF(res&msb != 0)
			d.c.setPF(res)
			if a == msb {
				d.c.RFLAGS |= FLAG_OF
			} else {
				d.c.RFLAGS &^= FLAG_OF
			}
		} else {
			res = (a - 1) & m
			d.c.SetZF(res == 0)
			d.c.SetSF(res&msb != 0)
			d.c.setPF(res)
			if a == msb { // DEC OF: underflow from min signed value (e.g. 0x80 -> 0x7F)
				d.c.RFLAGS |= FLAG_OF
			} else {
				d.c.RFLAGS &^= FLAG_OF
			}
		}
		return d.writeRM(&r, w, res)
	case 2: // CALL r/m
		d.finish(&r)
		tgt, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		if err := d.push(d.rip, 8); err != nil {
			return err
		}
		d.rip = tgt
		return nil
	case 4: // JMP r/m
		d.finish(&r)
		tgt, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		d.rip = tgt
		return nil
	case 6: // PUSH r/m
		d.finish(&r)
		v, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		return d.push(v, d.pushW())
	}
	return fmt.Errorf("invalid FE/FF group at rip=0x%x", d.start)
}

// execTwoByte handles 0x0F-prefixed two-byte opcodes.
func (d *decoder) execTwoByte() error {
	op, err := d.fetch8()
	if err != nil {
		return err
	}
	c := d.c

	switch {
	case op == 0x05: // SYSCALL
		c.RIP = d.rip
		return ErrSyscallTrapped
	case op == 0x0B: // UD2
		return fmt.Errorf("ud2 trap at rip=0x%x", d.start)
	case op == 0x06 || op == 0x09: // CLTS / WBINVD (privileged, ignored)
		return nil
	case op == 0x77: // EMMS
		return nil
	case op == 0x01: // SGDT/SIDT/LGDT/LIDT/SMSW/RDTSCP/swapgs...
		return d.exec0F01()
	case op == 0x1E || op == 0x1F: // ENDBR64 / multi-byte NOP
		_, _, _, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		return nil
	case op == 0x20 || op == 0x21: // MOV r64, CRn/DRn
		_, regRaw, regRef, r, err := d.parseRM(8)
		if err != nil {
			return err
		}
		if !r.isReg {
			return fmt.Errorf("invalid CR/DR move at rip=0x%x", d.start)
		}
		var v uint64
		if op == 0x20 {
			switch regRaw {
			case 0:
				v = c.CR0
			case 2:
				v = c.CR2
			case 3:
				v = c.CR3
			case 4:
				v = c.CR4
			}
		}
		c.Regs[regRef.idx] = v
		return nil
	case op == 0x22 || op == 0x23: // MOV CRn/DRn, r64
		_, regRaw, regRef, r, err := d.parseRM(8)
		if err != nil {
			return err
		}
		if !r.isReg {
			return fmt.Errorf("invalid CR/DR move at rip=0x%x", d.start)
		}
		if op == 0x22 {
			v := c.Regs[regRef.idx]
			switch regRaw {
			case 0:
				c.CR0 = v
			case 2:
				c.CR2 = v
			case 3:
				c.CR3 = v
			case 4:
				c.CR4 = v
			}
		}
		return nil
	case op == 0x10, op == 0x11, op == 0x28, op == 0x29, op == 0x6F, op == 0x7F, op == 0x57:
		return d.execSSE(op)
	case op == 0x6E: // MOVD/MOVQ xmm/mm, r/m
		w := 4
		if d.rexW {
			w = 8
		}
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		v, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		if !d.op66 { // MMX dest: value zero-extends into the slot
			c.FPUStack[(c.FPUTop+regRef.idx)&7] = maskW(v, w)
			return nil
		}
		for i := 0; i < 16; i++ {
			c.XMM[regRef.idx][i] = 0
		}
		for i := 0; i < w; i++ {
			c.XMM[regRef.idx][i] = byte(v >> (8 * i))
		}
		return nil
	case op == 0x7E: // MOVD/MOVQ forms
		modrm, _, regRef, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		mod := modrm >> 6
		switch {
		case d.opF3: // MOVQ xmm, xmm/m64
			if r.isReg {
				for i := 0; i < 8; i++ {
					c.XMM[regRef.idx][i] = c.XMM[r.idx][i]
				}
				for i := 8; i < 16; i++ {
					c.XMM[regRef.idx][i] = 0
				}
				return nil
			}
			b, err := d.readBytes(r.addr, 8)
			if err != nil {
				return err
			}
			copy(c.XMM[regRef.idx][0:8], b)
			for i := 8; i < 16; i++ {
				c.XMM[regRef.idx][i] = 0
			}
			return nil
		case d.op66 && mod == 3: // MOVD/MOVQ r/m, xmm
			w := 4
			if d.rexW {
				w = 8
			}
			var v uint64
			for i := 0; i < w; i++ {
				v |= uint64(c.XMM[regRef.idx][i]) << (8 * i)
			}
			return d.writeRM(&r, w, v)
		case d.op66: // MOVQ xmm, m64
			b, err := d.readBytes(r.addr, 8)
			if err != nil {
				return err
			}
			copy(c.XMM[regRef.idx][0:8], b)
			for i := 8; i < 16; i++ {
				c.XMM[regRef.idx][i] = 0
			}
			return nil
		default: // MOVD/MOVQ r/m32/64, mm (none prefix)
			w := 4
			if d.rexW {
				w = 8
			}
			v := c.FPUStack[(c.FPUTop+regRef.idx)&7]
			return d.writeRM(&r, w, maskW(v, w))
		}
	case op == 0x30: // WRMSR (ignored)
		return nil
	case op == 0x31: // RDTSC
		execRDTSC(c)
		return nil
	case op == 0x32: // RDMSR
		execRDMSR(c)
		return nil
	case op == 0x33: // RDTSCP
		execRDTSC(c)
		c.Regs[RCX] = 0
		return nil
	// system/segment misc
	case op == 0x00: // SLDT/STR/LLDT/LTR/VERR/VERW
		return d.exec0F00()
	case op == 0x02 || op == 0x03: // LAR / LSL
		return d.execLARLSL(op)
	case op == 0x08: // INVD (privileged, ignored)
		return nil
	case op == 0x0D || op == 0x0E: // prefetch group / FEMMS
		_, _, _, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		return nil
	case op == 0x18, op == 0x19, op == 0x1A, op == 0x1B: // prefetch hints / NOP r/m
		_, _, _, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		return nil
	case op == 0x34 || op == 0x35: // SYSENTER / SYSEXIT
		return fmt.Errorf("sysenter not implemented at rip=0x%x", d.start)

	// FS/GS push/pop (segment bases not modeled)
	case op == 0xA0, op == 0xA8: // PUSH FS / PUSH GS
		return d.push(0, d.pushW())
	case op == 0xA1, op == 0xA9: // POP FS / POP GS (discard)
		_, err := d.pop(d.pushW())
		return err

	// bit operations
	case op == 0xA3, op == 0xAB, op == 0xB3, op == 0xBB, op == 0xBA: // BT/BTS/BTR/BTC
		return d.execBT(op)
	case op == 0xA4, op == 0xA5, op == 0xAC, op == 0xAD: // SHLD/SHRD
		return d.execSHLD(op)
	case op == 0xB0, op == 0xB1: // CMPXCHG
		return d.execCMPXCHG(op)
	case op == 0xB2, op == 0xB4, op == 0xB5: // LSS / LFS / LGS
		return d.execLSS(op)
	case op == 0xB8: // POPCNT (F3)
		return d.execPOPCNT()
	case op == 0xC0, op == 0xC1: // XADD
		return d.execXADD(op)
	case op == 0xC3: // MOVNTI
		return d.execMOVNTI()
	case op == 0xC7: // CMPXCHG8B/16B, RDRAND, RDSEED
		return d.exec0FC7()

	// SSE/SSE2/SSSE3
	case op == 0x38, op == 0x3A:
		return d.execSSE3(op)
	case isSSEOp(op):
		return d.execSSE2(op)

	// CMOVcc r, r/m
	case op >= 0x40 && op <= 0x4F:
		w := d.width()
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		v, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		if evalCC(c, op&0xF) {
			return d.writeRM(&regRef, w, v)
		}
		return nil

	// Jcc rel32
	case op >= 0x80 && op <= 0x8F:
		rel, err := d.fetchImm(4, true)
		if err != nil {
			return err
		}
		if evalCC(c, op&0xF) {
			d.rip = uint64(int64(d.rip) + int64(rel))
		}
		return nil

	// SETcc r/m8
	case op >= 0x90 && op <= 0x9F:
		_, _, _, r, err := d.parseRM(1)
		if err != nil {
			return err
		}
		d.finish(&r)
		var v uint64
		if evalCC(c, op&0xF) {
			v = 1
		}
		return d.writeRM(&r, 1, v)

	case op == 0xA2: // CPUID
		execCPUID(c)
		return nil

	case op == 0xAE: // LFENCE/MFENCE/SFENCE/FXSAVE (treated as nop)
		_, _, _, r, err := d.parseRM(4)
		if err != nil {
			return err
		}
		d.finish(&r)
		return nil

	case op == 0xAF: // IMUL r, r/m
		w := d.width()
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		b, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		a := maskW(c.Regs[regRef.idx], w)
		return d.writeRM(&regRef, w, d.execIMulOp(a, b, w))

	// MOVZX / MOVSX
	case op == 0xB6, op == 0xB7, op == 0xBE, op == 0xBF:
		srcW := 1
		if op == 0xB7 || op == 0xBF {
			srcW = 2
		}
		w := d.width()
		_, _, regRef, r, err := d.parseRM(srcW)
		if err != nil {
			return err
		}
		d.finish(&r)
		v, err := d.readRM(&r, srcW)
		if err != nil {
			return err
		}
		if op == 0xBE || op == 0xBF {
			v = sext(v, srcW)
		}
		return d.writeRM(&regRef, w, v)

	// BSF / BSR
	case op == 0xBC, op == 0xBD:
		w := d.width()
		_, _, regRef, r, err := d.parseRM(w)
		if err != nil {
			return err
		}
		d.finish(&r)
		src, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		if d.opF3 { // TZCNT (BC) / LZCNT (BD)
			m := maskW(src, w)
			var idx int
			cf := m == 0
			switch {
			case m == 0:
				idx = w * 8
			case op == 0xBC:
				idx = bits.TrailingZeros64(m)
			default:
				idx = w*8 - bits.Len64(m)
			}
			if cf {
				c.RFLAGS |= FLAG_CF
			} else {
				c.RFLAGS &^= FLAG_CF
			}
			c.SetZF(idx == 0)
			return d.writeRM(&regRef, w, uint64(idx))
		}
		if maskW(src, w) == 0 {
			c.RFLAGS |= FLAG_ZF
			return nil
		}
		c.RFLAGS &^= FLAG_ZF
		var idx int
		if op == 0xBC {
			idx = bits.TrailingZeros64(src)
		} else {
			idx = bits.Len64(maskW(src, w)) - 1
		}
		return d.writeRM(&regRef, w, uint64(idx))

	// BSWAP r
	case op >= 0xC8 && op <= 0xCF:
		w := d.width()
		idx := int(op&7) + boolI(d.rexB)*8
		v := maskW(c.Regs[idx], w)
		var out uint64
		for i := 0; i < w; i++ {
			out |= ((v >> (8 * i)) & 0xFF) << (8 * (w - 1 - i))
		}
		ref := rmRef{isReg: true, idx: idx}
		return d.writeRM(&ref, w, out)
	}

	return fmt.Errorf("unsupported 2-byte opcode: 0x0F 0x%02X at rip=0x%x", op, d.start)
}

// execSSE handles SSE load/store/xor moves (no FP arithmetic).
func (d *decoder) execSSE(op byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	dst := regRef.idx

	var n int
	switch op {
	case 0x10, 0x11:
		switch {
		case d.opF3: // movss
			n = 4
		case d.opF2: // movsd
			n = 8
		default: // movups/movupd
			n = 16
		}
	case 0x28, 0x29, 0x57, 0x6F, 0x7F:
		n = 16
	}
	if (op == 0x6F || op == 0x7F) && !d.op66 && !d.opF3 { // MOVQ mm, mm/m64 / MOVQ mm/m64, mm
		dst := rmRef{isReg: true, idx: regRef.idx}
		if op == 0x6F { // load
			v, err := d.vecSrc(&r, 8, true)
			if err != nil {
				return err
			}
			return d.vecStore(&dst, v, true)
		}
		v, err := d.vecSrc(&dst, 8, true)
		if err != nil {
			return err
		}
		return d.vecStore(&r, v, true)
	}

	if op == 0x57 { // XORPS/XORPD xmm, xmm/m128
		var src [16]byte
		if r.isReg {
			src = d.c.XMM[r.idx]
		} else {
			b, err := d.readBytes(r.addr, 16)
			if err != nil {
				return err
			}
			copy(src[:], b)
		}
		for i := 0; i < 16; i++ {
			d.c.XMM[dst][i] ^= src[i]
		}
		return nil
	}

	isStore := op == 0x11 || op == 0x29 || op == 0x7F
	if !isStore { // load into xmm[dst]
		if r.isReg {
			copy(d.c.XMM[dst][0:n], d.c.XMM[r.idx][0:n])
			return nil
		}
		b, err := d.readBytes(r.addr, n)
		if err != nil {
			return err
		}
		copy(d.c.XMM[dst][0:n], b)
		if n < 16 { // movss/movsd memory load zeroes upper bits
			for i := n; i < 16; i++ {
				d.c.XMM[dst][i] = 0
			}
		}
		return nil
	}
	if r.isReg {
		copy(d.c.XMM[r.idx][0:n], d.c.XMM[dst][0:n])
		return nil
	}
	return d.writeBytes(r.addr, d.c.XMM[dst][0:n])
}

// exec0F01 handles descriptor-table and system encodings of 0F 01.
func (d *decoder) exec0F01() error {
	modrm, _, _, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	mod := modrm >> 6
	sub := (modrm >> 3) & 7
	c := d.c

	if mod == 3 {
		if modrm == 0xF9 { // RDTSCP
			execRDTSC(c)
			c.Regs[RCX] = 0
		}
		return nil // swapgs and friends ignored (no privilege ring / gs base)
	}
	d.finish(&r)

	switch sub {
	case 0, 1: // SGDT / SIDT
		limit, base := c.GDTRLimit, c.GDTRBase
		if sub == 1 {
			limit, base = c.IDTRLimit, c.IDTRBase
		}
		buf := make([]byte, 10)
		binary.LittleEndian.PutUint16(buf[0:2], uint16(limit))
		binary.LittleEndian.PutUint64(buf[2:10], base)
		return d.writeBytes(r.addr, buf)
	case 2, 3: // LGDT / LIDT
		buf, err := d.readBytes(r.addr, 10)
		if err != nil {
			return err
		}
		lim := uint64(binary.LittleEndian.Uint16(buf[0:2]))
		bs := binary.LittleEndian.Uint64(buf[2:10])
		if sub == 2 {
			c.GDTRLimit, c.GDTRBase = lim, bs
		} else {
			c.IDTRLimit, c.IDTRBase = lim, bs
		}
		return nil
	case 4: // SMSW
		return d.writeRM(&r, 2, c.CR0&0xFFFF)
	case 6: // LMSW
		v, err := d.readRM(&r, 2)
		if err != nil {
			return err
		}
		c.CR0 = (c.CR0 &^ 0xFFFF) | (v & 0xFFFF)
		return nil
	default: // INVLPG etc
		return nil
	}
}

// execMoffs handles MOV AL/AX/EAX/RAX <-> [moffs64] (0F A0-A3 is a different opcode set).
func (d *decoder) execMoffs(op byte) error {
	addr, err := d.fetchN(8)
	if err != nil {
		return err
	}
	axRef := rmRef{isReg: true, idx: RAX}
	w := d.width()
	switch op {
	case 0xA0: // MOV AL, moffs8
		b, err := d.readBytes(addr, 1)
		if err != nil {
			return err
		}
		d.c.Regs[RAX] = d.c.Regs[RAX]&^0xFF | uint64(b[0])
		return nil
	case 0xA1: // MOV eAX, moffs
		src := rmRef{addr: addr}
		v, err := d.readRM(&src, w)
		if err != nil {
			return err
		}
		return d.writeRM(&axRef, w, v)
	case 0xA2: // MOV moffs8, AL
		return d.writeBytes(addr, []byte{byte(d.c.Regs[RAX])})
	default: // MOV moffs, eAX
		buf := make([]byte, w)
		v := maskW(d.c.Regs[RAX], w)
		for i := 0; i < w; i++ {
			buf[i] = byte(v >> (8 * i))
		}
		return d.writeBytes(addr, buf)
	}
}

// exec0F00 handles SLDT/STR/LLDT/LTR/VERR/VERW (no LDT/TSS modeled).
func (d *decoder) exec0F00() error {
	modrm, _, _, r, err := d.parseRM(2)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	switch sub := (modrm >> 3) & 7; sub {
	case 0, 1: // SLDT / STR: no tables, store 0
		return d.writeRM(&r, 2, 0)
	case 2, 3: // LLDT / LTR: accept (no privilege model)
		return nil
	case 4, 5: // VERR / VERW: ZF=1 when selector lies inside the GDT
		sel, err := d.readRM(&r, 2)
		if err != nil {
			return err
		}
		if (sel&0xFFFF)&^7+7 <= c.GDTRLimit {
			c.RFLAGS &^= FLAG_ZF
		} else {
			c.RFLAGS |= FLAG_ZF
		}
		return nil
	}
	return nil
}

// execLARLSL implements LAR (0F 02) and LSL (0F 03) against the GDTR.
// ponytail: RPL/DPL privilege checks skipped; upgrade when guests rely on ring checks
func (d *decoder) execLARLSL(op byte) error {
	w := d.width()
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	sel, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	c := d.c
	off := sel & 0xFFFF &^ 7
	if off+7 > c.GDTRLimit {
		c.RFLAGS |= FLAG_ZF
		return nil
	}
	b, err := d.readBytes(c.GDTRBase+off, 8)
	if err != nil {
		return err
	}
	if b[5]&0x80 == 0 { // not present
		c.RFLAGS |= FLAG_ZF
		return nil
	}
	var v uint64
	if op == 0x02 { // LAR: access rights = byte6<<16 | byte5<<8
		v = uint64(b[6])<<16 | uint64(b[5])<<8
	} else { // LSL: limit, granularity-expanded when G=1
		v = uint64(b[0]) | uint64(b[1])<<8 | uint64(b[6]&0xF)<<16 | uint64(b[7])<<24
		if b[6]&0x80 != 0 {
			v = (v << 12) | 0xFFF
		}
	}
	c.RFLAGS &^= FLAG_ZF
	return d.writeRM(&regRef, w, v)
}

// execBT handles BT/BTS/BTR/BTC (0F A3/AB/B3/BB and imm8 group 0F BA).
func (d *decoder) execBT(op byte) error {
	w := d.width()
	modrm, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	kind := 0 // 0 BT, 1 BTS, 2 BTR, 3 BTC
	if op == 0xBA {
		switch sub := (modrm >> 3) & 7; sub {
		case 4, 5, 6, 7:
			kind = int(sub - 4)
		default:
			return fmt.Errorf("bad 0F BA /%d at rip=0x%x", sub, d.start)
		}
	} else {
		switch op {
		case 0xAB:
			kind = 1
		case 0xB3:
			kind = 2
		case 0xBB:
			kind = 3
		}
	}
	var off uint64
	if op == 0xBA {
		imm, err := d.fetchImm(1, true)
		if err != nil {
			return err
		}
		off = imm
	} else {
		off, err = d.readRM(&regRef, w)
		if err != nil {
			return err
		}
	}
	c := d.c
	if r.isReg {
		idx := off & uint64(w*8-1)
		v, err := d.readRM(&r, w)
		if err != nil {
			return err
		}
		if v>>idx&1 != 0 {
			c.RFLAGS |= FLAG_CF
		} else {
			c.RFLAGS &^= FLAG_CF
		}
		nv := v
		switch kind {
		case 1:
			nv = v | 1<<idx
		case 2:
			nv = v &^ (1 << idx)
		case 3:
			nv = v ^ (1 << idx)
		}
		return d.writeRM(&r, w, nv)
	}
	// memory: bit offset is signed; byte base shifts by off/8
	adr := r.addr + uint64(int64(sext(off, w))/8)
	bit := off & 7
	b, err := d.readBytes(adr, 1)
	if err != nil {
		return err
	}
	if b[0]>>bit&1 != 0 {
		c.RFLAGS |= FLAG_CF
	} else {
		c.RFLAGS &^= FLAG_CF
	}
	nb := b[0]
	switch kind {
	case 1:
		nb |= 1 << bit
	case 2:
		nb &^= 1 << bit
	case 3:
		nb ^= 1 << bit
	}
	return d.writeBytes(adr, []byte{nb})
}

// execSHLD handles SHLD (0F A4/A5) and SHRD (0F AC/AD).
func (d *decoder) execSHLD(op byte) error {
	w := d.width()
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	var cnt uint64
	if op == 0xA4 || op == 0xAC {
		imm, err := d.fetchImm(1, false)
		if err != nil {
			return err
		}
		cnt = imm
	} else {
		cnt = d.c.Regs[RCX] & 0xFF
	}
	if w == 8 {
		cnt &= 0x3F
	} else {
		cnt &= 0x1F
	}
	if cnt == 0 {
		return nil // no operand or flag change
	}
	dst, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	src, err := d.readRM(&regRef, w)
	if err != nil {
		return err
	}
	var res uint64
	var cf bool
	if op == 0xA4 || op == 0xA5 {
		res, cf = shldBits(dst, src, cnt, w)
	} else {
		res, cf = shrdBits(dst, src, cnt, w)
	}
	if err := d.writeRM(&r, w, res); err != nil {
		return err
	}
	if cf {
		d.c.RFLAGS |= FLAG_CF
	} else {
		d.c.RFLAGS &^= FLAG_CF
	}
	return nil
}

func shldBits(dst, src, cnt uint64, w int) (uint64, bool) {
	total := uint64(w * 8)
	m := maskW(^uint64(0), w)
	dst, src = dst&m, src&m
	if cnt > total {
		return 0, false // beyond width: undefined on hardware
	}
	res := (dst << cnt) | (src >> (total - cnt))
	cf := dst>>(total-cnt)&1 != 0
	return res & m, cf
}

func shrdBits(dst, src, cnt uint64, w int) (uint64, bool) {
	total := uint64(w * 8)
	m := maskW(^uint64(0), w)
	dst, src = dst&m, src&m
	if cnt > total {
		return 0, false
	}
	res := (dst >> cnt) | (src << (total - cnt))
	cf := dst>>(cnt-1)&1 != 0
	return res & m, cf
}

// execCMPXCHG handles 0F B0/B1.
func (d *decoder) execCMPXCHG(op byte) error {
	w := 1
	if op == 0xB1 {
		w = d.width()
	}
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	dst, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	src, err := d.readRM(&regRef, w)
	if err != nil {
		return err
	}
	accRef := rmRef{isReg: true, idx: RAX}
	acc, err := d.readRM(&accRef, w)
	if err != nil {
		return err
	}
	ALU(d.c, ALUCmp, acc, dst, w)
	if acc == dst {
		d.c.SetZF(true)
		return d.writeRM(&r, w, src)
	}
	d.c.SetZF(false)
	return d.writeRM(&accRef, w, dst)
}

// execLSS handles LSS/LFS/LGS (0F B2/B4/B5): loads offset, selector discarded.
func (d *decoder) execLSS(op byte) error {
	w := d.width()
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	off, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	_ = op
	return d.writeRM(&regRef, w, off)
}

// execXADD handles 0F C0/C1.
func (d *decoder) execXADD(op byte) error {
	w := 4
	if op == 0xC0 {
		w = 1
	} else {
		w = d.width()
	}
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	dst, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	src, err := d.readRM(&regRef, w)
	if err != nil {
		return err
	}
	res := ALU(d.c, ALUAdd, dst, src, w)
	if err := d.writeRM(&r, w, res); err != nil {
		return err
	}
	return d.writeRM(&regRef, w, dst)
}

// execPOPCNT handles F3 0F B8.
func (d *decoder) execPOPCNT() error {
	if !d.opF3 {
		return fmt.Errorf("unsupported 2-byte opcode: 0x0F 0xB8 at rip=0x%x", d.start)
	}
	w := d.width()
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	src, err := d.readRM(&r, w)
	if err != nil {
		return err
	}
	n := uint64(bits.OnesCount64(maskW(src, w)))
	c := d.c
	c.RFLAGS &^= FLAG_CF | FLAG_OF | FLAG_SF | FLAG_AF | FLAG_PF
	c.SetZF(n == 0)
	return d.writeRM(&regRef, w, n)
}

// execMOVNTI handles 0F C3: plain store to [r/m].
func (d *decoder) execMOVNTI() error {
	w := d.width()
	_, _, regRef, r, err := d.parseRM(w)
	if err != nil {
		return err
	}
	d.finish(&r)
	v, err := d.readRM(&regRef, w)
	if err != nil {
		return err
	}
	return d.writeRM(&r, w, v)
}

// exec0FC7 handles 0F C7: CMPXCHG8B/16B (mod!=3 /0), RDRAND (/6), RDSEED (/7).
func (d *decoder) exec0FC7() error {
	modrm, _, _, r, err := d.parseRM(8)
	if err != nil {
		return err
	}
	c := d.c
	sub := (modrm >> 3) & 7
	w := 4
	if d.rexW {
		w = 8
	}
	if modrm>>6 == 3 {
		switch sub {
		case 6: // RDRAND
			v := c.rngNext()
			c.RFLAGS |= FLAG_CF
			return d.writeRM(&r, w, v)
		case 7: // RDSEED
			v := c.rngNext() ^ 0xA5A5A5A5A5A5A5A5
			c.RFLAGS |= FLAG_CF
			return d.writeRM(&r, w, v)
		}
		return fmt.Errorf("unsupported 0F C7 /%d register form at rip=0x%x", sub, d.start)
	}
	if sub != 0 {
		return fmt.Errorf("unsupported 0F C7 /%d memory form at rip=0x%x", sub, d.start)
	}
	if d.rexW { // CMPXCHG16B m128: compare RDX:RAX, swap RCX:RBX
		b, err := d.readBytes(r.addr, 16)
		if err != nil {
			return err
		}
		curLo := le64(b[0:8])
		curHi := le64(b[8:16])
		if curLo == c.Regs[RAX] && curHi == c.Regs[RDX] {
			c.SetZF(true)
			nb := make([]byte, 16)
			putLe64(nb[0:8], c.Regs[RBX])
			putLe64(nb[8:16], c.Regs[RCX])
			return d.writeBytes(r.addr, nb)
		}
		c.SetZF(false)
		c.Regs[RAX] = curLo
		c.Regs[RDX] = curHi
		return nil
	}
	// CMPXCHG8B m64: compare EDX:EAX, swap ECX:EBX
	b, err := d.readBytes(r.addr, 8)
	if err != nil {
		return err
	}
	cur := le64(b)
	if uint64(uint32(c.Regs[RDX]))<<32|uint64(uint32(c.Regs[RAX])) == cur {
		c.SetZF(true)
		nb := make([]byte, 8)
		putLe64(nb, uint64(uint32(c.Regs[RCX]))<<32|uint64(uint32(c.Regs[RBX])))
		return d.writeBytes(r.addr, nb)
	}
	c.SetZF(false)
	c.Regs[RAX] = c.Regs[RAX]&^0xFFFFFFFF | uint64(uint32(cur))
	c.Regs[RDX] = c.Regs[RDX]&^0xFFFFFFFF | uint64(uint32(cur>>32))
	return nil
}

// rngNext is a deterministic LCG stand-in for RDRAND/RDSEED.
func (c *CPUState) rngNext() uint64 {
	if c.RNGState == 0 {
		c.RNGState = 0x9E3779B97F4A7C15
	}
	c.RNGState = c.RNGState*6364136223846793005 + 1442695040888963407
	return c.RNGState
}
