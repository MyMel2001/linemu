package uefi

import (
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
	"testing"
)

func setupFW(t *testing.T) (*cpu.CPUState, *mmu.RAM) {
	t.Helper()
	ram := mmu.NewRAM(64 * 1024 * 1024)
	c := cpu.NewCPUState()
	c.Regs[cpu.RSP] = 0x7FFFF0
	// Minimal table init: ResetFirmwareState needs live cells only.
	if err := SetupEFITables(ram, DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatalf("SetupEFITables: %v", err)
	}
	return c, ram
}

func callSvc(t *testing.T, c *cpu.CPUState, ram *mmu.RAM, id uint64) uint64 {
	t.Helper()
	c.Regs[cpu.RAX] = id
	handled, err := DispatchServices(c, ram, nil, nil)
	if err != nil {
		t.Fatalf("svc 0x%x: %v", id, err)
	}
	if !handled {
		if err := HandleService(c, ram); err != nil {
			t.Fatalf("HandleService 0x%x: %v", id, err)
		}
	}
	return c.Regs[cpu.RAX]
}

func TestAllocatorPagesPool(t *testing.T) {
	c, ram := setupFW(t)
	out := uint64(0x50000)
	c.Regs[cpu.RCX] = 0 // AllocateAnyPages
	c.Regs[cpu.RDX] = 0 // MemoryType EfiReservedMemoryType
	c.Regs[cpu.R8] = 2  // pages
	c.Regs[cpu.R9] = out
	if st := callSvc(t, c, ram, svcAllocatePages); st != 0 {
		t.Fatalf("AllocatePages = 0x%x", st)
	}
	a1, _ := ram.Read64(out)
	if a1 < 0x200000 {
		t.Fatalf("addr = 0x%x", a1)
	}
	c.Regs[cpu.RDX] = 64
	c.Regs[cpu.R8] = out + 8
	if st := callSvc(t, c, ram, svcAllocatePool); st != 0 {
		t.Fatalf("AllocatePool = 0x%x", st)
	}
	a2, _ := ram.Read64(out + 8)
	if a2 <= a1 {
		t.Fatalf("pool 0x%x not after pages 0x%x", a2, a1)
	}
	if st := callSvc(t, c, ram, svcFreePool); st != 0 {
		t.Fatalf("FreePool = 0x%x", st)
	}
}

func TestMemoryMapRoundTrip(t *testing.T) {
	c, ram := setupFW(t)
	sizePtr, buf := uint64(0x51000), uint64(0x52000)
	// Size query
	_ = ram.Write64(sizePtr, 0)
	c.Regs[cpu.RCX] = sizePtr
	c.Regs[cpu.RDX] = 0
	if st := callSvc(t, c, ram, svcGetMemoryMap); st != EFIStatusBufferTooSmall {
		t.Fatalf("size query = 0x%x, want BUFFER_TOO_SMALL", st)
	}
	need, _ := ram.Read64(sizePtr)
	if need == 0 || need%40 != 0 {
		t.Fatalf("need = %d", need)
	}
	// Full fetch with 5-pointer form
	c.Regs[cpu.RCX] = sizePtr
	c.Regs[cpu.RDX] = buf
	keyPtr, dsPtr, verPtr := uint64(0x51008), uint64(0x51010), uint64(0x51018)
	c.Regs[cpu.R8] = keyPtr
	c.Regs[cpu.R9] = dsPtr
	_ = ram.Write64(sizePtr, need)
	_ = ram.Write64(c.Regs[cpu.RSP]+0x28, verPtr)
	if st := callSvc(t, c, ram, svcGetMemoryMap); st != 0 {
		t.Fatalf("GetMemoryMap = 0x%x", st)
	}
	if ds, _ := ram.Read64(dsPtr); ds != 40 {
		t.Fatalf("descSize = %d", ds)
	}
	typ, _ := ram.Read32(buf)
	if typ > 11 {
		t.Fatalf("desc type = %d", typ)
	}
	// ExitBootServices with returned key succeeds; wrong key rejected
	key, _ := ram.Read64(keyPtr)
	c.Regs[cpu.RCX] = 0xE0030001
	c.Regs[cpu.RDX] = key ^ 1
	if st := callSvc(t, c, ram, svcExitBootServices); st != EFIStatusInvalidParameter {
		t.Fatalf("bad key = 0x%x", st)
	}
	c.Regs[cpu.RDX] = key
	if st := callSvc(t, c, ram, svcExitBootServices); st != 0 {
		t.Fatalf("ExitBootServices = 0x%x", st)
	}
}

