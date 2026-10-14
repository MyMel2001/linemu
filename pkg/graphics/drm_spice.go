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
	Framebuffer  []byte
	Renderer     *DisplayRenderer
	Input        InputDevice
}

func NewKernelDRMBridge() *KernelDRMBridge {
	w := uint32(1920)
	h := uint32(1080)
	fbSize := w * h * 4

	k := &KernelDRMBridge{
		BasePCIDevice: *pci.NewBasePCIDevice(0x1B36, 0x0100, 0x0300, 0x01), // Red Hat / QXL / VirtIO GPU
		Width:         w,
		Height:        h,
		BPP:           32,
		Pitch:         w * 4,
		FBAddress:     0xE0000000,
		ActiveAPI:     APIVulkan,
		KernelDevice:  "/dev/dri/card0",
		CommandRing:   make([]uint32, 0),
		Framebuffer:   make([]byte, fbSize),
		Renderer:      NewDisplayRenderer(DisplayWindow, w, h),
	}
	k.BARSizes[0] = 0x4000000 // 64MB Framebuffer
	k.BARSizes[1] = 0x2000000 // 32MB Command / Surface RAM
	k.BARSizes[2] = 0x2000    // 8KB MMIO / I/O space
	return k
}

func (k *KernelDRMBridge) AttachRenderer(renderer *DisplayRenderer) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Renderer = renderer
	if renderer != nil && k.Input != nil {
		renderer.AttachInput(k.Input)
	}
}

func (k *KernelDRMBridge) AttachInput(input InputDevice) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.Input = input
	if k.Renderer != nil {
		k.Renderer.AttachInput(input)
	}
}

// WritePixel updates a pixel at (x, y) with color in BGRA format
func (k *KernelDRMBridge) WritePixel(x, y uint32, b, g, r, a uint8) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if x >= k.Width || y >= k.Height {
		return
	}
	offset := (y*k.Width + x) * 4
	if offset+3 < uint32(len(k.Framebuffer)) {
		k.Framebuffer[offset] = b
		k.Framebuffer[offset+1] = g
		k.Framebuffer[offset+2] = r
		k.Framebuffer[offset+3] = a
	}
}

// Flush presents current framebuffer to live onscreen display
func (k *KernelDRMBridge) Flush() {
	k.mu.Lock()
	renderer := k.Renderer
	fb := k.Framebuffer
	k.mu.Unlock()

	if renderer != nil {
		renderer.PresentFrame(fb)
	}
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
			k.Framebuffer = make([]byte, k.Width*k.Height*4)
			if k.Renderer != nil {
				k.Renderer.Width = k.Width
				k.Renderer.Height = k.Height
			}
		}
	case 0x02: // Bind host hardware API (DirectX 12 / Vulkan / OpenGL)
		if len(args) >= 1 {
			k.ActiveAPI = GraphicsAPI(args[0])
		}
	case 0x03: // Submit command buffer directly to host kernel driver
		k.CommandRing = append(k.CommandRing, args...)
	case 0x04: // Flush / Swap buffers to screen
		if k.Renderer != nil {
			k.Renderer.PresentFrame(k.Framebuffer)
		}
	case 0x05: // Mouse input event (args: [dx, dy, buttons])
		if k.Input != nil && len(args) >= 3 {
			k.Input.SendMouseEvent(int(int32(args[0])), int(int32(args[1])), uint8(args[2]))
		}
	case 0x06: // Keyboard input event (args: [scancode, pressed])
		if k.Input != nil && len(args) >= 1 {
			if len(args) > 1 && args[1] == 0 {
				k.Input.SendScancode(uint8(args[0]) | 0x80)
			} else {
				k.Input.SendScancode(uint8(args[0]))
			}
		}
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
