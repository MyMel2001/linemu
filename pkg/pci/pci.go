package pci

import (
	"encoding/binary"
	"sync"
)

// Standard PCI Configuration Header Offsets
const (
	PCI_VENDOR_ID   = 0x00
	PCI_DEVICE_ID   = 0x02
	PCI_COMMAND     = 0x04
	PCI_STATUS      = 0x06
	PCI_REVISION_ID = 0x08
	PCI_CLASS_PROG  = 0x09
	PCI_CLASS_DEVICE= 0x0A
	PCI_CACHE_LINE_SIZE = 0x0C
	PCI_LATENCY_TIMER   = 0x0D
	PCI_HEADER_TYPE = 0x0E
	PCI_BIST        = 0x0F
	PCI_BAR0        = 0x10
	PCI_BAR1        = 0x14
	PCI_BAR2        = 0x18
	PCI_BAR3        = 0x1C
	PCI_BAR4        = 0x20
	PCI_BAR5        = 0x24
	PCI_SUBSYSTEM_VENDOR_ID = 0x2C
	PCI_SUBSYSTEM_ID        = 0x2E
	PCI_INTERRUPT_LINE      = 0x3C
	PCI_INTERRUPT_PIN       = 0x3D
)

type PCIDevice interface {
	GetConfigSpace() *[256]byte
	BARUpdated(barIdx int, oldAddr, newAddr uint32, size uint32)
	GetBARSize(barIdx int) uint32
}

type BasePCIDevice struct {
	Config [256]byte
	BARSizes [6]uint32
}

func (d *BasePCIDevice) GetConfigSpace() *[256]byte {
	return &d.Config
}

func (d *BasePCIDevice) GetBARSize(barIdx int) uint32 {
	if barIdx >= 0 && barIdx < 6 {
		return d.BARSizes[barIdx]
	}
	return 0
}

func (d *BasePCIDevice) BARUpdated(barIdx int, oldAddr, newAddr uint32, size uint32) {
}

func NewBasePCIDevice(vendorID, deviceID uint16, classCode uint16, revision uint8) *BasePCIDevice {
	dev := &BasePCIDevice{}
	binary.LittleEndian.PutUint16(dev.Config[PCI_VENDOR_ID:], vendorID)
	binary.LittleEndian.PutUint16(dev.Config[PCI_DEVICE_ID:], deviceID)
	// Status: Fast Back-to-Back, 66MHz capable, Devsel medium
	binary.LittleEndian.PutUint16(dev.Config[PCI_STATUS:], 0x0290)
	dev.Config[PCI_REVISION_ID] = revision
	binary.LittleEndian.PutUint16(dev.Config[PCI_CLASS_DEVICE:], classCode)
	dev.Config[PCI_HEADER_TYPE] = 0x00 // standard device
	return dev
}

// PCIBus provides host bridge configuration space mechanism #1 (0xCF8 address / 0xCFC data)
type PCIBus struct {
	mu          sync.Mutex
	configAddr  uint32
	devices     map[uint16]PCIDevice // key: (bus << 8) | (slot << 3) | func
}

func NewPCIBus() *PCIBus {
	return &PCIBus{
		devices: make(map[uint16]PCIDevice),
	}
}

func DeviceKey(bus, slot, fn uint8) uint16 {
	return (uint16(bus) << 8) | (uint16(slot&0x1F) << 3) | uint16(fn&0x07)
}

func (b *PCIBus) AttachDevice(bus, slot, fn uint8, dev PCIDevice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.devices[DeviceKey(bus, slot, fn)] = dev
}

func (b *PCIBus) Read(port uint16, size int) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if port == 0xCF8 { // PCI_CONFIG_ADDRESS
		return b.configAddr, nil
	}

	if port >= 0xCFC && port <= 0xCFF { // PCI_CONFIG_DATA
		if (b.configAddr & 0x80000000) == 0 { // Enable bit not set
			return 0xFFFFFFFF, nil
		}
		bus := uint8((b.configAddr >> 16) & 0xFF)
		slot := uint8((b.configAddr >> 11) & 0x1F)
		fn := uint8((b.configAddr >> 8) & 0x07)
		regOffset := uint8(b.configAddr&0xFC) + uint8(port-0xCFC)

		key := DeviceKey(bus, slot, fn)
		dev, ok := b.devices[key]
		if !ok {
			return 0xFFFFFFFF, nil
		}

		cfg := dev.GetConfigSpace()
		switch size {
		case 1:
			return uint32(cfg[regOffset]), nil
		case 2:
			return uint32(binary.LittleEndian.Uint16(cfg[regOffset : regOffset+2])), nil
		case 4:
			return binary.LittleEndian.Uint32(cfg[regOffset : regOffset+4]), nil
		}
	}

	return 0xFFFFFFFF, nil
}

func (b *PCIBus) Write(port uint16, val uint32, size int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if port == 0xCF8 {
		b.configAddr = val
		return nil
	}

	if port >= 0xCFC && port <= 0xCFF {
		if (b.configAddr & 0x80000000) == 0 {
			return nil
		}
		bus := uint8((b.configAddr >> 16) & 0xFF)
		slot := uint8((b.configAddr >> 11) & 0x1F)
		fn := uint8((b.configAddr >> 8) & 0x07)
		regOffset := uint8(b.configAddr&0xFC) + uint8(port-0xCFC)

		key := DeviceKey(bus, slot, fn)
		dev, ok := b.devices[key]
		if !ok {
			return nil
		}

		cfg := dev.GetConfigSpace()

		// Intercept BAR sizing queries and configuration
		if regOffset >= PCI_BAR0 && regOffset <= PCI_BAR5 && size == 4 {
			barIdx := int((regOffset - PCI_BAR0) / 4)
			barSize := dev.GetBARSize(barIdx)
			if val == 0xFFFFFFFF && barSize > 0 {
				// OS probes BAR size: write encoded size mask
				mask := ^(barSize - 1)
				binary.LittleEndian.PutUint32(cfg[regOffset:], mask)
				return nil
			}
			oldVal := binary.LittleEndian.Uint32(cfg[regOffset:])
			binary.LittleEndian.PutUint32(cfg[regOffset:], val)
			dev.BARUpdated(barIdx, oldVal, val, barSize)
			return nil
		}

		switch size {
		case 1:
			cfg[regOffset] = byte(val)
		case 2:
			binary.LittleEndian.PutUint16(cfg[regOffset:regOffset+2], uint16(val))
		case 4:
			binary.LittleEndian.PutUint32(cfg[regOffset:regOffset+4], val)
		}
	}

	return nil
}