func TestGOPMultiMode(t *testing.T) {
	c, ram := setupFW(t)
	c.Regs[cpu.RCX] = GOPProtocolAddr
	for mode := uint64(0); mode < GOPModeCount; mode++ {
		c.Regs[cpu.RDX] = mode
		szPtr, infoPtr := uint64(0x53000), uint64(0x53008)
		c.Regs[cpu.R8] = szPtr
		c.Regs[cpu.R9] = infoPtr
		if st := callSvc(t, c, ram, svcGopQueryMode); st != 0 {
			t.Fatalf("QueryMode(%d) = 0x%x", mode, st)
		}
		if sz, _ := ram.Read64(szPtr); sz != GOPInfoSize {
			t.Fatalf("mode %d size = %d", mode, sz)
		}
		info, _ := ram.Read64(infoPtr)
		w, _ := ram.Read32(info + 4)
		if w != gopModes[mode][0] {
			t.Fatalf("mode %d w = %d, want %d", mode, w, gopModes[mode][0])
		}
	}
	c.Regs[cpu.RDX] = GOPModeCount
	if st := callSvc(t, c, ram, svcGopQueryMode); st != EFIStatusInvalidParameter {
		t.Fatalf("bad mode = 0x%x", st)
	}
	// SetMode(2) -> 1024x768 active
	c.Regs[cpu.RCX] = GOPProtocolAddr
	c.Regs[cpu.RDX] = 2
	if st := callSvc(t, c, ram, svcGopSetMode); st != 0 {
		t.Fatalf("SetMode = 0x%x", st)
	}
	if m, _ := ram.Read32(GOPModeAddr + 4); m != 2 {
		t.Fatalf("Mode = %d", m)
	}
	if w, _ := ram.Read32(GOPInfoAddr + 4); w != 1024 {
		t.Fatalf("active w = %d", w)
	}
	c.Regs[cpu.RDX] = 9
	if st := callSvc(t, c, ram, svcGopSetMode); st != EFIStatusUnsupported {
		t.Fatalf("bad SetMode = 0x%x", st)
	}
}

func TestConOutFull(t *testing.T) {
	c, ram := setupFW(t)
	// QueryMode
	c.Regs[cpu.RCX] = ConOutBaseAddr
	c.Regs[cpu.RDX] = 0
	colsPtr, rowsPtr := uint64(0x54000), uint64(0x54008)
	c.Regs[cpu.R8] = colsPtr
	c.Regs[cpu.R9] = rowsPtr
	if st := callSvc(t, c, ram, svcConOutQueryMode); st != 0 {
		t.Fatalf("QueryMode = 0x%x", st)
	}
	if cols, _ := ram.Read64(colsPtr); cols != 120 {
		t.Fatalf("cols = %d", cols)
	}
	// SetCursor out of range rejected
	c.Regs[cpu.RDX] = 500
	c.Regs[cpu.R8] = 0
	if st := callSvc(t, c, ram, svcSetCursorPosition); st != EFIStatusInvalidParameter {
		t.Fatalf("bad cursor = 0x%x", st)
	}
	c.Regs[cpu.RDX] = 5
	c.Regs[cpu.R8] = 3
	if st := callSvc(t, c, ram, svcSetCursorPosition); st != 0 {
		t.Fatalf("SetCursor = 0x%x", st)
	}
	c.Regs[cpu.RDX] = 0x4F
	if st := callSvc(t, c, ram, svcSetAttribute); st != 0 {
		t.Fatalf("SetAttribute = 0x%x", st)
	}
	c.Regs[cpu.RDX] = 0xFF
	if st := callSvc(t, c, ram, svcSetAttribute); st != EFIStatusInvalidParameter {
		t.Fatalf("bad attr = 0x%x", st)
	}
	// TestString: ASCII ok, control rejected
	_ = ram.Write16(0x54100, 'A')
	_ = ram.Write16(0x54102, 0)
	c.Regs[cpu.RCX] = ConOutBaseAddr
	c.Regs[cpu.RDX] = 0x54100
	if st := callSvc(t, c, ram, svcTestString); st != 0 {
		t.Fatalf("TestString = 0x%x", st)
	}
	_ = ram.Write16(0x54100, 0x80)
	if st := callSvc(t, c, ram, svcTestString); st != EFIStatusUnsupported {
		t.Fatalf("TestString 0x80 = 0x%x", st)
	}
	// EnableCursor + Reset
	c.Regs[cpu.RDX] = 0
	if st := callSvc(t, c, ram, svcEnableCursor); st != 0 {
		t.Fatalf("EnableCursor = 0x%x", st)
	}
	if v, _ := ram.Read32(ConOutModeAddr + 20); v != 0 {
		t.Fatalf("visible = %d", v)
	}
	if st := callSvc(t, c, ram, svcConOutReset); st != 0 {
		t.Fatalf("Reset = 0x%x", st)
	}
}

