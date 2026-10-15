package graphics

import (
	"encoding/base64"
	"fmt"
	"linemu/pkg/pci"
	"sync"
)

var font8x8Data []byte

func init() {
	var err error
	font8x8Data, err = base64.StdEncoding.DecodeString("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGDw8GBgAGAA2NgAAAAAAADY2fzZ/NjYADD4DHjAfDAAAYzMYDGZjABw2HG47M24ABgYDAAAAAAAYDAYGBgwYAAYMGBgYDAYAAGY8/zxmAAAADAw/DAwAAAAAAAAADAwGAAAAPwAAAAAAAAAAAAwMAGAwGAwGAwEAPmNze29nPgAMDgwMDAw/AB4zMBwGMz8AHjMwHDAzHgA4PDYzfzB4AD8DHzAwMx4AHAYDHzMzHgA/MzAYDAwMAB4zMx4zMx4AHjMzPjAYDgAADAwAAAwMAAAMDAAADAwGGAwGAwYMGAAAAD8AAD8AAAYMGDAYDAYAHjMwGAwADAA+Y3t7ewMeAAweMzM/MzMAP2ZmPmZmPwA8ZgMDA2Y8AB82ZmZmNh8Af0YWHhZGfwB/RhYeFgYPADxmAwNzZnwAMzMzPzMzMwAeDAwMDAweAHgwMDAzMx4AZ2Y2HjZmZwAPBgYGRmZ/AGN3f39rY2MAY2dve3NjYwAcNmNjYzYcAD9mZj4GBg8AHjMzMzseOAA/ZmY+NmZnAB4zBw44Mx4APy0MDAwMHgAzMzMzMzM/ADMzMzMzHgwAY2Nja393YwBjYzYcHDZjADMzMx4MDB4Af2MxGExmfwAeBgYGBgYeAAMGDBgwYEAAHhgYGBgYHgAIHDZjAAAAAAAAAAAAAAD/DAwYAAAAAAAAAB4wPjNuAAcGBj5mZjsAAAAeMwMzHgA4MDA+MzNuAAAAHjM/Ax4AHDYGDwYGDwAAAG4zMz4wHwcGNm5mZmcADAAODAwMHgAwADAwMDMzHgcGZjYeNmcADgwMDAwMHgAAADN/f2tjAAAAHzMzMzMAAAAeMzMzHgAAADtmZj4GDwAAbjMzPjB4AAA7bmYGDwAAAD4DHjAfAAgMPgwMLBgAAAAzMzMzbgAAADMzMx4MAAAAY2t/fzYAAABjNhw2YwAAADMzMz4wHwAAPxkMJj8AOAwMBwwMOAAYGBgAGBgYAAcMDDgMDAcAbjsAAAAAAAAAAAAAAAAAAA==")
	if err != nil {
		panic(err)
	}
}

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
	// OnFramebufferChange notifies the memory bus owner that Framebuffer was reallocated
	// (mode switch), so stale MMIO mappings can be refreshed.
	OnFramebufferChange func(fb []byte)
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

// Font8x8Glyph returns the 8-byte bitmap glyph for ASCII ch, or nil if out of range.
func Font8x8Glyph(ch byte) []byte {
	off := int(ch) * 8
	if off+8 > len(font8x8Data) {
		return nil
	}
	return font8x8Data[off : off+8]
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

// FillRect fills rectangle in Framebuffer with BGRA color
func (k *KernelDRMBridge) FillRect(rx, ry, rw, rh uint32, b, g, r uint8) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for y := ry; y < ry+rh && y < k.Height; y++ {
		rowStart := y * k.Width * 4
		for x := rx; x < rx+rw && x < k.Width; x++ {
			offset := rowStart + x*4
			k.Framebuffer[offset] = b
			k.Framebuffer[offset+1] = g
			k.Framebuffer[offset+2] = r
			k.Framebuffer[offset+3] = 0xFF
		}
	}
}

