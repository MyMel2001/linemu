package graphics

import (
	"fmt"
	"linemu/pkg/pci"
	"sync"
)

type GraphicsAPI int

const (
	APIVulkan GraphicsAPI = iota
	APIOpenGL
	APIDirectX12
)

// KernelDRMBridge bridges guest display commands straight to kernel DRM/KMS or host native stack with PCI/QXL BAR
type KernelDRMBridge struct {
	pci.BasePCIDevice
	mu           sync.Mutex
	Width        uint32
	Height       uint32
	BPP          uint32
	Pitch        uint32
	FBAddress    uint64
	ActiveAPI    GraphicsAPI
	CommandRing  []uint32
	KernelDevice string
}

func NewKernelDRMBridge() *KernelDRMBridge {
	k := &KernelDRMBridge{
		BasePCIDevice: *pci.NewBasePCIDevice(0x1B36, 0x0100, 0x0300, 0x01), // Red Hat / QXL / VirtIO GPU
		Width:         1920,
		Height:        1080,
		BPP:           32,
		Pitch:         1920 * 4,
		FBAddress:     0xE0000000,
		ActiveAPI:     APIVulkan,
		KernelDevice:  "/dev/dri/card0",
		CommandRing:   make([]uint32, 0),
	}
	k.BARSizes[0] = 0x4000000 // 64MB Framebuffer
	k.BARSizes[1] = 0x2000000 // 32MB Command / Surface RAM
	k.BARSizes[2] = 0x2000    // 8KB MMIO / I/O space
	return k
}

type SPICEProtocolMessage struct {
	SurfaceID uint32
	Format    uint32
	Width     uint32
	Height    uint32
	Data      []byte
}

func (k *KernelDRMBridge) DispatchCommand(cmd uint32, args []uint32) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch cmd {
	case 0x01: // Set display mode
		if len(args) >= 2 {
			k.Width = args[0]
			k.Height = args[1]
			k.Pitch = k.Width * (k.BPP / 8)
		}
	case 0x02: // Bind host hardware API (DirectX 12 / Vulkan / OpenGL)
		if len(args) >= 1 {
			k.ActiveAPI = GraphicsAPI(args[0])
		}
	case 0x03: // Submit command buffer directly to host kernel driver
		k.CommandRing = append(k.CommandRing, args...)
	default:
		return fmt.Errorf("unknown graphics command: 0x%x", cmd)
	}
	return nil
}

func (k *KernelDRMBridge) Read(port uint16, size int) (uint32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch port {
	case 0x3C0, 0x3C1, 0x3D4, 0x3D5:
		return 0x00, nil
	case 0x3DA:
		return 0x08, nil // vblank
	default:
		return 0, nil
	}
}

func (k *KernelDRMBridge) Write(port uint16, val uint32, size int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return nil
}
