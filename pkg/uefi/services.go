package uefi

// Boot + runtime service hypercalls: allocator, memory map, events, protocols,
// timers, NVRAM variables, wall clock, and misc memory services.
//
// Guest RAM cells (per-RAM state, reset by ResetFirmwareState):
// ShiftState/E0Pending live in input.go. Everything else host-side lives in
// fwState guarded by fwMu. Handles are synthetic host values (>= 0xE0020000),
// never guest RAM addresses.

import (
	"hash/crc32"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/graphics"
	"linemu/pkg/mmu"
	"sort"
	"time"
)

const (
	AllocNextAddr  uint64 = 0x101AC0
	AllocEndAddr   uint64 = 0x101AC8
	MapKeyAddr     uint64 = 0x101AD0
	HandleSeqAddr  uint64 = 0x101AD8
	EventSeqAddr   uint64 = 0x101AE0
	MonoCountAddr  uint64 = 0x101AE8
	MonoHighAddr   uint64 = 0x101AF0
	TplCellAddr    uint64 = 0x101AF8
	ExitedFlagAddr uint64 = 0x101B00

	EvtConInKey   uint64 = 0xE0020001
	HandleGOP     uint64 = 0xE0030001
	HandleConOut  uint64 = 0xE0030002
	HandleConIn   uint64 = 0xE0030003
	HandleBootVol uint64 = 0xE0030004
	HandleBaseSeq uint64 = 0xE0030010
	EventBaseSeq  uint64 = 0xE0020002
)

// Additional EFI_STATUS codes
const (
	EFIStatusBufferTooSmall    uint64 = 0x8000000000000005
	EFIStatusNotReady          uint64 = 0x8000000000000006
	EFIStatusDeviceError       uint64 = 0x8000000000000007
	EFIStatusWriteProtected    uint64 = 0x8000000000000008
	EFIStatusOutOfResources    uint64 = 0x8000000000000009
	EFIStatusTimeout           uint64 = 0x8000000000000012
	EFIStatusInvalidParameter2 uint64 = 0x8000000000000002 // alias, same as EFIStatusInvalidParameter
)

// New hypercall service ids
const (
	svcReadKeyStroke       = EFIHypercallBase + 22
	svcConInReset          = EFIHypercallBase + 23
	svcWaitForKey          = EFIHypercallBase + 24
	svcAllocatePages       = EFIHypercallBase + 30
	svcFreePages           = EFIHypercallBase + 31
	svcGetMemoryMap        = EFIHypercallBase + 32
	svcAllocatePool        = EFIHypercallBase + 33
	svcFreePool            = EFIHypercallBase + 34
	svcCreateEvent         = EFIHypercallBase + 35
	svcSetTimer            = EFIHypercallBase + 36
	svcWaitForEvent        = EFIHypercallBase + 37
	svcSignalEvent         = EFIHypercallBase + 38
	svcCloseEvent          = EFIHypercallBase + 39
	svcCheckEvent          = EFIHypercallBase + 40
	svcInstallProtocol     = EFIHypercallBase + 41
	svcReinstallProtocol   = EFIHypercallBase + 42
	svcUninstallProtocol   = EFIHypercallBase + 43
	svcRegisterNotify      = EFIHypercallBase + 44
	svcLocateHandle        = EFIHypercallBase + 45
	svcInstallConfigTable  = EFIHypercallBase + 46
	svcExitBootServices    = EFIHypercallBase + 47
	svcGetNextMono         = EFIHypercallBase + 48
	svcStall               = EFIHypercallBase + 49
	svcConnectController   = EFIHypercallBase + 50
	svcDisconnectControl   = EFIHypercallBase + 51
	svcCloseProtocol       = EFIHypercallBase + 52
	svcOpenProtocolInfo    = EFIHypercallBase + 53
	svcProtocolsPerHandle  = EFIHypercallBase + 54
	svcLocateHandleBuffer  = EFIHypercallBase + 55
	svcInstallMultiple     = EFIHypercallBase + 56
	svcUninstallMultiple   = EFIHypercallBase + 57
	svcCrc32               = EFIHypercallBase + 58
	svcCopyMem             = EFIHypercallBase + 59
	svcSetMem              = EFIHypercallBase + 60
	svcCreateEventEx       = EFIHypercallBase + 61
	svcTestString          = EFIHypercallBase + 62
	svcConOutQueryMode     = EFIHypercallBase + 63
	svcConOutSetMode       = EFIHypercallBase + 64
	svcSetAttribute        = EFIHypercallBase + 65
	svcSetCursorPosition   = EFIHypercallBase + 66
	svcEnableCursor        = EFIHypercallBase + 67
	svcConOutReset         = EFIHypercallBase + 68
	svcExit                = EFIHypercallBase + 69
	svcRestoreTPL          = EFIHypercallBase + 70
	svcRaiseTPL            = EFIHypercallBase + 87
	svcGetTime             = EFIHypercallBase + 80
	svcSetVirtAddrMap      = EFIHypercallBase + 81
	svcGetVariable         = EFIHypercallBase + 82
	svcGetNextVariableName = EFIHypercallBase + 83
	svcSetVariable         = EFIHypercallBase + 84
	svcGetNextHighMono     = EFIHypercallBase + 85
	svcResetSystem         = EFIHypercallBase + 86
	svcBlockReset          = EFIHypercallBase + 88
	svcBlockRead           = EFIHypercallBase + 89
	svcBlockWrite          = EFIHypercallBase + 90
	svcBlockFlush          = EFIHypercallBase + 91
	svcFSOpenVolume        = EFIHypercallBase + 92
	svcFileOpen            = EFIHypercallBase + 93
	svcFileClose           = EFIHypercallBase + 94
	svcFileRead            = EFIHypercallBase + 95
	svcFileWrite           = EFIHypercallBase + 96
	svcFileGetInfo         = EFIHypercallBase + 97
	svcFileSetPos          = EFIHypercallBase + 98
	svcFileGetPos          = EFIHypercallBase + 99
	svcFileFlush           = EFIHypercallBase + 100
	svcLoadImage           = EFIHypercallBase + 101
	svcStartImage          = EFIHypercallBase + 102
	svcUnloadImage         = EFIHypercallBase + 103
	svcLocateDevicePath    = EFIHypercallBase + 104
)

