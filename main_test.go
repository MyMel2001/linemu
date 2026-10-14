package main

import (
	"bytes"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/syscalls"
	"os"
	"path/filepath"
	"testing"
)

func TestCPUMovAndAdd(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	code := []byte{
		0xB8, 0x0A, 0x00, 0x00, 0x00,
		0xBB, 0x14, 0x00, 0x00, 0x00,
		0x01, 0xC3,
		0xF4,
	}

	if err := vm.MMU.LoadBytes(0, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if err := vm.Run(100); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if vm.CPU().Regs[cpu.RAX] != 10 {
		t.Errorf("expected RAX=10, got %d", vm.CPU().Regs[cpu.RAX])
	}
	if vm.CPU().Regs[cpu.RBX] != 30 {
		t.Errorf("expected RBX=30, got %d", vm.CPU().Regs[cpu.RBX])
	}
	if !vm.CPU().Halted {
		t.Errorf("expected CPU to be halted")
	}
}

func TestJITTranslationLoop(t *testing.T) {
	vm := NewVM(1024*1024, 1)
	vm.UseJIT = true

	code := []byte{
		0xB8, 0x32, 0x00, 0x00, 0x00,
		0xBA, 0x14, 0x00, 0x00, 0x00,
		0x29, 0xD0,
		0xF4,
	}

	if err := vm.MMU.LoadBytes(0, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if err := vm.Run(100); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if vm.CPU().Regs[cpu.RAX] != 30 {
		t.Errorf("expected EAX=30 from JIT, got %d", vm.CPU().Regs[cpu.RAX])
	}
	if !vm.CPU().Halted {
		t.Errorf("expected CPU to halt")
	}
}

func TestMultiCoreExecution(t *testing.T) {
	// 4 vCPU cores with 2MB RAM
	vm := NewVM(2*1024*1024, 4)
	vm.UseJIT = false

	if len(vm.CPUs) != 4 {
		t.Fatalf("expected 4 vCPUs, got %d", len(vm.CPUs))
	}

	// Code: MOV EAX, 42; HLT
	code := []byte{0xB8, 0x2A, 0x00, 0x00, 0x00, 0xF4}
	if err := vm.MMU.LoadBytes(0, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	for i := 1; i < 4; i++ {
		vm.CPUs[i].RIP = 0
	}

	if err := vm.Run(10); err != nil {
		t.Fatalf("Run multi-core: %v", err)
	}

	for i := 0; i < 4; i++ {
		if vm.CPUs[i].Regs[cpu.RAX] != 42 {
			t.Errorf("core %d: expected EAX=42, got %d", i, vm.CPUs[i].Regs[cpu.RAX])
		}
		if !vm.CPUs[i].Halted {
			t.Errorf("core %d: expected halted", i)
		}
	}
}

func TestParseRAMSize(t *testing.T) {
	tests := []struct {
		input    string
		expected uint64
	}{
		{"512M", 512 * 1024 * 1024},
		{"2G", 2 * 1024 * 1024 * 1024},
		{"4GB", 4 * 1024 * 1024 * 1024},
		{"128MB", 128 * 1024 * 1024},
	}
	for _, tt := range tests {
		val, err := parseRAMSize(tt.input)
		if err != nil {
			t.Errorf("parseRAMSize(%s) error: %v", tt.input, err)
		}
		if val != tt.expected {
			t.Errorf("parseRAMSize(%s) = %d, expected %d", tt.input, val, tt.expected)
		}
	}
}

func TestPCIBusConfiguration(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	if err := vm.IO.Out(0xCF8, 0x80001000, 4); err != nil {
		t.Fatalf("Out CF8: %v", err)
	}

	val, err := vm.IO.In(0xCFC, 4)
	if err != nil {
		t.Fatalf("In CFC: %v", err)
	}

	if val != 0x100E8086 {
		t.Errorf("expected PCI ID 0x100e8086, got 0x%08x", val)
	}

	if err := vm.IO.Out(0xCF8, 0x80001010, 4); err != nil {
		t.Fatalf("Out CF8 BAR0: %v", err)
	}
	if err := vm.IO.Out(0xCFC, 0xFFFFFFFF, 4); err != nil {
		t.Fatalf("Out CFC BAR0 size probe: %v", err)
	}
	barSizeMask, err := vm.IO.In(0xCFC, 4)
	if err != nil {
		t.Fatalf("In CFC BAR0: %v", err)
	}
	if barSizeMask != 0xFFFE0000 {
		t.Errorf("expected BAR size mask 0xFFFE0000, got 0x%08x", barSizeMask)
	}
}

func TestSyscallTranslation(t *testing.T) {
	vm := NewVM(1024*1024, 1)
	vm.UseJIT = false

	vm.CPU().Regs[cpu.RAX] = syscalls.SYS_BRK
	vm.CPU().Regs[cpu.RDI] = 0x40001000

	code := []byte{0x0F, 0x05, 0xF4}
	if err := vm.MMU.LoadBytes(0, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if err := vm.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if vm.CPU().Regs[cpu.RAX] != 0x40001000 {
		t.Errorf("expected brk=0x40001000, got 0x%x", vm.CPU().Regs[cpu.RAX])
	}
}

func TestDeviceIntelE1000(t *testing.T) {
	nic := devices.NewIntelE1000(0xC000, [6]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55})
	val, err := nic.Read(0xC000+devices.E1000_STATUS, 4)
	if err != nil {
		t.Fatalf("Read status: %v", err)
	}
	if val == 0 {
		t.Errorf("expected link status non-zero")
	}

	pkt := []byte("ping test packet")
	nic.ReceivePacket(pkt)
}

func TestDeviceStorageWithUniqueIDs(t *testing.T) {
	storage := devices.NewStorageController(0x1F0)
	ramdisk := devices.NewMemoryBlockDevice(10)
	uniqueID := "DRIVE_UUID_A1B2C3"
	storage.Attach(0, uniqueID, devices.DriveHDD, ramdisk)

	info, ok := storage.GetDriveInfo(0)
	if !ok || info.UniqueID != uniqueID {
		t.Fatalf("expected unique ID %s, got %v", uniqueID, info)
	}

	payload := make([]byte, 512)
	copy(payload, []byte("LINUX_OR_WIN_BOOTSECTOR"))
	if err := ramdisk.WriteSector(0, payload); err != nil {
		t.Fatalf("WriteSector: %v", err)
	}

	var readBuf [512]byte
	if err := ramdisk.ReadSector(0, readBuf[:]); err != nil {
		t.Fatalf("ReadSector: %v", err)
	}
	if !bytes.Equal(payload, readBuf[:]) {
		t.Errorf("sector content mismatch")
	}

	_ = storage.Write(0x1F7, 0xEC, 1)
	idWord, _ := storage.Read(0x1F0, 2)
	if idWord != 0x0040 {
		t.Errorf("expected IDENTIFY signature 0x0040, got 0x%04x", idWord)
	}
}

func TestVirtualDiskCreationAndBoot(t *testing.T) {
	tmpDir := t.TempDir()
	imgFile := filepath.Join(tmpDir, "test_boot.img")

	if err := devices.CreateVirtualDiskImage(imgFile, 1); err != nil {
		t.Fatalf("CreateVirtualDiskImage: %v", err)
	}

	f, err := os.OpenFile(imgFile, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	mbr := make([]byte, 512)
	mbr[0] = 0xF4
	if _, err := f.Write(mbr); err != nil {
		t.Fatalf("write mbr: %v", err)
	}
	f.Close()

	vm := NewVM(1024*1024, 1)
	uid, err := vm.AttachImage(0, imgFile, devices.DriveHDD, false)
	if err != nil {
		t.Fatalf("AttachImage: %v", err)
	}
	if uid == "" {
		t.Errorf("expected non-empty unique drive ID")
	}

	if err := vm.BootFromSector(0); err != nil {
		t.Fatalf("BootFromSector: %v", err)
	}

	if vm.CPU().RIP != 0x7C00 {
		t.Errorf("expected RIP=0x7C00, got 0x%x", vm.CPU().RIP)
	}

	if err := vm.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !vm.CPU().Halted {
		t.Errorf("expected CPU halted from boot sector")
	}
}

func TestKernelDRMBridge(t *testing.T) {
	drm := graphics.NewKernelDRMBridge()
	if err := drm.DispatchCommand(0x01, []uint32{2560, 1440}); err != nil {
		t.Fatalf("DispatchCommand mode set: %v", err)
	}
	if drm.Width != 2560 || drm.Height != 1440 {
		t.Errorf("expected 2560x1440, got %dx%d", drm.Width, drm.Height)
	}

	if err := drm.DispatchCommand(0x02, []uint32{uint32(graphics.APIDirectX12)}); err != nil {
		t.Fatalf("DispatchCommand DX12 bind: %v", err)
	}
	if drm.ActiveAPI != graphics.APIDirectX12 {
		t.Errorf("expected DirectX12 mode")
	}

	var outBuf bytes.Buffer
	renderer := graphics.NewDisplayRenderer(graphics.DisplayTerminal, 100, 50)
	renderer.SetOutput(&outBuf)
	drm.AttachRenderer(renderer)
	if err := renderer.Start(); err != nil {
		t.Fatalf("renderer Start: %v", err)
	}
	defer renderer.Stop()

	drm.WritePixel(10, 10, 255, 0, 0, 255)
	drm.Flush()

	if err := drm.DispatchCommand(0x04, nil); err != nil {
		t.Fatalf("DispatchCommand Flush: %v", err)
	}
}

func TestPS2KeyboardAndMouse(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	// Status initially has SYS (bit 2) and INH (bit 4) set
	st, err := vm.IO.In(0x64, 1)
	if err != nil {
		t.Fatalf("In 0x64: %v", err)
	}
	if (st & devices.PS2StatusSYS) == 0 {
		t.Errorf("expected SYS bit set in status, got 0x%02x", st)
	}

	// 1. Controller command 0xAA (Self Test)
	if err := vm.IO.Out(0x64, 0xAA, 1); err != nil {
		t.Fatalf("Out 0x64 (0xAA): %v", err)
	}
	data, _ := vm.IO.In(0x60, 1)
	if data != 0x55 {
		t.Errorf("expected 0x55 from controller self-test, got 0x%02x", data)
	}

	// 2. Keyboard command 0xF2 (Identify)
	if err := vm.IO.Out(0x60, 0xF2, 1); err != nil {
		t.Fatalf("Out 0x60 (0xF2): %v", err)
	}
	ack, _ := vm.IO.In(0x60, 1)
	id1, _ := vm.IO.In(0x60, 1)
	id2, _ := vm.IO.In(0x60, 1)
	if ack != 0xFA || id1 != 0xAB || id2 != 0x83 {
		t.Errorf("expected 0xFA, 0xAB, 0x83 for keyboard identify; got 0x%02x, 0x%02x, 0x%02x", ack, id1, id2)
	}

	// 3. Mouse command 0xD4 prefix then 0xF2 (Get Device ID)
	if err := vm.IO.Out(0x64, 0xD4, 1); err != nil {
		t.Fatalf("Out 0x64 (0xD4): %v", err)
	}
	if err := vm.IO.Out(0x60, 0xF2, 1); err != nil {
		t.Fatalf("Out 0x60 (0xF2): %v", err)
	}
	// Check status bit 5 (AUXDATA) is set for mouse responses
	st, _ = vm.IO.In(0x64, 1)
	if (st & devices.PS2StatusAUXData) == 0 {
		t.Errorf("expected AUXDATA bit in status for mouse response, got 0x%02x", st)
	}
	mAck, _ := vm.IO.In(0x60, 1)
	mID, _ := vm.IO.In(0x60, 1)
	if mAck != 0xFA || mID != 0x00 {
		t.Errorf("expected mouse ID 0xFA, 0x00; got 0x%02x, 0x%02x", mAck, mID)
	}

	// 4. Inject keyboard key and read via CPU IN AL, DX
	vm.PS2.KeyDown(0x1E) // 'A' press
	vm.CPU().Regs[cpu.RDX] = 0x60
	code := []byte{0xEC, 0xF4} // IN AL, DX; HLT
	if err := vm.MMU.LoadBytes(0x1000, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	vm.CPU().RIP = 0x1000
	vm.CPU().Halted = false
	if err := vm.Run(10); err != nil {
		t.Fatalf("Run CPU IN AL, DX: %v", err)
	}
	if uint8(vm.CPU().Regs[cpu.RAX]) != 0x1E {
		t.Errorf("expected AL=0x1E, got 0x%02x", vm.CPU().Regs[cpu.RAX])
	}

	// 5. Inject mouse movement event
	vm.PS2.SendMouseEvent(15, -10, 0x01) // Left click, dx=15, dy=-10
	b0, _ := vm.IO.In(0x60, 1)
	b1, _ := vm.IO.In(0x60, 1)
	b2, _ := vm.IO.In(0x60, 1)
	if (b0 & 0x01) != 0x01 || (b0 & 0x08) != 0x08 || (b0 & 0x20) == 0 { // Y sign bit set since dy < 0
		t.Errorf("unexpected mouse packet header: 0x%02x", b0)
	}
	if int8(b1) != 15 || int8(b2) != -10 {
		t.Errorf("expected dx=15, dy=-10; got %d, %d", int8(b1), int8(b2))
	}
}

func TestDisplayInputInheritance(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	renderer := graphics.NewDisplayRenderer(graphics.DisplayTerminal, 80, 24)
	inputPipe := bytes.NewBufferString("\033[Ahello")
	renderer.SetInputSource(inputPipe)

	// Attaching renderer inherits input from vm.DRM
	vm.DRM.AttachRenderer(renderer)
	if renderer.Input() == nil {
		t.Fatalf("expected renderer to inherit input device from DRM bridge")
	}

	if err := renderer.Start(); err != nil {
		t.Fatalf("Start renderer: %v", err)
	}
	defer renderer.Stop()

	// Direct input forward test
	renderer.SendKey('x', true)
	code, err := vm.IO.In(0x60, 1)
	if err != nil || code != 0x2D {
		t.Errorf("expected scancode 0x2D for 'x', got 0x%02x (err=%v)", code, err)
	}

	// Mouse forwarding test
	renderer.SendMouseEvent(5, 5, 0)
	mb0, _ := vm.IO.In(0x60, 1)
	if (mb0 & 0x08) != 0x08 {
		t.Errorf("expected mouse packet header 0x08, got 0x%02x", mb0)
	}
}
