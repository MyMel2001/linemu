package cpu

// x87 D8-DF decoder. FPUStack slots hold float64 bits; MMX regs alias ST(i).
// Precision ceiling: 80-bit values round-trip through float64 (accepted).
// Skipped (explicit errors): FLDENV/FNSTENV/FRSTOR/FNSAVE, FBLD/FBSTP.

import (
	"encoding/binary"
	"fmt"
	"math"
)

// FPUStatus bits.
const (
	fpuC0 = 1 << 8
	fpuC1 = 1 << 9
	fpuC2 = 1 << 10
	fpuC3 = 1 << 14
)

func (c *CPUState) stF(i int) float64 {
	return math.Float64frombits(c.FPUStack[(c.FPUTop+i)&7])
}

func (c *CPUState) setST(i int, v float64) {
	c.FPUStack[(c.FPUTop+i)&7] = math.Float64bits(v)
}

func (c *CPUState) fpPush(v float64) {
	c.FPUTop = (c.FPUTop - 1) & 7
	c.FPUStack[c.FPUTop] = math.Float64bits(v)
}

func (c *CPUState) fpPop() float64 {
	v := math.Float64frombits(c.FPUStack[c.FPUTop])
	c.FPUTop = (c.FPUTop + 1) & 7
	return v
}

// setFCC sets C0/C1/C2/C3 from a compare result (C1 cleared).
func (c *CPUState) setFCC(c0, c2, c3 bool) {
	st := c.FPUStatus &^ (fpuC0 | fpuC1 | fpuC2 | fpuC3)
	if c0 {
		st |= fpuC0
	}
	if c2 {
		st |= fpuC2
	}
	if c3 {
		st |= fpuC3
	}
	c.FPUStatus = st
}

// fcomSet sets status condition codes for a vs b.
func (c *CPUState) fcomSet(a, b float64) {
	switch {
	case math.IsNaN(a) || math.IsNaN(b):
		c.setFCC(true, true, true) // unordered: C0+C2+C3
	case a < b:
		c.setFCC(true, false, false)
	case a == b:
		c.setFCC(false, false, true)
	default: // greater
		c.setFCC(false, false, false)
	}
}

// fcomiSet sets EFLAGS directly (FCOMI/FUCOMI/UCOMISx family).
func (c *CPUState) fcomiSet(a, b float64) {
	c.RFLAGS &^= FLAG_CF | FLAG_ZF | FLAG_PF | FLAG_OF | FLAG_SF | FLAG_AF
	switch {
	case math.IsNaN(a) || math.IsNaN(b):
		c.RFLAGS |= FLAG_CF | FLAG_ZF | FLAG_PF
	case a < b:
		c.RFLAGS |= FLAG_CF
	case a == b:
		c.RFLAGS |= FLAG_ZF
	}
}

// fpRc returns the rounding control from FPUControl bits 11:10.
func (c *CPUState) fpRc() int { return int((c.FPUControl >> 10) & 3) }

func fpRound(v float64, rc int) float64 {
	switch rc {
	case 1:
		return math.Floor(v)
	case 2:
		return math.Ceil(v)
	case 3:
		return math.Trunc(v)
	}
	return math.RoundToEven(v)
}

// f64ToIntW converts with rounding mode; NaN/out-of-range yields the
// indefinite integer for width w.
func f64ToIntW(v float64, rc int, w int) uint64 {
	indef := uint64(1) << (uint(w)*8 - 1)
	if math.IsNaN(v) {
		return indef
	}
	x := fpRound(v, rc)
	lo, hi := -math.Ldexp(1, int(w*8-1)), math.Ldexp(1, int(w*8-1))
	if x < lo || x >= hi {
		return indef
	}
	return uint64(int64(x)) & maskW(^uint64(0), w)
}

// extToFloat64 decodes an 80-bit extended value (10 bytes LE).
func extToFloat64(b []byte) float64 {
	mant := binary.LittleEndian.Uint64(b[0:8])
	se := binary.LittleEndian.Uint16(b[8:10])
	sign := math.Copysign(1, float64(int16(se)>>15))
	exp := int(se & 0x7FFF)
	switch {
	case exp == 0x7FFF:
		if mant&(1<<63) == 0 || mant&^(uint64(1)<<63) != 0 {
			return math.NaN()
		}
		return math.Inf(int(sign))
	case exp == 0:
		if mant == 0 {
			return sign * 0
		}
		return math.Ldexp(float64(mant), -16383-63)
	default:
		return sign * math.Ldexp(float64(mant), exp-16383-63)
	}
}