// DrawText renders string text at (x, y) with scale and color (b, g, r)
func (k *KernelDRMBridge) DrawText(startX, startY uint32, text string, b, g, r uint8, scale uint32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	curX := startX
	curY := startY
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\n' {
			curX = startX
			curY += 10 * scale
			continue
		}
		if int(ch)*8+8 <= len(font8x8Data) {
			glyph := font8x8Data[int(ch)*8 : int(ch)*8+8]
			for row := uint32(0); row < 8; row++ {
				rowBits := glyph[row]
				py := curY + row*scale
				if py >= k.Height {
					continue
				}
				for col := uint32(0); col < 8; col++ {
					if (rowBits & (1 << col)) != 0 {
						px := curX + col*scale
						for sy := uint32(0); sy < scale && py+sy < k.Height; sy++ {
							rowStart := (py + sy) * k.Width * 4
							for sx := uint32(0); sx < scale && px+sx < k.Width; sx++ {
								offset := rowStart + (px+sx)*4
								k.Framebuffer[offset] = b
								k.Framebuffer[offset+1] = g
								k.Framebuffer[offset+2] = r
								k.Framebuffer[offset+3] = 0xFF
							}
						}
					}
				}
			}
		}
		curX += 8 * scale
	}
}

// RenderBootScreen paints firmware boot screen onto Framebuffer
func (k *KernelDRMBridge) RenderBootScreen(ramMB uint64, cores int, displayMode string, bootStatus string) {
	// Deep navy background (0x22, 0x14, 0x0A)
	k.FillRect(0, 0, k.Width, k.Height, 0x22, 0x14, 0x0A)

	// Top banner bar (0x35, 0x22, 0x16)
	k.FillRect(0, 0, k.Width, 90, 0x35, 0x22, 0x16)
	k.FillRect(0, 90, k.Width, 4, 0x88, 0x55, 0x22)

	// Title
	k.DrawText(60, 25, "LinEmu UEFI Firmware v2.10", 0xFF, 0xFF, 0xFF, 4)

	// Box in center
	boxX := uint32(60)
	boxY := uint32(140)
	boxW := k.Width - 120
	boxH := k.Height - 240
	k.FillRect(boxX, boxY, boxW, boxH, 0x2C, 0x1A, 0x10)
	k.FillRect(boxX, boxY, boxW, 2, 0x55, 0x33, 0x20)
	k.FillRect(boxX, boxY+boxH-2, boxW, 2, 0x55, 0x33, 0x20)

	// Information lines
	textX := boxX + 40
	textY := boxY + 40
	spacing := uint32(40)

	k.DrawText(textX, textY, "System Architecture : x86_64", 0xEE, 0xCC, 0xAA, 3)
	textY += spacing
	k.DrawText(textX, textY, fmt.Sprintf("Processor Cores     : %d vCPU core(s)", cores), 0xEE, 0xCC, 0xAA, 3)
	textY += spacing
	k.DrawText(textX, textY, fmt.Sprintf("System Memory (RAM) : %d MB", ramMB), 0xEE, 0xCC, 0xAA, 3)
	textY += spacing
	k.DrawText(textX, textY, fmt.Sprintf("Graphics Display    : %dx%d 32bpp (%s)", k.Width, k.Height, displayMode), 0xEE, 0xCC, 0xAA, 3)
	textY += spacing
	k.DrawText(textX, textY, "PCI Subsystem       : Initialized (Host Bridge 0xCF8/0xCFC)", 0xEE, 0xCC, 0xAA, 3)
	textY += spacing + 20

	k.DrawText(textX, textY, fmt.Sprintf("Boot Status         : %s", bootStatus), 0x80, 0xFF, 0x80, 3)

	// Footer bar
	k.FillRect(0, k.Height-60, k.Width, 60, 0x1A, 0x0F, 0x08)
	k.DrawText(60, k.Height-42, "LinEmu x86_64 Hardware Translation Emulator - Press 'q' or close window to exit", 0xAA, 0xAA, 0xAA, 2)
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
			if k.OnFramebufferChange != nil {
				k.OnFramebufferChange(k.Framebuffer)
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
