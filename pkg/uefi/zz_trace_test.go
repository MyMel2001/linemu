package uefi

import (
	"fmt"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
	"testing"
)

func TestZZTraceWin11Entry(t *testing.T) {
	dev, err := devices.OpenFileBlockDevice("/var/home/rocketbob/win11.iso", true)
	if err != nil {
		t.Skipf("no iso: %v", err)
	}
	ram := mmu.NewRAM(512 * 1024 * 1024)
	c := cpu.NewCPUState()
	// minimal MMIO for FB so fbInfo works
	fb := make([]byte, 1920*1080*4)
	ram.MapMMIO(0xE0000000, fb)
	entry, err := BootUEFI(ram, c, dev, 0xE0000000, 1920, 1080)
	if err != nil {
		t.Fatalf("BootUEFI: %v", err)
	}
	t.Logf("entry=0x%x RCX=0x%x RDX=0x%x RSP=0x%x", entry, c.Regs[cpu.RCX], c.Regs[cpu.RDX], c.Regs[cpu.RSP])
	// dump entry bytes
	for i := uint64(0); i < 64; i += 16 {
		var s string
		for j := uint64(0); j < 16; j++ {
			b, _ := ram.Read8(entry + i + j)
			s += fmt.Sprintf("%02x ", b)
		}
		t.Logf("+0x%03x: %s", i, s)
	}
	// step interpreter only, log hypercalls + faults
	steps := 0
	calls := map[uint64]int{}
	for steps < 50000 && !c.Halted {
		ripBefore := c.RIP
		if ripBefore == 0x100d514c || steps < 80 || (steps >= 100 && steps <= 175) || steps >= 1400 {
			var s string
			for j := uint64(0); j < 12; j++ {
				b, e := ram.Read8(ripBefore + j)
				if e != nil {
					s += "?? "
				} else {
					s += fmt.Sprintf("%02x ", b)
				}
			}
			t.Logf("step %3d RIP=0x%x RSP=0x%x: %s", steps, ripBefore, c.Regs[cpu.RSP], s)
		}
		err := c.Step(ram, nilIO{})
		if err == cpu.ErrSyscallTrapped {
			id := c.Regs[cpu.RAX]
			calls[id]++
			isAlloc := id == svcAllocatePages || id == svcAllocatePool || id == svcGetMemoryMap || id == svcFreePages || id == svcFreePool
			if len(calls) < 30 || steps%1000 == 0 || id == svcHandleProtocol || id == svcLocateProtocol || id == svcOpenProtocol || id == svcGetVariable || isAlloc {
				extra := ""
				if id == svcHandleProtocol || id == svcOpenProtocol || id == svcLocateProtocol {
					gPtr := c.Regs[cpu.RDX]
					if id == svcLocateProtocol {
						gPtr = c.Regs[cpu.RCX]
					}
					var gs string
					for j := 0; j < 16; j++ {
						b, _ := ram.Read8(gPtr + uint64(j))
						gs += fmt.Sprintf("%02x", b)
					}
					extra = fmt.Sprintf(" guid=%s", gs)
				}
				t.Logf("step %d RIP=0x%x SYSCALL id=0x%x RCX=0x%x RDX=0x%x R8=0x%x R9=0x%x%s RAX_after=?", steps, ripBefore, id, c.Regs[cpu.RCX], c.Regs[cpu.RDX], c.Regs[cpu.R8], c.Regs[cpu.R9], extra)
			}
			// emulate ret from stub: need to advance past syscall? Step already set RIP past insn?
			// HandleService expects RAX=id; call it with nil ps2/drm
			if IsHypercall(id) {
				preCX, preDX := c.Regs[cpu.RCX], c.Regs[cpu.RDX]
				_ = HandleService(c, ram)
				if id == svcHandleProtocol || id == svcLocateProtocol || id == svcOpenProtocol {
					gPtr := c.Regs[cpu.RDX]
					if id == svcLocateProtocol {
						gPtr = uint64(0)
					}
					_ = gPtr
				}
				t.Logf("  -> RAX=0x%x", c.Regs[cpu.RAX])
				if c.Regs[cpu.RAX] == EFIStatusNotFound {
					// GUID bytes at pre-call RDX (Handle/Open) or RCX (Locate)
					gPtr := preDX
					if id == svcLocateProtocol {
						gPtr = preCX
					}
					var gs string
					for j := 0; j < 16; j++ {
						b, _ := ram.Read8(gPtr + uint64(j))
						gs += fmt.Sprintf("%02x", b)
					}
					// stack arg for LocateHandle (search key) / extra context
					stk, _ := ram.Read64(c.Regs[cpu.RSP])
					t.Logf("  NOT_FOUND guid=%s RCX=0x%x RDX=0x%x RSP0=0x%x", gs, preCX, preDX, stk)
					// variable name for GetVariable
					if id == svcGetVariable {
						var nm string
						for j := 0; j < 64; j++ {
							ch, _ := ram.Read16(preDX + uint64(j*2))
							if ch == 0 {
								break
							}
							nm += string(rune(ch))
						}
						t.Logf("  GetVariable name=%q vendor-ptr=0x%x", nm, c.Regs[cpu.R8])
					}
				}
			} else {
				t.Logf("non-uefi syscall nr=%d -> stop", id)
				break
			}
			steps++
			continue
		}
		if err != nil {
			t.Logf("FAULT at step %d RIP=0x%x: %v", steps, ripBefore, err)
			// dump regs
			t.Logf("RAX=0x%x RCX=0x%x RDX=0x%x RBX=0x%x RSP=0x%x RBP=0x%x RSI=0x%x RDI=0x%x R8=0x%x R9=0x%x R10=0x%x R11=0x%x", c.Regs[0], c.Regs[1], c.Regs[2], c.Regs[3], c.Regs[4], c.Regs[5], c.Regs[6], c.Regs[7], c.Regs[8], c.Regs[9], c.Regs[10], c.Regs[11])
			t.Logf("CR0=0x%x CR3=0x%x CR4=0x%x", c.CR0, c.CR3, c.CR4)
			if c.CR3 != 0 {
				for i := uint64(0); i < 8; i++ {
					v, e := ram.Read64((c.CR3 &^ 0xFFF) + i*8)
					t.Logf(" PML4[%d]=0x%x err=%v", i, v, e)
				}
				// walk fault addr
				fa := ripBefore
				if p, ok := c.Translate(fa, ram); ok {
					t.Logf(" translate 0x%x -> 0x%x", fa, p)
				} else {
					t.Logf(" translate 0x%x MISS", fa)
					pml4e, _ := ram.Read64((c.CR3 &^ 0xFFF) + ((fa>>39)&0x1FF)*8)
					t.Logf(" pml4e=0x%x", pml4e)
					if pml4e&1 != 0 {
						pdpe, _ := ram.Read64((pml4e &^ 0xFFF) + ((fa>>30)&0x1FF)*8)
						t.Logf(" pdpe=0x%x", pdpe)
						if pdpe&1 != 0 && pdpe&(1<<7) == 0 {
							pde, _ := ram.Read64((pdpe &^ 0xFFF) + ((fa>>21)&0x1FF)*8)
							t.Logf(" pde=0x%x", pde)
							if pde&1 != 0 && pde&(1<<7) == 0 {
								pte, _ := ram.Read64((pde &^ 0xFFF) + ((fa>>12)&0x1FF)*8)
								t.Logf(" pte=0x%x", pte)
							}
						}
					}
				}
			}
			// dump code around fault RIP
			for i := int64(-32); i < 32; i += 8 {
				var s string
				for j := int64(0); j < 8; j++ {
					b, e := ram.Read8(uint64(int64(ripBefore) + i + j))
					if e != nil {
						s += "?? "
					} else {
						s += fmt.Sprintf("%02x ", b)
					}
				}
				mark := " "
				if i == 0 {
					mark = ">"
				}
				t.Logf("%s 0x%x: %s", mark, uint64(int64(ripBefore)+i), s)
			}
			break
		}
		steps++
		if steps%5000 == 0 {
			t.Logf("... step %d RIP=0x%x", steps, c.RIP)
		}
	}
	t.Logf("done steps=%d halted=%v calls=%v", steps, c.Halted, calls)
}

type nilIO struct{}

func (nilIO) In(port uint16, size int) (uint32, error) { return 0, nil }
func (nilIO) Out(port uint16, val uint32, size int) error { return nil }
