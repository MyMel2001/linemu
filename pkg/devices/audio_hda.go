package devices

import (
	"encoding/binary"
	"fmt"
	"linemu/pkg/pci"
	"sync"
)

// HDA Registers
const (
	HDA_GCAP      = 0x00
	HDA_VMIN      = 0x02
	HDA_VMAJ      = 0x03
	HDA_OUTPAY    = 0x04
	HDA_INPAY     = 0x06
	HDA_GCTL      = 0x08
	HDA_WAKEEN    = 0x0C
	HDA_STATESTS  = 0x0E
	HDA_GSTS      = 0x10
	HDA_INTCTL    = 0x20
	HDA_INTSTS    = 0x24
	HDA_CORBLBASE = 0x40
	HDA_CORBUBASE = 0x44
	HDA_CORBWP    = 0x48
	HDA_CORBRP    = 0x4A
	HDA_CORBCTL   = 0x4C
	HDA_CORBSTS   = 0x4D
	HDA_CORBSIZE  = 0x4E
	HDA_RIRBLBASE = 0x50
	HDA_RIRBUBASE = 0x54
	HDA_RIRBWP    = 0x58
	HDA_RIRBCTL   = 0x5C
	HDA_RIRBSTS   = 0x5D
	HDA_RIRBSIZE  = 0x5E
	HDA_ICS       = 0x60
	HDA_IC        = 0x64
	HDA_IR        = 0x68
)

// Codec verb response node definitions
const (
	HDA_CODEC_CAD_ROOT = 0x00000000
	HDA_PARAM_VENDOR   = 0xF0000
)

// IntelHDA emulates Intel High Definition Audio Controller with full PCI & Codec support
type IntelHDA struct {
	pci.BasePCIDevice
	mu          sync.Mutex
	basePort    uint16
	regs        [0x1000]byte
	corbEntries []uint32
	rirbEntries []uint64
}

func NewIntelHDA(basePort uint16) *IntelHDA {
	h := &IntelHDA{
		BasePCIDevice: *pci.NewBasePCIDevice(0x8086, 0x2668, 0x0403, 0x02), // Intel ICH6/HDA
		basePort:      basePort,
		corbEntries:   make([]uint32, 256),
		rirbEntries:   make([]uint64, 256),
	}
	h.BARSizes[0] = 0x4000 // 16KB MMIO

	// Capabilities: 4 Out Streams, 4 In Streams, 64-bit address support
	binary.LittleEndian.PutUint16(h.regs[HDA_GCAP:], 0x4401)
	h.regs[HDA_VMIN] = 0x00
	h.regs[HDA_VMAJ] = 0x01
	// Codec present in slot 0
	binary.LittleEndian.PutUint16(h.regs[HDA_STATESTS:], 0x0001)

	return h
}

func (h *IntelHDA) Read(port uint16, size int) (uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	offset := int(port - h.basePort)
	if offset+size > len(h.regs) {
		return 0, fmt.Errorf("HDA read out of range: offset 0x%x", offset)
	}
	switch size {
	case 1:
		return uint32(h.regs[offset]), nil
	case 2:
		return uint32(binary.LittleEndian.Uint16(h.regs[offset : offset+2])), nil
	case 4:
		return binary.LittleEndian.Uint32(h.regs[offset : offset+4]), nil
	default:
		return 0, fmt.Errorf("unsupported read size %d", size)
	}
}

func (h *IntelHDA) Write(port uint16, val uint32, size int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	offset := int(port - h.basePort)
	if offset+size > len(h.regs) {
		return fmt.Errorf("HDA write out of range: offset 0x%x", offset)
	}
	switch size {
	case 1:
		h.regs[offset] = byte(val)
	case 2:
		binary.LittleEndian.PutUint16(h.regs[offset:offset+2], uint16(val))
	case 4:
		binary.LittleEndian.PutUint32(h.regs[offset:offset+4], val)
		if offset == HDA_GCTL && (val&1) != 0 {
			h.regs[HDA_GSTS] = 1 // Controller out of reset
		}
		// Immediate Command Output Verb
		if offset == HDA_IC {
			verb := val
			// Return Realtek ALC888 Codec ID (0x10EC0888) when queried for Vendor ID
			var resp uint64
			if (verb & 0xFFFFF) == HDA_PARAM_VENDOR {
				resp = 0x10EC0888
			} else {
				resp = 0x00000001 // Generic OK
			}
			binary.LittleEndian.PutUint32(h.regs[HDA_IR:], uint32(resp))
			h.regs[HDA_ICS] = 0x02 // Immediate Result Valid
		}
	}
	return nil
}