// float64ToExt encodes to 80-bit extended (10 bytes LE).
func float64ToExt(v float64) [10]byte {
	var out [10]byte
	if math.IsNaN(v) {
		out[7] = 0xC0 // integer bit set, nonzero fraction -> NaN
		out[9] = 0x7F
		return out
	}
	if math.IsInf(v, 0) {
		out[7] = 0x80
		out[8] = 0xFF
		out[9] = 0x7F
		if math.Signbit(v) {
			out[9] = 0xFF
		}
		return out
	}
	if v == 0 {
		if math.Signbit(v) {
			out[9] = 0x80
		}
		return out
	}
	sign := uint16(0)
	if math.Signbit(v) {
		sign = 0x8000
		v = -v
	}
	f, exp := math.Frexp(v) // |f| in [0.5,1)
	mant := uint64(f * math.Ldexp(1, 64))
	e := exp - 1 + 16383
	if e <= 0 { // below float64 normal range: flush to zero
		return out
	}
	binary.LittleEndian.PutUint64(out[0:8], mant)
	binary.LittleEndian.PutUint16(out[8:10], sign|uint16(e))
	return out
}

func intToFloatW(b []byte, w int) float64 {
	switch w {
	case 2:
		return float64(int16(binary.LittleEndian.Uint16(b)))
	case 4:
		return float64(int32(binary.LittleEndian.Uint32(b)))
	default:
		return float64(int64(binary.LittleEndian.Uint64(b)))
	}
}

// execX87 dispatches opcode 0xD8-0xDF.
func (d *decoder) execX87(op byte) error {
	modrm, sub, _, r, err := d.parseRM(4)
	if err != nil {
		return err
	}
	if modrm>>6 == 3 {
		return d.x87Reg(op, sub, int(modrm&7))
	}
	d.finish(&r)
	return d.x87Mem(op, sub, &r)
}

