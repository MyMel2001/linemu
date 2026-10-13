package main

import (
	"bytes"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/syscalls"
	"testing"
)

func TestCPUMovAndAdd(t *testing.T) {
	vm := NewVM(1024 * 1024)

	// x86_64 code:
	// MOV EAX, 10      (B8 0A 00 00 00)
	// MOV EBX, 20      (BB 14 00 00 00)
	// ADD EBX, EAX     (01 C3)
	// HLT              (F4)
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

	if vm.CPU.Regs[cpu.RAX] != 10 {
		t.Errorf("expected RAX=10, got %d", vm.CPU.Regs[cpu.RAX])
	}
	if vm.CPU.Regs[cpu.RBX] != 30 {
		t.Errorf("expected RBX=30, got %d", vm.CPU.Regs[cpu.RBX])
	}
	if !vm.CPU.Halted {
		t.Errorf("expected CPU to be halted")
	}
}

func TestJITTranslationLoop(t *testing.T) {
	vm := NewVM(1024 * 1024)
	vm.UseJIT = true

	// JIT block:
	// MOV EAX, 50 (B8 32 00 00 00)
	// SUB EAX, 20 -> MOV EDX, 20 (BA 14 00 00 00) + SUB EAX, EDX (29 D0)
	// HLT         (F4)
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

	if vm.CPU.Regs[cpu.RAX] != 30 {
		t.Errorf("expected EAX=30 from JIT, got %d", vm.CPU.Regs[cpu.RAX])
	}
	if !vm.CPU.Halted {
		t.Errorf("expected CPU to halt")
	}
}

func TestPCIBusConfiguration(t *testing.T) {
	vm := NewVM(1024 * 1024)

	// Probe NIC Vendor/Device ID via PCI Config ports
	// Bus 0, Slot 2 (NIC), Reg 0 -> 0x80000000 | (2 << 11) = 0x80001000
	if err := vm.IO.Out(0xCF8, 0x80001000, 4); err != nil {
		t.Fatalf("Out CF8: %v", err)
	}

	val, err := vm.IO.In(0xCFC, 4)
	if err != nil {
		t.Fatalf("In CFC: %v", err)
	}

	// Intel 82540EM: Vendor 0x8086, Device 0x100E -> 0x100E8086
	if val != 0x100E8086 {
		t.Errorf("expected PCI ID 0x100e8086, got 0x%08x", val)
	}

	// BAR0 size query
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
	// Expected mask for 128KB (0x20000): 0xFFFE0000
	if barSizeMask != 0xFFFE0000 {
		t.Errorf("expected BAR size mask 0xFFFE0000, got 0x%08x", barSizeMask)
	}
}

func TestSyscallTranslation(t *testing.T) {
	vm := NewVM(1024 * 1024)
	vm.UseJIT = false

	vm.CPU.Regs[cpu.RAX] = syscalls.SYS_BRK
	vm.CPU.Regs[cpu.RDI] = 0x40001000

	code := []byte{0x0F, 0x05, 0xF4}
	if err := vm.MMU.LoadBytes(0, code); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if err := vm.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if vm.CPU.Regs[cpu.RAX] != 0x40001000 {
		t.Errorf("expected brk=0x40001000, got 0x%x", vm.CPU.Regs[cpu.RAX])
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

func TestDeviceStorage(t *testing.T) {
	storage := devices.NewStorageController(0x1F0)
	ramdisk := devices.NewMemoryBlockDevice(10)
	storage.Attach(0, devices.DriveHDD, ramdisk)

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

	// Test IDENTIFY DEVICE (0xEC)
	_ = storage.Write(0x1F7, 0xEC, 1)
	idWord, _ := storage.Read(0x1F0, 2)
	if idWord != 0x0040 {
		t.Errorf("expected IDENTIFY signature 0x0040, got 0x%04x", idWord)
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
}
