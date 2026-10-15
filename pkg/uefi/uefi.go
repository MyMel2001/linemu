package uefi

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
	"strings"
	"unicode/utf16"
)

type BootDeviceType string

const (
	BootTypeCDROM  BootDeviceType = "cdrom"
	BootTypeHDD    BootDeviceType = "hdd"
	BootTypeFloppy BootDeviceType = "floppy"
	BootTypeUSB    BootDeviceType = "usb"
	BootTypeNet    BootDeviceType = "network"
)

const (
	EFISystemTableSignature    = 0x5453595320494249 // "IBI SYST"
	EFIBootServicesSignature   = 0x56524553544f4f42 // "BOOTSERV"
	EFIRuntimeServicesSignature = 0x5652455354554e52 // "RUNTSERV"

	SystemTableBaseAddr     uint64 = 0x100000 // 1MB mark
	BootServicesBaseAddr    uint64 = 0x100400
	RuntimeServicesBaseAddr uint64 = 0x100800
	ConOutBaseAddr          uint64 = 0x100C00
	LoadedImageBaseAddr     uint64 = 0x101000
	DefaultStackPointer     uint64 = 0x7FFFF0
	DefaultPayloadBase      uint64 = 0x1000000 // 16MB mark
)

// ParseBootOrder converts string specifications (e.g. "cd,hdd", "d,c", "iso,img") into typed boot priorities
func ParseBootOrder(spec string) []BootDeviceType {
	if strings.TrimSpace(spec) == "" {
		return []BootDeviceType{BootTypeCDROM, BootTypeHDD, BootTypeUSB}
	}

	parts := strings.Split(spec, ",")
	order := make([]BootDeviceType, 0, len(parts))

	for _, p := range parts {
		item := strings.ToLower(strings.TrimSpace(p))
		switch item {
		case "d", "cd", "cdrom", "iso", "dvd":
			order = append(order, BootTypeCDROM)
		case "c", "hdd", "hd", "disk", "img":
			order = append(order, BootTypeHDD)
		case "a", "b", "floppy", "fd":
			order = append(order, BootTypeFloppy)
		case "u", "usb":
			order = append(order, BootTypeUSB)
		case "n", "net", "network":
			order = append(order, BootTypeNet)
		default:
			// Check if integer drive ID specified e.g. "0", "1"
			if len(item) > 0 && item[0] >= '0' && item[0] <= '9' {
				order = append(order, BootDeviceType(item))
			}
		}
	}

	if len(order) == 0 {
		return []BootDeviceType{BootTypeCDROM, BootTypeHDD, BootTypeUSB}
	}
	return order
}

// SetupEFITables initializes minimal UEFI Specification tables in guest RAM
func SetupEFITables(ram *mmu.RAM, payloadBase uint64) error {
	// 1. EFI_SYSTEM_TABLE at SystemTableBaseAddr
	// Signature (8 bytes): "IBI SYST"
	_ = ram.Write64(SystemTableBaseAddr, EFISystemTableSignature)
	// Revision (4 bytes): 2.10 (0x00020064)
	_ = ram.Write32(SystemTableBaseAddr+8, 0x00020064)
	// HeaderSize (4 bytes): 120 bytes
	_ = ram.Write32(SystemTableBaseAddr+12, 120)

	// Vendor string in UCS-2 / UTF-16
	vendorStr := "LinEmu UEFI Firmware"
	vendorRunes := utf16.Encode([]rune(vendorStr))
	vendorAddr := SystemTableBaseAddr + 0x200
	for i, r := range vendorRunes {
		_ = ram.Write16(vendorAddr+uint64(i*2), r)
	}
	_ = ram.Write16(vendorAddr+uint64(len(vendorRunes)*2), 0)

	_ = ram.Write64(SystemTableBaseAddr+24, vendorAddr)              // FirmwareVendor
	_ = ram.Write32(SystemTableBaseAddr+32, 0x00010000)              // FirmwareRevision
	_ = ram.Write64(SystemTableBaseAddr+64, ConOutBaseAddr)          // ConOut
	_ = ram.Write64(SystemTableBaseAddr+72, ConOutBaseAddr)          // StdErr
	_ = ram.Write64(SystemTableBaseAddr+88, RuntimeServicesBaseAddr) // RuntimeServices
	_ = ram.Write64(SystemTableBaseAddr+96, BootServicesBaseAddr)    // BootServices

	// 2. Simple RET stub at 0x100080 for functions returning EFI_SUCCESS (0)
	// XOR EAX, EAX (31 C0); RET (C3)
	stubCode := []byte{0x31, 0xC0, 0xC3}
	stubAddr := SystemTableBaseAddr + 0x80
	if err := ram.LoadBytes(stubAddr, stubCode); err != nil {
		return err
	}

	// 3. EFI_BOOT_SERVICES at BootServicesBaseAddr
	_ = ram.Write64(BootServicesBaseAddr, EFIBootServicesSignature)
	_ = ram.Write32(BootServicesBaseAddr+8, 0x00020064)
	_ = ram.Write32(BootServicesBaseAddr+12, 400)
	// Populate critical function pointers (ExitBootServices, GetMemoryMap, AllocatePages) with stub
	for offset := uint64(24); offset < 400; offset += 8 {
		_ = ram.Write64(BootServicesBaseAddr+offset, stubAddr)
	}

	// 4. EFI_RUNTIME_SERVICES at RuntimeServicesBaseAddr
	_ = ram.Write64(RuntimeServicesBaseAddr, EFIRuntimeServicesSignature)
	_ = ram.Write32(RuntimeServicesBaseAddr+8, 0x00020064)
	_ = ram.Write32(RuntimeServicesBaseAddr+12, 112)
	for offset := uint64(24); offset < 112; offset += 8 {
		_ = ram.Write64(RuntimeServicesBaseAddr+offset, stubAddr)
	}

	// 5. EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL at ConOutBaseAddr
	_ = ram.Write64(ConOutBaseAddr+8, stubAddr) // OutputString pointer

	// 6. Loaded Image Protocol at LoadedImageBaseAddr
	_ = ram.Write32(LoadedImageBaseAddr, 0x00010000)             // Revision
	_ = ram.Write64(LoadedImageBaseAddr+32, payloadBase)         // ImageBase
	_ = ram.Write64(LoadedImageBaseAddr+40, 1024*1024)          // ImageSize dummy

	return nil
}

