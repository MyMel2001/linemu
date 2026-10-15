package uefi

import (
	"encoding/binary"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
	"testing"
)

func TestParseBootOrder(t *testing.T) {
	tests := []struct {
		input    string
		expected []BootDeviceType
	}{
		{"cd,hdd", []BootDeviceType{BootTypeCDROM, BootTypeHDD}},
		{"iso,img", []BootDeviceType{BootTypeCDROM, BootTypeHDD}},
		{"d,c", []BootDeviceType{BootTypeCDROM, BootTypeHDD}},
		{"hdd,iso,usb", []BootDeviceType{BootTypeHDD, BootTypeCDROM, BootTypeUSB}},
		{"0,1", []BootDeviceType{"0", "1"}},
		{"", []BootDeviceType{BootTypeCDROM, BootTypeHDD, BootTypeUSB}},
	}

	for _, tt := range tests {
		got := ParseBootOrder(tt.input)
		if len(got) != len(tt.expected) {
			t.Fatalf("ParseBootOrder(%q) length mismatch: got %d, want %d", tt.input, len(got), len(tt.expected))
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("ParseBootOrder(%q)[%d] = %s, want %s", tt.input, i, got[i], tt.expected[i])
			}
		}
	}
}

func TestSetupEFITables(t *testing.T) {
	ram := mmu.NewRAM(16 * 1024 * 1024)
	if err := SetupEFITables(ram, DefaultPayloadBase); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}

	// Verify System Table Signature
	sig, err := ram.Read64(SystemTableBaseAddr)
	if err != nil || sig != EFISystemTableSignature {
		t.Errorf("expected system table signature 0x%X, got 0x%X (err: %v)", EFISystemTableSignature, sig, err)
	}

	// Verify Boot Services Signature
	bsSig, err := ram.Read64(BootServicesBaseAddr)
	if err != nil || bsSig != EFIBootServicesSignature {
		t.Errorf("expected boot services signature 0x%X, got 0x%X", EFIBootServicesSignature, bsSig)
	}

	// Verify Runtime Services Signature
	rsSig, err := ram.Read64(RuntimeServicesBaseAddr)
	if err != nil || rsSig != EFIRuntimeServicesSignature {
		t.Errorf("expected runtime services signature 0x%X, got 0x%X", EFIRuntimeServicesSignature, rsSig)
	}

	// Verify ConOut pointer
	conOutPtr, _ := ram.Read64(SystemTableBaseAddr + 64)
	if conOutPtr != ConOutBaseAddr {
		t.Errorf("expected ConOut pointer 0x%X, got 0x%X", ConOutBaseAddr, conOutPtr)
	}

	// Test executing stub code at 0x100080 (31 C0 C3 = XOR EAX, EAX; RET)
	c := cpu.NewCPUState()
	c.Regs[cpu.RSP] = 0x200000
	retAddr := uint64(0x300000)
	_ = ram.Write64(c.Regs[cpu.RSP], retAddr)
	c.RIP = SystemTableBaseAddr + 0x80

	// Step XOR EAX, EAX
	if err := c.Step(ram, nil); err != nil {
		t.Fatalf("Step XOR: %v", err)
	}
	if c.Regs[cpu.RAX] != 0 {
		t.Errorf("expected RAX=0 after EFI stub, got %d", c.Regs[cpu.RAX])
	}

	// Step RET
	if err := c.Step(ram, nil); err != nil {
		t.Fatalf("Step RET: %v", err)
	}
	if c.RIP != retAddr {
		t.Errorf("expected RIP=0x%X after RET, got 0x%X", retAddr, c.RIP)
	}
}

func TestFindEFIBootPayloadRawAndGPT(t *testing.T) {
	dev := devices.NewMemoryBlockDevice(100)

	// Build protective MBR and GPT Header at Sector 1
	var gptHeader [512]byte
	copy(gptHeader[0:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(gptHeader[8:12], 0x00010000) // Revision
	binary.LittleEndian.PutUint32(gptHeader[12:16], 92)         // HeaderSize
	binary.LittleEndian.PutUint64(gptHeader[72:80], 2)          // PartitionEntryLBA = 2
	binary.LittleEndian.PutUint32(gptHeader[80:84], 1)          // NumberOfPartitionEntries = 1
	binary.LittleEndian.PutUint32(gptHeader[84:88], 128)        // SizeOfPartitionEntry = 128
	_ = dev.WriteSector(1, gptHeader[:])

	// Partition Entry at Sector 2: ESP GUID
	var partSec [512]byte
	espGUID := []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	copy(partSec[0:16], espGUID)
	binary.LittleEndian.PutUint64(partSec[32:40], 10) // Start LBA = 10
	binary.LittleEndian.PutUint64(partSec[40:48], 20) // End LBA = 20
	_ = dev.WriteSector(2, partSec[:])

	// FAT Boot Sector at Sector 10
	var fatBPB [512]byte
	binary.LittleEndian.PutUint16(fatBPB[11:13], 512) // bytesPerSec
	fatBPB[13] = 1                                    // secPerCluster
	binary.LittleEndian.PutUint16(fatBPB[14:16], 1)   // reservedSecs
	fatBPB[16] = 1                                    // numFATs
	binary.LittleEndian.PutUint16(fatBPB[17:19], 16)  // rootEntries
	binary.LittleEndian.PutUint16(fatBPB[22:24], 1)   // secPerFAT16 = 1
	_ = dev.WriteSector(10, fatBPB[:])

	// Root dir at Sector 10 + 1 + 1 = 12
	var dirSec [512]byte
	copy(dirSec[0:11], []byte("BOOTX64 EFI"))
	binary.LittleEndian.PutUint16(dirSec[26:28], 2)   // cluster = 2
	binary.LittleEndian.PutUint32(dirSec[28:32], 512) // file size = 512
	_ = dev.WriteSector(12, dirSec[:])

	// Data cluster 2 at 10 + 1 + 1 + 1 = 13
	var payloadSec [512]byte
	payloadSec[0] = 0x90 // NOP
	payloadSec[1] = 0xF4 // HLT
	_ = dev.WriteSector(13, payloadSec[:])

	payload, err := FindEFIBootPayload(dev)
	if err != nil {
		t.Fatalf("FindEFIBootPayload: %v", err)
	}
	if len(payload) != 512 || payload[0] != 0x90 || payload[1] != 0xF4 {
		t.Errorf("unexpected payload content")
	}

	// Test full UEFI boot execution
	ram := mmu.NewRAM(32 * 1024 * 1024)
	c := cpu.NewCPUState()
	entry, err := BootUEFI(ram, c, dev)
	if err != nil {
		t.Fatalf("BootUEFI: %v", err)
	}
	if entry != DefaultPayloadBase {
		t.Errorf("expected entry=0x%X, got 0x%X", DefaultPayloadBase, entry)
	}
	if c.Regs[cpu.RCX] != LoadedImageBaseAddr || c.Regs[cpu.RDX] != SystemTableBaseAddr {
		t.Errorf("UEFI registers not set properly: RCX=0x%X, RDX=0x%X", c.Regs[cpu.RCX], c.Regs[cpu.RDX])
	}
}