func (d *decoder) x87Reg(op byte, sub, st int) error {
	c := d.c
	switch op {
	case 0xD8: // fadd/fmul/fcom/fcomp/fsub/fsubr/fdiv/fdivr ST(0), ST(i)
		a, b := c.stF(0), c.stF(st)
		switch sub {
		case 0:
			c.setST(0, a+b)
		case 1:
			c.setST(0, a*b)
		case 2:
			c.fcomSet(a, b)
		case 3:
			c.fcomSet(a, b)
			c.fpPop()
		case 4:
			c.setST(0, a-b)
		case 5:
			c.setST(0, b-a)
		case 6:
			c.setST(0, a/b)
		case 7:
			c.setST(0, b/a)
		}
	case 0xDC: // fadd/fmul/fsubr/fsub/fdivr/fdiv ST(i), ST(0)
		a, b := c.stF(st), c.stF(0)
		switch sub {
		case 0:
			c.setST(st, a+b)
		case 1:
			c.setST(st, a*b)
		case 4: // fsubr: st = st0 - st
			c.setST(st, b-a)
		case 5: // fsub: st -= st0
			c.setST(st, a-b)
		case 6: // fdivr: st = st0 / st
			c.setST(st, b/a)
		case 7: // fdiv: st /= st0
			c.setST(st, a/b)
		default:
			return fmt.Errorf("invalid x87 opcode 0xDC 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	case 0xDE: // pop forms: faddp/fmulp/fsubrp/fsubp/fdivrp/fdivp ST(i), ST(0)
		a, b := c.stF(st), c.stF(0)
		switch sub {
		case 0:
			c.setST(st, a+b)
		case 1:
			c.setST(st, a*b)
		case 2:
			if st != 1 {
				return fmt.Errorf("invalid x87 fcompp at rip=0x%x", d.start)
			}
			c.fcomSet(c.stF(0), c.stF(1))
			c.fpPop()
			c.fpPop()
			return nil
		case 4: // fsubrp: st = st0 - st
			c.setST(st, b-a)
		case 5: // fsubp: st -= st0
			c.setST(st, a-b)
		case 6: // fdivrp: st = st0 / st
			c.setST(st, b/a)
		case 7: // fdivp: st /= st0
			c.setST(st, a/b)
		default:
			return fmt.Errorf("invalid x87 opcode 0xDE 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
		c.fpPop()
	case 0xD9:
		switch sub {
		case 0: // fld ST(i)
			c.fpPush(c.stF(st))
		case 1: // fxch ST(i)
			c.setST(0, c.stF(st))
			c.setST(st, c.stF(0))
		case 2: // fnop
		case 4:
			switch st {
			case 0: // fchs
				c.setST(0, math.Copysign(-1, c.stF(0))*c.stF(0))
			case 1: // fabs
				c.setST(0, math.Abs(c.stF(0)))
			case 4: // ftst
				c.fcomSet(c.stF(0), 0)
			case 5: // fxam
				x := c.stF(0)
				sign, c2, c3 := false, false, false
				if math.IsNaN(x) {
					c2, c3 = false, true
				} else if math.IsInf(x, 0) {
					c2, c3 = true, true
				} else if x == 0 {
					c2 = true
				}
				sign = math.Signbit(x)
				c.setFCC(sign, c2, c3)
			default:
				return fmt.Errorf("invalid x87 opcode 0xD9 0x%02X at rip=0x%x", 0xE0|st, d.start)
			}
		case 5: // E8+i: load constants
			switch st {
			case 0: // fld1
				c.fpPush(1)
			case 1: // fldl2t
				c.fpPush(3.321928094887362)
			case 2: // fldl2e
				c.fpPush(1.4426950408889634)
			case 3: // fldpi
				c.fpPush(math.Pi)
			case 4: // fldlg2
				c.fpPush(math.Log10(2))
			case 5: // fldln2
				c.fpPush(math.Log(2))
			case 6: // fldz
				c.fpPush(0)
			default:
				return fmt.Errorf("invalid x87 opcode 0xD9 0x%02X at rip=0x%x", 0xE8|st, d.start)
			}
		case 6: // F0+i
			switch st {
			case 0: // f2xm1
				c.setST(0, math.Pow(2, c.stF(0))-1)
			case 1: // fyl2x: st1 = st1 * log2(st0); pop
				v := c.stF(1) * math.Log2(c.stF(0))
				c.fpPop()
				c.setST(0, v)
			case 2: // fptan (D9 F2): st0 = tan; push 1
				c.setST(0, math.Tan(c.stF(0)))
				c.fpPush(1)
			case 4: // fxtract: ST1 = exponent, ST0 = significand
				// ponytail: Inf/NaN/0 edge cases follow math.Frexp; upgrade for IEEE parity
				x := c.stF(0)
				sig, exp := math.Frexp(x)
				c.setST(1, float64(exp))
				c.setST(0, sig)
			case 5: // fprem1 (IEEE)
				a, b := c.stF(0), c.stF(1)
				c.setST(0, a-math.RoundToEven(a/b)*b)
			case 6: // fdecstp
				c.FPUTop = (c.FPUTop - 1) & 7
			case 7: // fincstp
				c.FPUTop = (c.FPUTop + 1) & 7
			case 3: // fpatan (D9 F3): st1 = arctan(st1/st0); pop
				v := math.Atan2(c.stF(1), c.stF(0))
				c.fpPop()
				c.setST(0, v)
			default:
				return fmt.Errorf("invalid x87 opcode 0xD9 0x%02X at rip=0x%x", 0xF0|st, d.start)
			}
		case 7: // F8+i
			switch st {
			case 0: // fprem (truncating)
				c.setST(0, math.Mod(c.stF(0), c.stF(1)))
			case 1: // fyl2xp1
				v := c.stF(1) * math.Log2(c.stF(0)+1)
				c.fpPop()
				c.setST(0, v)
			case 2: // fsqrt
				c.setST(0, math.Sqrt(c.stF(0)))
			case 3: // fsincos: ST1 = sin, ST0 = cos
				x := c.stF(0)
				c.fpPush(math.Sin(x))
				c.setST(0, math.Cos(x))
			case 4: // frndint
				c.setST(0, fpRound(c.stF(0), c.fpRc()))
			case 5: // fscale
				c.setST(0, c.stF(0)*math.Ldexp(1, int(math.Trunc(c.stF(1)))))
			case 6: // fsin
				c.setST(0, math.Sin(c.stF(0)))
			case 7: // fcos
				c.setST(0, math.Cos(c.stF(0)))
			default:
				return fmt.Errorf("invalid x87 opcode 0xD9 0x%02X at rip=0x%x", 0xF8|st, d.start)
			}
		default:
			return fmt.Errorf("invalid x87 opcode 0xD9 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	case 0xDA: // fcmovcc + fucompp
		switch sub {
		case 0: // fcmovb
			if c.GetCF() {
				c.setST(0, c.stF(st))
			}
		case 1: // fcmove
			if c.GetZF() {
				c.setST(0, c.stF(st))
			}
		case 2: // fcmovbe
			if c.GetCF() || c.GetZF() {
				c.setST(0, c.stF(st))
			}
		case 3: // fcmovu
			if c.RFLAGS&FLAG_PF != 0 {
				c.setST(0, c.stF(st))
			}
		case 5:
			if st != 1 {
				return fmt.Errorf("invalid x87 fucompp at rip=0x%x", d.start)
			}
			c.fcomSet(c.stF(0), c.stF(1))
			c.fpPop()
			c.fpPop()
		default:
			return fmt.Errorf("invalid x87 opcode 0xDA 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	case 0xDB:
		switch sub {
		case 0: // fcmovnb
			if !c.GetCF() {
				c.setST(0, c.stF(st))
			}
		case 1: // fcmovne
			if !c.GetZF() {
				c.setST(0, c.stF(st))
			}
		case 2: // fcmovnbe
			if !c.GetCF() && !c.GetZF() {
				c.setST(0, c.stF(st))
			}
		case 3: // fcmovnu
			if c.RFLAGS&FLAG_PF == 0 {
				c.setST(0, c.stF(st))
			}
		case 4:
			switch st {
			case 2: // fclex
				c.FPUStatus = 0
			case 3: // fninit
				c.FPUTop = 0
				c.FPUStatus = 0
				c.FPUControl = 0x037F
			}
		case 5: // fucomi
			c.fcomiSet(c.stF(0), c.stF(st))
		case 6: // fcomi
			c.fcomiSet(c.stF(0), c.stF(st))
		default:
			return fmt.Errorf("invalid x87 opcode 0xDB 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	case 0xDD:
		switch sub {
		case 0: // ffree: no tag tracking
		case 2: // fst ST(i)
			c.setST(st, c.stF(0))
		case 3: // fstp ST(i)
			c.setST(st, c.stF(0))
			c.fpPop()
		case 4: // fucom
			c.fcomSet(c.stF(0), c.stF(st))
		case 5: // fucomp
			c.fcomSet(c.stF(0), c.stF(st))
			c.fpPop()
		default:
			return fmt.Errorf("invalid x87 opcode 0xDD 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	case 0xDF:
		switch sub {
		case 0: // ffreep
			c.fpPop()
		case 4:
			if st != 0 {
				return fmt.Errorf("invalid x87 opcode 0xDF 0x%02X at rip=0x%x", 0xE0|st, d.start)
			}
			axRef := rmRef{isReg: true, idx: RAX}
			if err := d.writeRM(&axRef, 2, uint64(c.FPUStatus)); err != nil {
				return err
			}
		case 5: // fucomip
			c.fcomiSet(c.stF(0), c.stF(st))
			c.fpPop()
		case 6: // fcomip
			c.fcomiSet(c.stF(0), c.stF(st))
			c.fpPop()
		default:
			return fmt.Errorf("invalid x87 opcode 0xDF 0x%02X at rip=0x%x", 0xC0|sub<<3|st, d.start)
		}
	}
	return nil
}

// f64ToIntF rounds per control word (FIST/FISTP family).
func (c *CPUState) f64ToIntF(v float64, w int) uint64 {
	return f64ToIntW(v, c.fpRc(), w)
}

// f64ToIntTrunc always truncates (FISTTP).
func f64ToIntTrunc(v float64, w int) uint64 {
	return f64ToIntW(v, 3, w)
}

func (d *decoder) x87Mem(op byte, sub int, r *rmRef) error {
	c := d.c
	readF32 := func() (float64, error) {
		b, err := d.readBytes(r.addr, 4)
		if err != nil {
			return 0, err
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
	}
	readF64 := func() (float64, error) {
		b, err := d.readBytes(r.addr, 8)
		if err != nil {
			return 0, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	}
	readI16 := func() (float64, error) {
		b, err := d.readBytes(r.addr, 2)
		if err != nil {
			return 0, err
		}
		return intToFloatW(b, 2), nil
	}
	readI32 := func() (float64, error) {
		b, err := d.readBytes(r.addr, 4)
		if err != nil {
			return 0, err
		}
		return intToFloatW(b, 4), nil
	}
	readI64 := func() (float64, error) {
		b, err := d.readBytes(r.addr, 8)
		if err != nil {
			return 0, err
		}
		return intToFloatW(b, 8), nil
	}
	binOp := func(get func() (float64, error)) error {
		m, err := get()
		if err != nil {
			return err
		}
		a := c.stF(0)
		switch sub {
		case 0:
			c.setST(0, a+m)
		case 1:
			c.setST(0, a*m)
		case 2:
			c.fcomSet(a, m)
		case 3:
			c.fcomSet(a, m)
			c.fpPop()
		case 4:
			c.setST(0, a-m)
		case 5:
			c.setST(0, m-a)
		case 6:
			c.setST(0, a/m)
		case 7:
			c.setST(0, m/a)
		}
		return nil
	}
	writeF32 := func(v float64) error {
		return d.writeBytes(r.addr, func() []byte {
			b := make([]byte, 4)
			binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v)))
			return b
		}())
	}
	writeF64 := func(v float64) error {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, math.Float64bits(v))
		return d.writeBytes(r.addr, b)
	}
	writeF80 := func(v float64) error {
		e := float64ToExt(v)
		return d.writeBytes(r.addr, e[:])
	}
	writeIntW := func(v uint64, w int) error {
		b := make([]byte, w)
		for i := 0; i < w; i++ {
			b[i] = byte(v >> (8 * i))
		}
		return d.writeBytes(r.addr, b)
	}

	switch op {
	case 0xD8: // f32 arith on ST(0)
		return binOp(readF32)
	case 0xDC: // f64 arith on ST(0)
		return binOp(readF64)
	case 0xDA: // int32 arith on ST(0)
		return binOp(readI32)
	case 0xDE: // int16 arith on ST(0)
		return binOp(readI16)
	case 0xD9:
		switch sub {
		case 0: // fld m32
			v, err := readF32()
			if err != nil {
				return err
			}
			c.fpPush(v)
		case 2: // fst m32
			v, err := readF32()
			if err != nil {
				return err
			}
			_ = v
			return writeF32(c.stF(0))
		case 3: // fstp m32
			return writeF32(c.fpPop())
		case 4:
			return fmt.Errorf("x87 FLDENV not implemented at rip=0x%x", d.start)
		case 5: // fldcw
			b, err := d.readBytes(r.addr, 2)
			if err != nil {
				return err
			}
			c.FPUControl = binary.LittleEndian.Uint16(b)
		case 6:
			return fmt.Errorf("x87 FNSTENV not implemented at rip=0x%x", d.start)
		case 7: // fnstcw
			b := make([]byte, 2)
			binary.LittleEndian.PutUint16(b, c.FPUControl)
			return d.writeBytes(r.addr, b)
		}
	case 0xDB:
		switch sub {
		case 0: // fild m32
			v, err := readI32()
			if err != nil {
				return err
			}
			c.fpPush(v)
		case 1: // fisttp m32
			b, err := d.readBytes(r.addr, 4)
			if err != nil {
				return err
			}
			_ = b
			return writeIntW(f64ToIntTrunc(c.stF(0), 4), 4)
		case 2: // fist m32
			return writeIntW(c.f64ToIntF(c.stF(0), 4), 4)
		case 3: // fistp m32
			return writeIntW(c.f64ToIntF(c.fpPop(), 4), 4)
		case 5: // fld m80
			b, err := d.readBytes(r.addr, 10)
			if err != nil {
				return err
			}
			c.fpPush(extToFloat64(b))
		case 7: // fstp m80
			return writeF80(c.fpPop())
		}
	case 0xDD:
		switch sub {
		case 0: // fld m64
			v, err := readF64()
			if err != nil {
				return err
			}
			c.fpPush(v)
		case 1: // fisttp m64
			return writeIntW(f64ToIntTrunc(c.stF(0), 8), 8)
		case 2: // fst m64
			return writeF64(c.stF(0))
		case 3: // fstp m64
			return writeF64(c.fpPop())
		case 4:
			return fmt.Errorf("x87 FRSTOR not implemented at rip=0x%x", d.start)
		case 6:
			return fmt.Errorf("x87 FNSAVE not implemented at rip=0x%x", d.start)
		case 7: // fnstsw m16
			b := make([]byte, 2)
			binary.LittleEndian.PutUint16(b, c.FPUStatus)
			return d.writeBytes(r.addr, b)
		}
	case 0xDF:
		switch sub {
		case 0: // fild m16
			v, err := readI16()
			if err != nil {
				return err
			}
			c.fpPush(v)
		case 1: // fisttp m16
			return writeIntW(f64ToIntTrunc(c.stF(0), 2), 2)
		case 2: // fist m16
			return writeIntW(c.f64ToIntF(c.stF(0), 2), 2)
		case 3: // fistp m16
			return writeIntW(c.f64ToIntF(c.fpPop(), 2), 2)
		case 4:
			return fmt.Errorf("x87 FBLD not implemented at rip=0x%x", d.start)
		case 5: // fild m64
			v, err := readI64()
			if err != nil {
				return err
			}
			c.fpPush(v)
		case 6:
			return fmt.Errorf("x87 FBSTP not implemented at rip=0x%x", d.start)
		case 7: // fistp m64
			return writeIntW(c.f64ToIntF(c.fpPop(), 8), 8)
		}
	}
	return fmt.Errorf("invalid x87 opcode 0x%02X /%d at rip=0x%x", op, sub, d.start)
}