type efiEvent struct {
	typ       uint64
	notifyFn  uint64
	notifyCtx uint64
	signaled  bool
	deadline  time.Time
	periodic  bool
	interval  time.Duration
}

type efiVar struct {
	attr uint32
	data []byte
}

type fwGlobals struct {
	protos  map[uint64]map[[16]byte]uint64 // handle -> guid -> iface
	events  map[uint64]*efiEvent
	vars    map[string]*efiVar
	configs [][2]any // (guid [16]byte, table uint64)

	bootDev devices.BlockStorage
	bootFAT uint64 // boot FAT start LBA (512-byte units)
	files   map[uint64]*fatFile
	images  map[uint64]uint64 // image handle -> entry point
	allocs  [][2]uint64       // explicit AllocateAddress ranges [base,end)
}

var fwState = &fwGlobals{
	protos: make(map[uint64]map[[16]byte]uint64),
	events: make(map[uint64]*efiEvent),
	vars:   make(map[string]*efiVar),
}

func bumpMapKey(ram *mmu.RAM) {
	k, _ := ram.Read64(MapKeyAddr)
	_ = ram.Write64(MapKeyAddr, k+1)
}

// ResetFirmwareState clears host-side firmware state and inits guest cells.
// Called from SetupEFITables so every boot starts clean.
func ResetFirmwareState(ram *mmu.RAM) {
	fwState.protos = make(map[uint64]map[[16]byte]uint64)
	fwState.events = make(map[uint64]*efiEvent)
	fwState.vars = make(map[string]*efiVar)
	fwState.configs = nil
	fwState.files = make(map[uint64]*fatFile)
	fwState.images = make(map[uint64]uint64)
	fwState.allocs = nil

	fwState.protos[HandleGOP] = map[[16]byte]uint64{gopGUID: GOPProtocolAddr}
	fwState.protos[HandleConOut] = map[[16]byte]uint64{textOutGUID: ConOutBaseAddr}
	fwState.protos[HandleConIn] = map[[16]byte]uint64{textInGUID: ConInAddr}
	fwState.protos[LoadedImageBaseAddr] = map[[16]byte]uint64{loadedImgGUID: LoadedImageBaseAddr}
	fwState.protos[HandleBootVol] = map[[16]byte]uint64{
		devicePathGUID: DevicePathAddr,
		blockIoGUID:    BlockIoAddr,
		simpleFSGUID:   SimpleFSAddr,
	}
	fwState.events[EvtConInKey] = &efiEvent{}

	top := ram.Size() &^ 0xFFF
	next := uint64(0x200000)
	if top <= next+0x1000 {
		top = next
	}
	_ = ram.Write64(AllocNextAddr, next)
	_ = ram.Write64(AllocEndAddr, top)
	_ = ram.Write64(MapKeyAddr, 1)
	_ = ram.Write64(HandleSeqAddr, HandleBaseSeq)
	_ = ram.Write64(EventSeqAddr, EventBaseSeq)
	_ = ram.Write64(MonoCountAddr, 0)
	_ = ram.Write64(MonoHighAddr, 0)
	_ = ram.Write64(TplCellAddr, 4)
	_ = ram.Write64(ExitedFlagAddr, 0)
	_ = ram.Write64(ShiftStateAddr, 0)
	_ = ram.Write64(E0PendingAddr, 0)
}

func fwExited(ram *mmu.RAM) bool {
	v, _ := ram.Read64(ExitedFlagAddr)
	return v != 0
}

func readGUID(ram *mmu.RAM, addr uint64) ([16]byte, error) {
	var g [16]byte
	for i := 0; i < 16; i++ {
		b, err := ram.Read8(addr + uint64(i))
		if err != nil {
			return g, err
		}
		g[i] = b
	}
	return g, nil
}

// allocBump carves size bytes (aligned up) out of the firmware pool,
// skipping the loaded image and explicit AllocateAddress ranges.
func allocBump(ram *mmu.RAM, size, align uint64) (uint64, error) {
	if align == 0 {
		align = 8
	}
	next, _ := ram.Read64(AllocNextAddr)
	end, _ := ram.Read64(AllocEndAddr)
	imgBase, _ := ram.Read64(LoadedImageBaseAddr + 64)
	imgSize, _ := ram.Read64(LoadedImageBaseAddr + 72)
	addr := (next + align - 1) &^ (align - 1)
	// Skip forbidden ranges; loop since skipping one may land in another.
	for {
		skipped := false
		if imgBase != 0 && imgSize != 0 && addr < imgBase+imgSize && addr+size > imgBase {
			addr = (imgBase + imgSize + align - 1) &^ (align - 1)
			skipped = true
		}
		for _, a := range fwState.allocs {
			if addr < a[1] && addr+size > a[0] {
				addr = (a[1] + align - 1) &^ (align - 1)
				skipped = true
			}
		}
		if !skipped {
			break
		}
	}
	if addr+size < addr || addr+size > end {
		return 0, errNoMem
	}
	_ = ram.Write64(AllocNextAddr, addr+size)
	bumpMapKey(ram)
	return addr, nil
}

type fwErr struct{ s string }

func (e *fwErr) Error() string { return e.s }

var errNoMem = &fwErr{"out of resources"}

func locateAny(g [16]byte) (uint64, bool) {
	for _, m := range fwState.protos {
		if iface, ok := m[g]; ok {
			return iface, true
		}
	}
	return 0, false
}

// ---- allocator + memory map ----

