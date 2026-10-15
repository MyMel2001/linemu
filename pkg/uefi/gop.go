package uefi

// UEFI Graphics Output Protocol (GOP) + text console hypercall services.
//
// BootServices/ConOut/GOP function pointers point at tiny guest stubs:
//
//	mov rax, <service id>   ; 48 B8 imm64
//	syscall                 ; 0F 05
//	ret                     ; C3
//
// SYSCALL traps to the host (cpu.ErrSyscallTrapped); stepCPU routes service
// ids in [EFIHypercallBase, EFIHypercallBase+0x100) here instead of the Linux
// syscall table. Microsoft x64 ABI applies: args in RCX/RDX/R8/R9, stack args
// at [RSP+0x28..]. Status returns in RAX.

import (
	"encoding/binary"
	"linemu/pkg/cpu"
	"linemu/pkg/graphics"
	"linemu/pkg/mmu"
)

const (
	GOPProtocolAddr  uint64 = 0x101400
	GOPModeAddr      uint64 = 0x101420
	GOPInfoAddr      uint64 = 0x101450
	ConOutModeAddr   uint64 = 0x101480
	ConInAddr        uint64 = 0x1014A0
	GOPGUIDAddr      uint64 = 0x1014C0
	TextOutGUIDAddr  uint64 = 0x101500
	TextInGUIDAddr   uint64 = 0x101510
	LoadedImgGUIDAdr uint64 = 0x101520
	StubBase         uint64 = 0x101600

	GOPInfoSize = 36

	// SuccessStubAddr is the xor eax,eax; ret stub written by SetupEFITables
	SuccessStubAddr = SystemTableBaseAddr + 0x80
)

// Stub slots at StubBase, 16 bytes each
const (
	stubHandleProtocol = StubBase + 0*16
	stubOpenProtocol   = StubBase + 1*16
	stubLocateProtocol = StubBase + 2*16
	stubGopQueryMode   = StubBase + 3*16
	stubGopSetMode     = StubBase + 4*16
	stubGopBlt         = StubBase + 5*16
	stubOutputString   = StubBase + 6*16
	stubClearScreen    = StubBase + 7*16
	stubReadKeyStroke  = StubBase + 8*16
)

// Hypercall service ids (SYSCALL with RAX in this range)
const (
	EFIHypercallBase = 0xE0000000
	svcHandleProtocol = EFIHypercallBase + 1
	svcOpenProtocol   = EFIHypercallBase + 2
	svcLocateProtocol = EFIHypercallBase + 3
	svcGopQueryMode   = EFIHypercallBase + 10
	svcGopSetMode     = EFIHypercallBase + 11
	svcGopBlt         = EFIHypercallBase + 12
	svcOutputString   = EFIHypercallBase + 20
	svcClearScreen    = EFIHypercallBase + 21
)

// EFI_STATUS error codes
const (
	EFIStatusSuccess           uint64 = 0
	EFIStatusInvalidParameter  uint64 = 0x8000000000000002
	EFIStatusUnsupported       uint64 = 0x8000000000000003
	EFIStatusNotFound          uint64 = 0x800000000000000E
)

// EFI_NOT_READY for ConIn.ReadKeyStroke
var notReadyStub = []byte{0x48, 0xB8, 0x06, 0, 0, 0, 0, 0, 0, 0x80, 0xC3}

