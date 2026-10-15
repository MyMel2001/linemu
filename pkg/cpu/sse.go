package cpu

import (
	"encoding/binary"
	"fmt"
	"math"
)

// sse.go: SSE2 packed ops. MMX regs alias FPUStack slots (ST(i)).

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

func putLe16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putLe32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putLe64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

func leN(b []byte) uint64 {
	var v uint64
	for i := 0; i < len(b); i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}

func (d *decoder) ssePfx() byte {
	switch {
	case d.opF3:
		return 0xF3
	case d.opF2:
		return 0xF2
	case d.op66:
		return 0x66
	}
	return 0
}

// vecSrc loads a vector operand into a 16-byte buffer (bytes past n stay 0).
// mmx selects the MMX bank; memory operands read 8 bytes there.
func (d *decoder) vecSrc(r *rmRef, n int, mmx bool) ([16]byte, error) {
	var out [16]byte
	if r.isReg {
		if mmx {
			v := d.c.FPUStack[(d.c.FPUTop+r.idx)&7]
			for i := 0; i < 8; i++ {
				out[i] = byte(v >> (8 * i))
			}
			return out, nil
		}
		return d.c.XMM[r.idx], nil
	}
	size := n
	if mmx {
		size = 8
	}
	b, err := d.readBytes(r.addr, size)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

func (d *decoder) vecStore(r *rmRef, v [16]byte, mmx bool) error {
	if r.isReg {
		if mmx {
			d.c.FPUStack[(d.c.FPUTop+r.idx)&7] = le64(v[:8])
			return nil
		}
		d.c.XMM[r.idx] = v
		return nil
	}
	if mmx {
		return d.writeBytes(r.addr, v[:8])
	}
	return d.writeBytes(r.addr, v[:16])
}

// vecCount reads a shift-count operand's low 64 bits (reg or m64).
func (d *decoder) vecCount(r *rmRef, mmx bool) (uint64, error) {
	if r.isReg {
		if mmx {
			return d.c.FPUStack[(d.c.FPUTop+r.idx)&7], nil
		}
		return le64(d.c.XMM[r.idx][:8]), nil
	}
	b, err := d.readBytes(r.addr, 8)
	if err != nil {
		return 0, err
	}
	return le64(b), nil
}

// sseSrc loads a scalar/partial XMM source reading only n memory bytes.
func (d *decoder) sseSrc(r *rmRef, n int) ([16]byte, error) {
	var out [16]byte
	if r.isReg {
		return d.c.XMM[r.idx], nil
	}
	b, err := d.readBytes(r.addr, n)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

func dstRefOf(regRef rmRef) rmRef { return rmRef{isReg: true, idx: regRef.idx} }

// ---- element helpers ------------------------------------------------------

// sgn sign-extends an ew-byte lane.
func sgn(x uint64, ew int) int64 {
	s := uint(64 - ew*8)
	return int64(x<<s) >> s
}

// satS clamps to the signed ew-byte range.
func satS(x int64, ew int) uint64 {
	if ew >= 8 {
		return uint64(x)
	}
	lo := -(int64(1) << (ew*8 - 1))
	hi := int64(1)<<(ew*8-1) - 1
	if x < lo {
		return uint64(lo)
	}
	if x > hi {
		return uint64(hi)
	}
	return uint64(x)
}

// satU clamps to the unsigned ew-byte range.
func satU(x int64, ew int) uint64 {
	if x < 0 {
		return 0
	}
	if ew >= 8 {
		return uint64(x)
	}
	hi := int64(1)<<(ew*8) - 1
	if x > hi {
		return uint64(hi)
	}
	return uint64(x)
}

func mapElems(dst []byte, n, ew int, f func(x uint64) uint64) {
	for i := 0; i < n; i++ {
		off := i * ew
		y := f(leN(dst[off : off+ew]))
		for j := 0; j < ew; j++ {
			dst[off+j] = byte(y >> (8 * j))
		}
	}
}

func mapElems2(dst, sb []byte, n, ew int, f func(x, y uint64) uint64) {
	for i := 0; i < n; i++ {
		off := i * ew
		y := f(leN(dst[off:off+ew]), leN(sb[off:off+ew]))
		for j := 0; j < ew; j++ {
			dst[off+j] = byte(y >> (8 * j))
		}
	}
}

func satAddU(x, y uint64, ew int) uint64 {
	s := x + y
	if ew >= 8 {
		return s
	}
	max := uint64(1)<<(ew*8) - 1
	if s > max || s < x {
		return max
	}
	return s
}

// cmpFloatFlags sets CF/ZF/PF for a vs b (unordered → all three set).
func (c *CPUState) cmpFloatFlags(a, b float64) {
	c.RFLAGS &^= FLAG_CF | FLAG_ZF | FLAG_PF | FLAG_OF | FLAG_AF | FLAG_SF
	switch {
	case math.IsNaN(a) || math.IsNaN(b):
		c.RFLAGS |= FLAG_CF | FLAG_ZF | FLAG_PF
	case a < b:
		c.RFLAGS |= FLAG_CF
	case a == b:
		c.RFLAGS |= FLAG_ZF
	}
}

func isSSEOp(op byte) bool {
	switch {
	case op >= 0x10 && op <= 0x17:
		return true
	case op >= 0x28 && op <= 0x2F:
		return true
	case op >= 0x50 && op <= 0x5F:
		return true
	case op >= 0x60 && op <= 0x6D:
		return true
	case op >= 0x70 && op <= 0x76:
		return true
	case op == 0x7C || op == 0x7D:
		return true
	case op == 0xC2 || op == 0xC4 || op == 0xC5 || op == 0xC6:
		return true
	case op >= 0xD0 && op <= 0xDF:
		return true
	case op >= 0xE0 && op <= 0xEF:
		return true
	case op >= 0xF0 && op <= 0xF7:
		return true
	case op >= 0xFC && op <= 0xFE:
		return true
	}
	return false
}

// execSSE2 dispatches two-byte SSE/SSE2 opcodes not covered by execSSE.
func (d *decoder) execSSE2(op byte) error {
	pfx := d.ssePfx()
	switch {
	case op >= 0x12 && op <= 0x17:
		return d.sseMoveLane(op, pfx)
	case op >= 0x2A && op <= 0x2F:
		return d.sseConv(op, pfx)
	case op == 0x50:
		return d.sseMovMask(pfx)
	case op == 0x5A || op == 0x5B:
		return d.sseCvtPS(op, pfx)
	case op >= 0x51 && op <= 0x5F:
		return d.sseFloatOp(op, pfx)
	case (op >= 0x60 && op <= 0x6D) || (op >= 0x74 && op <= 0x76) ||
		(op >= 0xD0 && op <= 0xDF) || (op >= 0xE0 && op <= 0xE5) ||
		(op >= 0xE8 && op <= 0xEF) || (op >= 0xF1 && op <= 0xF6) ||
		(op >= 0xFC && op <= 0xFE):
		return d.sseIntRow(op, pfx)
	case op == 0x70 || op == 0xC6:
		return d.sseShuf(op, pfx)
	case op >= 0x71 && op <= 0x73:
		return d.sseShiftImm(op, pfx)
	case op == 0x7C || op == 0x7D:
		return d.sseHaddSub(op, pfx)
	case op == 0xD0:
		return d.sseAddSub(pfx)
	case op == 0xC2:
		return d.sseCmpPacked(pfx)
	case op == 0xC4 || op == 0xC5:
		return d.sseWordInsert(op, pfx)
	case op == 0xE6:
		return d.sseCvtDQ(pfx)
	case op == 0xE7:
		return d.sseStoreNT(pfx)
	case op == 0xF0:
		return d.sseLddqu()
	case op == 0xF7:
		return d.sseMaskStore(pfx)
	}
	return fmt.Errorf("SSE opcode 0x0F 0x%02X not implemented at rip=0x%x", op, d.start)
}

// ---- loads/stores/unpacks (0F 12-17) --------------------------------------

func (d *decoder) sseMoveLane(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	dst := regRef.idx
	c := d.c

	switch op {
	case 0x12:
		switch pfx {
		case 0xF3: // MOVSLDUP
			src, err := d.sseSrc(&r, 16)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 4; i++ {
				copy(out[8*i:8*i+4], src[8*i:8*i+4])
				copy(out[8*i+4:8*i+8], src[8*i:8*i+4])
			}
			c.XMM[dst] = out
		case 0xF2: // MOVDDUP
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 2; i++ {
				copy(out[8*i:8*i+8], src[0:8])
			}
			c.XMM[dst] = out
		case 0x66: // MOVLPD xmm, m64
			if r.isReg {
				return fmt.Errorf("MOVLPD register form invalid at rip=0x%x", d.start)
			}
			b, err := d.readBytes(r.addr, 8)
			if err != nil {
				return err
			}
			copy(c.XMM[dst][0:8], b)
		default: // MOVLPS (mem) / MOVHLPS (reg)
			if r.isReg {
				copy(c.XMM[dst][0:8], c.XMM[r.idx][8:16])
			} else {
				b, err := d.readBytes(r.addr, 8)
				if err != nil {
					return err
				}
				copy(c.XMM[dst][0:8], b)
			}
		}
	case 0x13: // MOVLPS/MOVLPD m64, xmm (store low quad)
		if r.isReg {
			return fmt.Errorf("MOVLPS store requires memory operand at rip=0x%x", d.start)
		}
		return d.writeBytes(r.addr, c.XMM[dst][0:8])
	case 0x14, 0x15: // UNPCKLPS/PD, UNPCKHPS/PD
		srcN := 16
		if pfx == 0x66 {
			srcN = 8
		}
		src, err := d.sseSrc(&r, srcN)
		if err != nil {
			return err
		}
		var out [16]byte
		if pfx == 0x66 { // interleaved qwords: low pair or high pair
			if op == 0x14 {
				copy(out[0:8], c.XMM[dst][0:8])
				copy(out[8:16], src[0:8])
			} else {
				copy(out[0:8], c.XMM[dst][8:16])
				copy(out[8:16], src[8:16])
			}
		} else if op == 0x14 { // a0,b0,a1,b1
			copy(out[0:4], c.XMM[dst][0:4])
			copy(out[4:8], src[0:4])
			copy(out[8:12], c.XMM[dst][4:8])
			copy(out[12:16], src[4:8])
		} else { // a2,b2,a3,b3
			copy(out[0:4], c.XMM[dst][8:12])
			copy(out[4:8], src[8:12])
			copy(out[8:12], c.XMM[dst][12:16])
			copy(out[12:16], src[12:16])
		}
		c.XMM[dst] = out
	case 0x16:
		switch pfx {
		case 0xF3: // MOVSHDUP
			src, err := d.sseSrc(&r, 16)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 4; i++ {
				copy(out[8*i:8*i+4], src[8*i+4:8*i+8])
				copy(out[8*i+4:8*i+8], src[8*i+4:8*i+8])
			}
			c.XMM[dst] = out
		case 0x66: // MOVHPD xmm, m64
			if r.isReg {
				return fmt.Errorf("MOVHPD register form invalid at rip=0x%x", d.start)
			}
			b, err := d.readBytes(r.addr, 8)
			if err != nil {
				return err
			}
			copy(c.XMM[dst][8:16], b)
		default: // MOVLHPS (reg) / MOVHPS (mem)
			if r.isReg {
				copy(c.XMM[dst][8:16], c.XMM[r.idx][0:8])
			} else {
				b, err := d.readBytes(r.addr, 8)
				if err != nil {
					return err
				}
				copy(c.XMM[dst][8:16], b)
			}
		}
	case 0x17: // MOVHPS/MOVHPD m64, xmm (store high quad)
		if r.isReg {
			return fmt.Errorf("MOVHPS store requires memory operand at rip=0x%x", d.start)
		}
		return d.writeBytes(r.addr, c.XMM[dst][8:16])
	}
	return nil
}

// ---- conversions and compares (0F 2A-2F) ----------------------------------

func (d *decoder) sseConv(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dstRef := dstRefOf(regRef)

	switch op {
	case 0x2A:
		switch pfx {
		case 0: // CVTPI2PS xmm, mm/m64 (upper 8 zeroed)
			src, err := d.vecSrc(&r, 8, true)
			if err != nil {
				return err
			}
			for i := 0; i < 2; i++ {
				f := float32(float64(int32(le32(src[4*i:]))))
				putLe32(c.XMM[regRef.idx][4*i:], math.Float32bits(f))
			}
			for i := 8; i < 16; i++ {
				c.XMM[regRef.idx][i] = 0
			}
		case 0x66: // CVTPI2PD xmm, mm/m64
			src, err := d.vecSrc(&r, 8, true)
			if err != nil {
				return err
			}
			for i := 0; i < 2; i++ {
				f := float64(int32(le32(src[4*i:])))
				putLe64(c.XMM[regRef.idx][8*i:], math.Float64bits(f))
			}
		case 0xF3, 0xF2: // CVTSI2SS/SD xmm, r/m(32|64): upper preserved
			w := 4
			if d.rexW {
				w = 8
			}
			v, err := d.readRM(&r, w)
			if err != nil {
				return err
			}
			var f float64
			if w == 4 {
				f = float64(int32(uint32(v)))
			} else {
				f = float64(int64(v))
			}
			if pfx == 0xF3 {
				putLe32(c.XMM[regRef.idx][0:], math.Float32bits(float32(f)))
			} else {
				putLe64(c.XMM[regRef.idx][0:], math.Float64bits(f))
			}
		}
		return nil

	case 0x2B: // MOVNT* store
		if r.isReg {
			return fmt.Errorf("MOVNT requires memory operand at rip=0x%x", d.start)
		}
		n := 16
		switch pfx {
		case 0xF3:
			n = 4
		case 0xF2:
			n = 8
		}
		return d.writeBytes(r.addr, c.XMM[regRef.idx][:n])

	case 0x2C, 0x2D: // CVT[T]PS2PI / CVT[T]PD2PI (mm dest), CVT[T]SS2SI / CVT[T]SD2SI (gpr dest)
		rc := 0
		if op == 0x2C {
			rc = 3
		}
		switch pfx {
		case 0: // cvt[t]ps2pi mm, xmm/m64: 2×f32 -> i32
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 2; i++ {
				f := float64(math.Float32frombits(le32(src[4*i:])))
				putLe32(out[4*i:], uint32(f64ToIntW(f, rc, 4)))
			}
			return d.vecStore(&dstRef, out, true)
		case 0x66: // cvt[t]pd2pi mm, xmm/m64: 2×f64 -> i32
			src, err := d.sseSrc(&r, 16)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 2; i++ {
				f := math.Float64frombits(le64(src[8*i:]))
				putLe32(out[4*i:], uint32(f64ToIntW(f, rc, 4)))
			}
			return d.vecStore(&dstRef, out, true)
		case 0xF3: // cvt[t]ss2si r32/64, xmm/m32
			src, err := d.sseSrc(&r, 4)
			if err != nil {
				return err
			}
			w := 4
			if d.rexW {
				w = 8
			}
			f := float64(math.Float32frombits(le32(src[0:4])))
			return d.writeRM(&regRef, w, f64ToIntW(f, rc, w))
		case 0xF2: // cvt[t]sd2si r32/64, xmm/m64
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			w := 4
			if d.rexW {
				w = 8
			}
			f := math.Float64frombits(le64(src[0:8]))
			return d.writeRM(&regRef, w, f64ToIntW(f, rc, w))
		}
		return nil

	case 0x2E, 0x2F: // UCOMISS/COMISS (none), UCOMISD/COMISD (66)
		if pfx == 0xF2 || pfx == 0xF3 {
			return fmt.Errorf("UCOMIS invalid prefix at rip=0x%x", d.start)
		}
		if pfx == 0x66 {
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			a := math.Float64frombits(le64(c.XMM[regRef.idx][0:8]))
			b := math.Float64frombits(le64(src[0:8]))
			c.cmpFloatFlags(a, b)
			return nil
		}
		src, err := d.sseSrc(&r, 4)
		if err != nil {
			return err
		}
		a := math.Float32frombits(le32(c.XMM[regRef.idx][0:4]))
		b := math.Float32frombits(le32(src[0:4]))
		c.cmpFloatFlags(float64(a), float64(b))
		return nil
	}
	return nil
}

// sseMovMask (0F 50) moves vector sign bits to a GPR.
func (d *decoder) sseMovMask(pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	if !r.isReg {
		return fmt.Errorf("MOVMSKPS requires register source at rip=0x%x", d.start)
	}
	var mask uint64
	if pfx == 0x66 { // movmskpd: bit per qword
		for i := 0; i < 2; i++ {
			if d.c.XMM[r.idx][8*i]&0x80 != 0 {
				mask |= 1 << i
			}
		}
	} else { // movmskps: bit per byte
		for i := 0; i < 8; i++ {
			if d.c.XMM[r.idx][i]&0x80 != 0 {
				mask |= 1 << i
			}
		}
	}
	return d.writeRM(&regRef, 4, mask)
}

// sseCvtPS handles 0F 5A (cvtps2pd/cvtpd2ps/cvtss2sd/cvtsd2ss) and 0F 5B
// (cvtdq2ps/cvtps2dq/cvttps2dq).
func (d *decoder) sseCvtPS(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx

	if op == 0x5A {
		switch pfx {
		case 0: // CVTPS2PD xmm, xmm/m64: 2×f32 -> 2×f64
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 2; i++ {
				f := float64(math.Float32frombits(le32(src[4*i:])))
				putLe64(out[8*i:], math.Float64bits(f))
			}
			c.XMM[dst] = out
		case 0x66: // CVTPD2PS: 2×f64 -> 2×f32, upper 8 zeroed
			src, err := d.sseSrc(&r, 16)
			if err != nil {
				return err
			}
			var out [16]byte
			for i := 0; i < 2; i++ {
				f := math.Float64frombits(le64(src[8*i:]))
				putLe32(out[4*i:], math.Float32bits(float32(f)))
			}
			c.XMM[dst] = out
		case 0xF3: // CVTSS2SD: lane0, upper preserved
			src, err := d.sseSrc(&r, 4)
			if err != nil {
				return err
			}
			f := float64(math.Float32frombits(le32(src[0:4])))
			putLe64(c.XMM[dst][0:], math.Float64bits(f))
		case 0xF2: // CVTSD2SS: lane0, upper preserved
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			f := math.Float64frombits(le64(src[0:8]))
			putLe32(c.XMM[dst][0:], math.Float32bits(float32(f)))
		}
		return nil
	}

	// 0F 5B
	rc := 0
	if pfx == 0xF3 {
		rc = 3
	}
	if pfx == 0xF2 {
		return fmt.Errorf("CVTPS2DQ F2 form invalid at rip=0x%x", d.start)
	}
	src, err := d.sseSrc(&r, 16)
	if err != nil {
		return err
	}
	var out [16]byte
	if pfx == 0 { // CVTDQ2PS: 4×i32 -> 4×f32
		for i := 0; i < 4; i++ {
			f := float64(int32(le32(src[4*i:])))
			putLe32(out[4*i:], math.Float32bits(float32(f)))
		}
	} else { // CVTPS2dq / CVTTPS2DQ
		for i := 0; i < 4; i++ {
			f := float64(math.Float32frombits(le32(src[4*i:])))
			putLe32(out[4*i:], uint32(f64ToIntW(f, rc, 4)))
		}
	}
	c.XMM[dst] = out
	return nil
}

// sseFloatOp handles 0F 51-5F (sqrt/rsqrt/rcp/bitwise/arithmetic).
func (d *decoder) sseFloatOp(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx

	// Bitwise ops work on the full 16 bytes regardless of prefix.
	if op >= 0x54 && op <= 0x57 {
		src, err := d.sseSrc(&r, 16)
		if err != nil {
			return err
		}
		var out [16]byte
		for i := 0; i < 16; i++ {
			switch op {
			case 0x54:
				out[i] = c.XMM[dst][i] & src[i]
			case 0x55:
				out[i] = ^c.XMM[dst][i] & src[i]
			case 0x56:
				out[i] = c.XMM[dst][i] | src[i]
			default:
				out[i] = c.XMM[dst][i] ^ src[i]
			}
		}
		c.XMM[dst] = out
		return nil
	}

	srcN := 16
	if pfx == 0xF3 {
		srcN = 4
	} else if pfx == 0xF2 {
		srcN = 8
	}
	src, err := d.sseSrc(&r, srcN)
	if err != nil {
		return err
	}
	dstV := c.XMM[dst]
	out := dstV // scalar forms preserve the upper lanes

	if pfx == 0xF3 { // scalar f32: lane0
		a := math.Float32frombits(le32(dstV[0:4]))
		b := math.Float32frombits(le32(src[0:4]))
		putLe32(out[0:], math.Float32bits(sseF32(op, a, b)))
		c.XMM[dst] = out
		return nil
	}
	if pfx == 0xF2 { // scalar f64: lane0
		a := math.Float64frombits(le64(dstV[0:8]))
		b := math.Float64frombits(le64(src[0:8]))
		putLe64(out[0:], math.Float64bits(sseF64(op, a, b)))
		c.XMM[dst] = out
		return nil
	}
	if pfx == 0x66 { // packed doubles: 2×f64
		for i := 0; i < 2; i++ {
			a := math.Float64frombits(le64(dstV[8*i:]))
			b := math.Float64frombits(le64(src[8*i:]))
			putLe64(out[8*i:], math.Float64bits(sseF64(op, a, b)))
		}
	} else { // packed singles: 4×f32
		for i := 0; i < 4; i++ {
			a := math.Float32frombits(le32(dstV[4*i:]))
			b := math.Float32frombits(le32(src[4*i:]))
			putLe32(out[4*i:], math.Float32bits(sseF32(op, a, b)))
		}
	}
	c.XMM[dst] = out
	return nil
}

func sseF32(op byte, a, b float32) float32 {
	switch op {
	case 0x51:
		return float32(math.Sqrt(float64(a)))
	case 0x52:
		return float32(1 / math.Sqrt(float64(a)))
	case 0x53:
		return float32(1 / float64(a))
	case 0x58:
		return a + b
	case 0x59:
		return a * b
	case 0x5C:
		return a - b
	case 0x5D:
		if a < b {
			return a
		}
		return b
	case 0x5E:
		return a / b
	case 0x5F:
		if a > b {
			return a
		}
		return b
	}
	return 0
}

func sseF64(op byte, a, b float64) float64 {
	switch op {
	case 0x51:
		return math.Sqrt(a)
	case 0x52:
		return 1 / math.Sqrt(a)
	case 0x53:
		return 1 / a
	case 0x58:
		return a + b
	case 0x59:
		return a * b
	case 0x5C:
		return a - b
	case 0x5D:
		if a < b {
			return a
		}
		return b
	case 0x5E:
		return a / b
	case 0x5F:
		if a > b {
			return a
		}
		return b
	}
	return 0
}

// ---- integer rows (0F 60-6D, 74-76, D0-DF, E0-E5, E8-EF, F1-F6, FC-FE) ----

func (d *decoder) sseIntRow(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	mmx := pfx != 0x66
	dstRef := dstRefOf(regRef)
	srcN := 16
	if mmx {
		srcN = 8
	}

	switch op {
	// PUNPCKL* / PUNPCKH* interleave
	case 0x60, 0x61, 0x62, 0x68, 0x69, 0x6A:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		ew := 1 << ((op - 0x60) & 3)
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		half := nel / 2
		var out [16]byte
		for i := 0; i < half; i++ {
			si := i
			if op >= 0x68 {
				si = half + i
			}
			copy(out[2*i*ew:2*i*ew+ew], a[si*ew:si*ew+ew])
			copy(out[(2*i+1)*ew:(2*i+1)*ew+ew], b[si*ew:si*ew+ew])
		}
		return d.vecStore(&dstRef, out, mmx)

	// PUNPCKLQDQ / PUNPCKHQDQ (66 only, treated as XMM)
	case 0x6C, 0x6D:
		a, err := d.vecSrc(&dstRef, 16, false)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, 16, false)
		if err != nil {
			return err
		}
		var out [16]byte
		off := 0
		if op == 0x6D {
			off = 8
		}
		copy(out[0:8], a[off:off+8])
		copy(out[8:16], b[off:off+8])
		d.c.XMM[regRef.idx] = out
		return nil

	// PACKSSWB / PACKUSWB / PACKSSDW (saturating narrowing)
	case 0x63, 0x67, 0x6B:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		ewIn, ewOut := 2, 1
		if op == 0x6B {
			ewIn, ewOut = 4, 2
		}
		signed := op != 0x67
		nIn := 16 / ewIn
		if mmx {
			nIn = 8 / ewIn
		}
		var out [16]byte
		k := 0
		for _, v := range [][16]byte{a, b} {
			for i := 0; i < nIn; i++ {
				off := i * ewIn
				var y uint64
				if signed {
					y = satS(sgn(leN(v[off:off+ewIn]), ewIn), ewOut)
				} else {
					y = satU(sgn(leN(v[off:off+ewIn]), ewIn), ewOut)
				}
				for j := 0; j < ewOut; j++ {
					out[k+j] = byte(y >> (8 * j))
				}
				k += ewOut
			}
		}
		return d.vecStore(&dstRef, out, mmx)

	// PCMPEQ* / PCMPGT*
	case 0x64, 0x65, 0x66, 0x74, 0x75, 0x76:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		ew := 1 << (op & 3)
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		if op < 0x70 { // pcmpgt: signed greater
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if sgn(x, ew) > sgn(y, ew) {
					return ^uint64(0)
				}
				return 0
			})
		} else { // pcmpeq
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if x == y {
					return ^uint64(0)
				}
				return 0
			})
		}
		return d.vecStore(&dstRef, out, mmx)

	// shift by register count: PSRLW/PSRLD/PSRLQ, PSRAW/PSRAD, PSLLW/PSLLD/PSLLQ
	case 0xD1, 0xD2, 0xD3, 0xE1, 0xE2, 0xF1, 0xF2, 0xF3:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		cnt, err := d.vecCount(&r, mmx)
		if err != nil {
			return err
		}
		var ew, kind int // kind: 0 logical right, 1 arithmetic right, 2 left
		switch op {
		case 0xD1:
			ew, kind = 2, 0
		case 0xD2:
			ew, kind = 4, 0
		case 0xD3:
			ew, kind = 8, 0
		case 0xE1:
			ew, kind = 2, 1
		case 0xE2:
			ew, kind = 4, 1
		case 0xF1:
			ew, kind = 2, 2
		case 0xF2:
			ew, kind = 4, 2
		case 0xF3:
			ew, kind = 8, 2
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		mapElems(out[:nel*ew], nel, ew, func(x uint64) uint64 {
			switch kind {
			case 0:
				if cnt >= uint64(ew*8) {
					return 0
				}
				return x >> cnt
			case 1:
				if cnt >= uint64(ew*8) {
					if sgn(x, ew) < 0 {
						return ^uint64(0)
					}
					return 0
				}
				return uint64(sgn(x, ew) >> cnt)
			default:
				if cnt >= uint64(ew*8) {
					return 0
				}
				return x << cnt
			}
		})
		return d.vecStore(&dstRef, out, mmx)

	// PADDQ / PMULLW
	case 0xD4, 0xD5:
		ew := 8
		if op == 0xD5 {
			ew = 2
		}
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		if op == 0xD4 { // PADDQ: wrapping 64-bit add
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return x + y })
		} else { // PMULLW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return x * y })
		}
		return d.vecStore(&dstRef, out, mmx)

	// MOVQ forms
	case 0xD6:
		switch pfx {
		case 0x66: // movq m64, xmm (store low quad)
			if r.isReg {
				return fmt.Errorf("MOVQ store requires memory operand at rip=0x%x", d.start)
			}
			return d.writeBytes(r.addr, d.c.XMM[regRef.idx][0:8])
		case 0xF3: // MOVQ2DQ xmm, mm
			var out [16]byte
			if r.isReg {
				v := d.c.FPUStack[(d.c.FPUTop+r.idx)&7]
				for i := 0; i < 8; i++ {
					out[i] = byte(v >> (8 * i))
				}
			} else {
				b, err := d.readBytes(r.addr, 8)
				if err != nil {
					return err
				}
				copy(out[:8], b)
			}
			d.c.XMM[regRef.idx] = out
			return nil
		case 0xF2: // MOVDQ2Q mm, xmm
			if !r.isReg {
				return fmt.Errorf("MOVDQ2Q requires register source at rip=0x%x", d.start)
			}
			d.c.FPUStack[(d.c.FPUTop+regRef.idx)&7] = le64(d.c.XMM[r.idx][0:8])
			return nil
		default: // movq m64, mm (store)
			if r.isReg {
				return fmt.Errorf("MOVQ store requires memory operand at rip=0x%x", d.start)
			}
			a, err := d.vecSrc(&dstRef, 8, true)
			if err != nil {
				return err
			}
			return d.writeBytes(r.addr, a[0:8])
		}

	// PMOVMSKB r32, mm/xmm
	case 0xD7:
		n := 8
		if pfx == 0x66 {
			n = 16
		}
		src, err := d.vecSrc(&r, n, pfx != 0x66)
		if err != nil {
			return err
		}
		var mask uint64
		for i := 0; i < n; i++ {
			if src[i]&0x80 != 0 {
				mask |= 1 << i
			}
		}
		return d.writeRM(&regRef, 4, mask)

	// saturating unsigned sub/add, min/max, logical (D8-DF)
	case 0xD8, 0xD9, 0xDA, 0xDB, 0xDC, 0xDD, 0xDE, 0xDF:
		ew := 1
		if op == 0xD9 || op == 0xDD {
			ew = 2
		}
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		switch op {
		case 0xD8: // PSUBUSB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if x > y {
					return x - y
				}
				return 0
			})
		case 0xD9: // PSUBUSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if x > y {
					return x - y
				}
				return 0
			})
		case 0xDA: // PMINUB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if x < y {
					return x
				}
				return y
			})
		case 0xDB: // PAND
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return x & y })
		case 0xDC: // PADDUSB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satAddU(x, y, ew)
			})
		case 0xDD: // PADDUSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satAddU(x, y, ew)
			})
		case 0xDE: // PMAXUB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if x > y {
					return x
				}
				return y
			})
		case 0xDF: // PANDN
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return ^x & y })
		}
		return d.vecStore(&dstRef, out, mmx)

	// PAVGB / PAVGW / PMULHUW / PMULHW (E0-E5, minus shift counts)
	case 0xE0, 0xE3, 0xE4, 0xE5:
		ew := 1
		if op != 0xE0 {
			ew = 2
		}
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		switch op {
		case 0xE0: // PAVGB
			mapElems2(out[:nel], b[:nel], nel, 1, func(x, y uint64) uint64 {
				return (x + y + 1) >> 1
			})
		case 0xE3: // PAVGW
			mapElems2(out[:nel*2], b[:nel*2], nel, 2, func(x, y uint64) uint64 {
				return (x + y + 1) >> 1
			})
		case 0xE4: // PMULHUW
			mapElems2(out[:nel*2], b[:nel*2], nel, 2, func(x, y uint64) uint64 {
				return (x * y) >> 16
			})
		case 0xE5: // PMULHW
			mapElems2(out[:nel*2], b[:nel*2], nel, 2, func(x, y uint64) uint64 {
				return uint64((int64(sgn(x, 2))*int64(sgn(y, 2)))>>16) & 0xFFFF
			})
		}
		return d.vecStore(&dstRef, out, mmx)

	// saturating signed sub/add, signed min/max (E8-EF, minus logicals)
	case 0xE8, 0xE9, 0xEA, 0xEC, 0xED, 0xEE:
		ew := 1
		if op == 0xE9 || op == 0xED || op == 0xEA || op == 0xEE {
			ew = 2
		}
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		switch op {
		case 0xE8: // PSUBSB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satS(sgn(x, ew)-sgn(y, ew), ew)
			})
		case 0xE9: // PSUBSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satS(sgn(x, ew)-sgn(y, ew), ew)
			})
		case 0xEA: // PMINSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if sgn(x, ew) < sgn(y, ew) {
					return x
				}
				return y
			})
		case 0xEC: // PADDSB
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satS(sgn(x, ew)+sgn(y, ew), ew)
			})
		case 0xED: // PADDSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				return satS(sgn(x, ew)+sgn(y, ew), ew)
			})
		case 0xEE: // PMAXSW
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 {
				if sgn(x, ew) > sgn(y, ew) {
					return x
				}
				return y
			})
		}
		return d.vecStore(&dstRef, out, mmx)

	// POR / PXOR
	case 0xEB, 0xEF:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16
		if mmx {
			nel = 8
		}
		out := a
		if op == 0xEB {
			for i := 0; i < nel; i++ {
				out[i] |= b[i]
			}
		} else {
			for i := 0; i < nel; i++ {
				out[i] ^= b[i]
			}
		}
		return d.vecStore(&dstRef, out, mmx)

	// PMULUDQ (F4)
	case 0xF4:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 2
		if mmx {
			nel = 1
		}
		out := a
		for i := 0; i < nel; i++ {
			x := le32(a[8*i:])
			y := le32(b[8*i:])
			putLe64(out[8*i:], uint64(x)*uint64(y))
		}
		return d.vecStore(&dstRef, out, mmx)

	// PMADDWD (F5)
	case 0xF5:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nw := 8 // i16 lanes per vector
		if mmx {
			nw = 4
		}
		var out [16]byte
		for i := 0; i < nw/2; i++ {
			x0 := int32(int16(leN(a[4*i:])))
			x1 := int32(int16(leN(a[4*i+2:])))
			y0 := int32(int16(leN(b[4*i:])))
			y1 := int32(int16(leN(b[4*i+2:])))
			putLe32(out[4*i:], uint32(x0*y0+x1*y1))
		}
		return d.vecStore(&dstRef, out, mmx)

	// PSADBW (F6)
	case 0xF6:
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		blocks := 2
		if mmx {
			blocks = 1
		}
		var out [16]byte
		for blk := 0; blk < blocks; blk++ {
			var sum uint64
			for i := 0; i < 8; i++ {
				x, y := int64(a[blk*8+i]), int64(b[blk*8+i])
				if x > y {
					sum += uint64(x - y)
				} else {
					sum += uint64(y - x)
				}
			}
			putLe64(out[8*blk:], sum)
		}
		return d.vecStore(&dstRef, out, mmx)

	// PSUBB/PSUBW/PSUBD/PSUBQ, PADDB/PADDW/PADDD
	case 0xF8, 0xF9, 0xFA, 0xFB, 0xFC, 0xFD, 0xFE:
		ew := 1 << (op & 3)
		a, err := d.vecSrc(&dstRef, srcN, mmx)
		if err != nil {
			return err
		}
		b, err := d.vecSrc(&r, srcN, mmx)
		if err != nil {
			return err
		}
		nel := 16 / ew
		if mmx {
			nel = 8 / ew
		}
		out := a
		if op <= 0xFB { // psub
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return x - y })
		} else { // padd
			mapElems2(out[:nel*ew], b[:nel*ew], nel, ew, func(x, y uint64) uint64 { return x + y })
		}
		return d.vecStore(&dstRef, out, mmx)
	}
	return fmt.Errorf("SSE integer opcode 0x0F 0x%02X not implemented at rip=0x%x", op, d.start)
}

