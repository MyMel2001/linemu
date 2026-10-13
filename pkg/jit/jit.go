package jit

import (
	"fmt"
	"linemu/pkg/bus"
	"linemu/pkg/cpu"
	"sync"
)

// OpType represents internal IR operations
type OpType int

const (
	OpNop OpType = iota
	OpMovImm
	OpMovReg
	OpAddReg
	OpSubReg
	OpXorReg
	OpAndReg
	OpOrReg
	OpInPort
	OpOutPort
	OpJmpRel
	OpJzRel
	OpJnzRel
	OpHalt
	OpSyscall
)

// IROp defines translated intermediate representation instruction
type IROp struct {
	Type   OpType
	DstReg int
	SrcReg int
	Imm    uint64
	Size   int
}

// BasicBlock is a compiled sequential instruction sequence ending in a control transfer
type BasicBlock struct {
	GuestPC   uint64
	ByteLen   uint64
	Ops       []IROp
	ExecCount uint64
}

// BlockExecutor runs the JIT basic block against architectural CPU state
func (b *BasicBlock) Execute(c *cpu.CPUState, mem cpu.Memory, io bus.Device) error {
	b.ExecCount++
	for _, op := range b.Ops {
		switch op.Type {
		case OpNop:
			// nothing

		case OpMovImm:
			c.Regs[op.DstReg] = op.Imm

		case OpMovReg:
			c.Regs[op.DstReg] = c.Regs[op.SrcReg]

		case OpAddReg:
			res := c.Regs[op.DstReg] + c.Regs[op.SrcReg]
			c.UpdateFlags(c.Regs[op.DstReg], c.Regs[op.SrcReg], res, false)
			c.Regs[op.DstReg] = res

		case OpSubReg:
			res := c.Regs[op.DstReg] - c.Regs[op.SrcReg]
			c.UpdateFlags(c.Regs[op.DstReg], c.Regs[op.SrcReg], res, true)
			c.Regs[op.DstReg] = res

		case OpXorReg:
			res := c.Regs[op.DstReg] ^ c.Regs[op.SrcReg]
			c.Regs[op.DstReg] = res
			c.SetZF(res == 0)
			c.SetSF((res & 0x8000000000000000) != 0)

		case OpAndReg:
			res := c.Regs[op.DstReg] & c.Regs[op.SrcReg]
			c.Regs[op.DstReg] = res
			c.SetZF(res == 0)
			c.SetSF((res & 0x8000000000000000) != 0)

		case OpOrReg:
			res := c.Regs[op.DstReg] | c.Regs[op.SrcReg]
			c.Regs[op.DstReg] = res
			c.SetZF(res == 0)
			c.SetSF((res & 0x8000000000000000) != 0)

		case OpInPort:
			val, err := io.Read(uint16(op.Imm), op.Size)
			if err != nil {
				return err
			}
			c.Regs[op.DstReg] = uint64(val)

		case OpOutPort:
			err := io.Write(uint16(op.Imm), uint32(c.Regs[op.SrcReg]), op.Size)
			if err != nil {
				return err
			}

		case OpJmpRel:
			c.RIP = uint64(int64(c.RIP) + int64(op.Imm))
			return nil

		case OpJzRel:
			if c.GetZF() {
				c.RIP = uint64(int64(c.RIP) + int64(op.Imm))
				return nil
			}

		case OpJnzRel:
			if !c.GetZF() {
				c.RIP = uint64(int64(c.RIP) + int64(op.Imm))
				return nil
			}

		case OpHalt:
			c.Halted = true
			return nil

		case OpSyscall:
			c.RIP += b.ByteLen
			return cpu.ErrSyscallTrapped
		}
	}
	c.RIP += b.ByteLen
	return nil
}

// TranslationCache stores compiled JIT blocks indexed by entry RIP
type TranslationCache struct {
	mu     sync.RWMutex
	blocks map[uint64]*BasicBlock
}

func NewTranslationCache() *TranslationCache {
	return &TranslationCache{
		blocks: make(map[uint64]*BasicBlock),
	}
}

func (tc *TranslationCache) Get(pc uint64) (*BasicBlock, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	b, ok := tc.blocks[pc]
	return b, ok
}

func (tc *TranslationCache) Put(pc uint64, b *BasicBlock) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.blocks[pc] = b
}

