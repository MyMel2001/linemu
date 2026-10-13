package syscalls

import (
	"linemu/pkg/cpu"
	"linemu/pkg/mmu"
	"os"
)

// Linux x86_64 Syscall Numbers
const (
	SYS_READ   = 0
	SYS_WRITE  = 1
	SYS_OPEN   = 2
	SYS_CLOSE  = 3
	SYS_MMAP   = 9
	SYS_BRK    = 12
	SYS_IOCTL  = 16
	SYS_EXIT   = 60
	SYS_ARCH_PRCTL = 158
)

type SyscallHandler struct {
	brkCurrent uint64
}

func NewSyscallHandler() *SyscallHandler {
	return &SyscallHandler{
		brkCurrent: 0x40000000,
	}
}

// Handle translates x86_64 guest Linux/Windows kernel traps
func (s *SyscallHandler) Handle(state *cpu.CPUState, mem *mmu.RAM) error {
	nr := state.Regs[cpu.RAX]
	arg1 := state.Regs[cpu.RDI]
	arg2 := state.Regs[cpu.RSI]
	arg3 := state.Regs[cpu.RDX]
	_ = state.Regs[cpu.R10]
	_ = state.Regs[cpu.R8]
	_ = state.Regs[cpu.R9]

	switch nr {
	case SYS_READ:
		buf, err := mem.RawBuffer(arg2, arg3)
		if err != nil {
			state.Regs[cpu.RAX] = ^uint64(13) // -EFAULT
			return nil
		}
		if arg1 == 0 { // stdin
			n, err := os.Stdin.Read(buf)
			if err != nil && n == 0 {
				state.Regs[cpu.RAX] = ^uint64(4) // -EIO
				return nil
			}
			state.Regs[cpu.RAX] = uint64(n)
			return nil
		}
		state.Regs[cpu.RAX] = 0 // EOF for dummy fds

	case SYS_WRITE:
		buf, err := mem.RawBuffer(arg2, arg3)
		if err != nil {
			state.Regs[cpu.RAX] = ^uint64(13) // -EFAULT
			return nil
		}
		if arg1 == 1 || arg1 == 2 {
			w := os.Stdout
			if arg1 == 2 {
				w = os.Stderr
			}
			n, _ := w.Write(buf)
			state.Regs[cpu.RAX] = uint64(n)
			return nil
		}
		state.Regs[cpu.RAX] = arg3

	case SYS_BRK:
		if arg1 != 0 && arg1 >= s.brkCurrent {
			s.brkCurrent = arg1
		}
		state.Regs[cpu.RAX] = s.brkCurrent

	case SYS_MMAP:
		// arg2: length, arg1: hint addr
		length := arg2
		if length == 0 {
			state.Regs[cpu.RAX] = ^uint64(21) // -EINVAL
			return nil
		}
		// Allocate virtual bump from current brk
		allocAddr := s.brkCurrent
		s.brkCurrent += (length + 0xFFF) & ^uint64(0xFFF) // 4K align
		state.Regs[cpu.RAX] = allocAddr

	case SYS_ARCH_PRCTL:
		// Subcode 0x1002 is ARCH_SET_FS
		state.Regs[cpu.RAX] = 0

	case SYS_IOCTL:
		// ponytail: forward DRM and graphics ioctl to card0 device; return success stub
		state.Regs[cpu.RAX] = 0

	case SYS_EXIT:
		state.Halted = true
		state.Regs[cpu.RAX] = 0
		return nil

	default:
		// Return -ENOSYS
		state.Regs[cpu.RAX] = ^uint64(37)
	}

	return nil
}