// ---- shuffles (0F 70, C6) and immediate shift groups (0F 71-73) ----------

func (d *decoder) sseShuf(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	imm, err := d.fetch8()
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx

	if op == 0xC6 { // SHUFPD (66) / SHUFPS
		if pfx == 0x66 {
			src, err := d.sseSrc(&r, 8)
			if err != nil {
				return err
			}
			a, b := c.XMM[dst], src
			var out [16]byte
			for i := 0; i < 2; i++ {
				if imm&(1<<i) != 0 {
					copy(out[8*i:], b[8*i:])
				} else {
					copy(out[8*i:], a[8*i:])
				}
			}
			c.XMM[dst] = out
			return nil
		}
		src, err := d.sseSrc(&r, 16)
		if err != nil {
			return err
		}
		a, b := c.XMM[dst], src
		var out [16]byte
		for i := 0; i < 4; i++ {
			copy(out[8*i:], a[4*((imm>>(2*i))&3):4*((imm>>(2*i))&3)+4])
			copy(out[8*i+4:], b[4*((imm>>(2*i))&3):4*((imm>>(2*i))&3)+4])
		}
		c.XMM[dst] = out
		return nil
	}

	// 0F 70: PSHUFW (none, mm) / PSHUFD (66) / PSHUFHW (F3) / PSHUFLW (F2)
	switch pfx {
	case 0: // pshufw mm, mm/m64
		src, err := d.vecSrc(&r, 8, true)
		if err != nil {
			return err
		}
		var out [16]byte
		dr := dstRefOf(regRef)
		for i := 0; i < 4; i++ {
			wi := int((imm >> (2 * i)) & 3)
			putLe16(out[2*i:], uint16(leN(src[2*wi:2*wi+2])))
		}
		return d.vecStore(&dr, out, true)
	case 0x66: // pshufd xmm, xmm/m128
		src, err := d.sseSrc(&r, 16)
		if err != nil {
			return err
		}
		var out [16]byte
		for i := 0; i < 4; i++ {
			putLe32(out[4*i:], le32(src[4*((imm>>(2*i))&3):]))
		}
		c.XMM[dst] = out
		return nil
	case 0xF3, 0xF2: // pshufhw / pshuflw
		src, err := d.sseSrc(&r, 16)
		if err != nil {
			return err
		}
		out := src
		lo := 0 // word range to shuffle
		if pfx == 0xF3 {
			lo = 8 // high words
		}
		for i := 0; i < 4; i++ {
			wi := lo + 2*int((imm>>(2*i))&3)
			putLe16(out[lo+2*i:], uint16(leN(src[wi:wi+2])))
		}
		c.XMM[dst] = out
		return nil
	}
	return nil
}

