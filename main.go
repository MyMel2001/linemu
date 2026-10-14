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
	"strconv"
	"strings"
	"sync"
)

type VM struct {
	CPUs       []*cpu.CPUState
	MMU        *mmu.RAM
	IO         *bus.IOBus
	PCI        *pci.PCIBus
	JIT        *jit.Translator
	NIC        *devices.IntelE1000
	HDA        *devices.IntelHDA
	Storage    *devices.StorageController
	DRM        *graphics.KernelDRMBridge
	PS2        *devices.PS2Controller
	Syscalls   *syscalls.SyscallHandler
	UseJIT     bool
	NumCores   int
}

// CPU returns the primary bootstrap CPU (vCPU 0)
func (v *VM) CPU() *cpu.CPUState {
	if len(v.CPUs) > 0 {
		return v.CPUs[0]
	}
	return nil
}

func generateDriveUUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%X", b)
}

func parseRAMSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 64 * 1024 * 1024, nil
	}

	multiplier := uint64(1024 * 1024) // Default MB
	numStr := s

	if strings.HasSuffix(s, "G") || strings.HasSuffix(s, "GB") {
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimRight(s, "GB")
	} else if strings.HasSuffix(s, "M") || strings.HasSuffix(s, "MB") {
		multiplier = 1024 * 1024
		numStr = strings.TrimRight(s, "MB")
	} else if strings.HasSuffix(s, "K") || strings.HasSuffix(s, "KB") {
		multiplier = 1024
		numStr = strings.TrimRight(s, "KB")
	}

	val, err := strconv.ParseUint(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory string '%s': %w", s, err)
	}
	return val * multiplier, nil
}

func NewVM(ramBytes uint64, numCores int) *VM {
	if numCores <= 0 {
		numCores = 1
	}

	cpus := make([]*cpu.CPUState, numCores)
	for i := 0; i < numCores; i++ {
		cpus[i] = cpu.NewCPUState()
	}

	vm := &VM{
		CPUs:       cpus,
		MMU:        mmu.NewRAM(ramBytes),
		IO:         bus.NewIOBus(),
		PCI:        pci.NewPCIBus(),
		JIT:        jit.NewTranslator(),
		NIC:        devices.NewIntelE1000(0xC000, [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}),
		HDA:        devices.NewIntelHDA(0x0400),
		Storage:    devices.NewStorageController(0x01F0),
		DRM:        graphics.NewKernelDRMBridge(),
		PS2:        devices.NewPS2Controller(),
		Syscalls:   syscalls.NewSyscallHandler(),
		UseJIT:     true,
		NumCores:   numCores,
	}

	vm.DRM.AttachInput(vm.PS2)

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

	vm.IO.Register(0x60, vm.PS2)
	vm.IO.Register(0x64, vm.PS2)

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
	if err := v.MMU.LoadBytes(0x7C00, bootSector[:]); err != nil {
		return err
	}
	v.CPUs[0].RIP = 0x7C00
	return nil
}

func (v *VM) stepCPU(c *cpu.CPUState) error {
	if v.UseJIT {
		block, err := v.JIT.Compile(c.RIP, v.MMU)
		if err == nil && len(block.Ops) > 0 {
			errExec := block.Execute(c, v.MMU, v.IO)
			if errExec == cpu.ErrSyscallTrapped {
				if errSys := v.Syscalls.Handle(c, v.MMU); errSys != nil {
					return errSys
				}
				return nil
			}
			return errExec
		}
	}

	err := c.Step(v.MMU, v.IO)
	if err == cpu.ErrSyscallTrapped {
		return v.Syscalls.Handle(c, v.MMU)
	}
	return err
}

func (v *VM) Run(maxInstructions int) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(v.CPUs))

	for coreID := 0; coreID < len(v.CPUs); coreID++ {
		wg.Add(1)
		go func(c *cpu.CPUState) {
			defer wg.Done()
			for i := 0; i < maxInstructions && !c.Halted; i++ {
				if err := v.stepCPU(c); err != nil {
					errs <- err
					return
				}
			}
		}(v.CPUs[coreID])
	}

	wg.Wait()
	close(errs)

	for err := range errs {
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
	displayMode := flag.String("display", "window", "Graphics display mode: 'window' (real onscreen window), 'terminal' (ANSI 24-bit color), 'none' (headless)")
	ramStr := flag.String("m", "64M", "Custom guest RAM size (e.g. 512M, 2G, 4GB)")
	cores := flag.Int("smp", 1, "Number of vCPU cores")
	flag.Parse()

	ramBytes, err := parseRAMSize(*ramStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

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

	vm := NewVM(ramBytes, *cores)

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

	renderer := graphics.NewDisplayRenderer(graphics.DisplayMode(*displayMode), vm.DRM.Width, vm.DRM.Height)
	renderer.AttachInput(vm.PS2)
	if graphics.DisplayMode(*displayMode) != graphics.DisplayHeadless {
		renderer.SetInputSource(os.Stdin)
	}
	vm.DRM.AttachRenderer(renderer)
	if err := renderer.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not start display renderer (%v)\n", err)
	} else {
		fmt.Printf("Live graphics display active: mode=%s, resolution=%dx%d\n", *displayMode, vm.DRM.Width, vm.DRM.Height)
		defer renderer.Stop()
	}

	fmt.Printf("LinEMU initialized successfully: %d MB RAM, %d vCPU core(s), PCI Host Bridge, JIT Ready.\n", ramBytes/(1024*1024), *cores)
}
