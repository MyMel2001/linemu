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
	"linemu/pkg/devices"
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
	DevicePathGUIDAd uint64 = 0x101530
	BlockIoGUIDAddr  uint64 = 0x101540
	SimpleFSGUIDAddr uint64 = 0x101550
	FileProtoGUIDAd  uint64 = 0x101560
	DevicePathAddr   uint64 = 0x101900 // 4B end node
	BlockIoAddr      uint64 = 0x101910 // EFI_BLOCK_IO_PROTOCOL (48B, ends 0x101940)
	BlockIoMediaAddr uint64 = 0x101940 // EFI_BLOCK_IO_MEDIA (32B, ends 0x101960)
	SimpleFSAddr     uint64 = 0x101960 // EFI_SIMPLE_FILE_SYSTEM_PROTOCOL (16B, ends 0x101970)
	FileRootAddr     uint64 = 0x101970 // EFI_FILE_PROTOCOL root (88B, ends 0x1019C8)
	// Protocol-member stubs (13B each, 16B spacing) before firmware cells.
	stubBlockReset   uint64 = 0x1019D0
	stubBlockRead    uint64 = 0x1019E0
	stubBlockWrite   uint64 = 0x1019F0
	stubBlockFlush   uint64 = 0x101A00
	stubFSOpenVolume uint64 = 0x101A10
	stubFileOpen     uint64 = 0x101A20
	stubFileClose    uint64 = 0x101A30
	stubFileRead     uint64 = 0x101A40
	stubFileWrite    uint64 = 0x101A50
	stubFileGetInfo  uint64 = 0x101A60
	stubFileSetPos   uint64 = 0x101A70
	stubFileGetPos   uint64 = 0x101A80
	stubFileFlush    uint64 = 0x101A90
	StubBase         uint64 = 0x101600

	GOPModeInfoBase uint64 = 0x101800 // 4 x 36B MODE_INFO entries (ends 0x101890)
	BootScratchBase uint64 = 0x102000 // 32 x 16B sequential stubs (ends 0x102200)
	RtScratchBase   uint64 = 0x102300 // 16 x 16B sequential stubs (ends 0x102400)
	ScratchBase     uint64 = 0x102000 // compat alias for boot scratch
	GOPModeCount           = 4
	GOPInfoSize            = 36

	// SuccessStubAddr is the xor eax,eax; ret stub written by SetupEFITables
	SuccessStubAddr = SystemTableBaseAddr + 0x80
)

// Stub slots at StubBase, 16 bytes each. 32 slots cover all services.
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
	stubConInReset     = StubBase + 9*16
	stubTestString     = StubBase + 10*16
	stubConOutQuery    = StubBase + 11*16
	stubConOutSetMode  = StubBase + 12*16
	stubSetAttribute   = StubBase + 13*16
	stubSetCursorPos   = StubBase + 14*16
	stubEnableCursor   = StubBase + 15*16
	stubConOutReset    = StubBase + 16*16
	stubAllocPages     = StubBase + 17*16
	stubFreePages      = StubBase + 18*16
	stubGetMemoryMap   = StubBase + 19*16
	stubAllocPool      = StubBase + 20*16
	stubFreePool       = StubBase + 21*16
	stubCreateEvent    = StubBase + 22*16
	stubExitBoot       = StubBase + 23*16
	stubStall          = StubBase + 24*16
	stubCopyMem        = StubBase + 25*16
	stubSetMem         = StubBase + 26*16
	stubCrc32          = StubBase + 27*16
	stubLocateHandle   = StubBase + 28*16
	stubGetTime        = StubBase + 29*16
	stubGetVariable    = StubBase + 30*16
	stubSetVariable    = StubBase + 31*16
)

// Hypercall service ids (SYSCALL with RAX in this range)
const (
	EFIHypercallBase  = 0xE0000000
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
	EFIStatusSuccess          uint64 = 0
	EFIStatusInvalidParameter uint64 = 0x8000000000000002
	EFIStatusUnsupported      uint64 = 0x8000000000000003
	EFIStatusNotFound         uint64 = 0x800000000000000E
)

