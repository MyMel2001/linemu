package devices

import (
	"encoding/binary"
	"fmt"
	"linemu/pkg/pci"
	"os"
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

// MemoryBlockDevice is an in-memory block storage
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

// FileBlockDevice provides image file persistence (ISO, IMG, raw virtual disk)
type FileBlockDevice struct {
	mu      sync.RWMutex
	file    *os.File
	sectors uint64
}

func OpenFileBlockDevice(path string, readOnly bool) (*FileBlockDevice, error) {
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &FileBlockDevice{
		file:    f,
		sectors: uint64(info.Size() / 512),
	}, nil
}

func CreateVirtualDiskImage(path string, sizeMB int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(sizeMB * 1024 * 1024)
}

func (f *FileBlockDevice) ReadSector(lba uint64, buf []byte) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if lba >= f.sectors || len(buf) < 512 {
		return fmt.Errorf("read out of bounds: lba=%d", lba)
	}
	_, err := f.file.ReadAt(buf[:512], int64(lba*512))
	return err
}

func (f *FileBlockDevice) WriteSector(lba uint64, buf []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lba >= f.sectors || len(buf) < 512 {
		return fmt.Errorf("write out of bounds: lba=%d", lba)
	}
	_, err := f.file.WriteAt(buf[:512], int64(lba*512))
	return err
}

func (f *FileBlockDevice) TotalSectors() uint64 {
	return f.sectors
}

func (f *FileBlockDevice) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Close()
}

type DriveInfo struct {
	UniqueID string
	Type     DriveType
	Storage  BlockStorage
	Model    string
	Serial   string
}

// StorageController manages IDE/ATA & AHCI with unique device identifiers
type StorageController struct {
	pci.BasePCIDevice
	mu         sync.Mutex
	basePort   uint16
	drives     map[int]*DriveInfo
	curDrive   int
	curLBA     uint64
	curSecCnt  uint8
	buf        [512]byte
	bufIdx     int
	bmCmd      uint8
	bmStatus   uint8
	bmPRDTable uint32
}

func NewStorageController(basePort uint16) *StorageController {
	s := &StorageController{
		BasePCIDevice: *pci.NewBasePCIDevice(0x8086, 0x7010, 0x0101, 0x00),
		basePort:      basePort,
		drives:        make(map[int]*DriveInfo),
	}
	s.BARSizes[0] = 0x08
	s.BARSizes[1] = 0x04
	s.BARSizes[4] = 0x10
	return s
}

func (s *StorageController) Attach(driveID int, uniqueID string, dType DriveType, storage BlockStorage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drives[driveID] = &DriveInfo{
		UniqueID: uniqueID,
		Type:     dType,
		Storage:  storage,
		Model:    fmt.Sprintf("LINEMU DRIVE %s", uniqueID),
		Serial:   fmt.Sprintf("SN-%s", uniqueID),
	}
}

func (s *StorageController) GetDriveInfo(driveID int) (*DriveInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.drives[driveID]
	return info, ok
}

func (s *StorageController) Read(port uint16, size int) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset := port - s.basePort

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

	switch offset {
	case 0:
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
	case 7:
		status := uint32(0x40)
		if s.bufIdx < 512 {
			status |= 0x08
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

	if offset >= 0x08 && offset <= 0x0F {
		switch offset - 0x08 {
		case 0:
			s.bmCmd = uint8(val)
			if (s.bmCmd & 1) != 0 {
				s.bmStatus |= 0x01
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

	switch offset {
	case 0:
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
				_ = drv.Storage.WriteSector(s.curLBA, s.buf[:])
			}
		}
	case 2:
		s.curSecCnt = uint8(val)
	case 3:
		s.curLBA = (s.curLBA & ^uint64(0xFF)) | uint64(val&0xFF)
	case 4:
		s.curLBA = (s.curLBA & ^uint64(0xFF00)) | (uint64(val&0xFF) << 8)
	case 5:
		s.curLBA = (s.curLBA & ^uint64(0xFF0000)) | (uint64(val&0xFF) << 16)
	case 6:
		s.curDrive = int((val >> 4) & 1)
		s.curLBA = (s.curLBA & ^uint64(0xF000000)) | (uint64(val&0x0F) << 24)
	case 7:
		switch uint8(val) {
		case 0x20:
			if drv, ok := s.drives[s.curDrive]; ok {
				_ = drv.Storage.ReadSector(s.curLBA, s.buf[:])
				s.bufIdx = 0
			}
		case 0x30:
			s.bufIdx = 0
		case 0xC8, 0xCA:
			if drv, ok := s.drives[s.curDrive]; ok {
				_ = drv.Storage.ReadSector(s.curLBA, s.buf[:])
				s.bmStatus |= 0x04
			}
		case 0xEC:
			s.generateIdentifyData(s.curDrive)
			s.bufIdx = 0
		}
	}
	return nil
}

func (s *StorageController) generateIdentifyData(driveID int) {
	var identify [512]byte
	binary.LittleEndian.PutUint16(identify[0:], 0x0040)
	binary.LittleEndian.PutUint16(identify[2:], 16383)
	binary.LittleEndian.PutUint16(identify[6:], 16)
	binary.LittleEndian.PutUint16(identify[12:], 63)

	serial := "LINEMU_DISK_000000  "
	model := "LINEMU HARDDISK CONTROLLER              "
	if drv, ok := s.drives[driveID]; ok {
		if len(drv.Serial) > 0 {
			copy(identify[20:40], fmt.Sprintf("%-20s", drv.Serial))
		}
		if len(drv.Model) > 0 {
			copy(identify[54:94], fmt.Sprintf("%-40s", drv.Model))
		}
	} else {
		copy(identify[20:40], serial)
		copy(identify[54:94], model)
	}

	binary.LittleEndian.PutUint16(identify[98:], 0x0300)
	binary.LittleEndian.PutUint32(identify[120:], 268435455)
	copy(s.buf[:], identify[:])
}
