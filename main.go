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
	"linemu/pkg/uefi"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type BootMode int

const (
	BootAuto BootMode = iota
	BootUEFIOnly
	BootLegacyOnly
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
	BootOrder  []uefi.BootDeviceType
	BootMode   BootMode
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
		if i > 0 {
			// Application processors stay halted until MP bring-up (no SIPI path yet)
			cpus[i].Halted = true
		}
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
		BootOrder:  []uefi.BootDeviceType{uefi.BootTypeCDROM, uefi.BootTypeHDD, uefi.BootTypeUSB},
		BootMode:   BootAuto,
	}

	vm.DRM.AttachInput(vm.PS2)

	// Map guest framebuffer at FBAddress so guest pixel writes hit the displayed buffer
	vm.MMU.MapMMIO(vm.DRM.FBAddress, vm.DRM.Framebuffer)
	vm.DRM.OnFramebufferChange = func(fb []byte) {
		vm.MMU.MapMMIO(vm.DRM.FBAddress, fb)
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
	allZero := true
	for _, b := range bootSector {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return fmt.Errorf("drive %d boot sector is empty", driveID)
	}
	if err := v.MMU.LoadBytes(0x7C00, bootSector[:]); err != nil {
		return err
	}
	v.CPUs[0].RIP = 0x7C00
	return nil
}

func (v *VM) BootUEFI(driveID int) error {
	info, ok := v.Storage.GetDriveInfo(driveID)
	if !ok {
		return fmt.Errorf("drive %d not attached", driveID)
	}
	_, err := uefi.BootUEFI(v.MMU, v.CPUs[0], info.Storage, v.DRM.FBAddress, v.DRM.Width, v.DRM.Height)
	return err
}

func (v *VM) Boot() (int, string, error) {
	drives := v.Storage.GetAllDrives()
	if len(drives) == 0 {
		return -1, "", fmt.Errorf("no storage drives attached")
	}

	for _, target := range v.BootOrder {
		for id, info := range drives {
			match := false
			if strconv.Itoa(id) == string(target) {
				match = true
			} else {
				switch target {
				case uefi.BootTypeCDROM:
					match = (info.Type == devices.DriveCDDVD)
				case uefi.BootTypeHDD:
					match = (info.Type == devices.DriveHDD)
				case uefi.BootTypeFloppy:
					match = (info.Type == devices.DriveFloppy)
				case uefi.BootTypeUSB:
					match = (info.Type == devices.DriveUSB)
				}
			}
			if !match {
				continue
			}

			// Try UEFI first
			if v.BootMode != BootLegacyOnly {
				if err := v.BootUEFI(id); err == nil {
					return id, "UEFI", nil
				}
			}

			// Fallback: Legacy MBR
			if v.BootMode != BootUEFIOnly {
				if err := v.BootFromSector(id); err == nil {
					return id, "Legacy MBR", nil
				}
			}
		}
	}

	return -1, "", fmt.Errorf("no bootable media found in boot order: %v", v.BootOrder)
}