// Protocol GUIDs in EFI_GUID memory layout (Data1/Data2/Data3 little-endian, Data4 raw)
var (
	gopGUID = [16]byte{0xDE, 0xA9, 0x42, 0x90, 0xDC, 0x23, 0x38, 0x4A,
		0x96, 0xFB, 0x7A, 0xDE, 0xD0, 0x80, 0x51, 0x6A}
	textOutGUID = [16]byte{0xC2, 0x77, 0x74, 0x38, 0xC7, 0x69, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
	textInGUID = [16]byte{0xC1, 0x77, 0x74, 0x38, 0xC7, 0x69, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
	loadedImgGUID = [16]byte{0xA1, 0x31, 0x1B, 0x5B, 0x62, 0x95, 0xD2, 0x11,
		0x8E, 0x3F, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
)

// IsHypercall reports whether a trapped SYSCALL RAX targets an EFI service.
func IsHypercall(nr uint64) bool {
	return nr >= EFIHypercallBase && nr < EFIHypercallBase+0x100
}

func writeServiceStub(ram *mmu.RAM, addr, svc uint64) error {
	code := []byte{0x48, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0x0F, 0x05, 0xC3}
	binary.LittleEndian.PutUint64(code[2:], svc)
	return ram.LoadBytes(addr, code)
}

// SetupGOP installs the EFI_GRAPHICS_OUTPUT_PROTOCOL, ConIn, GUIDs and
// hypercall stubs describing a linear framebuffer at fbBase.
func SetupGOP(ram *mmu.RAM, fbBase uint64, fbWidth, fbHeight uint32) error {
	stubs := []uint64{svcHandleProtocol, svcOpenProtocol, svcLocateProtocol,
		svcGopQueryMode, svcGopSetMode, svcGopBlt, svcOutputString, svcClearScreen}
	for i, svc := range stubs {
		if err := writeServiceStub(ram, StubBase+uint64(i)*16, svc); err != nil {
			return err
		}
	}
	if err := ram.LoadBytes(stubReadKeyStroke, notReadyStub); err != nil {
		return err
	}

	// EFI_GRAPHICS_OUTPUT_PROTOCOL { QueryMode*, SetMode*, Blt*, Mode* }
	_ = ram.Write64(GOPProtocolAddr+0, stubGopQueryMode)
	_ = ram.Write64(GOPProtocolAddr+8, stubGopSetMode)
	_ = ram.Write64(GOPProtocolAddr+16, stubGopBlt)
	_ = ram.Write64(GOPProtocolAddr+24, GOPModeAddr)

	// EFI_GRAPHICS_OUTPUT_PROTOCOL_MODE
	_ = ram.Write32(GOPModeAddr+0, 1) // MaxMode
	_ = ram.Write32(GOPModeAddr+4, 0) // Mode
	_ = ram.Write64(GOPModeAddr+8, GOPInfoAddr)
	_ = ram.Write64(GOPModeAddr+16, GOPInfoSize)
	_ = ram.Write64(GOPModeAddr+24, fbBase)
	_ = ram.Write64(GOPModeAddr+32, uint64(fbWidth)*uint64(fbHeight)*4)

	// EFI_GRAPHICS_OUTPUT_PROTOCOL_MODE_INFO
	// PixelFormat 1 = PixelBlueGreenRedReserved8BitPerColor (byte0=blue),
	// matching the DRM framebuffer byte order (b,g,r,a).
	_ = ram.Write32(GOPInfoAddr+4, fbWidth)
	_ = ram.Write32(GOPInfoAddr+8, fbHeight)
	_ = ram.Write32(GOPInfoAddr+12, 1)
	_ = ram.Write32(GOPInfoAddr+32, fbWidth) // PixelsPerScanLine

	// EFI_SIMPLE_TEXT_OUTPUT_MODE at ConOutModeAddr
	_ = ram.Write32(ConOutModeAddr+0, 1)  // MaxMode
	_ = ram.Write32(ConOutModeAddr+8, 7)  // Attribute (light grey on black)
	_ = ram.Write32(ConOutModeAddr+20, 1) // CursorVisible

	// EFI_SIMPLE_TEXT_INPUT_PROTOCOL at ConInAddr
	_ = ram.Write64(ConInAddr+0, SuccessStubAddr)         // Reset
	_ = ram.Write64(ConInAddr+8, stubReadKeyStroke) // ReadKeyStroke -> EFI_NOT_READY
	_ = ram.Write64(ConInAddr+16, SuccessStubAddr)         // WaitForKey

	// Protocol GUIDs stored in guest RAM for HandleProtocol/LocateProtocol compares
	for _, g := range []struct {
		addr  uint64
		bytes [16]byte
	}{
		{GOPGUIDAddr, gopGUID},
		{TextOutGUIDAddr, textOutGUID},
		{TextInGUIDAddr, textInGUID},
		{LoadedImgGUIDAdr, loadedImgGUID},
	} {
		if err := ram.LoadBytes(g.addr, g.bytes[:]); err != nil {
			return err
		}
	}
	return nil
}

// SetupProtocolOverrides points protocol-dispatch boot services and ConOut
// members at hypercall stubs. Call after SetupEFITables filled the tables.
func SetupProtocolOverrides(ram *mmu.RAM) {
	// EFI_BOOT_SERVICES offsets: HandleProtocol 152, OpenProtocol 280, LocateProtocol 320
	_ = ram.Write64(BootServicesBaseAddr+152, stubHandleProtocol)
	_ = ram.Write64(BootServicesBaseAddr+280, stubOpenProtocol)
	_ = ram.Write64(BootServicesBaseAddr+320, stubLocateProtocol)

	// EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL at ConOutBaseAddr: fill all members with
	// success stub, then override OutputString (+8) and ClearScreen (+48).
	for off := uint64(0); off < 64; off += 8 {
		_ = ram.Write64(ConOutBaseAddr+off, SuccessStubAddr)
	}
	_ = ram.Write64(ConOutBaseAddr+8, stubOutputString)
	_ = ram.Write64(ConOutBaseAddr+48, stubClearScreen)
	_ = ram.Write64(ConOutBaseAddr+72, ConOutModeAddr) // Mode*
}

// HandleService executes an EFI hypercall trapped from a guest stub.
func HandleService(state *cpu.CPUState, ram *mmu.RAM) error {
	switch state.Regs[cpu.RAX] {
	case svcHandleProtocol, svcOpenProtocol:
		// (Handle, Protocol*, Interface**) — GUID in RDX
		return guidDispatch(state, ram, state.Regs[cpu.RDX], state.Regs[cpu.R8])
	case svcLocateProtocol:
		// (Protocol*, Registration, Interface**) — GUID in RCX
		return guidDispatch(state, ram, state.Regs[cpu.RCX], state.Regs[cpu.R8])
	case svcGopQueryMode:
		return gopQueryMode(state, ram)
	case svcGopSetMode:
		if state.Regs[cpu.RDX] != 0 {
			state.Regs[cpu.RAX] = EFIStatusUnsupported
			return nil
		}
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	case svcGopBlt:
		return gopBlt(state, ram)
	case svcOutputString:
		return conOutOutputString(state, ram)
	case svcClearScreen:
		return conOutClearScreen(state, ram)
	default:
		state.Regs[cpu.RAX] = EFIStatusUnsupported
		return nil
	}
}

func guidDispatch(state *cpu.CPUState, ram *mmu.RAM, guidAddr, outAddr uint64) error {
	var g [16]byte
	for i := 0; i < 16; i++ {
		b, err := ram.Read8(guidAddr + uint64(i))
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		g[i] = b
	}
	var iface uint64
	switch g {
	case gopGUID:
		iface = GOPProtocolAddr
	case textOutGUID:
		iface = ConOutBaseAddr
	case textInGUID:
		iface = ConInAddr
	case loadedImgGUID:
		iface = LoadedImageBaseAddr
	default:
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	if outAddr != 0 {
		_ = ram.Write64(outAddr, iface)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func gopQueryMode(state *cpu.CPUState, ram *mmu.RAM) error {
	if state.Regs[cpu.RDX] != 0 { // MaxMode = 1
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if p := state.Regs[cpu.R8]; p != 0 {
		_ = ram.Write64(p, GOPInfoSize)
	}
	if p := state.Regs[cpu.R9]; p != 0 {
		_ = ram.Write64(p, GOPInfoAddr)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// fbInfo reads the linear framebuffer geometry installed by SetupGOP.
func fbInfo(ram *mmu.RAM) (fb []byte, fbW, fbH, pitch uint64, ok bool) {
	base, err := ram.Read64(GOPModeAddr + 24)
	if err != nil {
		return
	}
	size, err := ram.Read64(GOPModeAddr + 32)
	if err != nil {
		return
	}
	fbW, err = ram.Read64(GOPInfoAddr + 4)
	if err != nil {
		return
	}
	fbH, err = ram.Read64(GOPInfoAddr + 8)
	if err != nil {
		return
	}
	ppsl, err := ram.Read64(GOPInfoAddr + 32)
	if err != nil {
		return
	}
	fb, err = ram.RawBuffer(base, size)
	if err != nil {
		return
	}
	pitch = ppsl * 4
	ok = true
	return
}

// gopBlt implements Blt(This, BltBuffer, Operation, SourceX, SourceY,
// DestinationX, DestinationY, Width, Height, Delta). BltBuffer pixels are
// BGRA — identical to the DRM framebuffer byte order, so copies are raw.
func gopBlt(state *cpu.CPUState, ram *mmu.RAM) error {
	rsp := state.Regs[cpu.RSP]
	stk := func(off uint64) uint64 {
		v, _ := ram.Read64(rsp + off)
		return v
	}
	bufAddr := state.Regs[cpu.RDX]
	op := state.Regs[cpu.R8]
	srcX := uint64(state.Regs[cpu.R9])
	srcY := stk(0x28)
	dstX := stk(0x30)
	dstY := stk(0x38)
	width := stk(0x40)
	height := stk(0x48)
	delta := stk(0x50)

	fb, fbW, fbH, pitch, ok := fbInfo(ram)
	if !ok || width == 0 || height == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	stride := delta
	if stride == 0 {
		stride = width * 4
	}
	rowBytes := width * 4

	var buf []byte
	if op != 2 { // VideoToVideo touches only the framebuffer
		extent := (srcY+height)*stride + (srcX+width)*4
		if e2 := (dstY+height)*stride + (dstX+width)*4; e2 > extent {
			extent = e2
		}
		var err error
		buf, err = ram.RawBuffer(bufAddr, extent)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
	}

	clip := func(x, w uint64) (uint64, uint64) {
		if x >= fbW {
			return 0, 0
		}
		if x+w > fbW {
			w = fbW - x
		}
		return x, w
	}
	var tmp []byte

	switch op {
	case 0: // VideoToBuffer
		x, w := clip(srcX, width)
		for row := uint64(0); row < height && srcY+row < fbH; row++ {
			srcOff := ((srcY+row)*pitch + x) * 4
			dstOff := (dstY+row)*stride + dstX*4
			if dstOff+w*4 > uint64(len(buf)) || srcOff+w*4 > uint64(len(fb)) {
				break
			}
			copy(buf[dstOff:dstOff+w*4], fb[srcOff:srcOff+w*4])
		}
	case 1: // BufferToVideo
		x, w := clip(dstX, width)
		for row := uint64(0); row < height && dstY+row < fbH; row++ {
			srcOff := (srcY+row)*stride + srcX*4
			dstOff := (dstY+row)*pitch + x*4
			if srcOff+w*4 > uint64(len(buf)) || dstOff+w*4 > uint64(len(fb)) {
				break
			}
			copy(fb[dstOff:dstOff+w*4], buf[srcOff:srcOff+w*4])
		}
	case 2: // VideoToVideo (overlap-safe via per-row temp)
		x, w := clip(dstX, width)
		tmp = make([]byte, w*4)
		for row := uint64(0); row < height && dstY+row < fbH; row++ {
			srcOff := (srcY+row)*pitch + srcX*4
			dstOff := (dstY+row)*pitch + x*4
			if srcOff+w*4 > uint64(len(fb)) || dstOff+w*4 > uint64(len(fb)) {
				break
			}
			copy(tmp, fb[srcOff:srcOff+w*4])
			copy(fb[dstOff:dstOff+w*4], tmp)
		}
	case 3: // BufferToBuffer (Go copy is overlap-safe)
		for row := uint64(0); row < height; row++ {
			srcOff := (srcY+row)*stride + srcX*4
			dstOff := (dstY+row)*stride + dstX*4
			if dstOff+rowBytes > uint64(len(buf)) || srcOff+rowBytes > uint64(len(buf)) {
				break
			}
			copy(buf[dstOff:dstOff+rowBytes], buf[srcOff:srcOff+rowBytes])
		}
	default:
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutOutputString renders a UCS-2 string into the GOP framebuffer, honoring
// \r \n \b \t, scrolling at the bottom. Cursor state lives in the guest-visible
// EFI_SIMPLE_TEXT_OUTPUT_MODE struct.
func conOutOutputString(state *cpu.CPUState, ram *mmu.RAM) error {
	strAddr := state.Regs[cpu.RDX]
	fb, fbW, _, pitch, ok := fbInfo(ram)
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusSuccess // no console framebuffer: discard text
		return nil
	}

	const cellW, cellH = 16, 16 // 8x8 font at 2x scale
	cols := int64(fbW) / cellW
	row, _ := ram.Read32(ConOutModeAddr + 16)
	col, _ := ram.Read32(ConOutModeAddr + 12)
	rRow, rCol := int64(row), int64(col)

	for i := 0; i < 4096; i++ {
		ch, err := ram.Read16(strAddr + uint64(i*2))
		if err != nil || ch == 0 {
			break
		}
		switch ch {
		case '\r':
			rCol = 0
			continue
		case '\n':
			rCol, rRow = 0, rRow+1
			continue
		case '\b':
			if rCol > 0 {
				rCol--
			}
			continue
		case '\t':
			rCol = (rCol + 8) &^ 7
			continue
		}
		b := byte(ch)
		if b < 0x20 {
			continue
		}
		if b >= 0x80 {
			b = '?'
		}
		drawGlyph(fb, pitch, uint32(rCol*cellW), uint32(rRow*cellH), b)
		rCol++
		if rCol >= cols {
			rCol, rRow = 0, rRow+1
		}
	}

	// Scroll until cursor visible
	for rRow >= int64(len(fb))/int64(pitch)/cellH {
		line := pitch * cellH
		copy(fb, fb[line:])
		for i := len(fb) - int(line); i < len(fb); i++ {
			fb[i] = 0
		}
		rRow--
	}
	_ = ram.Write32(ConOutModeAddr+12, uint32(rCol))
	_ = ram.Write32(ConOutModeAddr+16, uint32(rRow))
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func conOutClearScreen(state *cpu.CPUState, ram *mmu.RAM) error {
	if fb, _, _, _, ok := fbInfo(ram); ok {
		for i := range fb {
			fb[i] = 0
		}
	}
	_ = ram.Write32(ConOutModeAddr+12, 0)
	_ = ram.Write32(ConOutModeAddr+16, 0)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func drawGlyph(fb []byte, pitch uint64, x, y uint32, ch byte) {
	glyph := graphics.Font8x8Glyph(ch)
	if glyph == nil {
		return
	}
	const scale = uint64(2)
	for r := uint32(0); r < 8; r++ {
		bits := glyph[r]
		for c := uint32(0); c < 8; c++ {
			if bits&(1<<c) == 0 {
				continue
			}
			for sy := uint32(0); sy < 2; sy++ {
				off := (uint64(y)+uint64(r)*scale+uint64(sy))*pitch + (uint64(x)+uint64(c)*scale)*4
				for sx := uint32(0); sx < 2; sx++ {
					p := off + uint64(sx)*4
					if p+3 >= uint64(len(fb)) {
						return
					}
					fb[p] = 0xFF
					fb[p+1] = 0xFF
					fb[p+2] = 0xFF
					fb[p+3] = 0xFF
				}
			}
		}
	}
}