func (tc *TranslationCache) Invalidate() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.blocks = make(map[uint64]*BasicBlock)
}

// Translator translates guest byte sequences into basic blocks
type Translator struct {
	Cache *TranslationCache
}

func NewTranslator() *Translator {
	return &Translator{
		Cache: NewTranslationCache(),
	}
}

// Compile compiles from pc up to a terminator (HLT, JMP, SYSCALL)
func (t *Translator) Compile(pc uint64, mem cpu.Memory) (*BasicBlock, error) {
	if b, ok := t.Cache.Get(pc); ok {
		return b, nil
	}

	block := &BasicBlock{
		GuestPC: pc,
		Ops:     make([]IROp, 0),
	}

	curPC := pc
	terminated := false

	for !terminated && len(block.Ops) < 64 {
		b, err := mem.Read8(curPC)
		if err != nil {
			return nil, err
		}

		rexW := false
		rexR := false
		rexB := false

		if b >= 0x40 && b <= 0x4F {
			rexW = (b & 0x08) != 0
			rexR = (b & 0x04) != 0
			rexB = (b & 0x01) != 0
			curPC++
			b, err = mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
		}

		switch b {
		case 0x90: // NOP
			block.Ops = append(block.Ops, IROp{Type: OpNop})
			curPC++

		case 0xF4: // HLT
			block.Ops = append(block.Ops, IROp{Type: OpHalt})
			curPC++
			terminated = true

		case 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF: // MOV reg, imm
			reg := int(b - 0xB8)
			if rexB {
				reg += 8
			}
			curPC++
			if rexW {
				imm, err := mem.Read64(curPC)
				if err != nil {
					return nil, err
				}
				block.Ops = append(block.Ops, IROp{Type: OpMovImm, DstReg: reg, Imm: imm, Size: 8})
				curPC += 8
			} else {
				imm, err := mem.Read32(curPC)
				if err != nil {
					return nil, err
				}
				block.Ops = append(block.Ops, IROp{Type: OpMovImm, DstReg: reg, Imm: uint64(imm), Size: 4})
				curPC += 4
			}

		case 0x01: // ADD r/m, r
			curPC++
			modrm, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			reg := int((modrm >> 3) & 7)
			rm := int(modrm & 7)
			if rexR {
				reg += 8
			}
			if rexB {
				rm += 8
			}
			block.Ops = append(block.Ops, IROp{Type: OpAddReg, DstReg: rm, SrcReg: reg})

		case 0x29: // SUB r/m, r
			curPC++
			modrm, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			reg := int((modrm >> 3) & 7)
			rm := int(modrm & 7)
			if rexR {
				reg += 8
			}
			if rexB {
				rm += 8
			}
			block.Ops = append(block.Ops, IROp{Type: OpSubReg, DstReg: rm, SrcReg: reg})

		case 0x31: // XOR r/m, r
			curPC++
			modrm, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			reg := int((modrm >> 3) & 7)
			rm := int(modrm & 7)
			if rexR {
				reg += 8
			}
			if rexB {
				rm += 8
			}
			block.Ops = append(block.Ops, IROp{Type: OpXorReg, DstReg: rm, SrcReg: reg})

		case 0xEB: // JMP rel8
			curPC++
			rel, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			block.Ops = append(block.Ops, IROp{Type: OpJmpRel, Imm: uint64(int8(rel))})
			terminated = true

		case 0x74: // JZ/JE rel8
			curPC++
			rel, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			block.Ops = append(block.Ops, IROp{Type: OpJzRel, Imm: uint64(int8(rel))})
			terminated = true

		case 0x75: // JNZ/JNE rel8
			curPC++
			rel, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			block.Ops = append(block.Ops, IROp{Type: OpJnzRel, Imm: uint64(int8(rel))})
			terminated = true

		case 0x0F: // Two-byte
			curPC++
			b2, err := mem.Read8(curPC)
			if err != nil {
				return nil, err
			}
			curPC++
			if b2 == 0x05 { // SYSCALL
				block.Ops = append(block.Ops, IROp{Type: OpSyscall})
				terminated = true
			} else {
				return nil, fmt.Errorf("unsupported 2-byte opcode: 0x0F 0x%02x", b2)
			}

		default:
			// Fallback: stop block compilation here
			terminated = true
		}
	}

	block.ByteLen = curPC - pc
	t.Cache.Put(pc, block)
	return block, nil
}