func TestKeyboardInput(t *testing.T) {
	ram := mmu.NewRAM(64 * 1024 * 1024)
	if err := SetupEFITables(ram, DefaultPayloadBase, 0xE0000000, 1920, 1080); err != nil {
		t.Fatal(err)
	}
	c := cpu.NewCPUState()
	c.Regs[cpu.RSP] = 0x7FFFF0
	ps2 := devices.NewPS2Controller()
	ps2.SendScancode(0x2A) // LShift down
	ps2.SendScancode(0x23) // 'h' make
	ps2.SendScancode(0xA3) // 'h' break
	ps2.SendScancode(0xAA) // LShift up
	c.Regs[cpu.RAX] = svcReadKeyStroke
	c.Regs[cpu.RCX] = ConInAddr
	keyOut := uint64(0x55000)
	c.Regs[cpu.RDX] = keyOut
	handled, err := DispatchServices(c, ram, ps2, nil)
	if err != nil || !handled {
		t.Fatalf("ReadKeyStroke handled=%v err=%v", handled, err)
	}
	if c.Regs[cpu.RAX] != 0 {
		t.Fatalf("status = 0x%x", c.Regs[cpu.RAX])
	}
	uni, _ := ram.Read16(keyOut + 2)
	if uni != 'H' {
		t.Fatalf("uni = 0x%x, want 'H'", uni)
	}
	// queue drained now
	c.Regs[cpu.RAX] = svcReadKeyStroke
	if _, err := DispatchServices(c, ram, ps2, nil); err != nil {
		t.Fatal(err)
	}
	if c.Regs[cpu.RAX] != EFIStatusNotReady {
		t.Fatalf("empty = 0x%x, want NOT_READY", c.Regs[cpu.RAX])
	}
	// E0 arrows
	ps2.SendScancode(0xE0)
	ps2.SendScancode(0x48)
	c.Regs[cpu.RAX] = svcReadKeyStroke
	if _, err := DispatchServices(c, ram, ps2, nil); err != nil {
		t.Fatal(err)
	}
	scan, _ := ram.Read16(keyOut)
	if scan != 0x01 {
		t.Fatalf("scan = 0x%x, want up(1)", scan)
	}
}

func TestLocateHandleBufferAndProtocols(t *testing.T) {
	c, ram := setupFW(t)
	// ByProtocol for GOP guid
	_ = ram.LoadBytes(0x56000, gopGUID[:])
	c.Regs[cpu.RCX] = 2
	c.Regs[cpu.RDX] = 0x56000
	c.Regs[cpu.R8] = 0 // SearchKey
	nPtr, bPtr := uint64(0x56020), uint64(0)
	c.Regs[cpu.R9] = nPtr
	_ = ram.Write64(c.Regs[cpu.RSP]+0x28, 0x56028)
	if st := callSvc(t, c, ram, svcLocateHandleBuffer); st != 0 {
		t.Fatalf("LocateHandleBuffer = 0x%x", st)
	}
	var n uint64
	n, _ = ram.Read64(nPtr)
	if n == 0 {
		t.Fatal("no handles")
	}
	buf, _ := ram.Read64(0x56028)
	_ = buf
	_ = bPtr
	// ProtocolsPerHandle on GOP handle: (Handle, Buffer***, Count*)
	h := HandleGOP
	c.Regs[cpu.RCX] = h
	c.Regs[cpu.RDX] = 0x56030
	c.Regs[cpu.R8] = 0x56038
	if st := callSvc(t, c, ram, svcProtocolsPerHandle); st != 0 {
		t.Fatalf("ProtocolsPerHandle = 0x%x", st)
	}
	if n2, _ := ram.Read64(0x56038); n2 != 1 {
		t.Fatalf("proto count = %d", n2)
	}
}

