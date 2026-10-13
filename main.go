package main

import (
	"fmt"
	"linemu/pkg/bus"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/jit"
	"linemu/pkg/mmu"
	"linemu/pkg/pci"
	"linemu/pkg/syscalls"
)

type VM struct {
	CPU        *cpu.CPUState
	MMU        *mmu.RAM
	IO         *bus.IOBus
	PCI        *pci.PCIBus
	JIT        *jit.Translator
	NIC        *devices.IntelE1000
	HDA        *devices.IntelHDA
	Storage    *devices.StorageController
	DRM        *graphics.KernelDRMBridge
	Syscalls   *syscalls.SyscallHandler
	UseJIT     bool
}

func NewVM(ramBytes uint64) *VM {
	vm := &VM{
		CPU:        cpu.NewCPUState(),
		MMU:        mmu.NewRAM(ramBytes),
		IO:         bus.NewIOBus(),
		PCI:        pci.NewPCIBus(),
		JIT:        jit.NewTranslator(),
		NIC:        devices.NewIntelE1000(0xC000, [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}),
		HDA:        devices.NewIntelHDA(0x0400),
		Storage:    devices.NewStorageController(0x01F0),
		DRM:        graphics.NewKernelDRMBridge(),
		Syscalls:   syscalls.NewSyscallHandler(),
		UseJIT:     true,
	}

	// Attach devices to PCI bus (Slot 1: IDE, Slot 2: NIC, Slot 3: HDA, Slot 4: GPU)
	vm.PCI.AttachDevice(0, 1, 0, vm.Storage)
	vm.PCI.AttachDevice(0, 2, 0, vm.NIC)
	vm.PCI.AttachDevice(0, 3, 0, vm.HDA)
	vm.PCI.AttachDevice(0, 4, 0, vm.DRM)

	// Register PCI Configuration Space ports (0xCF8 / 0xCFC)
	vm.IO.Register(0xCF8, vm.PCI)
	vm.IO.RegisterRange(0xCFC, 4, vm.PCI)

	// Register I/O ports
	vm.IO.RegisterRange(0xC000, 0x100, vm.NIC)
	vm.IO.RegisterRange(0x0400, 0x100, vm.HDA)
	vm.IO.RegisterRange(0x01F0, 0x10, vm.Storage)
	vm.IO.RegisterRange(0x03C0, 0x20, vm.DRM)

	return vm
}

func (v *VM) Run(maxInstructions int) error {
	for i := 0; i < maxInstructions && !v.CPU.Halted; i++ {
		if v.UseJIT {
			block, err := v.JIT.Compile(v.CPU.RIP, v.MMU)
			if err == nil && len(block.Ops) > 0 {
				errExec := block.Execute(v.CPU, v.MMU, v.IO)
				if errExec == cpu.ErrSyscallTrapped {
					if errSys := v.Syscalls.Handle(v.CPU, v.MMU); errSys != nil {
						return errSys
					}
					continue
				}
				if errExec != nil {
					return errExec
				}
				continue
			}
		}

		err := v.CPU.Step(v.MMU, v.IO)
		if err == cpu.ErrSyscallTrapped {
			if errSys := v.Syscalls.Handle(v.CPU, v.MMU); errSys != nil {
				return errSys
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func main() {
	vm := NewVM(64 * 1024 * 1024)
	fmt.Printf("linemu initialized: 64MB RAM, full PCI bus, JIT translation active\n")
	_ = vm
}