func svcAllocPages(state *cpu.CPUState, ram *mmu.RAM) error {
	typ := state.Regs[cpu.RCX]
	pages := state.Regs[cpu.R8]
	out := state.Regs[cpu.R9]
	if out == 0 || pages == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	size := pages * 4096
	var addr uint64
	switch typ {
	case 2: // AllocateAddress
		req, err := ram.Read64(out)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		if req < 0x200000 || req+size < req || req+size > ram.Size() {
			state.Regs[cpu.RAX] = EFIStatusNotFound
			return nil
		}
		next, _ := ram.Read64(AllocNextAddr)
		_ = next
		imgBase, _ := ram.Read64(LoadedImageBaseAddr + 64)
		imgSize, _ := ram.Read64(LoadedImageBaseAddr + 72)
		fbBase, _ := ram.Read64(GOPModeAddr + 24)
		fbSize, _ := ram.Read64(GOPModeAddr + 32)
		overlap := func(b, e uint64) bool { return req < e && req+size > b }
		// ponytail: allow AllocateAddress inside bump high-water; freed pages are reused via this path.
		if imgBase != 0 && imgSize != 0 && overlap(imgBase, imgBase+imgSize) {
			state.Regs[cpu.RAX] = EFIStatusNotFound
			return nil
		}
		if fbBase != 0 && fbSize != 0 && overlap(fbBase&^0xFFF, ((fbBase+fbSize+0xFFF)&^0xFFF)) {
			// Only reject if fb inside RAM; MMIO above top never overlaps valid req.
			if fbBase < uint64(ram.Size()) {
				state.Regs[cpu.RAX] = EFIStatusNotFound
				return nil
			}
		}
		addr = req
		already := false
		for _, a := range fwState.allocs {
			if overlap(a[0], a[1]) {
				already = true
				break
			}
		}
		if !already {
			fwState.allocs = append(fwState.allocs, [2]uint64{req, req + size})
		}
		bumpMapKey(ram)
	default: // AllocateAnyPages / AllocateMaxAddress
		a, err := allocBump(ram, size, 4096)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusOutOfResources
			return nil
		}
		addr = a
		if typ == 1 { // must fit below max address in [out]
			max, err := ram.Read64(out)
			if err != nil || addr+size-1 > max {
				state.Regs[cpu.RAX] = EFIStatusOutOfResources
				return nil
			}
		}
	}
	_ = ram.Write64(out, addr)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func svcAllocPool(state *cpu.CPUState, ram *mmu.RAM) error {
	size := state.Regs[cpu.RDX]
	out := state.Regs[cpu.R8]
	if out == 0 || size == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	addr, err := allocBump(ram, (size+7)&^7, 8)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusOutOfResources
		return nil
	}
	_ = ram.Write64(out, addr)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

type memDesc struct {
	typ   uint32
	start uint64
	pages uint64
	attr  uint64
}

func memMapEntries(ram *mmu.RAM) []memDesc {
	top := ram.Size() &^ 0xFFF
	next, _ := ram.Read64(AllocNextAddr)
	fbBase, _ := ram.Read64(GOPModeAddr + 24)
	fbSize, _ := ram.Read64(GOPModeAddr + 32)
	imgBase, _ := ram.Read64(LoadedImageBaseAddr + 64)
	imgSize, _ := ram.Read64(LoadedImageBaseAddr + 72)
	out := []memDesc{
		{typ: 7, start: 0, pages: 256},
		{typ: 0, start: 0x100000, pages: 256},
	}
	// Reserved ranges inside [0x200000, top): loaded image + fb + AllocateAddress.
	// ponytail: only initial image + fb + AllocateAddress carved; dynamic LoadImage ranges not tracked.
	type rng struct {
		base, end uint64
		typ       uint32
		attr      uint64
	}
	var reserved []rng
	addRsv := func(b, e uint64, typ uint32, attr uint64) {
		b &= ^uint64(0xFFF)
		e = (e + 0xFFF) &^ 0xFFF
		if b < 0x200000 {
			b = 0x200000
		}
		if e > top {
			e = top
		}
		if e > b && b < top {
			reserved = append(reserved, rng{b, e, typ, attr})
			out = append(out, memDesc{typ: typ, start: b, pages: (e - b) / 4096, attr: attr})
		}
	}
	if imgBase != 0 && imgSize != 0 {
		addRsv(imgBase, imgBase+imgSize, 2, 0)
	}
	if fbBase != 0 && fbSize != 0 {
		addRsv(fbBase, fbBase+fbSize, 11, 0x1)
	}
	for _, a := range fwState.allocs {
		addRsv(a[0], a[1], 4, 0)
	}
	// insertion sort: <=4 entries.
	for i := 1; i < len(reserved); i++ {
		for j := i; j > 0 && reserved[j].base < reserved[j-1].base; j-- {
			reserved[j], reserved[j-1] = reserved[j-1], reserved[j]
		}
	}
	// Emit runs in [base,end) minus reserved.
	emitMinus := func(base, end uint64, typ uint32) {
		cur := base
		for _, r := range reserved {
			if r.base >= end || r.end <= cur {
				continue
			}
			if r.base > cur {
				out = append(out, memDesc{typ: typ, start: cur, pages: (r.base - cur) / 4096})
			}
			if r.end > cur {
				cur = r.end
			}
		}
		if cur < end {
			out = append(out, memDesc{typ: typ, start: cur, pages: (end - cur) / 4096})
		}
	}
	if next > 0x200000 && next < top {
		emitMinus(0x200000, next&^0xFFF, 4)
	}
	freeBase := next
	if freeBase < 0x200000 {
		freeBase = 0x200000
	}
	freeBase &= ^uint64(0xFFF)
	if freeBase < top {
		emitMinus(freeBase, top, 7)
	}
	return out
}

