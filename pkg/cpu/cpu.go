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
	CR3    uint64
	CR4    uint64
	Halted bool
}

func NewCPUState() *CPUState {
	return &CPUState{
		RFLAGS: 0x2,
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

func (c *CPUState) UpdateFlags(a, b, res uint64, sub bool) {
	c.SetZF(res == 0)
	c.SetSF((res & 0x8000000000000000) != 0)
	if sub {
		c.SetCF(a < b)
	} else {
		c.SetCF(res < a)
	}
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

func (c *CPUState) Step(mem Memory, io IOBus) error {
	if c.Halted {
		return nil
	}

	b, err := mem.Read8(c.RIP)
	if err != nil {
		return fmt.Errorf("fetch failed at rip=0x%x: %w", c.RIP, err)
	}

	rexW := false
	rexR := false
	rexX := false
	rexB := false

	if b >= 0x40 && b <= 0x4f {
		rexW = (b & 0x08) != 0
		rexR = (b & 0x04) != 0
		rexX = (b & 0x02) != 0
		rexB = (b & 0x01) != 0
		_ = rexX
		c.RIP++
		b, err = mem.Read8(c.RIP)
		if err != nil {
			return err
		}
	}

	switch b {
	case 0x90: // NOP
		c.RIP++
		return nil

	case 0xF4: // HLT
		c.Halted = true
		c.RIP++
		return nil

	case 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF: // MOV reg, imm
		regIdx := int(b - 0xB8)
		if rexB {
			regIdx += 8
		}
		c.RIP++
		if rexW {
			imm, err := mem.Read64(c.RIP)
			if err != nil {
				return err
			}
			c.Regs[regIdx] = imm
			c.RIP += 8
		} else {
			low, err := mem.Read32(c.RIP)
			if err != nil {
				return err
			}
			c.Regs[regIdx] = uint64(low)
			c.RIP += 4
		}
		return nil

	case 0x01: // ADD r/m, r
		c.RIP++
		modrm, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		mod := (modrm >> 6) & 3
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if rexR {
			reg += 8
		}
		if rexB {
			rm += 8
		}

		if mod == 3 {
			res := c.Regs[rm] + c.Regs[reg]
			c.UpdateFlags(c.Regs[rm], c.Regs[reg], res, false)
			if !rexW {
				res &= 0xFFFFFFFF
			}
			c.Regs[rm] = res
			return nil
		}
		addr := c.Regs[rm]
		val, err := mem.Read64(addr)
		if err != nil {
			return err
		}
		res := val + c.Regs[reg]
		c.UpdateFlags(val, c.Regs[reg], res, false)
		return mem.Write64(addr, res)

	case 0x29: // SUB r/m, r
		c.RIP++
		modrm, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		mod := (modrm >> 6) & 3
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if rexR {
			reg += 8
		}
		if rexB {
			rm += 8
		}

		if mod == 3 {
			res := c.Regs[rm] - c.Regs[reg]
			c.UpdateFlags(c.Regs[rm], c.Regs[reg], res, true)
			if !rexW {
				res &= 0xFFFFFFFF
			}
			c.Regs[rm] = res
			return nil
		}
		addr := c.Regs[rm]
		val, err := mem.Read64(addr)
		if err != nil {
			return err
		}
		res := val - c.Regs[reg]
		c.UpdateFlags(val, c.Regs[reg], res, true)
		return mem.Write64(addr, res)

	case 0x31: // XOR r/m, r
		c.RIP++
		modrm, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		mod := (modrm >> 6) & 3
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if rexR {
			reg += 8
		}
		if rexB {
			rm += 8
		}

		if mod == 3 {
			res := c.Regs[rm] ^ c.Regs[reg]
			if !rexW {
				res &= 0xFFFFFFFF
			}
			c.Regs[rm] = res
			c.SetZF(res == 0)
			c.SetSF((res & 0x8000000000000000) != 0)
			return nil
		}
		addr := c.Regs[rm]
		val, err := mem.Read64(addr)
		if err != nil {
			return err
		}
		res := val ^ c.Regs[reg]
		c.SetZF(res == 0)
		return mem.Write64(addr, res)

	case 0x89: // MOV r/m, r
		c.RIP++
		modrm, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		mod := (modrm >> 6) & 3
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if rexR {
			reg += 8
		}
		if rexB {
			rm += 8
		}

		if mod == 3 {
			if rexW {
				c.Regs[rm] = c.Regs[reg]
			} else {
				c.Regs[rm] = c.Regs[reg] & 0xFFFFFFFF
			}
			return nil
		}
		addr := c.Regs[rm]
		if rexW {
			return mem.Write64(addr, c.Regs[reg])
		}
		return mem.Write32(addr, uint32(c.Regs[reg]))

	case 0x8B: // MOV r, r/m
		c.RIP++
		modrm, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		mod := (modrm >> 6) & 3
		reg := int((modrm >> 3) & 7)
		rm := int(modrm & 7)
		if rexR {
			reg += 8
		}
		if rexB {
			rm += 8
		}

		if mod == 3 {
			if rexW {
				c.Regs[reg] = c.Regs[rm]
			} else {
				c.Regs[reg] = c.Regs[rm] & 0xFFFFFFFF
			}
			return nil
		}
		addr := c.Regs[rm]
		if rexW {
			val, err := mem.Read64(addr)
			if err != nil {
				return err
			}
			c.Regs[reg] = val
		} else {
			val, err := mem.Read32(addr)
			if err != nil {
				return err
			}
			c.Regs[reg] = uint64(val)
		}
		return nil

	case 0xEB: // JMP rel8
		c.RIP++
		rel, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		c.RIP = uint64(int64(c.RIP) + int64(int8(rel)))
		return nil

	case 0x74: // JZ/JE rel8
		c.RIP++
		rel, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		if c.GetZF() {
			c.RIP = uint64(int64(c.RIP) + int64(int8(rel)))
		}
		return nil

	case 0x75: // JNZ/JNE rel8
		c.RIP++
		rel, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		if !c.GetZF() {
			c.RIP = uint64(int64(c.RIP) + int64(int8(rel)))
		}
		return nil

	case 0xE4, 0xE5: // IN AL/eAX, imm8
		c.RIP++
		port, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		size := 1
		if b == 0xE5 {
			size = 4
		}
		val, err := io.In(uint16(port), size)
		if err != nil {
			return err
		}
		if size == 1 {
			c.Regs[RAX] = (c.Regs[RAX] & ^uint64(0xFF)) | uint64(val&0xFF)
		} else {
			c.Regs[RAX] = uint64(val)
		}
		return nil

	case 0xE6, 0xE7: // OUT imm8, AL/eAX
		c.RIP++
		port, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		size := 1
		if b == 0xE7 {
			size = 4
		}
		return io.Out(uint16(port), uint32(c.Regs[RAX]), size)

	case 0xEC: // IN AL, DX
		c.RIP++
		port := uint16(c.Regs[RDX] & 0xFFFF)
		val, err := io.In(port, 1)
		if err != nil {
			return err
		}
		c.Regs[RAX] = (c.Regs[RAX] & ^uint64(0xFF)) | uint64(val&0xFF)
		return nil

	case 0xED: // IN EAX, DX
		c.RIP++
		port := uint16(c.Regs[RDX] & 0xFFFF)
		val, err := io.In(port, 4)
		if err != nil {
			return err
		}
		c.Regs[RAX] = uint64(val)
		return nil

	case 0xEE: // OUT DX, AL
		c.RIP++
		port := uint16(c.Regs[RDX] & 0xFFFF)
		return io.Out(port, uint32(c.Regs[RAX]&0xFF), 1)

	case 0xEF: // OUT DX, EAX
		c.RIP++
		port := uint16(c.Regs[RDX] & 0xFFFF)
		return io.Out(port, uint32(c.Regs[RAX]), 4)

	case 0x0F: // Two-byte opcodes
		c.RIP++
		b2, err := mem.Read8(c.RIP)
		if err != nil {
			return err
		}
		c.RIP++
		if b2 == 0x05 { // SYSCALL
			return ErrSyscallTrapped
		}
		return fmt.Errorf("unsupported 2-byte opcode: 0x0F 0x%02x at rip=0x%x", b2, c.RIP-2)

	default:
		return fmt.Errorf("unsupported opcode: 0x%02x at rip=0x%x", b, c.RIP)
	}
}