// sseShiftImm handles the 0F 71-73 immediate shift groups (register form only).
func (d *decoder) sseShiftImm(op byte, pfx byte) error {
	modrm, sub, regRef, _, err := d.parseRM(4)
	if err != nil {
		return err
	}
	if modrm>>6 != 3 {
		return fmt.Errorf("SSE shift group 0x0F 0x%02X requires register operand at rip=0x%x", op, d.start)
	}
	imm, err := d.fetch8()
	if err != nil {
		return err
	}
	mmx := pfx != 0x66
	dstRef := dstRefOf(regRef)
	a, err := d.vecSrc(&dstRef, 16, mmx)
	if err != nil {
		return err
	}
	cnt := uint64(imm)
	out := a

	var ew, kind int // kind: 0 logical right, 1 arithmetic right, 2 left, 3/4 byte shifts
	switch op {
	case 0x71:
		switch sub {
		case 2:
			ew, kind = 2, 0
		case 4:
			ew, kind = 2, 1
		case 6:
			ew, kind = 2, 2
		default:
			return fmt.Errorf("bad 0F 71 /%d at rip=0x%x", sub, d.start)
		}
	case 0x72:
		switch sub {
		case 2:
			ew, kind = 4, 0
		case 4:
			ew, kind = 4, 1
		case 6:
			ew, kind = 4, 2
		default:
			return fmt.Errorf("bad 0F 72 /%d at rip=0x%x", sub, d.start)
		}
	default: // 0x73
		switch sub {
		case 2:
			ew, kind = 8, 0
		case 6:
			ew, kind = 8, 2
		case 3:
			kind = 3 // PSRLDQ
		case 7:
			kind = 4 // PSLLDQ
		default:
			return fmt.Errorf("bad 0F 73 /%d at rip=0x%x", sub, d.start)
		}
	}

	if kind >= 3 { // whole-byte shift of the 128-bit lane (66 only)
		if !d.op66 {
			return fmt.Errorf("PSRLDQ/PSLLDQ require 66 prefix at rip=0x%x", d.start)
		}
		if cnt >= 16 {
			if kind == 4 {
				for i := 0; i < 16; i++ {
					out[i] = 0xFF
				}
			} else {
				for i := 0; i < 16; i++ {
					out[i] = 0
				}
			}
		} else if kind == 3 {
			for i := 0; i < 16; i++ {
				if i+int(cnt) < 16 {
					out[i] = a[i+int(cnt)]
				} else {
					out[i] = 0
				}
			}
		} else {
			for i := 0; i < 16; i++ {
				if i >= int(cnt) {
					out[i] = a[i-int(cnt)]
				} else {
					out[i] = 0
				}
			}
		}
		return d.vecStore(&dstRef, out, mmx)
	}

	nel := 16 / ew
	if mmx {
		nel = 8 / ew
	}
	mapElems(out[:nel*ew], nel, ew, func(x uint64) uint64 {
		switch kind {
		case 0:
			if cnt >= uint64(ew*8) {
				return 0
			}
			return x >> cnt
		case 1:
			if cnt >= uint64(ew*8) {
				if sgn(x, ew) < 0 {
					return ^uint64(0)
				}
				return 0
			}
			return uint64(sgn(x, ew) >> cnt)
		default:
			if cnt >= uint64(ew*8) {
				return 0
			}
			return x << cnt
		}
	})
	return d.vecStore(&dstRef, out, mmx)
}