// FindEFIBootPayload scans a block device for UEFI executable (GPT ESP, MBR ESP, El Torito, FAT, or raw PE)
func FindEFIBootPayload(dev devices.BlockStorage) ([]byte, error) {
	if dev == nil || dev.TotalSectors() == 0 {
		return nil, fmt.Errorf("device empty or unreadable")
	}

	// 1. Check GPT Partition Table (LBA 1 has "EFI PART")
	var gptHeader [512]byte
	if err := dev.ReadSector(1, gptHeader[:]); err == nil && string(gptHeader[0:8]) == "EFI PART" {
		partLBA := binary.LittleEndian.Uint64(gptHeader[72:80])
		numParts := binary.LittleEndian.Uint32(gptHeader[80:84])
		partSize := binary.LittleEndian.Uint32(gptHeader[84:88])
		if partSize >= 128 && numParts > 0 && partLBA > 0 {
			// Search for EFI System Partition GUID: C12A7328-F81F-11D2-BA4B-00A0C93EC93B
			// Little-endian bytes: 28 73 2A C1 1F F8 D2 11 BA 4B 00 A0 C9 3E C9 3B
			espGUID := []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
			var entryBuf [512]byte
			curSec := uint64(^uint64(0))
			for i := uint32(0); i < numParts && i < 16; i++ {
				secOffset := (i * partSize) % 512
				secIndex := partLBA + uint64((i*partSize)/512)
				if secIndex != curSec {
					curSec = secIndex
					_ = dev.ReadSector(curSec, entryBuf[:])
				}
				if bytes.Equal(entryBuf[secOffset:secOffset+16], espGUID) {
					startLBA := binary.LittleEndian.Uint64(entryBuf[secOffset+32 : secOffset+40])
					if payload, err := extractFATPayload(dev, startLBA); err == nil && len(payload) > 0 {
						return payload, nil
					}
				}
			}
		}
	}

	// 2. Check MBR Partition Table (LBA 0)
	var mbr [512]byte
	if err := dev.ReadSector(0, mbr[:]); err == nil && mbr[510] == 0x55 && mbr[511] == 0xAA {
		for i := 0; i < 4; i++ {
			entryOffset := 446 + (i * 16)
			pType := mbr[entryOffset+4]
			// 0xEF = EFI System Partition, 0x0B/0x0C = FAT32, 0x01/0x04/0x06/0x0E = FAT12/FAT16
			if pType == 0xEF || pType == 0x0B || pType == 0x0C || pType == 0x06 || pType == 0x0E {
				startLBA := uint64(binary.LittleEndian.Uint32(mbr[entryOffset+8 : entryOffset+12]))
				if startLBA > 0 {
					if payload, err := extractFATPayload(dev, startLBA); err == nil && len(payload) > 0 {
						return payload, nil
					}
				}
			}
		}
	}

	// 3. Check ISO9660 El Torito Boot Record (Sector 17 / LBA 68 in 512-byte blocks)
	var isoBootRec [512]byte
	for _, lba := range []uint64{68, 17} {
		if err := dev.ReadSector(lba, isoBootRec[:]); err == nil {
			if string(isoBootRec[7:39]) == "EL TORITO SPECIFICATION\x00\x00\x00\x00\x00\x00\x00\x00\x00" ||
				bytes.Contains(isoBootRec[:], []byte("EL TORITO SPECIFICATION")) {
				catLBA := uint64(binary.LittleEndian.Uint32(isoBootRec[71:75]))
				if catLBA > 0 {
					// Convert ISO 2048-byte sector to 512-byte LBA
					catalogSector := catLBA * 4
					var cat [512]byte
					if err := dev.ReadSector(catalogSector, cat[:]); err == nil {
						// Look for Section Header with EFI Platform ID (0xEF)
						for offset := 32; offset < 512; offset += 32 {
							if cat[offset] == 0x90 || cat[offset] == 0x91 { // Section header
								if cat[offset+1] == 0xEF { // Platform ID 0xEF = EFI
									entrySec := catalogSector + uint64(offset/512)
									_ = dev.ReadSector(entrySec, cat[:])
									loadLBA := uint64(binary.LittleEndian.Uint32(cat[offset+32+8 : offset+32+12]))
									if loadLBA > 0 {
										if payload, err := extractFATPayload(dev, loadLBA*4); err == nil && len(payload) > 0 {
											return payload, nil
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// 4. Check Raw FAT filesystem at LBA 0 (superfloppy format)
	if payload, err := extractFATPayload(dev, 0); err == nil && len(payload) > 0 {
		return payload, nil
	}

	// 5. Fast Scan: Direct search for PE32+ header or BOOTX64.EFI signatures in first sectors
	// ponytail: scan first 8192 sectors for PE32+ EFI image when partition tables are omitted
	var sec [512]byte
	maxProbe := dev.TotalSectors()
	if maxProbe > 4096 {
		maxProbe = 4096
	}
	for s := uint64(0); s < maxProbe; s++ {
		if err := dev.ReadSector(s, sec[:]); err != nil {
			break
		}
		if sec[0] == 'M' && sec[1] == 'Z' {
			peOff := binary.LittleEndian.Uint32(sec[0x3C:0x40])
			if peOff < 500 && peOff+4 < 512 {
				if sec[peOff] == 'P' && sec[peOff+1] == 'E' && sec[peOff+2] == 0 && sec[peOff+3] == 0 {
					// Read consecutive sectors to form binary payload
					buf := make([]byte, 64*1024)
					for j := uint64(0); j < 128 && s+j < dev.TotalSectors(); j++ {
						_ = dev.ReadSector(s+j, buf[j*512:(j+1)*512])
					}
					return buf, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no UEFI boot payload found on device")
}

// extractFATPayload inspects a FAT12/FAT16/FAT32 filesystem for an EFI boot binary
func extractFATPayload(dev devices.BlockStorage, startLBA uint64) ([]byte, error) {
	var bpb [512]byte
	if err := dev.ReadSector(startLBA, bpb[:]); err != nil {
		return nil, err
	}

	bytesPerSec := binary.LittleEndian.Uint16(bpb[11:13])
	secPerCluster := uint64(bpb[13])
	reservedSecs := binary.LittleEndian.Uint16(bpb[14:16])
	numFATs := uint64(bpb[16])
	rootEntries := binary.LittleEndian.Uint16(bpb[17:19])
	secPerFAT16 := binary.LittleEndian.Uint16(bpb[22:24])
	secPerFAT32 := binary.LittleEndian.Uint32(bpb[36:40])

	if bytesPerSec != 512 || secPerCluster == 0 || reservedSecs == 0 {
		return nil, fmt.Errorf("invalid FAT BPB")
	}

	var rootLBA uint64
	var fatSize uint64
	if secPerFAT16 != 0 {
		fatSize = uint64(secPerFAT16)
		rootLBA = startLBA + uint64(reservedSecs) + (numFATs * fatSize)
	} else if secPerFAT32 != 0 {
		fatSize = uint64(secPerFAT32)
		rootCluster := uint64(binary.LittleEndian.Uint32(bpb[44:48]))
		if rootCluster < 2 {
			rootCluster = 2
		}
		dataLBA := startLBA + uint64(reservedSecs) + (numFATs * fatSize)
		rootLBA = dataLBA + (rootCluster-2)*secPerCluster
	} else {
		return nil, fmt.Errorf("unrecognized FAT type")
	}

	// Scan Root Directory for EFI directory or BOOTX64.EFI
	var dirSec [512]byte
	for s := uint64(0); s < 64; s++ {
		if err := dev.ReadSector(rootLBA+s, dirSec[:]); err != nil {
			break
		}
		for entry := 0; entry < 512; entry += 32 {
			if dirSec[entry] == 0x00 { // End of entries
				break
			}
			if dirSec[entry] == 0xE5 { // Deleted entry
				continue
			}

			name := string(dirSec[entry : entry+11])
			// Check if file is directly an EFI binary or matches BOOTX64
			if strings.HasSuffix(name, "EFI") || strings.Contains(name, "BOOT") {
				fileSize := binary.LittleEndian.Uint32(dirSec[entry+28 : entry+32])
				clusterLow := binary.LittleEndian.Uint16(dirSec[entry+26 : entry+28])
				clusterHigh := binary.LittleEndian.Uint16(dirSec[entry+20 : entry+22])
				cluster := (uint32(clusterHigh) << 16) | uint32(clusterLow)

				if cluster >= 2 && fileSize > 0 && fileSize < 32*1024*1024 {
					dataLBA := startLBA + uint64(reservedSecs) + (numFATs * fatSize) + (uint64(rootEntries)*32+511)/512
					fileLBA := dataLBA + uint64(cluster-2)*secPerCluster
					payload := make([]byte, ((fileSize+511)/512)*512)
					for j := uint64(0); j < uint64(len(payload)/512); j++ {
						_ = dev.ReadSector(fileLBA+j, payload[j*512:(j+1)*512])
					}
					return payload[:fileSize], nil
				}
			}
		}
	}

	return nil, fmt.Errorf("EFI binary not found in FAT")
}

// LoadEFIPayload parses and maps PE32+ executable sections into guest physical RAM
func LoadEFIPayload(ram *mmu.RAM, state *cpu.CPUState, payload []byte) (uint64, error) {
	if len(payload) == 0 {
		return 0, fmt.Errorf("payload empty")
	}

	// Attempt standard PE parsing via stdlib debug/pe
	peFile, err := pe.NewFile(bytes.NewReader(payload))
	if err == nil {
		defer peFile.Close()

		var entryPoint uint64
		var imageBase uint64 = DefaultPayloadBase

		switch opt := peFile.OptionalHeader.(type) {
		case *pe.OptionalHeader64:
			entryPoint = uint64(opt.AddressOfEntryPoint)
			if opt.ImageBase != 0 {
				imageBase = opt.ImageBase
			}
		case *pe.OptionalHeader32:
			entryPoint = uint64(opt.AddressOfEntryPoint)
			if opt.ImageBase != 0 {
				imageBase = uint64(opt.ImageBase)
			}
		}

		for _, sec := range peFile.Sections {
			data, err := sec.Data()
			if err != nil {
				continue
			}
			destAddr := imageBase + uint64(sec.VirtualAddress)
			_ = ram.LoadBytes(destAddr, data)
		}

		return imageBase + entryPoint, nil
	}

	// Fallback: raw executable / test binary loaded directly at DefaultPayloadBase
	// ponytail: load unformatted binary image at default 16MB base
	if err := ram.LoadBytes(DefaultPayloadBase, payload); err != nil {
		return 0, err
	}
	return DefaultPayloadBase, nil
}

// BootUEFI configures guest CPU and memory to boot the specified block device in 64-bit UEFI mode
func BootUEFI(ram *mmu.RAM, c *cpu.CPUState, dev devices.BlockStorage) (uint64, error) {
	payload, err := FindEFIBootPayload(dev)
	if err != nil {
		return 0, err
	}

	entryPoint, err := LoadEFIPayload(ram, c, payload)
	if err != nil {
		return 0, fmt.Errorf("failed loading EFI payload: %w", err)
	}

	if err := SetupEFITables(ram, DefaultPayloadBase); err != nil {
		return 0, fmt.Errorf("failed configuring EFI tables: %w", err)
	}

	// UEFI x86_64 Calling Convention:
	// RCX = ImageHandle
	// RDX = SystemTable
	// RSP = Stack pointer
	// RIP = Entry point
	c.Regs[cpu.RCX] = LoadedImageBaseAddr
	c.Regs[cpu.RDX] = SystemTableBaseAddr
	c.Regs[cpu.RSP] = DefaultStackPointer
	c.RIP = entryPoint
	c.RFLAGS = 0x202
	c.CR0 = 0x80000011
	c.Halted = false

	return entryPoint, nil
}
