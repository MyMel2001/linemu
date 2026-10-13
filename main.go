package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"linemu/pkg/bus"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/jit"
	"linemu/pkg/mmu"
	"linemu/pkg/pci"
	"linemu/pkg/syscalls"
	"os"
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

func generateDriveUUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%X", b)
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

	vm.PCI.AttachDevice(0, 1, 0, vm.Storage)
	vm.PCI.AttachDevice(0, 2, 0, vm.NIC)
	vm.PCI.AttachDevice(0, 3, 0, vm.HDA)
	vm.PCI.AttachDevice(0, 4, 0, vm.DRM)

	vm.IO.Register(0xCF8, vm.PCI)
	vm.IO.RegisterRange(0xCFC, 4, vm.PCI)

	vm.IO.RegisterRange(0xC000, 0x100, vm.NIC)
	vm.IO.RegisterRange(0x0400, 0x100, vm.HDA)
	vm.IO.RegisterRange(0x01F0, 0x10, vm.Storage)
	vm.IO.RegisterRange(0x03C0, 0x20, vm.DRM)

	return vm
}

func (v *VM) AttachImage(driveID int, path string, dType devices.DriveType, readOnly bool) (string, error) {
	dev, err := devices.OpenFileBlockDevice(path, readOnly)
	if err != nil {
		return "", err
	}
	uid := generateDriveUUID()
	v.Storage.Attach(driveID, uid, dType, dev)
	return uid, nil
}

func (v *VM) BootFromSector(driveID int) error {
	info, ok := v.Storage.GetDriveInfo(driveID)
	if !ok {
		return fmt.Errorf("drive %d not attached", driveID)
	}
	var bootSector [512]byte
	if err := info.Storage.ReadSector(0, bootSector[:]); err != nil {
		return fmt.Errorf("failed to read boot sector: %w", err)
	}
	// Load boot sector into standard 0x7C00 x86 boot address
	if err := v.MMU.LoadBytes(0x7C00, bootSector[:]); err != nil {
		return err
	}
	v.CPU.RIP = 0x7C00
	return nil
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
	isoPath := flag.String("iso", "", "Path to bootable ISO image")
	imgPath := flag.String("img", "", "Path to virtual disk IMG file")
	createHDD := flag.String("create-hdd", "", "Create new virtual HDD image (format: path,size_in_mb e.g. disk.img,1024)")
	flag.Parse()

	if *createHDD != "" {
		var path string
		var sizeMB int64 = 1024
		n, _ := fmt.Sscanf(*createHDD, "%s %d", &path, &sizeMB)
		if n == 0 {
			path = *createHDD
		}
		if err := devices.CreateVirtualDiskImage(path, sizeMB); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create HDD image: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created virtual HDD image: %s (%d MB)\n", path, sizeMB)
		if *isoPath == "" && *imgPath == "" {
			return
		}
	}

	vm := NewVM(64 * 1024 * 1024)

	slot := 0
	if *imgPath != "" {
		uid, err := vm.AttachImage(slot, *imgPath, devices.DriveHDD, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error attaching IMG: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Attached HDD image [%s] with Unique ID: %s (Drive %d)\n", *imgPath, uid, slot)
		slot++
	}

	if *isoPath != "" {
		uid, err := vm.AttachImage(slot, *isoPath, devices.DriveCDDVD, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error attaching ISO: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Attached ISO image [%s] with Unique ID: %s (Drive %d)\n", *isoPath, uid, slot)
		if err := vm.BootFromSector(slot); err != nil {
			fmt.Printf("Notice: could not read MBR from ISO (%v)\n", err)
		} else {
			fmt.Printf("Loaded boot sector from ISO drive %d (Unique ID: %s) to 0x7C00\n", slot, uid)
		}
	}

	fmt.Printf("LinEMU initialized successfully: 64MB RAM, PCI Host Bridge, JIT Compiler Ready.\n")
}