// sseAddSub handles 0F D0: ADDSUBPD (66) / ADDSUBPS (F2) interleaved sub/add.
func (d *decoder) sseAddSub(pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx
	src, err := d.sseSrc(&r, 16)
	if err != nil {
		return err
	}
	a, b := c.XMM[dst], src
	var out [16]byte
	if pfx == 0x66 { // pd: even lane sub, odd lane add (f64)
		for i := 0; i < 2; i++ {
			x := math.Float64frombits(le64(a[8*i:]))
			y := math.Float64frombits(le64(b[8*i:]))
			z := x - y
			if i&1 != 0 {
				z = x + y
			}
			putLe64(out[8*i:], math.Float64bits(z))
		}
	} else { // ps: even lanes sub, odd lanes add (f32)
		for i := 0; i < 4; i++ {
			x := math.Float32frombits(le32(a[4*i:]))
			y := math.Float32frombits(le32(b[4*i:]))
			z := x - y
			if i&1 != 0 {
				z = x + y
			}
			putLe32(out[4*i:], math.Float32bits(z))
		}
	}
	c.XMM[dst] = out
	return nil
}

// sseHaddSub handles 0F 7C/7D: HADD/HSUB ps (none/F2) or pd (66).
func (d *decoder) sseHaddSub(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx
	src, err := d.sseSrc(&r, 16)
	if err != nil {
		return err
	}
	a, b := c.XMM[dst], src
	sub := op == 0x7D
	var out [16]byte
	if pfx == 0x66 { // pd: 2×f64
		for i := 0; i < 2; i++ {
			x := math.Float64frombits(le64(a[8*i:]))
			y := math.Float64frombits(le64(a[8*i+8:]))
			if i == 1 {
				x = math.Float64frombits(le64(b[0:]))
				y = math.Float64frombits(le64(b[8:]))
			}
			z := x + y
			if sub {
				z = x - y
			}
			putLe64(out[8*i:], math.Float64bits(z))
		}
	} else { // ps: 4×f32, pairs from a then b
		for i := 0; i < 4; i++ {
			base := 0
			if i >= 2 {
				base = 8
			}
			srcV := a
			if i >= 2 {
				srcV = b
			}
			x := math.Float32frombits(le32(srcV[base:]))
			y := math.Float32frombits(le32(srcV[base+4:]))
			z := x + y
			if sub {
				z = x - y
			}
			putLe32(out[4*i:], math.Float32bits(z))
		}
	}
	c.XMM[dst] = out
	return nil
}