func (v *VM) stepCPU(c *cpu.CPUState) error {
	if v.UseJIT {
		block, err := v.JIT.Compile(c.RIP, v.MMU)
		if err == nil && len(block.Ops) > 0 {
			errExec := block.Execute(c, v.MMU, v.IO)
			if errExec == cpu.ErrSyscallTrapped {
				if uefi.IsHypercall(c.Regs[cpu.RAX]) {
					return uefi.HandleService(c, v.MMU)
				}
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
		if uefi.IsHypercall(c.Regs[cpu.RAX]) {
			return uefi.HandleService(c, v.MMU)
		}
		return v.Syscalls.Handle(c, v.MMU)
	}
	return err
}

func (v *VM) Run(maxInstructions int) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(v.CPUs))

	for coreID := 0; coreID < len(v.CPUs); coreID++ {
		wg.Add(1)
		go func(id int, c *cpu.CPUState) {
			defer wg.Done()
			for i := 0; (maxInstructions <= 0 || i < maxInstructions) && !c.Halted; i++ {
				if err := v.stepCPU(c); err != nil {
					errs <- fmt.Errorf("vCPU %d: %w", id, err)
					return
				}
			}
		}(coreID, v.CPUs[coreID])
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
	bootOrderStr := flag.String("boot", "cdrom,hdd", "Boot device order (e.g. 'cd,hdd', 'iso,img', 'd,c', 'hdd,cd')")
	uefiStr := flag.String("uefi", "auto", "UEFI boot mode: 'auto' (UEFI with Legacy fallback), 'true'/'on' (UEFI only), 'false'/'legacy' (Legacy MBR only)")
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
	vm.BootOrder = uefi.ParseBootOrder(*bootOrderStr)

	switch strings.ToLower(strings.TrimSpace(*uefiStr)) {
	case "true", "on", "1", "uefi":
		vm.BootMode = BootUEFIOnly
	case "false", "off", "0", "legacy", "mbr":
		vm.BootMode = BootLegacyOnly
	default:
		vm.BootMode = BootAuto
	}

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
		slot++
	}

	bootStatus := "No bootable media specified (System Idle / Halted)"
	if slot > 0 {
		bootDrive, bootMethod, err := vm.Boot()
		if err != nil {
			bootStatus = fmt.Sprintf("Boot failed: %v", err)
			fmt.Printf("Notice: boot sequence: %v\n", err)
		} else {
			info, _ := vm.Storage.GetDriveInfo(bootDrive)
			bootStatus = fmt.Sprintf("Booted via %s from Drive %d (Unique ID: %s, RIP: 0x%X)",
				bootMethod, bootDrive, info.UniqueID, vm.CPU().RIP)
			fmt.Printf("Booted successfully via %s from Drive %d (Unique ID: %s, RIP: 0x%X)\n",
				bootMethod, bootDrive, info.UniqueID, vm.CPU().RIP)
		}
	} else {
		if vm.CPU() != nil {
			vm.CPU().Halted = true
		}
	}

	vm.DRM.RenderBootScreen(ramBytes/(1024*1024), *cores, *displayMode, bootStatus)

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
		vm.DRM.Flush()
	}

	fmt.Printf("LinEMU initialized successfully: %d MB RAM, %d vCPU core(s), PCI Host Bridge, JIT Ready.\n", ramBytes/(1024*1024), *cores)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	if graphics.DisplayMode(*displayMode) != graphics.DisplayHeadless {
		refreshRate := 33 * time.Millisecond
		if graphics.DisplayMode(*displayMode) == graphics.DisplayTerminal {
			refreshRate = 100 * time.Millisecond
		}
		ticker := time.NewTicker(refreshRate)
		defer ticker.Stop()
		go func() {
			for range ticker.C {
				vm.DRM.Flush()
			}
		}()
	}

	vmDone := make(chan struct{})
	go func() {
		if err := vm.Run(0); err != nil {
			// Surface guest fault instead of silently going black/idle
			fmt.Fprintf(os.Stderr, "\nLinEMU: guest vCPU fault: %v\n", err)
			vm.DRM.FillRect(60, vm.DRM.Height/2-70, vm.DRM.Width-120, 140, 0x30, 0x00, 0x00)
			vm.DRM.DrawText(90, vm.DRM.Height/2-50, "GUEST CPU FAULT - EMULATION STOPPED", 0xFF, 0x66, 0x66, 3)
			vm.DRM.DrawText(90, vm.DRM.Height/2, err.Error(), 0xFF, 0xCC, 0xCC, 1)
			vm.DRM.Flush()
			return // keep display alive for diagnosis; exit via signal
		}
		close(vmDone)
	}()

	// ponytail: viewer lifecycle; window stays open until user closes viewer or sends signal
	if renderer.HasExternalViewer() {
		waitCh := make(chan struct{})
		go func() {
			_ = renderer.Wait()
			close(waitCh)
		}()
		select {
		case <-sig:
		case <-waitCh:
		}
	} else if graphics.DisplayMode(*displayMode) == graphics.DisplayTerminal {
		select {
		case <-sig:
		}
	} else {
		select {
		case <-sig:
		case <-vmDone:
		}
	}
}