// gopModes lists all video modes. Mode 0 is native so legacy guests and
// existing tests see 1920x1080 without calling SetMode.
var gopModes = [GOPModeCount][2]uint32{
	{1920, 1080},
	{1280, 720},
	{1024, 768},
	{800, 600},
}

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
	// 09576E91-6D3F-11D2-8E39-00A0C969723B Device Path
	devicePathGUID = [16]byte{0x91, 0x6E, 0x57, 0x09, 0x3F, 0x6D, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
	// 964E5B21-6459-11D2-8E39-00A0C969723B Block I/O
	blockIoGUID = [16]byte{0x21, 0x5B, 0x4E, 0x96, 0x59, 0x64, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
	// 964E5B22-6459-11D2-8E39-00A0C969723B Simple File System
	simpleFSGUID = [16]byte{0x22, 0x5B, 0x4E, 0x96, 0x59, 0x64, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
	// 964E5B23-6459-11D2-8E39-00A0C969723B File Protocol (opened volumes/files)
	fileProtoGUID = [16]byte{0x23, 0x5B, 0x4E, 0x96, 0x59, 0x64, 0xD2, 0x11,
		0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}
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

func writeModeInfo(ram *mmu.RAM, addr uint64, w, h uint32) {
	_ = ram.Write32(addr+0, 0) // Version
	_ = ram.Write32(addr+4, w)
	_ = ram.Write32(addr+8, h)
	_ = ram.Write32(addr+12, 1) // PixelFormat BGRX
	_ = ram.Write32(addr+16, 0) // PixelInformation Rsvd
	_ = ram.Write32(addr+20, 0)
	_ = ram.Write32(addr+24, 0)
	_ = ram.Write32(addr+28, 0)
	_ = ram.Write32(addr+32, w) // PixelsPerScanLine
}

// SetupGOP installs the EFI_GRAPHICS_OUTPUT_PROTOCOL, ConIn, GUIDs and
// hypercall stubs describing a linear framebuffer at fbBase.
func SetupGOP(ram *mmu.RAM, fbBase uint64, fbWidth, fbHeight uint32) error {
	stubs := map[uint64]uint64{
		stubHandleProtocol: svcHandleProtocol,
		stubOpenProtocol:   svcOpenProtocol,
		stubLocateProtocol: svcLocateProtocol,
		stubGopQueryMode:   svcGopQueryMode,
		stubGopSetMode:     svcGopSetMode,
		stubGopBlt:         svcGopBlt,
		stubOutputString:   svcOutputString,
		stubClearScreen:    svcClearScreen,
		stubReadKeyStroke:  svcReadKeyStroke,
		stubConInReset:     svcConInReset,
		stubTestString:     svcTestString,
		stubConOutQuery:    svcConOutQueryMode,
		stubConOutSetMode:  svcConOutSetMode,
		stubSetAttribute:   svcSetAttribute,
		stubSetCursorPos:   svcSetCursorPosition,
		stubEnableCursor:   svcEnableCursor,
		stubConOutReset:    svcConOutReset,
		stubAllocPages:     svcAllocatePages,
		stubFreePages:      svcFreePages,
		stubGetMemoryMap:   svcGetMemoryMap,
		stubAllocPool:      svcAllocatePool,
		stubFreePool:       svcFreePool,
		stubCreateEvent:    svcCreateEvent,
		stubExitBoot:       svcExitBootServices,
		stubStall:          svcStall,
		stubCopyMem:        svcCopyMem,
		stubSetMem:         svcSetMem,
		stubCrc32:          svcCrc32,
		stubLocateHandle:   svcLocateHandle,
		stubGetTime:        svcGetTime,
		stubGetVariable:    svcGetVariable,
		stubSetVariable:    svcSetVariable,
	}
	for addr, svc := range stubs {
		if err := writeServiceStub(ram, addr, svc); err != nil {
			return err
		}
	}

	// EFI_GRAPHICS_OUTPUT_PROTOCOL { QueryMode*, SetMode*, Blt*, Mode* }
	_ = ram.Write64(GOPProtocolAddr+0, stubGopQueryMode)
	_ = ram.Write64(GOPProtocolAddr+8, stubGopSetMode)
	_ = ram.Write64(GOPProtocolAddr+16, stubGopBlt)
	_ = ram.Write64(GOPProtocolAddr+24, GOPModeAddr)

	// Mode table: mode 0 tracks the host framebuffer size passed in.
	gopModes[0][0], gopModes[0][1] = fbWidth, fbHeight
	for i := 0; i < GOPModeCount; i++ {
		writeModeInfo(ram, GOPModeInfoBase+uint64(i*GOPInfoSize), gopModes[i][0], gopModes[i][1])
	}
	// EFI_GRAPHICS_OUTPUT_PROTOCOL_MODE
	_ = ram.Write32(GOPModeAddr+0, GOPModeCount) // MaxMode
	_ = ram.Write32(GOPModeAddr+4, 0)            // Mode
	_ = ram.Write64(GOPModeAddr+8, GOPInfoAddr)
	_ = ram.Write64(GOPModeAddr+16, GOPInfoSize)
	_ = ram.Write64(GOPModeAddr+24, fbBase)
	_ = ram.Write64(GOPModeAddr+32, uint64(fbWidth)*uint64(fbHeight)*4)
	writeModeInfo(ram, GOPInfoAddr, fbWidth, fbHeight)

	// EFI_SIMPLE_TEXT_OUTPUT_MODE { MaxMode, Mode, Attribute, CursorColumn, CursorRow, CursorVisible }
	_ = ram.Write32(ConOutModeAddr+0, 1)  // MaxMode
	_ = ram.Write32(ConOutModeAddr+4, 0)  // Mode
	_ = ram.Write32(ConOutModeAddr+8, 7)  // Attribute (light grey on black)
	_ = ram.Write32(ConOutModeAddr+12, 0) // CursorColumn
	_ = ram.Write32(ConOutModeAddr+16, 0) // CursorRow
	_ = ram.Write32(ConOutModeAddr+20, 1) // CursorVisible

	// EFI_SIMPLE_TEXT_INPUT_PROTOCOL { Reset*, ReadKeyStroke*, WaitForKey }
	// WaitForKey is an event id (not code): guests pass it to WaitForEvent.
	_ = ram.Write64(ConInAddr+0, stubConInReset)
	_ = ram.Write64(ConInAddr+8, stubReadKeyStroke)
	_ = ram.Write64(ConInAddr+16, EvtConInKey)

	// Protocol GUIDs stored in guest RAM for HandleProtocol/LocateProtocol compares
	for _, g := range []struct {
		addr  uint64
		bytes [16]byte
	}{
		{GOPGUIDAddr, gopGUID},
		{TextOutGUIDAddr, textOutGUID},
		{TextInGUIDAddr, textInGUID},
		{LoadedImgGUIDAdr, loadedImgGUID},
		{DevicePathGUIDAd, devicePathGUID},
		{BlockIoGUIDAddr, blockIoGUID},
		{SimpleFSGUIDAddr, simpleFSGUID},
		{FileProtoGUIDAd, fileProtoGUID},
	} {
		if err := ram.LoadBytes(g.addr, g.bytes[:]); err != nil {
			return err
		}
	}

	for addr, svc := range map[uint64]uint64{
		stubBlockReset:   svcBlockReset,
		stubBlockRead:    svcBlockRead,
		stubBlockWrite:   svcBlockWrite,
		stubBlockFlush:   svcBlockFlush,
		stubFSOpenVolume: svcFSOpenVolume,
		stubFileOpen:     svcFileOpen,
		stubFileClose:    svcFileClose,
		stubFileRead:     svcFileRead,
		stubFileWrite:    svcFileWrite,
		stubFileGetInfo:  svcFileGetInfo,
		stubFileSetPos:   svcFileSetPos,
		stubFileGetPos:   svcFileGetPos,
		stubFileFlush:    svcFileFlush,
	} {
		if err := writeServiceStub(ram, addr, svc); err != nil {
			return err
		}
	}

	// Device path end node: { Type 0x7F, SubType 0xFF, Length 4 }.
	_ = ram.LoadBytes(DevicePathAddr, []byte{0x7F, 0xFF, 0x04, 0x00})

	// EFI_BLOCK_IO_PROTOCOL { Revision u64, Media*, Reset*, Read*, Write*, Flush* }.
	_ = ram.Write64(BlockIoAddr+0, 0x00020001)
	_ = ram.Write64(BlockIoAddr+8, BlockIoMediaAddr)
	_ = ram.Write64(BlockIoAddr+16, stubBlockReset)
	_ = ram.Write64(BlockIoAddr+24, stubBlockRead)
	_ = ram.Write64(BlockIoAddr+32, stubBlockWrite)
	_ = ram.Write64(BlockIoAddr+40, stubBlockFlush)

	// EFI_BLOCK_IO_MEDIA: MediaId@0, Present@5, BlockSize@12, LastBlock@24.
	_ = ram.Write32(BlockIoMediaAddr+0, 1)
	_ = ram.Write8(BlockIoMediaAddr+5, 1) // MediaPresent
	_ = ram.Write32(BlockIoMediaAddr+12, 512)
	_ = ram.Write64(BlockIoMediaAddr+24, 0) // patched by SetBootDevice

	// EFI_SIMPLE_FILE_SYSTEM_PROTOCOL { Revision u64, OpenVolume* }.
	_ = ram.Write64(SimpleFSAddr+0, 0x00010000)
	_ = ram.Write64(SimpleFSAddr+8, stubFSOpenVolume)
	return nil
}

// bootSvcStub maps a BootServices byte offset to its hypercall stub.
// Kept for tests/docs; SetupProtocolOverrides is authoritative.
func bootSvcStub(off uint64) uint64 {
	switch off {
	case 40:
		return stubAllocPages
	case 48:
		return stubFreePages
	case 56:
		return stubGetMemoryMap
	case 64:
		return stubAllocPool
	case 72:
		return stubFreePool
	case 152:
		return stubHandleProtocol
	case 280:
		return stubOpenProtocol
	case 320:
		return stubLocateProtocol
	case 344:
		return stubCrc32
	case 352:
		return stubCopyMem
	case 360:
		return stubSetMem
	case 368:
		return stubCreateEvent
	}
	return 0
}

// SetupProtocolOverrides points protocol-dispatch boot services and ConOut
// members at hypercall stubs. Call after SetupEFITables filled the tables.
// Each table offset gets a dedicated 13B stub in scratch RAM; stubs are
// allocated sequentially (16B spacing) so large offsets (e.g. 368) can never
// collide with the GOP mode table or firmware cells.
func SetupProtocolOverrides(ram *mmu.RAM) {
	offToSvc := map[uint64]uint64{
		24:  svcRaiseTPL,
		32:  svcRestoreTPL,
		80:  svcCreateEvent,
		88:  svcSetTimer,
		96:  svcWaitForEvent,
		104: svcSignalEvent,
		112: svcCloseEvent,
		120: svcCheckEvent,
		128: svcInstallProtocol,
		136: svcReinstallProtocol,
		144: svcUninstallProtocol,
		152: svcHandleProtocol,
		168: svcRegisterNotify,
		176: svcLocateHandle,
		184: svcLocateDevicePath,
		192: svcInstallConfigTable,
		200: svcLoadImage,
		208: svcStartImage,
		216: svcExit,
		224: svcUnloadImage,
		232: svcExitBootServices,
		240: svcGetNextMono,
		248: svcStall,
		264: svcConnectController,
		272: svcDisconnectControl,
		280: svcOpenProtocol,
		288: svcCloseProtocol,
		296: svcOpenProtocolInfo,
		304: svcProtocolsPerHandle,
		312: svcLocateHandleBuffer,
		320: svcLocateProtocol,
		328: svcInstallMultiple,
		336: svcUninstallMultiple,
		344: svcCrc32,
		352: svcCopyMem,
		360: svcSetMem,
		368: svcCreateEventEx,
	}
	// Sort offsets for deterministic stub addresses (insertion sort: 32 entries).
	offs := make([]uint64, 0, len(offToSvc))
	for off := range offToSvc {
		offs = append(offs, off)
	}
	for i := 1; i < len(offs); i++ {
		for j := i; j > 0 && offs[j] < offs[j-1]; j-- {
			offs[j], offs[j-1] = offs[j-1], offs[j]
		}
	}
	for i, off := range offs {
		addr := BootScratchBase + uint64(i*16)
		_ = writeServiceStub(ram, addr, offToSvc[off])
		_ = ram.Write64(BootServicesBaseAddr+off, addr)
	}
	// Direct boot-service entry points with fixed slots (ids already encoded):
	_ = ram.Write64(BootServicesBaseAddr+40, stubAllocPages)
	_ = ram.Write64(BootServicesBaseAddr+48, stubFreePages)
	_ = ram.Write64(BootServicesBaseAddr+56, stubGetMemoryMap)
	_ = ram.Write64(BootServicesBaseAddr+64, stubAllocPool)
	_ = ram.Write64(BootServicesBaseAddr+72, stubFreePool)

	// EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL members
	_ = ram.Write64(ConOutBaseAddr+0, stubConOutReset)    // Reset
	_ = ram.Write64(ConOutBaseAddr+8, stubOutputString)   // OutputString
	_ = ram.Write64(ConOutBaseAddr+16, stubTestString)    // TestString
	_ = ram.Write64(ConOutBaseAddr+24, stubConOutQuery)   // QueryMode
	_ = ram.Write64(ConOutBaseAddr+32, stubConOutSetMode) // SetMode
	_ = ram.Write64(ConOutBaseAddr+40, stubSetAttribute)  // SetAttribute
	_ = ram.Write64(ConOutBaseAddr+48, stubClearScreen)   // ClearScreen
	_ = ram.Write64(ConOutBaseAddr+56, stubSetCursorPos)  // SetCursorPosition
	_ = ram.Write64(ConOutBaseAddr+64, stubEnableCursor)  // EnableCursor
	_ = ram.Write64(ConOutBaseAddr+72, ConOutModeAddr)    // Mode*

	// Runtime services used by guests
	_ = ram.Write64(RuntimeServicesBaseAddr+24, stubGetTime)
	_ = ram.Write64(RuntimeServicesBaseAddr+72, stubGetVariable)
	_ = ram.Write64(RuntimeServicesBaseAddr+88, stubSetVariable)
	rtOverride := map[uint64]uint64{
		56: svcConOutReset, // SetVirtualAddressMap: success stub slot
		80: svcGetNextVariableName,
		96: svcGetNextHighMono,
	}
	rtOffs := make([]uint64, 0, len(rtOverride))
	for off := range rtOverride {
		rtOffs = append(rtOffs, off)
	}
	for i := 1; i < len(rtOffs); i++ {
		for j := i; j > 0 && rtOffs[j] < rtOffs[j-1]; j-- {
			rtOffs[j], rtOffs[j-1] = rtOffs[j-1], rtOffs[j]
		}
	}
	for i, off := range rtOffs {
		addr := RtScratchBase + uint64(i*16)
		_ = writeServiceStub(ram, addr, rtOverride[off])
		_ = ram.Write64(RuntimeServicesBaseAddr+off, addr)
	}
}

// HandleService executes an EFI hypercall trapped from a guest stub.
func HandleService(state *cpu.CPUState, ram *mmu.RAM) error {
	return HandleServiceEx(state, ram, nil, nil)
}

// HandleServiceEx routes hypercalls with PS2 keyboard and DRM resize support.
func HandleServiceEx(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller, drm *graphics.KernelDRMBridge) error {
	if handled, err := DispatchServices(state, ram, ps2, drm); handled {
		return err
	}
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
		return gopSetMode(state, ram, drm)
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
	g, err := readGUID(ram, guidAddr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if iface, ok := locateAny(g); ok {
		if outAddr != 0 {
			_ = ram.Write64(outAddr, iface)
		}
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
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
	mode := state.Regs[cpu.RDX]
	if mode >= GOPModeCount {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if p := state.Regs[cpu.R8]; p != 0 {
		_ = ram.Write64(p, GOPInfoSize)
	}
	if p := state.Regs[cpu.R9]; p != 0 {
		_ = ram.Write64(p, GOPModeInfoBase+mode*GOPInfoSize)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// gopSetMode switches the logical mode. The physical framebuffer stays the
// host 1920x1080 buffer; smaller modes render top-left. Clears screen.
func gopSetMode(state *cpu.CPUState, ram *mmu.RAM, drm *graphics.KernelDRMBridge) error {
	mode := state.Regs[cpu.RDX]
	if mode >= GOPModeCount {
		state.Regs[cpu.RAX] = EFIStatusUnsupported
		return nil
	}
	w, h := gopModes[mode][0], gopModes[mode][1]
	_ = ram.Write32(GOPModeAddr+4, uint32(mode))
	_ = ram.Write64(GOPModeAddr+8, GOPModeInfoBase+mode*GOPInfoSize)
	writeModeInfo(ram, GOPInfoAddr, w, h)
	if fb, _, _, _, ok := fbInfo(ram); ok {
		for i := range fb {
			fb[i] = 0
		}
	}
	_ = ram.Write32(ConOutModeAddr+12, 0)
	_ = ram.Write32(ConOutModeAddr+16, 0)
	_ = drm // physical resize deferred; logical mode only
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
	fbW32, err := ram.Read32(GOPInfoAddr + 4)
	if err != nil {
		return
	}
	fbH32, err := ram.Read32(GOPInfoAddr + 8)
	if err != nil {
		return
	}
	ppsl32, err := ram.Read32(GOPInfoAddr + 32)
	if err != nil {
		return
	}
	fbW, fbH = uint64(fbW32), uint64(fbH32)
	pitch = uint64(ppsl32) * 4
	fb, err = ram.RawBuffer(base, size)
	if err != nil {
		return
	}
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

	var buf []byte
	if op != 3 { // VideoToVideo touches only the framebuffer
		// Per-op buffer extent. Spec ops: 0=VideoFill (1 pixel),
		// 1=VideoToBuffer (dst in buffer), 2=BufferToVideo (src in buffer),
		// 3=VideoToVideo (no buffer).
		var extent uint64
		switch op {
		case 0:
			extent = 4
		case 1:
			extent = (dstY+height)*stride + (dstX+width)*4
		case 2:
			extent = (srcY+height)*stride + (srcX+width)*4
		default:
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
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
	case 0: // VideoFill: fill dst rect with buf[0:4] color
		x, w := clip(dstX, width)
		for row := uint64(0); row < height && dstY+row < fbH; row++ {
			dstOff := (dstY+row)*pitch + x*4
			if dstOff+w*4 > uint64(len(fb)) {
				break
			}
			for i := uint64(0); i < w; i++ {
				copy(fb[dstOff+i*4:dstOff+i*4+4], buf[:4])
			}
		}
	case 1: // VideoToBuffer
		x, w := clip(srcX, width)
		for row := uint64(0); row < height && srcY+row < fbH; row++ {
			srcOff := (srcY+row)*pitch + x*4
			dstOff := (dstY+row)*stride + dstX*4
			if dstOff+w*4 > uint64(len(buf)) || srcOff+w*4 > uint64(len(fb)) {
				break
			}
			copy(buf[dstOff:dstOff+w*4], fb[srcOff:srcOff+w*4])
		}
	case 2: // BufferToVideo
		x, w := clip(dstX, width)
		for row := uint64(0); row < height && dstY+row < fbH; row++ {
			srcOff := (srcY+row)*stride + srcX*4
			dstOff := (dstY+row)*pitch + x*4
			if srcOff+w*4 > uint64(len(buf)) || dstOff+w*4 > uint64(len(fb)) {
				break
			}
			copy(fb[dstOff:dstOff+w*4], buf[srcOff:srcOff+w*4])
		}
	case 3: // VideoToVideo (overlap-safe via per-row temp)
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
	default:
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// efiColors maps EFI text colors (0-15) to BGRA bytes.
var efiColors = [16][4]byte{
	{0x00, 0x00, 0x00, 0xFF}, // black
	{0x80, 0x00, 0x00, 0xFF}, // blue
	{0x00, 0x80, 0x00, 0xFF}, // green
	{0x80, 0x80, 0x00, 0xFF}, // cyan
	{0x00, 0x00, 0x80, 0xFF}, // red
	{0x80, 0x00, 0x80, 0xFF}, // magenta
	{0x00, 0x80, 0x80, 0xFF}, // brown
	{0xC0, 0xC0, 0xC0, 0xFF}, // light gray
	{0x80, 0x80, 0x80, 0xFF}, // dark gray
	{0xFF, 0x00, 0x00, 0xFF}, // light blue
	{0x00, 0xFF, 0x00, 0xFF}, // light green
	{0xFF, 0xFF, 0x00, 0xFF}, // light cyan
	{0x00, 0x00, 0xFF, 0xFF}, // light red
	{0xFF, 0x00, 0xFF, 0xFF}, // light magenta
	{0x00, 0xFF, 0xFF, 0xFF}, // yellow
	{0xFF, 0xFF, 0xFF, 0xFF}, // white
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
	attr, _ := ram.Read32(ConOutModeAddr + 8)
	fg := efiColors[attr&0x0F]
	bg := efiColors[(attr>>4)&0x07] // bit7 = bright bit, not blink in text mode

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
		drawGlyphColor(fb, pitch, uint32(rCol*cellW), uint32(rRow*cellH), b, fg, bg)
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
	drawGlyphColor(fb, pitch, x, y, ch,
		[4]byte{0xFF, 0xFF, 0xFF, 0xFF}, [4]byte{0x00, 0x00, 0x00, 0xFF})
}

func drawGlyphColor(fb []byte, pitch uint64, x, y uint32, ch byte, fg, bg [4]byte) {
	glyph := graphics.Font8x8Glyph(ch)
	if glyph == nil {
		return
	}
	const scale = uint64(2)
	for r := uint32(0); r < 8; r++ {
		bits := glyph[r]
		for c := uint32(0); c < 8; c++ {
			set := bits&(1<<c) != 0
			var px [4]byte
			if set {
				px = fg
			} else {
				px = bg
			}
			for sy := uint32(0); sy < 2; sy++ {
				off := (uint64(y)+uint64(r)*scale+uint64(sy))*pitch + (uint64(x)+uint64(c)*scale)*4
				for sx := uint32(0); sx < 2; sx++ {
					p := off + uint64(sx)*4
					if p+3 >= uint64(len(fb)) {
						return
					}
					fb[p] = px[0]
					fb[p+1] = px[1]
					fb[p+2] = px[2]
					fb[p+3] = px[3]
				}
			}
		}
	}
}