// sseCmpPacked handles 0F C2: CMPPS/PD/SS/SD with imm8 predicate.
func (d *decoder) sseCmpPacked(pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	imm, err := d.fetch8()
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx
	pred := imm & 7

	srcN := 16
	if pfx == 0xF3 {
		srcN = 4
	} else if pfx == 0xF2 {
		srcN = 8
	}
	src, err := d.sseSrc(&r, srcN)
	if err != nil {
		return err
	}
	a := c.XMM[dst]
	out := a
	test := func(x, y float64) bool {
		unord := math.IsNaN(x) || math.IsNaN(y)
		switch pred {
		case 0:
			return !unord && x == y
		case 1:
			return !unord && x < y
		case 2:
			return !unord && x <= y
		case 3:
			return unord
		case 4:
			return unord || x != y
		case 5:
			return !unord && x >= y
		case 6:
			return !unord && x > y
		default:
			return !unord
		}
	}
	switch pfx {
	case 0x66: // pd: 2×f64
		for i := 0; i < 2; i++ {
			x := math.Float64frombits(le64(a[8*i:]))
			y := math.Float64frombits(le64(src[8*i:]))
			if test(x, y) {
				for j := 0; j < 8; j++ {
					out[8*i+j] = 0xFF
				}
			} else {
				for j := 0; j < 8; j++ {
					out[8*i+j] = 0
				}
			}
		}
	case 0xF3, 0xF2: // scalar: lane0 only, upper preserved
		var x, y float64
		if pfx == 0xF3 {
			x = float64(math.Float32frombits(le32(a[0:4])))
			y = float64(math.Float32frombits(le32(src[0:4])))
			if test(x, y) {
				for j := 0; j < 4; j++ {
					out[j] = 0xFF
				}
			} else {
				for j := 0; j < 4; j++ {
					out[j] = 0
				}
			}
		} else {
			x = math.Float64frombits(le64(a[0:8]))
			y = math.Float64frombits(le64(src[0:8]))
			if test(x, y) {
				for j := 0; j < 8; j++ {
					out[j] = 0xFF
				}
			} else {
				for j := 0; j < 8; j++ {
					out[j] = 0
				}
			}
		}
	default: // ps: 4×f32
		for i := 0; i < 4; i++ {
			x := float64(math.Float32frombits(le32(a[4*i:])))
			y := float64(math.Float32frombits(le32(src[4*i:])))
			mask := byte(0)
			if test(x, y) {
				mask = 0xFF
			}
			for j := 0; j < 4; j++ {
				out[4*i+j] = mask
			}
		}
	}
	c.XMM[dst] = out
	return nil
}

