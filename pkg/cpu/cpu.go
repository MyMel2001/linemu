package cpu

import (
	"fmt"
)

// Register indices
const (
	RAX = 0
	RCX = 1
	RDX = 2
	RBX = 3
	RSP = 4
	RBP = 5
	RSI = 6
	RDI = 7
	R8  = 8
	R9  = 9
	R10 = 10
	R11 = 11
	R12 = 12
	R13 = 13
	R14 = 14
	R15 = 15
)

// RFLAGS bits
const (
	FLAG_CF = 1 << 0
	FLAG_PF = 1 << 2
	FLAG_AF = 1 << 4
	FLAG_ZF = 1 << 6
	FLAG_SF = 1 << 7
	FLAG_TF = 1 << 8
	FLAG_IF = 1 << 9
	FLAG_DF = 1 << 10
	FLAG_OF = 1 << 11
)

type CPUState struct {
	Regs   [16]uint64
	RIP    uint64
	RFLAGS uint64
	CR0    uint64
	CR2    uint64
	CR3    uint64
	CR4    uint64
	Halted bool
	XMM    [16][16]byte
	// GDTR/IDTR descriptor tables (stored for SGDT/SIDT round-trip)
	GDTRLimit, GDTRBase uint64
	IDTRLimit, IDTRBase uint64

	// x87/MMX state: MMX reg i aliases ST(i) = FPUStack[(FPUTop+i)&7]
	FPUStack   [8]uint64
	FPUTop     int
	FPUStatus  uint16 // FNSTSW: C0=bit8 C1=bit9 C2=bit10 C3=bit14
	FPUControl uint16 // FNSTCW/FLDCW
	RNGState   uint64 // RDRAND/RDSEED LCG state
}

func NewCPUState() *CPUState {
	return &CPUState{
		RFLAGS:     0x2,
		FPUControl: 0x037F,
	}
}

func (c *CPUState) SetZF(v bool) {
	if v {
		c.RFLAGS |= FLAG_ZF
	} else {
		c.RFLAGS &= ^uint64(FLAG_ZF)
	}
}

func (c *CPUState) GetZF() bool {
	return (c.RFLAGS & FLAG_ZF) != 0
}

func (c *CPUState) SetSF(v bool) {
	if v {
		c.RFLAGS |= FLAG_SF
	} else {
		c.RFLAGS &= ^uint64(FLAG_SF)
	}
}

func (c *CPUState) GetSF() bool {
	return (c.RFLAGS & FLAG_SF) != 0
}

func (c *CPUState) SetCF(v bool) {
	if v {
		c.RFLAGS |= FLAG_CF
	} else {
		c.RFLAGS &= ^uint64(FLAG_CF)
	}
}

func (c *CPUState) GetCF() bool {
	return (c.RFLAGS & FLAG_CF) != 0
}

type Memory interface {
	Read8(addr uint64) (uint8, error)
	Read16(addr uint64) (uint16, error)
	Read32(addr uint64) (uint32, error)
	Read64(addr uint64) (uint64, error)
	Write8(addr uint64, val uint8) error
	Write16(addr uint64, val uint16) error
	Write32(addr uint64, val uint32) error
	Write64(addr uint64, val uint64) error
}

type IOBus interface {
	In(port uint16, size int) (uint32, error)
	Out(port uint16, val uint32, size int) error
}

var ErrSyscallTrapped = fmt.Errorf("syscall trap")
