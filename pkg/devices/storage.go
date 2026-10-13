package devices

import (
	"encoding/binary"
	"fmt"
	"linemu/pkg/pci"
	"sync"
)

type DriveType int

const (
	DriveHDD DriveType = iota
	DriveCDDVD
	DriveFloppy
	DriveUSB
)

type BlockStorage interface {
	ReadSector(lba uint64, buf []byte) error
	WriteSector(lba uint64, buf []byte) error
	TotalSectors() uint64
}

type MemoryBlockDevice struct {
	mu      sync.RWMutex
	data    []byte
	sectors uint64
}

func NewMemoryBlockDevice(sectors uint64) *MemoryBlockDevice {
	return &MemoryBlockDevice{
		data:    make([]byte, sectors*512),
		sectors: sectors,
	}
}

func (m *MemoryBlockDevice) ReadSector(lba uint64, buf []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if lba >= m.sectors || len(buf) < 512 {
		return fmt.Errorf("read out of bounds: lba=%d", lba)
	}
	copy(buf[:512], m.data[lba*512:(lba+1)*512])
	return nil
}

func (m *MemoryBlockDevice) WriteSector(lba uint64, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lba >= m.sectors || len(buf) < 512 {
		return fmt.Errorf("write out of bounds: lba=%d", lba)
	}
	copy(m.data[lba*512:(lba+1)*512], buf[:512])
	return nil
}

func (m *MemoryBlockDevice) TotalSectors() uint64 {
	return m.sectors
}

// StorageController manages IDE/ATA & AHCI with Bus Master DMA
type StorageController struct {
	pci.BasePCIDevice
	mu         sync.Mutex
	basePort   uint16
	drives     map[int]BlockStorage
	types      map[int]DriveType
	curDrive   int
	curLBA     uint64
	curSecCnt  uint8
	buf        [512]byte
	bufIdx     int
	// Bus Master IDE registers (8 bytes per channel)
	bmCmd      uint8
	bmStatus   uint8
	bmPRDTable uint32
}

func NewStorageController(basePort uint16) *StorageController {
	s := &StorageController{
		BasePCIDevice: *pci.NewBasePCIDevice(0x8086, 0x7010, 0x0101, 0x00), // Intel PIIX3 IDE / ATA controller
		basePort:      basePort,
		drives:        make(map[int]BlockStorage),
		types:         make(map[int]DriveType),
	}
	s.BARSizes[0] = 0x08   // Primary IDE
	s.BARSizes[1] = 0x04   // Primary Control
	s.BARSizes[4] = 0x10   // Bus Master I/O
	return s
}

func (s *StorageController) Attach(driveID int, dType DriveType, storage BlockStorage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drives[driveID] = storage
	s.types[driveID] = dType
}

func (s *StorageController) Read(port uint16, size int) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset := port - s.basePort

	// Bus Master DMA registers (Offset 0x08 - 0x0F)
	if offset >= 0x08 && offset <= 0x0F {
		switch offset - 0x08 {
		case 0:
			return uint32(s.bmCmd), nil
		case 2:
			return uint32(s.bmStatus), nil
		case 4:
			return s.bmPRDTable, nil
		}
		return 0, nil
	}

	// Legacy IDE Task File (0x00 - 0x07)
	switch offset {
	case 0: // Data port (0x1F0)
		if s.bufIdx < 512 {
			val := uint32(s.buf[s.bufIdx])
			s.bufIdx++
			if size == 2 && s.bufIdx < 512 {
				val |= uint32(s.buf[s.bufIdx]) << 8
				s.bufIdx++
			}
			return val, nil
		}
		return 0, nil
	case 7: // Status port (0x1F7)
		status := uint32(0x40) // DRDY
		if s.bufIdx < 512 {
			status |= 0x08 // DRQ
		}
		return status, nil
	default:
		return 0, nil
	}
}

func (s *StorageController) Write(port uint16, val uint32, size int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset := port - s.basePort

	// Bus Master DMA write
	if offset >= 0x08 && offset <= 0x0F {
		switch offset - 0x08 {
		case 0:
			s.bmCmd = uint8(val)
			if (s.bmCmd & 1) != 0 {
				s.bmStatus |= 0x01 // Active DMA transfer
			} else {
				s.bmStatus &= ^uint8(0x01)
			}
		case 2:
			s.bmStatus = uint8(val)
		case 4:
			s.bmPRDTable = val
		}
		return nil
	}

	// Legacy IDE registers
	switch offset {
	case 0: // Data Port write
		if s.bufIdx < 512 {
			s.buf[s.bufIdx] = byte(val)
			s.bufIdx++
			if size == 2 && s.bufIdx < 512 {
				s.buf[s.bufIdx] = byte(val >> 8)
				s.bufIdx++
			}
		}
		if s.bufIdx >= 512 {
			if drv, ok := s.drives[s.curDrive]; ok {
				_ = drv.WriteSector(s.curLBA, s.buf[:])
			}
		}
	case 2: // Sector count
		s.curSecCnt = uint8(val)
	case 3: // LBA 0-7
		s.curLBA = (s.curLBA & ^uint64(0xFF)) | uint64(val&0xFF)
	case 4: // LBA 8-15
		s.curLBA = (s.curLBA & ^uint64(0xFF00)) | (uint64(val&0xFF) << 8)
	case 5: // LBA 16-23
		s.curLBA = (s.curLBA & ^uint64(0xFF0000)) | (uint64(val&0xFF) << 16)
	case 6: // Drive select / LBA 24-27
		s.curDrive = int((val >> 4) & 1)
		s.curLBA = (s.curLBA & ^uint64(0xF000000)) | (uint64(val&0x0F) << 24)
	case 7: // Command port
		switch uint8(val) {
		case 0x20: // READ SECTORS (PIO)
			if drv, ok := s.drives[s.curDrive]; ok {
				_ = drv.ReadSector(s.curLBA, s.buf[:])
				s.bufIdx = 0
			}
		case 0x30: // WRITE SECTORS (PIO)
			s.bufIdx = 0
		case 0xC8, 0xCA: // READ DMA / WRITE DMA
			if drv, ok := s.drives[s.curDrive]; ok {
				_ = drv.ReadSector(s.curLBA, s.buf[:])
				s.bmStatus |= 0x04 // DMA Interrupt pending
			}
		case 0xEC: // IDENTIFY DEVICE
			s.generateIdentifyData()
			s.bufIdx = 0
		}
	}
	return nil
}

func (s *StorageController) generateIdentifyData() {
	var identify [512]byte
	// General configuration
	binary.LittleEndian.PutUint16(identify[0:], 0x0040) // Fixed disk
	binary.LittleEndian.PutUint16(identify[2:], 16383)  // Cylinders
	binary.LittleEndian.PutUint16(identify[6:], 16)     // Heads
	binary.LittleEndian.PutUint16(identify[12:], 63)    // Sectors per track
	// Serial number
	copy(identify[20:40], "LINEMU_VIRTUAL_DISK ")
	// Model number
	copy(identify[54:94], "LINEMU HARDDISK CONTROLLER              ")
	// Capabilities: LBA and DMA support
	binary.LittleEndian.PutUint16(identify[98:], 0x0300)
	// Total sectors 28-bit LBA
	binary.LittleEndian.PutUint32(identify[120:], 268435455)
	copy(s.buf[:], identify[:])
}