// sseWordInsert handles 0F C4 (PINSRW) and 0F C5 (PEXTRW).
func (d *decoder) sseWordInsert(op byte, pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	imm, err := d.fetch8()
	if err != nil {
		return err
	}
	d.finish(&r)
	mmx := pfx != 0x66

	if op == 0xC5 { // PEXTRW r32, mm/xmm, imm8
		src, err := d.vecSrc(&r, 16, mmx)
		if err != nil {
			return err
		}
		nwords := 8
		if mmx {
			nwords = 4
		}
		idx := int(imm) % nwords
		return d.writeRM(&regRef, 4, uint64(leN(src[2*idx:])))
	}

	// PINSRW mm/xmm, r/m32, imm8 — dest is the reg field
	v, err := d.readRM(&r, 4)
	if err != nil {
		return err
	}
	dstRef := dstRefOf(regRef)
	dst, err := d.vecSrc(&dstRef, 16, mmx)
	if err != nil {
		return err
	}
	nwords := 8
	if mmx {
		nwords = 4
	}
	// ponytail: hardware masks index to 3/4 bits and drops out-of-range writes; we wrap mod nwords
	idx := int(imm) % nwords
	putLe16(dst[2*idx:], uint16(v))
	return d.vecStore(&dstRef, dst, mmx)
}