func TestMemHelpersAndVars(t *testing.T) {
	c, ram := setupFW(t)
	_ = ram.LoadBytes(0x57000, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	c.Regs[cpu.RCX] = 0x57100
	c.Regs[cpu.RDX] = 0x57000
	c.Regs[cpu.R8] = 8
	if st := callSvc(t, c, ram, svcCopyMem); st != 0 {
		t.Fatalf("CopyMem = 0x%x", st)
	}
	if b, _ := ram.Read8(0x57103); b != 4 {
		t.Fatalf("copied = %d", b)
	}
	c.Regs[cpu.RCX] = 0x57100
	c.Regs[cpu.RDX] = 8
	c.Regs[cpu.R8] = 0xAB
	if st := callSvc(t, c, ram, svcSetMem); st != 0 {
		t.Fatalf("SetMem = 0x%x", st)
	}
	if b, _ := ram.Read8(0x57100); b != 0xAB {
		t.Fatalf("set = 0x%x", b)
	}
	// CRC32 of "123456789" == 0xCBF43926
	_ = ram.LoadBytes(0x57200, []byte("123456789"))
	c.Regs[cpu.RCX] = 0x57200
	c.Regs[cpu.RDX] = 9
	c.Regs[cpu.R8] = 0x57210
	if st := callSvc(t, c, ram, svcCrc32); st != 0 {
		t.Fatalf("Crc32 = 0x%x", st)
	}
	if v, _ := ram.Read32(0x57210); v != 0xCBF43926 {
		t.Fatalf("crc = 0x%x", v)
	}
	// SetVariable + GetVariable
	_ = ram.Write16(0x57300, 'T')
	_ = ram.Write16(0x57302, 0)
	_ = ram.LoadBytes(0x57310, gopGUID[:])
	_ = ram.LoadBytes(0x57320, []byte{9, 8, 7})
	c.Regs[cpu.RCX] = 0x57300
	c.Regs[cpu.RDX] = 0x57310
	c.Regs[cpu.R8] = 7
	c.Regs[cpu.R9] = 3
	_ = ram.Write64(c.Regs[cpu.RSP]+0x28, 0x57320)
	if st := callSvc(t, c, ram, svcSetVariable); st != 0 {
		t.Fatalf("SetVariable = 0x%x", st)
	}
	c.Regs[cpu.RCX] = 0x57300
	c.Regs[cpu.RDX] = 0x57310
	c.Regs[cpu.R8] = 0x57330
	c.Regs[cpu.R9] = 0x57338
	_ = ram.Write64(0x57338, 3)
	_ = ram.Write64(c.Regs[cpu.RSP]+0x28, 0x57340)
	if st := callSvc(t, c, ram, svcGetVariable); st != 0 {
		t.Fatalf("GetVariable = 0x%x", st)
	}
	if b, _ := ram.Read8(0x57340); b != 9 {
		t.Fatalf("var[0] = %d", b)
	}
}

func TestStallAndTime(t *testing.T) {
	c, ram := setupFW(t)
	c.Regs[cpu.RCX] = 1000
	if st := callSvc(t, c, ram, svcStall); st != 0 {
		t.Fatalf("Stall = 0x%x", st)
	}
	c.Regs[cpu.RCX] = 0x58000
	c.Regs[cpu.RDX] = 0
	if st := callSvc(t, c, ram, svcGetTime); st != 0 {
		t.Fatalf("GetTime = 0x%x", st)
	}
	yr, _ := ram.Read16(0x58000)
	if yr < 2024 || yr > 2030 {
		t.Fatalf("year = %d", yr)
	}
}
