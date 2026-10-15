package main

import (
	"bytes"
	"linemu/pkg/bus"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/syscalls"
	"linemu/pkg/uefi"
	"os"
	"path/filepath"
	"strings"
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
		vm.CPUs[i].Halted = false // APs start halted; release for MP test
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
	if (b0&0x01) != 0x01 || (b0&0x08) != 0x08 || (b0&0x20) == 0 { // Y sign bit set since dy < 0
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

func TestUEFIBootSequenceAndOrder(t *testing.T) {
	vm := NewVM(32*1024*1024, 1)

	// Drive 0: HDD with legacy MBR boot code (MOV EAX, 0x1111; HLT)
	hdd := devices.NewMemoryBlockDevice(10)
	var mbr [512]byte
	mbrCode := []byte{0xB8, 0x11, 0x11, 0x00, 0x00, 0xF4}
	copy(mbr[:], mbrCode)
	_ = hdd.WriteSector(0, mbr[:])
	vm.Storage.Attach(0, "HDD_DISK_0", devices.DriveHDD, hdd)

	// Drive 1: CDROM with raw UEFI payload (MOV EAX, 0x2222; HLT)
	cdrom := devices.NewMemoryBlockDevice(10)
	var efiSec [512]byte
	efiCode := []byte{0xB8, 0x22, 0x22, 0x00, 0x00, 0xF4}
	copy(efiSec[:], efiCode)
	// Write FAT BPB and directory entry pointing to cluster 2
	var bpb [512]byte
	bpb[11] = 0x00
	bpb[12] = 0x02 // 512 bytes/sec
	bpb[13] = 1    // 1 sec/cluster
	bpb[14] = 1    // 1 reserved sec
	bpb[16] = 1    // 1 FAT
	bpb[17] = 16   // 16 root entries
	bpb[22] = 1    // 1 sec/FAT
	_ = cdrom.WriteSector(0, bpb[:])
	var dir [512]byte
	copy(dir[0:11], []byte("BOOTX64 EFI"))
	dir[26] = 2 // cluster 2
	dir[28] = 6 // file size 6 bytes
	_ = cdrom.WriteSector(2, dir[:])
	_ = cdrom.WriteSector(3, efiSec[:])
	vm.Storage.Attach(1, "CDROM_DISC_1", devices.DriveCDDVD, cdrom)

	// Test 1: Boot order CDROM first ("cdrom,hdd") -> Boots UEFI from Drive 1
	vm.BootOrder = []uefi.BootDeviceType{uefi.BootTypeCDROM, uefi.BootTypeHDD}
	bootID, method, err := vm.Boot()
	if err != nil {
		t.Fatalf("vm.Boot cdrom,hdd: %v", err)
	}
	if bootID != 1 || method != "UEFI" {
		t.Errorf("expected Drive 1 via UEFI, got Drive %d via %s", bootID, method)
	}
	if vm.CPU().RIP != uefi.DefaultPayloadBase {
		t.Errorf("expected RIP=0x%X, got 0x%X", uefi.DefaultPayloadBase, vm.CPU().RIP)
	}
	if err := vm.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if vm.CPU().Regs[cpu.RAX] != 0x2222 {
		t.Errorf("expected RAX=0x2222, got 0x%X", vm.CPU().Regs[cpu.RAX])
	}

	// Test 2: Boot order HDD first ("hdd,cdrom") with BootLegacyOnly -> Boots Legacy MBR from Drive 0
	vm.BootOrder = []uefi.BootDeviceType{uefi.BootTypeHDD, uefi.BootTypeCDROM}
	vm.BootMode = BootLegacyOnly
	bootID, method, err = vm.Boot()
	if err != nil {
		t.Fatalf("vm.Boot hdd,cdrom legacy: %v", err)
	}
	if bootID != 0 || method != "Legacy MBR" {
		t.Errorf("expected Drive 0 via Legacy MBR, got Drive %d via %s", bootID, method)
	}
	if vm.CPU().RIP != 0x7C00 {
		t.Errorf("expected RIP=0x7C00, got 0x%X", vm.CPU().RIP)
	}
	vm.CPU().Halted = false
	if err := vm.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if vm.CPU().Regs[cpu.RAX] != 0x1111 {
		t.Errorf("expected RAX=0x1111, got 0x%X", vm.CPU().Regs[cpu.RAX])
	}

	// Test 3: Specific Drive ID boot order ("1,0")
	vm.BootOrder = []uefi.BootDeviceType{"1", "0"}
	vm.BootMode = BootAuto
	bootID, method, err = vm.Boot()
	if err != nil {
		t.Fatalf("vm.Boot 1,0: %v", err)
	}
	if bootID != 1 {
		t.Errorf("expected Drive 1, got Drive %d (method=%s)", bootID, method)
	}

	// Test 4: Boot fallback when first device is unbootable
	emptyDev := devices.NewMemoryBlockDevice(10)
	vm.Storage.Attach(2, "EMPTY_DRIVE_2", devices.DriveFloppy, emptyDev)
	vm.BootOrder = []uefi.BootDeviceType{uefi.BootTypeFloppy, uefi.BootTypeHDD}
	bootID, method, err = vm.Boot()
	if err != nil {
		t.Fatalf("vm.Boot fallback: %v", err)
	}
	if bootID != 0 {
		t.Errorf("expected fallback to Drive 0, got Drive %d (method=%s)", bootID, method)
	}
}

func TestGuestFaultSurfaces(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	// 0x06 (PUSH ES) is invalid in 64-bit mode — must surface as a fault.
	// (Common EFI prologue 48 83 EC 28 now executes; see TestBatchDecode.)
	code := []byte{0x06, 0xF4}
	if err := vm.MMU.LoadBytes(0x1000, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	vm.CPU().RIP = 0x1000

	err := vm.Run(100)
	if err == nil {
		t.Fatal("expected vCPU fault for unsupported opcode, got nil")
	}
	if !strings.Contains(err.Error(), "vCPU 0") || !strings.Contains(err.Error(), "unsupported opcode") {
		t.Errorf("expected attributed unsupported-opcode fault, got: %v", err)
	}
}

func TestAndALImm8(t *testing.T) {
	vm := NewVM(1024*1024, 1)

	// MOV EAX,0xF0; AND AL,0x3C; HLT  -> AL=0x30, ZF clear
	code := []byte{0xB8, 0xF0, 0x00, 0x00, 0x00, 0x24, 0x3C, 0xF4}
	if err := vm.MMU.LoadBytes(0x2000, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	vm.CPU().RIP = 0x2000

	if err := vm.Run(20); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if uint8(vm.CPU().Regs[cpu.RAX]) != 0x30 {
		t.Errorf("expected AL=0x30, got 0x%02X", uint8(vm.CPU().Regs[cpu.RAX]))
	}
	if vm.CPU().GetZF() {
		t.Errorf("expected ZF clear")
	}
	if vm.CPU().RFLAGS&cpu.FLAG_CF != 0 {
		t.Errorf("expected CF clear after AND")
	}
}

// TestBatchDecode runs a hand-assembled program exercising the batch x86_64
// decoder: REX.W forms, RIP-relative LEA, group1/2/3/5, setcc/cmov-era jcc,
// call/ret, push/pop, CPUID, RDTSC, and rep stosq.
func TestBatchDecode(t *testing.T) {
	vm := NewVM(16*1024*1024, 1)

	base := uint64(0x1000)
	code := []byte{
		0x48, 0xC7, 0xC0, 0x10, 0x00, 0x00, 0x00, // 0x00 mov rax, 0x10
		0x55,                                     // 0x07 push rbp
		0x48, 0x89, 0xE5,                         // 0x08 mov rbp, rsp
		0x48, 0x83, 0xEC, 0x20,                   // 0x0B sub rsp, 0x20
		0x48, 0x89, 0x45, 0xF8,                   // 0x0F mov [rbp-8], rax
		0x48, 0x8B, 0x5D, 0xF8,                   // 0x13 mov rbx, [rbp-8]
		0x48, 0x83, 0xFB, 0x10,                   // 0x17 cmp rbx, 0x10
		0x0F, 0x94, 0xC0,                         // 0x1B sete al
		0x0F, 0xB6, 0xC8,                         // 0x1E movzx ecx, al
		0xC1, 0xE1, 0x02,                         // 0x21 shl ecx, 2
		0x6B, 0xC9, 0x03,                         // 0x24 imul ecx, ecx, 3
		0x4C, 0x8D, 0x2D, 0x40, 0x00, 0x00, 0x00, // 0x27 lea r13, [rip+0x40] -> 0x106E
		0xB8, 0xEF, 0xBE, 0xAD, 0xDE,             // 0x2E mov eax, 0xDEADBEEF
		0x48, 0xC1, 0xC0, 0x10,                   // 0x33 rol rax, 16
		0x50,                                     // 0x37 push rax
		0x58,                                     // 0x38 pop rax
		0xE8, 0x52, 0x00, 0x00, 0x00,             // 0x39 call 0x90 (stub)
		0xB9, 0x07, 0x00, 0x00, 0x00,             // 0x3E mov ecx, 7
		0x83, 0xF9, 0x07,                         // 0x43 cmp ecx, 7
		0x74, 0x03,                               // 0x46 je 0x4B (taken, skips marker)
		0x41, 0xB5, 0xEE,                         // 0x48 mov r13b, 0xEE (must not run)
		0x0F, 0x9D, 0xC1,                         // 0x4B setge cl
		0x0F, 0xB7, 0xD9,                         // 0x4E movzx ebx, cl
		0xF6, 0xE1,                               // 0x51 mul cl
		0xD1, 0xE1,                               // 0x53 shl ecx, 1
		0x01, 0xD9,                               // 0x55 add ecx, ebx
		0x83, 0xF9, 0x03,                         // 0x57 cmp ecx, 3
		0x75, 0x2B,                               // 0x5A jne 0x87 (hlt; must not be taken)
		0xBF, 0x00, 0x20, 0x00, 0x00,             // 0x5C mov edi, 0x2000
		0x48, 0xB8, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, // 0x61 mov rax, 0x4141...
		0x48, 0xC7, 0xC1, 0x03, 0x00, 0x00, 0x00, // 0x6B mov rcx, 3
		0xF3, 0x48, 0xAB,                         // 0x72 rep stosq
		0xB8, 0x00, 0x00, 0x00, 0x00,             // 0x75 mov eax, 0
		0x0F, 0xA2,                               // 0x7A cpuid
		0x0F, 0x31,                               // 0x7C rdtsc
		0xB8, 0xEF, 0xBE, 0xAD, 0xDE,             // 0x7E mov eax, 0xDEADBEEF
		0x48, 0xC1, 0xC0, 0x10,                   // 0x83 rol rax, 16
		0xF4, // 0x87 hlt
	}
	stub := []byte{
		0x41, 0xB4, 0x99, // 0x90 mov r12b, 0x99
		0xC3, // 0x93 ret
	}
	if err := vm.MMU.LoadBytes(base, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if err := vm.MMU.LoadBytes(base+0x90, stub); err != nil {
		t.Fatalf("LoadBytes stub: %v", err)
	}

	c := vm.CPU()
	c.RIP = base
	c.Regs[cpu.RSP] = 0x7FFFF0
	if err := vm.Run(100000); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.Halted {
		t.Fatal("expected HLT")
	}

	want := map[int]uint64{
		cpu.RAX: 0xDEADBEEF0000,     // mov eax + rol rax,16 (0xDEADBEEF<<16, top bits were 0)
		cpu.RBX: 0x756E6547,         // cpuid leaf 0 vendor "Genu"
		cpu.RCX: 0x6C65746E,         // cpuid vendor "ntel" (rep stosq emptied rcx first)
		cpu.RDI: 0x2018,             // 3 rep stosq iterations
		cpu.R12: 0x99,               // call/ret reached stub
		cpu.R13: 0x106E,             // rip-relative lea; skipped marker byte proves je taken
		cpu.RBP: 0x7FFFE8,           // mov rbp, rsp after push rbp
		cpu.RSP: 0x7FFFC8,           // push/pop/call/ret net -0x28
	}
	for reg, exp := range want {
		if c.Regs[reg] != exp {
			t.Errorf("reg[%d] = 0x%X, want 0x%X", reg, c.Regs[reg], exp)
		}
	}

	// stack slot written in prologue: mov [rbp-8], rax (rax was 0x10)
	v, err := vm.MMU.Read64(0x7FFFE0)
	if err != nil || v != 0x10 {
		t.Errorf("[rbp-8] = 0x%X, err=%v, want 0x10", v, err)
	}
	// rep stosq fill
	for i := 0; i < 24; i++ {
		b, err := vm.MMU.Read8(0x2000 + uint64(i))
		if err != nil || b != 0x41 {
			t.Fatalf("mem[0x2000+%d] = 0x%X, err=%v, want 0x41", i, b, err)
		}
	}
}

type testIODev struct{ buf []byte }

func (d *testIODev) Read(port uint16, size int) (uint32, error) { return 0xAB, nil }
func (d *testIODev) Write(port uint16, val uint32, size int) error {
	d.buf = append(d.buf, byte(val))
	return nil
}

var _ bus.Device = (*testIODev)(nil)

// TestStringIO covers INS/OUTS with REP (0x6C-0x6F).
func TestStringIO(t *testing.T) {
	vm := NewVM(1024*1024, 1)
	dev := &testIODev{}
	vm.IO.Register(0xE9, dev)

	base := uint64(0x1000)
	code := []byte{
		0xBA, 0xE9, 0x00, 0x00, 0x00, // mov dx, 0xE9
		0xBE, 0x00, 0x20, 0x00, 0x00, // mov esi, 0x2000
		0xBF, 0x00, 0x30, 0x00, 0x00, // mov edi, 0x3000
		0xB9, 0x04, 0x00, 0x00, 0x00, // mov ecx, 4
		0xF3, 0x6E,                   // rep outsb
		0xBE, 0x00, 0x40, 0x00, 0x00, // mov esi, 0x4000
		0xB9, 0x04, 0x00, 0x00, 0x00, // mov ecx, 4
		0xF3, 0x6C,                   // rep insb
		0x6E,                         // outsb (single)
		0xF4,                         // hlt
	}
	if err := vm.MMU.LoadBytes(base, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	for i, v := range []byte{0x11, 0x22, 0x33, 0x44} {
		if err := vm.MMU.Write8(0x2000+uint64(i), v); err != nil {
			t.Fatal(err)
		}
	}
	if err := vm.MMU.Write8(0x4000, 0x77); err != nil {
		t.Fatal(err)
	}

	c := vm.CPU()
	c.RIP = base
	if err := vm.Run(1000); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []byte{0x11, 0x22, 0x33, 0x44, 0x77}
	if !bytes.Equal(dev.buf, want) {
		t.Errorf("device got % X, want % X", dev.buf, want)
	}
	if c.Regs[cpu.RSI] != 0x4001 {
		t.Errorf("RSI = 0x%X, want 0x4001", c.Regs[cpu.RSI])
	}
	if c.Regs[cpu.RDI] != 0x3004 {
		t.Errorf("RDI = 0x%X, want 0x3004", c.Regs[cpu.RDI])
	}
	if c.Regs[cpu.RCX] != 0 {
		t.Errorf("RCX = %d, want 0", c.Regs[cpu.RCX])
	}
	for i := 0; i < 4; i++ {
		b, err := vm.MMU.Read8(0x3000 + uint64(i))
		if err != nil || b != 0xAB {
			t.Fatalf("mem[0x3000+%d] = 0x%X, err=%v, want 0xAB", i, b, err)
		}
	}
	if !c.Halted {
		t.Fatal("expected HLT")
	}
}

func runSnippet(t *testing.T, code []byte, setup map[uint64][]byte) *VM {
	t.Helper()
	vm := NewVM(16*1024*1024, 1)
	base := uint64(0x1000)
	if err := vm.MMU.LoadBytes(base, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	for addr, data := range setup {
		if err := vm.MMU.LoadBytes(addr, data); err != nil {
			t.Fatalf("LoadBytes data @0x%X: %v", addr, err)
		}
	}
	c := vm.CPU()
	c.RIP = base
	c.Regs[cpu.RSP] = 0x7FFFF0
	if err := vm.Run(10000); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.Halted {
		t.Fatal("expected HLT")
	}
	return vm
}

// TestX87Compare covers faddp/fstp/fcom + fnstsw + sahf (compiler compare pattern).
func TestX87Compare(t *testing.T) {
	code := []byte{
		0xD9, 0xE8, // fld1
		0xD9, 0xEE, // fldz
		0xDE, 0xC1, // faddp st(1), st(0) -> 1.0
		0xDD, 0x1C, 0x25, 0x00, 0x20, 0x00, 0x00, // fstp qword [0x2000]
		0xD9, 0xE8, // fld1
		0xD9, 0xE8, // fld1
		0xD8, 0xD1, // fcom st(1) -> equal, sets C3
		0xDF, 0xE0, // fnstsw ax
		0x9E,       // sahf (ZF <- C3)
		0x0F, 0x94, 0xC1, // sete cl
		0xF4,
	}
	vm := runSnippet(t, code, nil)
	c := vm.CPU()
	v, err := vm.MMU.Read64(0x2000)
	if err != nil || v != 0x3FF0000000000000 {
		t.Errorf("fstp [0x2000] = 0x%X err=%v, want float64(1.0)", v, err)
	}
	if c.Regs[cpu.RAX]&0xFFFF != 0x4000 {
		t.Errorf("AX = 0x%X, want 0x4000 (C3 set)", c.Regs[cpu.RAX]&0xFFFF)
	}
	if c.Regs[cpu.RCX]&0xFF != 1 {
		t.Errorf("CL = %d, want 1 (ZF after sahf)", c.Regs[cpu.RCX]&0xFF)
	}
}

// TestSSEAddSS covers movss/addss round-trip through memory.
func TestSSEAddSS(t *testing.T) {
	code := []byte{
		0xF3, 0x0F, 0x10, 0x04, 0x25, 0x00, 0x21, 0x00, 0x00, // movss xmm0, [0x2100]
		0xF3, 0x0F, 0x10, 0x0C, 0x25, 0x04, 0x21, 0x00, 0x00, // movss xmm1, [0x2104]
		0xF3, 0x0F, 0x58, 0xC1,                               // addss xmm0, xmm1
		0xF3, 0x0F, 0x11, 0x04, 0x25, 0x00, 0x21, 0x00, 0x00, // movss [0x2100], xmm0
		0xF4,
	}
	data := make([]byte, 8)
	copy(data[0:], []byte{0x00, 0x00, 0xC0, 0x3F}) // 1.5f
	copy(data[4:], []byte{0x00, 0x00, 0x10, 0x40}) // 2.25f
	vm := runSnippet(t, code, map[uint64][]byte{0x2100: data})
	v, err := vm.MMU.Read32(0x2100)
	if err != nil || v != 0x40700000 {
		t.Errorf("addss result = 0x%X err=%v, want 0x40700000 (3.75f)", v, err)
	}
}

// TestLARLSL covers 0F 02/03 against a hand-built GDT plus LGDT (0F 01).
func TestLARLSL(t *testing.T) {
	code := []byte{
		0x0F, 0x01, 0x14, 0x25, 0x00, 0x30, 0x00, 0x00, // lgdt [0x3000]
		0xB9, 0x08, 0x00, 0x00, 0x00,                   // mov ecx, 8
		0x0F, 0x02, 0xC1,                               // lar eax, ecx
		0x48, 0x0F, 0x03, 0xD1,                         // lsl rdx, rcx
		0xF4,
	}
	gdt := make([]byte, 10) // LGDT operand: limit + base (points at table 0x3010)
	copy(gdt[0:], []byte{0x17, 0x00})
	copy(gdt[2:], []byte{0x10, 0x30, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	table := make([]byte, 16) // GDT: null entry + code descriptor at +8 (selector 0x08)
	copy(table[8:], []byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x9A, 0xAF, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	vm := runSnippet(t, code, map[uint64][]byte{0x3000: gdt, 0x3010: table})
	c := vm.CPU()
	if c.Regs[cpu.RAX] != 0x00AF9A00 {
		t.Errorf("LAR = 0x%X, want 0x00AF9A00", c.Regs[cpu.RAX])
	}
	if c.Regs[cpu.RDX] != 0xFFFFFFFF {
		t.Errorf("LSL = 0x%X, want 0xFFFFFFFF", c.Regs[cpu.RDX])
	}
}

// TestSSSE3Pshufb covers the 0F 38 three-byte map.
func TestSSSE3Pshufb(t *testing.T) {
	code := []byte{
		0xF3, 0x0F, 0x6F, 0x04, 0x25, 0x00, 0x40, 0x00, 0x00, // movdqu xmm0, [0x4000]
		0xF3, 0x0F, 0x6F, 0x0C, 0x25, 0x00, 0x41, 0x00, 0x00, // movdqu xmm1, [0x4100]
		0x66, 0x0F, 0x38, 0x00, 0xC1,                         // pshufb xmm0, xmm1
		0xF3, 0x0F, 0x7F, 0x04, 0x25, 0x00, 0x50, 0x00, 0x00, // movdqu [0x5000], xmm0
		0xF4,
	}
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(i)
	}
	mask := []byte{0x00, 0x80, 0x0F, 0x04, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F}
	vm := runSnippet(t, code, map[uint64][]byte{0x4000: data, 0x4100: mask})
	want := []byte{0x00, 0x00, 0x0F, 0x04, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F}
	got := make([]byte, 16)
	for i := 0; i < 16; i++ {
		b, err := vm.MMU.Read8(0x5000 + uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		got[i] = b
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pshufb result = % X, want % X", got, want)
	}
}

// TestBTBTS covers 0F A3 (BT) / 0F AB (BTS) and CF via SETcc.
func TestBTBTS(t *testing.T) {
	code := []byte{
		0x48, 0x31, 0xC0, // xor rax, rax
		0x48, 0xC7, 0xC3, 0x04, 0x00, 0x00, 0x00, // mov rbx, 4
		0x0F, 0xA3, 0xD8, // bt rax, rbx (bit clear -> CF=0)
		0x0F, 0x92, 0xC1, // setc cl
		0x0F, 0xAB, 0xD8, // bts rax, rbx (set bit 4)
		0x0F, 0xA3, 0xD8, // bt rax, rbx (bit set -> CF=1)
		0x0F, 0x92, 0xC2, // setc dl
		0xF4,
	}
	vm := runSnippet(t, code, nil)
	c := vm.CPU()
	if c.Regs[cpu.RAX] != 0x10 {
		t.Errorf("RAX = 0x%X, want 0x10 (bts set bit 4)", c.Regs[cpu.RAX])
	}
	if c.Regs[cpu.RCX]&0xFF != 0 {
		t.Errorf("CL = %d, want 0 (CF clear before bts)", c.Regs[cpu.RCX]&0xFF)
	}
	if c.Regs[cpu.RDX]&0xFF != 1 {
		t.Errorf("DL = %d, want 1 (CF set after bts)", c.Regs[cpu.RDX]&0xFF)
	}
}

// Guest MMIO write to FBAddress reaches the displayed framebuffer.
func TestFramebufferMMIOWrite(t *testing.T) {
	// mov eax, 0x44332211; movabs rbx, 0xE0000000; mov [rbx], eax; hlt
	code := []byte{
		0xB8, 0x11, 0x22, 0x33, 0x44,
		0x48, 0xBB, 0x00, 0x00, 0x00, 0xE0, 0x00, 0x00, 0x00, 0x00,
		0x89, 0x03,
		0xF4,
	}
	vm := runSnippet(t, code, nil)
	fb := vm.DRM.Framebuffer
	if fb[0] != 0x11 || fb[1] != 0x22 || fb[2] != 0x33 || fb[3] != 0x44 {
		t.Fatalf("framebuffer bytes = %02x %02x %02x %02x, want 11 22 33 44",
			fb[0], fb[1], fb[2], fb[3])
	}
}

// GOP tables: protocol points at Mode, Mode advertises FrameBufferBase/pitch.
func TestGOPTables(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	if err := uefi.SetupEFITables(vm.MMU, uefi.DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	v, err := vm.MMU.Read64(uefi.GOPProtocolAddr + 24)
	if err != nil || v != uefi.GOPModeAddr {
		t.Fatalf("GOP.Mode = 0x%x (err=%v), want 0x%x", v, err, uefi.GOPModeAddr)
	}
	if v, _ := vm.MMU.Read64(uefi.GOPModeAddr + 24); v != 0xE0000000 {
		t.Fatalf("FrameBufferBase = 0x%x, want 0xE0000000", v)
	}
	if v, _ := vm.MMU.Read64(uefi.GOPModeAddr + 32); v != 1920*1080*4 {
		t.Fatalf("FrameBufferSize = %d", v)
	}
	if v, _ := vm.MMU.Read32(uefi.GOPInfoAddr + 4); v != 1920 {
		t.Fatalf("HRes = %d", v)
	}
	if v, _ := vm.MMU.Read32(uefi.GOPInfoAddr + 12); v != 1 {
		t.Fatalf("PixelFormat = %d, want 1 (BGRX)", v)
	}
	// BootServices HandleProtocol slot points at hypercall stub, not success stub
	if v, _ := vm.MMU.Read64(uefi.BootServicesBaseAddr + 152); v < uefi.StubBase {
		t.Fatalf("HandleProtocol = 0x%x, want hypercall stub >= 0x%x", v, uefi.StubBase)
	}
}

// LocateProtocol hypercall returns GOP address for the GOP GUID.
func TestGOPLocateProtocol(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	if err := uefi.SetupEFITables(vm.MMU, uefi.DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	c := vm.CPU()
	c.Regs[cpu.RAX] = 0xE0000000 + 3 // svcLocateProtocol
	c.Regs[cpu.RCX] = uefi.GOPGUIDAddr
	out := uint64(0x20000)
	c.Regs[cpu.R8] = out
	if err := uefi.HandleService(c, vm.MMU); err != nil {
		t.Fatalf("HandleService: %v", err)
	}
	if got := c.Regs[cpu.RAX]; got != 0 {
		t.Fatalf("status = 0x%x, want EFI_SUCCESS", got)
	}
	if v, _ := vm.MMU.Read64(out); v != uefi.GOPProtocolAddr {
		t.Fatalf("interface = 0x%x, want 0x%x", v, uefi.GOPProtocolAddr)
	}
}

// Unknown GUID yields EFI_NOT_FOUND.
func TestGOPUnknownGUID(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	zero := make([]byte, 16)
	if err := vm.MMU.LoadBytes(0x30000, zero); err != nil {
		t.Fatal(err)
	}
	c := vm.CPU()
	c.Regs[cpu.RAX] = 0xE0000000 + 3
	c.Regs[cpu.RCX] = 0x30000
	c.Regs[cpu.R8] = 0x30010
	if err := uefi.HandleService(c, vm.MMU); err != nil {
		t.Fatalf("HandleService: %v", err)
	}
	if got := c.Regs[cpu.RAX]; got != 0x800000000000000E {
		t.Fatalf("status = 0x%x, want EFI_NOT_FOUND", got)
	}
}

// Blt BufferToVideo paints a guest buffer into the framebuffer.
func TestGOPBltBufferToVideo(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	if err := uefi.SetupEFITables(vm.MMU, uefi.DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	// 2x1 pixel buffer: blue then green (BGRA)
	buf := []byte{0xFF, 0x00, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF}
	if err := vm.MMU.LoadBytes(0x40000, buf); err != nil {
		t.Fatal(err)
	}
	c := vm.CPU()
	c.Regs[cpu.RAX] = 0xE0000000 + 12 // svcGopBlt
	c.Regs[cpu.RDX] = 0x40000         // BltBuffer
	c.Regs[cpu.R8] = 1                // BufferToVideo
	c.Regs[cpu.R9] = 0                // SourceX
	c.Regs[cpu.RSP] = 0x7FFFF0
	stack := []struct {
		off uint64
		val uint64
	}{
		{0x28, 0},    // SourceY
		{0x30, 10},   // DestX
		{0x38, 20},   // DestY
		{0x40, 2},    // Width
		{0x48, 1},    // Height
		{0x50, 8},    // Delta
	}
	for _, s := range stack {
		if err := vm.MMU.Write64(0x7FFFF0+s.off, s.val); err != nil {
			t.Fatal(err)
		}
	}
	if err := uefi.HandleService(c, vm.MMU); err != nil {
		t.Fatalf("HandleService: %v", err)
	}
	if got := c.Regs[cpu.RAX]; got != 0 {
		t.Fatalf("status = 0x%x", got)
	}
	off := (20*1920 + 10) * 4
	fb := vm.DRM.Framebuffer
	if fb[off] != 0xFF || fb[off+1] != 0x00 || fb[off+2] != 0x00 {
		t.Fatalf("pixel0 = %02x %02x %02x, want ff 00 00", fb[off], fb[off+1], fb[off+2])
	}
	if fb[off+4] != 0x00 || fb[off+5] != 0xFF || fb[off+6] != 0x00 {
		t.Fatalf("pixel1 = %02x %02x %02x, want 00 ff 00", fb[off+4], fb[off+5], fb[off+6])
	}
}

// ConOut OutputString renders guest text into the framebuffer.
func TestConOutOutputString(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	if err := uefi.SetupEFITables(vm.MMU, uefi.DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	str := []byte{'H', 0, 'i', 0, 0, 0}
	if err := vm.MMU.LoadBytes(0x50000, str); err != nil {
		t.Fatal(err)
	}
	c := vm.CPU()
	c.Regs[cpu.RAX] = 0xE0000000 + 20 // svcOutputString
	c.Regs[cpu.RDX] = 0x50000
	if err := uefi.HandleService(c, vm.MMU); err != nil {
		t.Fatalf("HandleService: %v", err)
	}
	if got := c.Regs[cpu.RAX]; got != 0 {
		t.Fatalf("status = 0x%x", got)
	}
	lit := 0
	for i := range vm.DRM.Framebuffer {
		if vm.DRM.Framebuffer[i] != 0 {
			lit++
		}
	}
	if lit == 0 {
		t.Fatal("framebuffer unchanged after OutputString")
	}
	if v, _ := vm.MMU.Read32(uefi.ConOutModeAddr + 12); v != 2 {
		t.Fatalf("cursor col = %d, want 2", v)
	}
}

// Full guest path: call stub with mov rax,svc; syscall; ret through the CPU.
func TestGOPQueryModeGuestStub(t *testing.T) {
	vm := NewVM(64*1024*1024, 1)
	if err := uefi.SetupEFITables(vm.MMU, uefi.DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	sizeOut := uint64(0x60000)
	infoOut := uint64(0x60008)
	// mov rdx,0; mov r8,sizeOut; mov r9,infoOut; mov rax,svcQM; syscall; hlt
	code := []byte{
		0x48, 0xC7, 0xC2, 0x00, 0x00, 0x00, 0x00,
		0x49, 0xB8, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x49, 0xB9, 0x08, 0x00, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x48, 0xB8, 0x0A, 0x00, 0x00, 0xE0, 0x00, 0x00, 0x00, 0x00,
		0x0F, 0x05,
		0xF4,
	}
	if err := vm.MMU.LoadBytes(0x1000, code); err != nil {
		t.Fatal(err)
	}
	c := vm.CPU()
	c.RIP = 0x1000
	c.Regs[cpu.RSP] = 0x7FFFF0
	if err := vm.Run(10000); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.Halted {
		t.Fatal("expected HLT")
	}
	if c.Regs[cpu.RAX] != 0 {
		t.Fatalf("QueryMode status = 0x%x", c.Regs[cpu.RAX])
	}
	if v, _ := vm.MMU.Read64(sizeOut); v != 36 {
		t.Fatalf("SizeOfInfo = %d, want 36", v)
	}
	if v, _ := vm.MMU.Read64(infoOut); v != uefi.GOPInfoAddr {
		t.Fatalf("Info = 0x%x, want 0x%x", v, uefi.GOPInfoAddr)
	}
}