// sseCvtDQ handles 0F E6: CVT[T]PD2DQ / CVTDQ2PD.
func (d *decoder) sseCvtDQ(pfx byte) error {
	if pfx == 0 {
		pfx = 0x66 // none-prefix form behaves like CVTTPD2DQ
	}
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	c := d.c
	dst := regRef.idx

	switch pfx {
	case 0xF3: // CVTDQ2PD xmm, xmm/m64: 2×i32 -> 2×f64
		src, err := d.sseSrc(&r, 8)
		if err != nil {
			return err
		}
		var out [16]byte
		for i := 0; i < 2; i++ {
			f := float64(int32(le32(src[4*i:])))
			putLe64(out[8*i:], math.Float64bits(f))
		}
		c.XMM[dst] = out
	default: // CVT[T]PD2DQ: 2×f64 -> 2×i32, upper 8 zeroed
		rc := 0
		if pfx == 0x66 {
			rc = 3
		}
		src, err := d.sseSrc(&r, 16)
		if err != nil {
			return err
		}
		var out [16]byte
		for i := 0; i < 2; i++ {
			f := math.Float64frombits(le64(src[8*i:]))
			putLe32(out[4*i:], uint32(f64ToIntW(f, rc, 4)))
		}
		c.XMM[dst] = out
	}
	return nil
}