func svcGetMemMap(state *cpu.CPUState, ram *mmu.RAM) error {
	sizePtr := state.Regs[cpu.RCX]
	buf := state.Regs[cpu.RDX]
	keyPtr := state.Regs[cpu.R8]
	descSizePtr := state.Regs[cpu.R9]
	var verPtr uint64
	_ = verPtr
	rsp := state.Regs[cpu.RSP]
	verPtr, _ = ram.Read64(rsp + 0x28)

	if sizePtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	descs := memMapEntries(ram)
	need := uint64(len(descs) * 40)
	have, _ := ram.Read64(sizePtr)
	if buf == 0 || have < need {
		_ = ram.Write64(sizePtr, need)
		state.Regs[cpu.RAX] = EFIStatusBufferTooSmall
		return nil
	}
	for i, d := range descs {
		base := buf + uint64(i*40)
		_ = ram.Write32(base+0, d.typ)
		_ = ram.Write32(base+4, 0)
		_ = ram.Write64(base+8, d.start)
		_ = ram.Write64(base+16, d.start)
		_ = ram.Write64(base+24, d.pages)
		_ = ram.Write64(base+32, d.attr)
	}
	key, _ := ram.Read64(MapKeyAddr)
	if keyPtr != 0 {
		_ = ram.Write64(keyPtr, key)
	}
	if descSizePtr != 0 {
		_ = ram.Write64(descSizePtr, 40)
	}
	if verPtr != 0 {
		_ = ram.Write32(verPtr, 1)
	}
	_ = ram.Write64(sizePtr, need)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- events / timers ----

func sortedEventIDs() []uint64 {
	ids := make([]uint64, 0, len(fwState.events))
	for id := range fwState.events {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func allocEventID(ram *mmu.RAM) uint64 {
	seq, _ := ram.Read64(EventSeqAddr)
	_ = ram.Write64(EventSeqAddr, seq+1)
	return seq
}

func eventReady(ram *mmu.RAM, ps2 *devices.PS2Controller, id uint64) bool {
	ev, ok := fwState.events[id]
	if !ok {
		return false
	}
	if id == EvtConInKey {
		if ps2 == nil {
			return false
		}
		_, has := ps2.PeekScancode()
		return has
	}
	if ev.signaled {
		return true
	}
	if !ev.deadline.IsZero() && !time.Now().Before(ev.deadline) {
		ev.signaled = true
		return true
	}
	return false
}

func do_svcCreateEvent(state *cpu.CPUState, ram *mmu.RAM, ex bool) error {
	typ := state.Regs[cpu.RCX]
	fn := state.Regs[cpu.R8]
	ctx := state.Regs[cpu.R9]
	rsp := state.Regs[cpu.RSP]
	var outPtr uint64
	if ex {
		outPtr, _ = ram.Read64(rsp + 0x30)
	} else {
		outPtr, _ = ram.Read64(rsp + 0x28)
	}
	if outPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	id := allocEventID(ram)
	fwState.events[id] = &efiEvent{typ: typ, notifyFn: fn, notifyCtx: ctx}
	_ = ram.Write64(outPtr, id)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcSetTimer(state *cpu.CPUState, ram *mmu.RAM) error {
	id := state.Regs[cpu.RCX]
	typ := state.Regs[cpu.RDX]
	trigger := state.Regs[cpu.R8]
	ev, ok := fwState.events[id]
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	switch typ {
	case 0: // TimerCancel
		ev.deadline = time.Time{}
		ev.periodic = false
		ev.signaled = false
	case 1, 2: // TimerPeriodic / TimerRelative
		d := time.Duration(trigger) * 100 * time.Nanosecond
		ev.deadline = time.Now().Add(d)
		ev.periodic = typ == 1
		ev.interval = d
		ev.signaled = false
	default:
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcWaitForEvent(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller) error {
	count := state.Regs[cpu.RCX]
	arr := state.Regs[cpu.RDX]
	idxPtr := state.Regs[cpu.R8]
	if count == 0 || arr == 0 || idxPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	// Poll briefly; blocking the host 30s on an idle guest hangs the VM.
	// Guests spin on WaitForEvent+ReadKeyStroke, so TIMEOUT is safe.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		for i := uint64(0); i < count; i++ {
			id, err := ram.Read64(arr + i*8)
			if err != nil {
				continue
			}
			if eventReady(ram, ps2, id) {
				_ = ram.Write64(idxPtr, i)
				state.Regs[cpu.RAX] = EFIStatusSuccess
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			state.Regs[cpu.RAX] = EFIStatusTimeout
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func do_svcCheckEvent(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller) error {
	id := state.Regs[cpu.RCX]
	if _, ok := fwState.events[id]; !ok {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if eventReady(ram, ps2, id) {
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	}
	state.Regs[cpu.RAX] = EFIStatusNotReady
	return nil
}

// ---- protocols ----

func svcInstallProto(state *cpu.CPUState, ram *mmu.RAM) error {
	hPtr := state.Regs[cpu.RCX]
	gPtr := state.Regs[cpu.RDX]
	iface := state.Regs[cpu.R9] // (Handle*, Protocol*, InterfaceType, Interface): iface is 4th arg
	if hPtr == 0 || gPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	g, err := readGUID(ram, gPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	h, _ := ram.Read64(hPtr)
	if h == 0 {
		seq, _ := ram.Read64(HandleSeqAddr)
		h = seq
		_ = ram.Write64(HandleSeqAddr, seq+1)
		_ = ram.Write64(hPtr, h)
	}
	m := fwState.protos[h]
	if m == nil {
		m = make(map[[16]byte]uint64)
		fwState.protos[h] = m
	}
	m[g] = iface
	bumpMapKey(ram)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func svcReinstallProto(state *cpu.CPUState, ram *mmu.RAM) error {
	h := state.Regs[cpu.RCX]
	gPtr := state.Regs[cpu.RDX]
	oldIf := state.Regs[cpu.R8]
	newIf := state.Regs[cpu.R9]
	g, err := readGUID(ram, gPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	m := fwState.protos[h]
	if m == nil {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	cur, ok := m[g]
	if !ok || (oldIf != 0 && cur != oldIf) {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	m[g] = newIf
	bumpMapKey(ram)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func svcUninstallProto(state *cpu.CPUState, ram *mmu.RAM) error {
	h := state.Regs[cpu.RCX]
	gPtr := state.Regs[cpu.RDX]
	g, err := readGUID(ram, gPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	m := fwState.protos[h]
	if m == nil {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	if _, ok := m[g]; !ok {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	delete(m, g)
	if len(m) == 0 {
		delete(fwState.protos, h)
	}
	bumpMapKey(ram)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func collectHandles(searchType uint64, g *[16]byte) []uint64 {
	var out []uint64
	switch searchType {
	case 2: // ByProtocol
		if g == nil {
			return nil
		}
		for h, m := range fwState.protos {
			if _, ok := m[*g]; ok {
				out = append(out, h)
			}
		}
	default: // AllHandles (0) and others: enumerate everything
		for h := range fwState.protos {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func do_svcLocateHandle(state *cpu.CPUState, ram *mmu.RAM, buffered bool) error {
	searchType := state.Regs[cpu.RCX]
	gPtr := state.Regs[cpu.RDX]
	var g *[16]byte
	if searchType == 2 {
		if gPtr == 0 {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		gg, err := readGUID(ram, gPtr)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		g = &gg
	}
	handles := collectHandles(searchType, g)
	if searchType == 2 && len(handles) == 0 {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	if buffered {
		// LocateHandleBuffer(SearchType, Protocol, SearchKey, NoHandles*, Buffer**):
		// 4th arg NoHandles* in R9, 5th arg Buffer** on stack.
		nPtr := state.Regs[cpu.R9]
		rsp := state.Regs[cpu.RSP]
		bPtr, _ := ram.Read64(rsp + 0x28)
		if nPtr == 0 || bPtr == 0 {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		var buf uint64
		if len(handles) > 0 {
			b, err := allocBump(ram, uint64(len(handles)*8), 8)
			if err != nil {
				state.Regs[cpu.RAX] = EFIStatusOutOfResources
				return nil
			}
			buf = b
			for i, h := range handles {
				_ = ram.Write64(buf+uint64(i*8), h)
			}
		}
		_ = ram.Write64(nPtr, uint64(len(handles)))
		_ = ram.Write64(bPtr, buf)
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	}
	// (SearchType, Protocol, SearchKey, BufferSize*, Buffer)
	sizePtr := state.Regs[cpu.R9]
	rsp := state.Regs[cpu.RSP]
	buf, _ := ram.Read64(rsp + 0x28)
	if sizePtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	need := uint64(len(handles) * 8)
	have, _ := ram.Read64(sizePtr)
	if buf == 0 || have < need {
		_ = ram.Write64(sizePtr, need)
		state.Regs[cpu.RAX] = EFIStatusBufferTooSmall
		return nil
	}
	for i, h := range handles {
		_ = ram.Write64(buf+uint64(i*8), h)
	}
	_ = ram.Write64(sizePtr, need)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcProtocolsPerHandle(state *cpu.CPUState, ram *mmu.RAM) error {
	// (Handle, ProtocolBuffer***, ProtocolBufferCount*)
	h := state.Regs[cpu.RCX]
	bufPtr := state.Regs[cpu.RDX]
	countPtr := state.Regs[cpu.R8]
	if countPtr == 0 || bufPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	m := fwState.protos[h]
	if m == nil {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	guids := make([][16]byte, 0, len(m))
	for g := range m {
		guids = append(guids, g)
	}
	sort.Slice(guids, func(i, j int) bool {
		for k := 0; k < 16; k++ {
			if guids[i][k] != guids[j][k] {
				return guids[i][k] < guids[j][k]
			}
		}
		return false
	})
	var arr, storage uint64
	if len(guids) > 0 {
		a, err := allocBump(ram, uint64(len(guids)*8), 8)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusOutOfResources
			return nil
		}
		s, err := allocBump(ram, uint64(len(guids)*16), 8)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusOutOfResources
			return nil
		}
		arr, storage = a, s
		for i, g := range guids {
			gg := g
			_ = ram.LoadBytes(storage+uint64(i*16), gg[:])
			_ = ram.Write64(arr+uint64(i*8), storage+uint64(i*16))
		}
	}
	_ = ram.Write64(countPtr, uint64(len(guids)))
	_ = ram.Write64(bufPtr, arr)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func svcOpenProtoInfo(state *cpu.CPUState, ram *mmu.RAM) error {
	// (Handle, Protocol, EntryBuffer***, EntryCount*)
	h := state.Regs[cpu.RCX]
	gPtr := state.Regs[cpu.RDX]
	bufPtr := state.Regs[cpu.R8]
	countPtr := state.Regs[cpu.R9]
	if countPtr == 0 || bufPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	g, err := readGUID(ram, gPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	m := fwState.protos[h]
	if m == nil {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	if _, ok := m[g]; !ok {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	entry, err := allocBump(ram, 24, 8)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusOutOfResources
		return nil
	}
	_ = ram.Write64(entry+16, 0x00000006) // Attributes+OpenCount packed
	_ = ram.Write64(bufPtr, entry)
	_ = ram.Write64(countPtr, 1)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func multiProto(state *cpu.CPUState, ram *mmu.RAM, install bool) error {
	hPtr := state.Regs[cpu.RCX]
	if hPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	// Variadic pairs: (RDX,R8), (R9,stack+0x28), (stack+0x30,stack+0x38),
	// ... terminated by NULL GUID*. R9 is the 4th MS-x64 register arg.
	type pair struct {
		g     [16]byte
		iface uint64
	}
	slots := []uint64{state.Regs[cpu.RDX], state.Regs[cpu.R8], state.Regs[cpu.R9]}
	rsp := state.Regs[cpu.RSP]
	for i := 0; i < 64; i++ {
		gPtr, _ := ram.Read64(rsp + 0x28 + uint64(i*16))
		ifPtr, _ := ram.Read64(rsp + 0x30 + uint64(i*16))
		slots = append(slots, gPtr, ifPtr)
	}
	var pairs []pair
	for i := 0; i+1 < len(slots); i += 2 {
		gPtr, iface := slots[i], slots[i+1]
		if gPtr == 0 {
			break
		}
		g, err := readGUID(ram, gPtr)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		pairs = append(pairs, pair{g, iface})
		if len(pairs) > 32 {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
	}
	if len(pairs) == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	h, _ := ram.Read64(hPtr)
	if install && h == 0 {
		seq, _ := ram.Read64(HandleSeqAddr)
		h = seq
		_ = ram.Write64(HandleSeqAddr, seq+1)
		_ = ram.Write64(hPtr, h)
	}
	for _, p := range pairs {
		if install {
			m := fwState.protos[h]
			if m == nil {
				m = make(map[[16]byte]uint64)
				fwState.protos[h] = m
			}
			m[p.g] = p.iface
		} else {
			m := fwState.protos[h]
			if m == nil {
				state.Regs[cpu.RAX] = EFIStatusNotFound
				return nil
			}
			if _, ok := m[p.g]; !ok {
				state.Regs[cpu.RAX] = EFIStatusNotFound
				return nil
			}
			delete(m, p.g)
			if len(m) == 0 {
				delete(fwState.protos, h)
			}
		}
	}
	bumpMapKey(ram)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- config tables ----

func rebuildConfigTable(ram *mmu.RAM) error {
	n := len(fwState.configs)
	var arr uint64
	if n > 0 {
		a, err := allocBump(ram, uint64(n*24), 8)
		if err != nil {
			return err
		}
		arr = a
		for i, e := range fwState.configs {
			g := e[0].([16]byte)
			_ = ram.LoadBytes(arr+uint64(i*24), g[:])
			_ = ram.Write64(arr+uint64(i*24)+16, e[1].(uint64))
		}
	}
	_ = ram.Write64(SystemTableBaseAddr+104, uint64(n))
	_ = ram.Write64(SystemTableBaseAddr+112, arr)
	return nil
}

func do_svcInstallConfigTable(state *cpu.CPUState, ram *mmu.RAM) error {
	gPtr := state.Regs[cpu.RCX]
	table := state.Regs[cpu.RDX]
	if gPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	g, err := readGUID(ram, gPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	kept := fwState.configs[:0]
	for _, e := range fwState.configs {
		if e[0].([16]byte) != g {
			kept = append(kept, e)
		}
	}
	fwState.configs = kept
	if table != 0 {
		fwState.configs = append(fwState.configs, [2]any{g, table})
	}
	if err := rebuildConfigTable(ram); err != nil {
		state.Regs[cpu.RAX] = EFIStatusOutOfResources
		return nil
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- misc memory services ----

func copyGuestMem(ram *mmu.RAM, dst, src, length uint64) error {
	if length == 0 {
		return nil
	}
	if length > 1<<31 {
		return errNoMem
	}
	if sd, err1 := ram.RawBuffer(src, length); err1 == nil {
		if dd, err2 := ram.RawBuffer(dst, length); err2 == nil {
			copy(dd, sd)
			return nil
		}
	}
	const chunk = 1 << 20
	tmp := make([]byte, chunk)
	for off := uint64(0); off < length; {
		n := length - off
		if n > chunk {
			n = chunk
		}
		for i := uint64(0); i < n; i++ {
			b, err := ram.Read8(src + off + i)
			if err != nil {
				return err
			}
			tmp[i] = b
		}
		for i := uint64(0); i < n; i++ {
			if err := ram.Write8(dst+off+i, tmp[i]); err != nil {
				return err
			}
		}
		off += n
	}
	return nil
}

func do_svcCopyMem(state *cpu.CPUState, ram *mmu.RAM) error {
	if err := copyGuestMem(ram, state.Regs[cpu.RCX], state.Regs[cpu.RDX], state.Regs[cpu.R8]); err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcSetMem(state *cpu.CPUState, ram *mmu.RAM) error {
	dst := state.Regs[cpu.RCX]
	size := state.Regs[cpu.RDX]
	val := uint8(state.Regs[cpu.R8])
	if size > 1<<31 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if buf, err := ram.RawBuffer(dst, size); err == nil {
		for i := range buf {
			buf[i] = val
		}
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	}
	for i := uint64(0); i < size; i++ {
		if err := ram.Write8(dst+i, val); err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcCrc32(state *cpu.CPUState, ram *mmu.RAM) error {
	data := state.Regs[cpu.RCX]
	length := state.Regs[cpu.RDX]
	out := state.Regs[cpu.R8]
	if out == 0 || length > 1<<31 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	h := crc32.NewIEEE()
	const chunk = 1 << 20
	for off := uint64(0); off < length; {
		n := length - off
		if n > chunk {
			n = chunk
		}
		if buf, err := ram.RawBuffer(data+off, n); err == nil {
			_, _ = h.Write(buf)
		} else {
			for i := uint64(0); i < n; i++ {
				b, err := ram.Read8(data + off + i)
				if err != nil {
					state.Regs[cpu.RAX] = EFIStatusInvalidParameter
					return nil
				}
				_, _ = h.Write([]byte{b})
			}
		}
		off += n
	}
	_ = ram.Write32(out, h.Sum32())
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- boot flow ----

func do_svcExitBootServices(state *cpu.CPUState, ram *mmu.RAM) error {
	key, _ := ram.Read64(MapKeyAddr)
	if state.Regs[cpu.RDX] != key {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram.Write64(ExitedFlagAddr, 1)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcExit(state *cpu.CPUState, ram *mmu.RAM) error {
	state.Regs[cpu.RAX] = state.Regs[cpu.RDX]
	state.Halted = true
	return nil
}

func do_svcStall(state *cpu.CPUState, ram *mmu.RAM) error {
	us := state.Regs[cpu.RCX]
	if us > 1000000 {
		us = 1000000
	}
	time.Sleep(time.Duration(us) * time.Microsecond)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func svcMono(state *cpu.CPUState, ram *mmu.RAM, high bool) error {
	out := state.Regs[cpu.RCX]
	if out == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	if high {
		v, _ := ram.Read64(MonoHighAddr)
		_ = ram.Write64(MonoHighAddr, v+1)
		_ = ram.Write64(out, v+1)
	} else {
		v, _ := ram.Read64(MonoCountAddr)
		_ = ram.Write64(MonoCountAddr, v+1)
		_ = ram.Write64(out, v+1)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- runtime services ----

func do_svcGetTime(state *cpu.CPUState, ram *mmu.RAM) error {
	tPtr := state.Regs[cpu.RCX]
	capPtr := state.Regs[cpu.RDX]
	if tPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	now := time.Now().UTC()
	_ = ram.Write16(tPtr+0, uint16(now.Year()))
	_ = ram.Write8(tPtr+2, uint8(now.Month()))
	_ = ram.Write8(tPtr+3, uint8(now.Day()))
	_ = ram.Write8(tPtr+4, uint8(now.Hour()))
	_ = ram.Write8(tPtr+5, uint8(now.Minute()))
	_ = ram.Write8(tPtr+6, uint8(now.Second()))
	_ = ram.Write8(tPtr+7, 0)
	_ = ram.Write32(tPtr+8, uint32(now.Nanosecond()))
	_ = ram.Write16(tPtr+12, 0x07FF)
	_ = ram.Write8(tPtr+14, 0)
	_ = ram.Write8(tPtr+15, 0)
	if capPtr != 0 {
		_ = ram.Write64(capPtr, 0)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func readUCS2Name(ram *mmu.RAM, addr uint64) (string, bool) {
	var u16 []uint16
	for i := 0; i < 512; i++ {
		ch, err := ram.Read16(addr + uint64(i*2))
		if err != nil {
			return "", false
		}
		if ch == 0 {
			break
		}
		u16 = append(u16, ch)
	}
	if len(u16) == 0 || len(u16) >= 512 {
		return "", false
	}
	runes := make([]rune, 0, len(u16))
	for _, c := range u16 {
		runes = append(runes, rune(c))
	}
	return string(runes), true
}

func varKey(name string, g [16]byte) string {
	return name + "|" + string(g[:])
}

func sortedVarKeys() []string {
	keys := make([]string, 0, len(fwState.vars))
	for k := range fwState.vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func splitVarKey(k string) (string, [16]byte) {
	var g [16]byte
	for i := 0; i < 16 && i < len(k); i++ {
		_ = i
	}
	// key format: name + "|" + 16 raw bytes
	idx := -1
	for i := len(k) - 17; i >= 0; i-- {
		if k[i] == '|' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return k, g
	}
	copy(g[:], k[idx+1:])
	return k[:idx], g
}

func writeUCS2(ram *mmu.RAM, addr uint64, s string) uint64 {
	i := 0
	for _, r := range s {
		c := uint16(r)
		if r > 0xFFFF {
			c = '?'
		}
		_ = ram.Write16(addr+uint64(i*2), c)
		i++
	}
	_ = ram.Write16(addr+uint64(i*2), 0)
	return uint64(i+1) * 2
}

func do_svcGetVariable(state *cpu.CPUState, ram *mmu.RAM) error {
	namePtr := state.Regs[cpu.RCX]
	guidPtr := state.Regs[cpu.RDX]
	attrPtr := state.Regs[cpu.R8]
	sizePtr := state.Regs[cpu.R9]
	rsp := state.Regs[cpu.RSP]
	dataPtr, _ := ram.Read64(rsp + 0x28)
	if namePtr == 0 || guidPtr == 0 || sizePtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	name, ok := readUCS2Name(ram, namePtr)
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	g, err := readGUID(ram, guidPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	v, ok := fwState.vars[varKey(name, g)]
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	need := uint64(len(v.data))
	have, _ := ram.Read64(sizePtr)
	if dataPtr == 0 || have < need {
		_ = ram.Write64(sizePtr, need)
		state.Regs[cpu.RAX] = EFIStatusBufferTooSmall
		return nil
	}
	if need > 0 {
		if buf, err := ram.RawBuffer(dataPtr, need); err == nil {
			copy(buf, v.data)
		} else {
			for i, b := range v.data {
				_ = ram.Write8(dataPtr+uint64(i), b)
			}
		}
	}
	_ = ram.Write64(sizePtr, need)
	if attrPtr != 0 {
		_ = ram.Write32(attrPtr, v.attr)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcSetVariable(state *cpu.CPUState, ram *mmu.RAM) error {
	namePtr := state.Regs[cpu.RCX]
	guidPtr := state.Regs[cpu.RDX]
	attr := uint32(state.Regs[cpu.R8])
	size := state.Regs[cpu.R9]
	rsp := state.Regs[cpu.RSP]
	dataPtr, _ := ram.Read64(rsp + 0x28)
	if namePtr == 0 || guidPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	name, ok := readUCS2Name(ram, namePtr)
	if !ok || name == "" {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	g, err := readGUID(ram, guidPtr)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	k := varKey(name, g)
	if size == 0 || attr == 0 {
		delete(fwState.vars, k)
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	}
	if size > 1<<20 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	data := make([]byte, size)
	if buf, err := ram.RawBuffer(dataPtr, size); err == nil {
		copy(data, buf)
	} else {
		for i := uint64(0); i < size; i++ {
			b, err := ram.Read8(dataPtr + i)
			if err != nil {
				state.Regs[cpu.RAX] = EFIStatusInvalidParameter
				return nil
			}
			data[i] = b
		}
	}
	fwState.vars[k] = &efiVar{attr: attr, data: data}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcGetNextVariableName(state *cpu.CPUState, ram *mmu.RAM) error {
	sizePtr := state.Regs[cpu.RCX]
	namePtr := state.Regs[cpu.RDX]
	guidPtr := state.Regs[cpu.R8]
	if sizePtr == 0 || namePtr == 0 || guidPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	keys := sortedVarKeys()
	cur := -1
	name, ok := readUCS2Name(ram, namePtr)
	if ok && name != "" {
		g, err := readGUID(ram, guidPtr)
		if err == nil {
			k := varKey(name, g)
			for i, kk := range keys {
				if kk == k {
					cur = i
					break
				}
			}
			if cur < 0 {
				state.Regs[cpu.RAX] = EFIStatusInvalidParameter
				return nil
			}
		}
	}
	if cur+1 >= len(keys) {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	nm, g := splitVarKey(keys[cur+1])
	need := uint64(len([]rune(nm))+1) * 2
	have, _ := ram.Read64(sizePtr)
	if have < need {
		_ = ram.Write64(sizePtr, need)
		state.Regs[cpu.RAX] = EFIStatusBufferTooSmall
		return nil
	}
	writeUCS2(ram, namePtr, nm)
	_ = ram.LoadBytes(guidPtr, g[:])
	_ = ram.Write64(sizePtr, need)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func do_svcResetSystem(state *cpu.CPUState, ram *mmu.RAM) error {
	state.Halted = true
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// DispatchServices routes the extended hypercall set. Returns handled=false
// when the id belongs to the core GOP/text handler in gop.go.
func DispatchServices(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller, drm *graphics.KernelDRMBridge) (bool, error) {
	_ = drm
	switch state.Regs[cpu.RAX] {
	case svcReadKeyStroke:
		return true, conInReadKeyStroke(state, ram, ps2)
	case svcConInReset:
		return true, conInReset(state, ram, ps2)
	case svcWaitForKey:
		return true, conInWaitForKey(state, ram)
	case svcAllocatePages:
		return true, svcAllocPages(state, ram)
	case svcFreePages, svcFreePool:
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcGetMemoryMap:
		return true, svcGetMemMap(state, ram)
	case svcAllocatePool:
		return true, svcAllocPool(state, ram)
	case svcCreateEvent:
		return true, do_svcCreateEvent(state, ram, false)
	case svcCreateEventEx:
		return true, do_svcCreateEvent(state, ram, true)
	case svcSetTimer:
		return true, do_svcSetTimer(state, ram)
	case svcWaitForEvent:
		return true, do_svcWaitForEvent(state, ram, ps2)
	case svcSignalEvent:
		if ev, ok := fwState.events[state.Regs[cpu.RCX]]; ok {
			ev.signaled = true
			state.Regs[cpu.RAX] = EFIStatusSuccess
		} else {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		}
		return true, nil
	case svcCloseEvent:
		id := state.Regs[cpu.RCX]
		if id == EvtConInKey {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return true, nil
		}
		if _, ok := fwState.events[id]; !ok {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return true, nil
		}
		delete(fwState.events, id)
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcCheckEvent:
		return true, do_svcCheckEvent(state, ram, ps2)
	case svcInstallProtocol:
		return true, svcInstallProto(state, ram)
	case svcReinstallProtocol:
		return true, svcReinstallProto(state, ram)
	case svcUninstallProtocol:
		return true, svcUninstallProto(state, ram)
	case svcRegisterNotify:
		// (Protocol, Event, Registration**): store event in a bump cell.
		if state.Regs[cpu.R8] == 0 {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return true, nil
		}
		cell, err := allocBump(ram, 8, 8)
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusOutOfResources
			return true, nil
		}
		_ = ram.Write64(cell, state.Regs[cpu.RDX])
		_ = ram.Write64(state.Regs[cpu.R8], cell)
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcLocateHandle:
		return true, do_svcLocateHandle(state, ram, false)
	case svcLocateHandleBuffer:
		return true, do_svcLocateHandle(state, ram, true)
	case svcInstallConfigTable:
		return true, do_svcInstallConfigTable(state, ram)
	case svcExitBootServices:
		return true, do_svcExitBootServices(state, ram)
	case svcGetNextMono:
		return true, svcMono(state, ram, false)
	case svcGetNextHighMono:
		return true, svcMono(state, ram, true)
	case svcStall:
		return true, do_svcStall(state, ram)
	case svcConnectController, svcDisconnectControl, svcCloseProtocol:
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcOpenProtocolInfo:
		return true, svcOpenProtoInfo(state, ram)
	case svcProtocolsPerHandle:
		return true, do_svcProtocolsPerHandle(state, ram)
	case svcInstallMultiple:
		return true, multiProto(state, ram, true)
	case svcUninstallMultiple:
		return true, multiProto(state, ram, false)
	case svcCrc32:
		return true, do_svcCrc32(state, ram)
	case svcCopyMem:
		return true, do_svcCopyMem(state, ram)
	case svcSetMem:
		return true, do_svcSetMem(state, ram)
	case svcTestString:
		return true, conOutTestString(state, ram)
	case svcConOutQueryMode:
		return true, conOutQueryMode(state, ram)
	case svcConOutSetMode:
		return true, conOutSetMode(state, ram)
	case svcSetAttribute:
		return true, conOutSetAttribute(state, ram)
	case svcSetCursorPosition:
		return true, conOutSetCursor(state, ram)
	case svcEnableCursor:
		return true, conOutEnableCursor(state, ram)
	case svcConOutReset:
		return true, conOutReset(state, ram)
	case svcExit:
		return true, do_svcExit(state, ram)
	case svcRaiseTPL, svcRestoreTPL:
		old, _ := ram.Read64(TplCellAddr)
		_ = ram.Write64(TplCellAddr, state.Regs[cpu.RCX])
		state.Regs[cpu.RAX] = old
		return true, nil
	case svcGetTime:
		return true, do_svcGetTime(state, ram)
	case svcSetVirtAddrMap:
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcGetVariable:
		return true, do_svcGetVariable(state, ram)
	case svcGetNextVariableName:
		return true, do_svcGetNextVariableName(state, ram)
	case svcSetVariable:
		return true, do_svcSetVariable(state, ram)
	case svcResetSystem:
		return true, do_svcResetSystem(state, ram)
	case svcBlockReset, svcBlockFlush, svcFileClose, svcFileFlush:
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcBlockRead:
		return true, doBlockRead(state, ram)
	case svcBlockWrite:
		return true, doBlockWrite(state, ram)
	case svcFSOpenVolume:
		return true, doFSOpenVolume(state, ram)
	case svcFileOpen:
		return true, doFileOpen(state, ram)
	case svcFileRead:
		return true, doFileRead(state, ram)
	case svcFileGetPos:
		return true, doFileGetPos(state, ram)
	case svcFileSetPos:
		return true, doFileSetPos(state, ram)
	case svcFileGetInfo:
		return true, doFileGetInfo(state, ram)
	case svcFileWrite:
		state.Regs[cpu.RAX] = EFIStatusWriteProtected
		return true, nil
	case svcLoadImage:
		return true, doLoadImage(state, ram)
	case svcStartImage:
		return true, doStartImage(state, ram)
	case svcUnloadImage:
		delete(fwState.images, state.Regs[cpu.RCX])
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return true, nil
	case svcLocateDevicePath:
		return true, doLocateDevicePath(state, ram)
	}
	return false, nil
}