// sseStoreNT handles 0F E7: MOVNTQ (none) / MOVNTDQ (66) stores.
func (d *decoder) sseStoreNT(pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	if r.isReg {
		return fmt.Errorf("MOVNT requires memory operand at rip=0x%x", d.start)
	}
	if pfx == 0x66 {
		return d.writeBytes(r.addr, d.c.XMM[regRef.idx][0:16])
	}
	dr := dstRefOf(regRef)
	v, err := d.vecSrc(&dr, 8, true)
	if err != nil {
		return err
	}
	return d.writeBytes(r.addr, v[0:8])
}

// sseLddqu handles 0F F0: LDDQU xmm, m128.
func (d *decoder) sseLddqu() error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	if r.isReg {
		return fmt.Errorf("LDDQU requires memory operand at rip=0x%x", d.start)
	}
	b, err := d.readBytes(r.addr, 16)
	if err != nil {
		return err
	}
	copy(d.c.XMM[regRef.idx][0:16], b)
	return nil
}

// sseMaskStore handles 0F F7: MASKMOVQ / MASKMOVDQU via [RDI].
func (d *decoder) sseMaskStore(pfx byte) error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	mmx := pfx != 0x66
	nel := 16
	if mmx {
		nel = 8
	}
	dr := dstRefOf(regRef)
	data, err := d.vecSrc(&dr, nel, mmx)
	if err != nil {
		return err
	}
	mask, err := d.vecSrc(&r, nel, mmx)
	if err != nil {
		return err
	}
	cur, err := d.readBytes(d.c.Regs[RDI], nel)
	if err != nil {
		return err
	}
	for i := 0; i < nel; i++ {
		if mask[i]&0x80 != 0 {
			cur[i] = data[i]
		}
	}
	return d.writeBytes(d.c.Regs[RDI], cur)
}

// ---- SSSE3 (0F 38 00 pshufb, 0F 3A 0F palignr) ----------------------------

// execSSE3 decodes three-byte 0F 38/3A map opcodes.
func (d *decoder) execSSE3(mapOp byte) error {
	sub, err := d.fetch8()
	if err != nil {
		return err
	}
	switch {
	case mapOp == 0x38 && sub == 0x00:
		return d.ssePshufb()
	case mapOp == 0x3A && sub == 0x0F:
		return d.ssePalignr()
	}
	return fmt.Errorf("unsupported 3-byte opcode: 0x0F 0x%02X 0x%02X at rip=0x%x", mapOp, sub, d.start)
}

func (d *decoder) ssePshufb() error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	d.finish(&r)
	mmx := !d.op66
	nel := 16
	if mmx {
		nel = 8
	}
	src, err := d.vecSrc(&r, nel, mmx)
	if err != nil {
		return err
	}
	dstRef := dstRefOf(regRef)
	dst, err := d.vecSrc(&dstRef, nel, mmx)
	if err != nil {
		return err
	}
	var out [16]byte
	for i := 0; i < nel; i++ {
		s := src[i]
		if s&0x80 != 0 {
			continue // out[i] stays 0
		}
		out[i] = dst[int(s)&(nel-1)]
	}
	return d.vecStore(&dstRef, out, mmx)
}

func (d *decoder) ssePalignr() error {
	_, _, regRef, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	imm, err := d.fetch8()
	if err != nil {
		return err
	}
	d.finish(&r)
	mmx := !d.op66
	n := 16
	if mmx {
		n = 8
	}
	src, err := d.vecSrc(&r, n, mmx)
	if err != nil {
		return err
	}
	dstRef := dstRefOf(regRef)
	dst, err := d.vecSrc(&dstRef, n, mmx)
	if err != nil {
		return err
	}
	var out [16]byte
	if int(imm) >= 2*n {
		return d.vecStore(&dstRef, out, mmx) // shifted entirely out
	}
	tmp := make([]byte, 2*n)
	copy(tmp[0:n], src[0:n])
	copy(tmp[n:2*n], dst[0:n])
	copy(out[:], tmp[int(imm):int(imm)+n])
	return d.vecStore(&dstRef, out, mmx)
}
