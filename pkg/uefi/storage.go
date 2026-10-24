package uefi

// Minimal boot-volume services: Block I/O, Simple File System, EFI_FILE,
// LoadImage/StartImage/UnloadImage, LocateDevicePath. Backed by the host
// BlockStorage captured at BootUEFI time. FAT16/FAT32 8.3 paths only;
// directory reads return empty (enumeration ends), writes are rejected.

import (
	"bytes"
	"debug/pe"
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
	"strings"
)

type fatFile struct {
	data []byte
	pos  uint64
	root bool
	name string
}

// EFI_BLOCK_IO_MEDIA at BlockIoMediaAddr: MediaId u32, flags, BlockSize, LastBlock.
const (
	mediaLastBlockOff = 24
	fileStructSize    = 96
)

// fileInfoGUID = 09576E92-6D3F-11D2-8E39-00A0C969723B
var fileInfoGUID = [16]byte{0x92, 0x6E, 0x57, 0x09, 0x3F, 0x6D, 0xD2, 0x11,
	0x8E, 0x39, 0x00, 0xA0, 0xC9, 0x69, 0x72, 0x3B}

// SetBootDevice captures the boot volume for BlockIo/File services.
func SetBootDevice(dev devices.BlockStorage, fatLBA uint64) {
	fwState.bootDev = dev
	fwState.bootFAT = fatLBA
}

func bootDev() devices.BlockStorage { return fwState.bootDev }

// ---- Block I/O ----

// (This, MediaId, LBA, BufferSize, Buffer)
func doBlockRead(state *cpu.CPUState, ram *mmu.RAM) error {
	return doBlockRW(state, ram, false)
}

func doBlockWrite(state *cpu.CPUState, ram *mmu.RAM) error {
	return doBlockRW(state, ram, true)
}

func doBlockRW(state *cpu.CPUState, ram *mmu.RAM, write bool) error {
	dev := fwState.bootDev
	lba := state.Regs[cpu.R8]
	size := state.Regs[cpu.R9]
	bufPtr, _ := ram.Read64(state.Regs[cpu.RSP] + 0x28)
	if dev == nil || bufPtr == 0 || size == 0 || size%512 != 0 || size > 1<<24 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	n := size / 512
	if lba+n < lba || lba+n > dev.TotalSectors() {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	sec := make([]byte, 512)
	for i := uint64(0); i < n; i++ {
		if write {
			for j := 0; j < 512; j++ {
				b, err := ram.Read8(bufPtr + i*512 + uint64(j))
				if err != nil {
					state.Regs[cpu.RAX] = EFIStatusInvalidParameter
					return nil
				}
				sec[j] = b
			}
			if err := dev.WriteSector(lba+i, sec); err != nil {
				state.Regs[cpu.RAX] = EFIStatusWriteProtected
				return nil
			}
		} else {
			if err := dev.ReadSector(lba+i, sec); err != nil {
				state.Regs[cpu.RAX] = EFIStatusDeviceError
				return nil
			}
			if dst, err := ram.RawBuffer(bufPtr+i*512, 512); err == nil {
				copy(dst, sec)
			} else {
				for j := 0; j < 512; j++ {
					if err := ram.Write8(bufPtr+i*512+uint64(j), sec[j]); err != nil {
						state.Regs[cpu.RAX] = EFIStatusInvalidParameter
						return nil
					}
				}
			}
		}
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- Simple File System ----

// (This, Root**)
func doFSOpenVolume(state *cpu.CPUState, ram *mmu.RAM) error {
	out := state.Regs[cpu.RDX]
	if out == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram.Write64(out, FileRootAddr)
	fwState.files[FileRootAddr] = &fatFile{root: true, name: "\\"}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

func fileStruct(ram *mmu.RAM, f *fatFile) (uint64, error) {
	addr, err := allocBump(ram, fileStructSize, 8)
	if err != nil {
		return 0, err
	}
	_ = ram.Write64(addr+0, 0x00010000) // Revision
	_ = ram.Write64(addr+8, stubFileOpen)
	_ = ram.Write64(addr+16, stubFileClose)
	_ = ram.Write64(addr+24, SuccessStubAddr) // Delete: always SUCCESS
	_ = ram.Write64(addr+32, stubFileRead)
	_ = ram.Write64(addr+40, stubFileWrite)
	_ = ram.Write64(addr+48, stubFileGetPos)
	_ = ram.Write64(addr+56, stubFileSetPos)
	_ = ram.Write64(addr+64, stubFileGetInfo)
	_ = ram.Write64(addr+72, SuccessStubAddr) // SetInfo: always SUCCESS
	_ = ram.Write64(addr+80, stubFileFlush)
	_ = ram.Write64(addr+88, 0)
	fwState.files[addr] = f
	return addr, nil
}

func lookupFile(this uint64) *fatFile {
	if this == FileRootAddr {
		if f, ok := fwState.files[this]; ok {
			return f
		}
		return &fatFile{root: true, name: "\\"}
	}
	return fwState.files[this]
}

// (This, NewHandle**, FileName*, OpenMode, Attributes)
func doFileOpen(state *cpu.CPUState, ram *mmu.RAM) error {
	this := state.Regs[cpu.RCX]
	outPtr := state.Regs[cpu.RDX]
	namePtr := state.Regs[cpu.R8]
	if outPtr == 0 || namePtr == 0 || lookupFile(this) == nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	name, ok := readUCS2Name(ram, namePtr)
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	data, fname, err := fatReadPath(name)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	addr, err := fileStruct(ram, &fatFile{data: data, name: fname})
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusOutOfResources
		return nil
	}
	_ = ram.Write64(outPtr, addr)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// (This, BufferSize*, Buffer)
func doFileRead(state *cpu.CPUState, ram *mmu.RAM) error {
	f := lookupFile(state.Regs[cpu.RCX])
	sizePtr := state.Regs[cpu.RDX]
	buf := state.Regs[cpu.R8]
	if f == nil || sizePtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	want, _ := ram.Read64(sizePtr)
	if f.root {
		// ponytail: no directory enumeration; guests open paths directly.
		_ = ram.Write64(sizePtr, 0)
		state.Regs[cpu.RAX] = EFIStatusSuccess
		return nil
	}
	if f.pos > uint64(len(f.data)) {
		f.pos = uint64(len(f.data))
	}
	avail := uint64(len(f.data)) - f.pos
	if want > avail {
		want = avail
	}
	if dst, err := ram.RawBuffer(buf, want); err == nil {
		copy(dst, f.data[f.pos:f.pos+want])
	} else {
		for i := uint64(0); i < want; i++ {
			if err := ram.Write8(buf+i, f.data[f.pos+i]); err != nil {
				state.Regs[cpu.RAX] = EFIStatusInvalidParameter
				return nil
			}
		}
	}
	f.pos += want
	_ = ram.Write64(sizePtr, want)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// (This, Position)
func doFileSetPos(state *cpu.CPUState, ram *mmu.RAM) error {
	f := lookupFile(state.Regs[cpu.RCX])
	if f == nil {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	pos := state.Regs[cpu.RDX]
	if pos == 0xFFFFFFFFFFFFFFFF {
		pos = uint64(len(f.data))
	}
	if pos > uint64(len(f.data)) {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	f.pos = pos
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// (This, Position*)
func doFileGetPos(state *cpu.CPUState, ram *mmu.RAM) error {
	f := lookupFile(state.Regs[cpu.RCX])
	out := state.Regs[cpu.RDX]
	if f == nil || out == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram.Write64(out, f.pos)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// (This, InfoType*, BufferSize*, Buffer): always EFI_FILE_INFO.
func doFileGetInfo(state *cpu.CPUState, ram *mmu.RAM) error {
	f := lookupFile(state.Regs[cpu.RCX])
	sizePtr := state.Regs[cpu.R8]
	buf := state.Regs[cpu.R9]
	if f == nil || sizePtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	nameU16 := []uint16{0}
	if f.name != "" {
		nameU16 = nameU16[:0]
		for _, r := range f.name {
			nameU16 = append(nameU16, uint16(r))
		}
		nameU16 = append(nameU16, 0)
	}
	need := uint64(80 + len(nameU16)*2)
	have, _ := ram.Read64(sizePtr)
	if buf == 0 || have < need {
		_ = ram.Write64(sizePtr, need)
		state.Regs[cpu.RAX] = EFIStatusBufferTooSmall
		return nil
	}
	_ = ram.Write64(buf+0, need)
	_ = ram.Write64(buf+8, uint64(len(f.data)))
	_ = ram.Write64(buf+16, uint64(len(f.data)))
	for i := uint64(24); i < 72; i++ {
		_ = ram.Write8(buf+i, 0)
	}
	var attr uint64
	if f.root {
		attr = 0x10
	}
	_ = ram.Write64(buf+72, attr)
	for i, ch := range nameU16 {
		_ = ram.Write16(buf+80+uint64(i*2), ch)
	}
	_ = ram.Write64(sizePtr, need)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// ---- FAT reader (8.3, FAT16/FAT32) ----

type fatCtx struct {
	dev           devices.BlockStorage
	fatLBA        uint64
	secPerCluster uint64
	reserved      uint64
	numFATs       uint64
	fatSize       uint64
	fat32         bool
	rootEntries   uint64
	rootLBA       uint64
	dataLBA       uint64
}

func fatParse(dev devices.BlockStorage, startLBA uint64) (*fatCtx, error) {
	var bpb [512]byte
	if err := dev.ReadSector(startLBA, bpb[:]); err != nil {
		return nil, err
	}
	if bpb[11] != 0 || bpb[12] != 2 { // bytes/sector must be 512
		return nil, errNoMem
	}
	c := &fatCtx{dev: dev}
	c.secPerCluster = uint64(bpb[13])
	c.reserved = uint64(bpb[14]) | uint64(bpb[15])<<8
	c.numFATs = uint64(bpb[16])
	c.rootEntries = uint64(bpb[17]) | uint64(bpb[18])<<8
	f16 := uint64(bpb[22]) | uint64(bpb[23])<<8
	f32 := uint64(bpb[36]) | uint64(bpb[37])<<8 | uint64(bpb[38])<<16 | uint64(bpb[39])<<24
	if c.secPerCluster == 0 || c.reserved == 0 {
		return nil, errNoMem
	}
	if f16 != 0 {
		c.fatSize = f16
		c.rootLBA = startLBA + c.reserved + c.numFATs*c.fatSize
		rootSecs := (c.rootEntries*32 + 511) / 512
		c.dataLBA = c.rootLBA + rootSecs
	} else if f32 != 0 {
		c.fatSize = f32
		c.fat32 = true
		rootCl := uint64(bpb[44]) | uint64(bpb[45])<<8 | uint64(bpb[46])<<16 | uint64(bpb[47])<<24
		if rootCl < 2 {
			rootCl = 2
		}
		c.dataLBA = startLBA + c.reserved + c.numFATs*c.fatSize
		c.rootLBA = c.dataLBA + (rootCl-2)*c.secPerCluster
	} else {
		return nil, errNoMem
	}
	c.fatLBA = startLBA + c.reserved
	return c, nil
}

func (c *fatCtx) nextCluster(cl uint64) (uint64, bool) {
	var off uint64
	var sec [512]byte
	if c.fat32 {
		off = cl * 4
		if err := c.dev.ReadSector(c.fatLBA+off/512, sec[:]); err != nil {
			return 0, true
		}
		v := uint64(sec[off%512]) | uint64(sec[off%512+1])<<8 | uint64(sec[off%512+2])<<16 | uint64(sec[off%512+3])<<24
		v &= 0x0FFFFFFF
		if v >= 0x0FFFFFF8 || v < 2 {
			return 0, true
		}
		return v, false
	}
	off = cl * 2
	if err := c.dev.ReadSector(c.fatLBA+off/512, sec[:]); err != nil {
		return 0, true
	}
	v := uint64(sec[off%512]) | uint64(sec[off%512+1])<<8
	if v >= 0xFFF8 || v < 2 {
		return 0, true
	}
	return v, false
}

func (c *fatCtx) clusterLBA(cl uint64) uint64 {
	return c.dataLBA + (cl-2)*c.secPerCluster
}

func (c *fatCtx) readChain(first uint64, size uint64) ([]byte, error) {
	out := make([]byte, 0, size)
	sec := make([]byte, 512)
	for cl := first; uint64(len(out)) < size; {
		for s := uint64(0); s < c.secPerCluster && uint64(len(out)) < size; s++ {
			if err := c.dev.ReadSector(c.clusterLBA(cl)+s, sec); err != nil {
				return nil, err
			}
			need := size - uint64(len(out))
			if need > 512 {
				need = 512
			}
			out = append(out, sec[:need]...)
		}
		nx, end := c.nextCluster(cl)
		if end {
			break
		}
		cl = nx
	}
	if uint64(len(out)) > size {
		out = out[:size]
	}
	return out, nil
}

type fatEntry struct {
	name11 [11]byte
	attr   uint8
	first  uint64
	size   uint32
}

func scanDirSector(sec []byte, out *[]fatEntry) bool {
	done := false
	for o := 0; o < 512; o += 32 {
		if sec[o] == 0x00 {
			done = true
			break
		}
		if sec[o] == 0xE5 {
			continue
		}
		attr := sec[o+11]
		if attr == 0x0F { // LFN, skip (8.3 names suffice)
			continue
		}
		var e fatEntry
		copy(e.name11[:], sec[o:o+11])
		e.attr = attr
		lo := uint64(sec[o+26]) | uint64(sec[o+27])<<8
		hi := uint64(sec[o+20]) | uint64(sec[o+21])<<8
		e.first = lo | hi<<16
		e.size = uint32(uint64(sec[o+28]) | uint64(sec[o+29])<<8 | uint64(sec[o+30])<<16 | uint64(sec[o+31])<<24)
		*out = append(*out, e)
	}
	return done
}

func (c *fatCtx) listDir(first uint64, isRoot bool) ([]fatEntry, error) {
	var out []fatEntry
	sec := make([]byte, 512)
	if isRoot && !c.fat32 {
		nSecs := (c.rootEntries*32 + 511) / 512
		for s := uint64(0); s < nSecs; s++ {
			if err := c.dev.ReadSector(c.rootLBA+s, sec); err != nil {
				return nil, err
			}
			if scanDirSector(sec, &out) {
				break
			}
		}
		return out, nil
	}
	cl := first
	if isRoot {
		// FAT32 root cluster derived from rootLBA at parse
		cl = 2 + (c.rootLBA-c.dataLBA)/c.secPerCluster
	}
	for {
		for s := uint64(0); s < c.secPerCluster; s++ {
			if err := c.dev.ReadSector(c.clusterLBA(cl)+s, sec); err != nil {
				return nil, err
			}
			if scanDirSector(sec, &out) {
				return out, nil
			}
		}
		nx, end := c.nextCluster(cl)
		if end {
			break
		}
		cl = nx
	}
	return out, nil
}

func fat83(comp string) [11]byte {
	var n [11]byte
	for i := range n {
		n[i] = ' '
	}
	comp = strings.ToUpper(comp)
	base, ext := comp, ""
	if i := strings.LastIndex(comp, "."); i >= 0 {
		base, ext = comp[:i], comp[i+1:]
	}
	for i := 0; i < len(base) && i < 8; i++ {
		n[i] = base[i]
	}
	for i := 0; i < len(ext) && i < 3; i++ {
		n[8+i] = ext[i]
	}
	return n
}

// fatReadPath resolves an EFI path (e.g. \EFI\BOOT\BOOTX64.EFI) on the boot FAT.
func fatReadPath(path string) ([]byte, string, error) {
	dev := fwState.bootDev
	if dev == nil {
		return nil, "", errNoMem
	}
	c, err := fatParse(dev, fwState.bootFAT)
	if err != nil {
		return nil, "", err
	}
	path = strings.ReplaceAll(path, "/", "\\")
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '\\' })
	if len(parts) == 0 {
		return nil, "", errNoMem
	}
	var entries []fatEntry
	first, isRoot := uint64(0), true
	for i, p := range parts {
		var err error
		entries, err = c.listDir(first, isRoot)
		if err != nil {
			return nil, "", err
		}
		want := fat83(p)
		found := false
		for _, e := range entries {
			if e.name11 == want {
				found = true
				if i == len(parts)-1 {
					if e.attr&0x10 != 0 {
						return []byte{}, p, nil
					}
					if e.first < 2 {
						return []byte{}, p, nil
					}
					data, err := c.readChain(e.first, uint64(e.size))
					return data, p, err
				}
				if e.attr&0x10 == 0 || e.first < 2 {
					return nil, "", errNoMem
				}
				first, isRoot = e.first, false
				break
			}
		}
		if !found {
			return nil, "", errNoMem
		}
	}
	return nil, "", errNoMem
}

// ---- images ----

func loadPEImage(ram *mmu.RAM, payload []byte) (entry, base, size uint64, err error) {
	pf, err := pe.NewFile(bytes.NewReader(payload))
	if err != nil {
		return 0, 0, 0, err
	}
	defer pf.Close()
	base = DefaultPayloadBase
	var ep uint32
	var imgBase uint64
	switch opt := pf.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		ep, imgBase = opt.AddressOfEntryPoint, opt.ImageBase
	case *pe.OptionalHeader32:
		ep, imgBase = opt.AddressOfEntryPoint, uint64(opt.ImageBase)
	}
	if imgBase != 0 {
		base = imgBase
	}
	var end uint64
	for _, sec := range pf.Sections {
		data, err := sec.Data()
		if err != nil {
			continue
		}
		_ = ram.LoadBytes(base+uint64(sec.VirtualAddress), data)
		if e := uint64(sec.VirtualAddress) + uint64(sec.VirtualSize); e > end {
			end = e
		}
		if e := uint64(sec.VirtualAddress) + uint64(len(data)); e > end {
			end = e
		}
	}
	return base + uint64(ep), base, end, nil
}

// (BootPolicy, ParentHandle, FilePath, SourceBuffer, SourceSize, ImageHandle*)
func doLoadImage(state *cpu.CPUState, ram *mmu.RAM) error {
	filePath := state.Regs[cpu.R8]
	srcBuf := state.Regs[cpu.R9]
	rsp := state.Regs[cpu.RSP]
	srcSize, _ := ram.Read64(rsp + 0x28)
	outPtr, _ := ram.Read64(rsp + 0x30)
	if outPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	var payload []byte
	if srcBuf != 0 {
		if srcSize == 0 || srcSize > 64<<20 {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		if buf, err := ram.RawBuffer(srcBuf, srcSize); err == nil {
			payload = append([]byte(nil), buf...)
		} else {
			payload = make([]byte, srcSize)
			for i := range payload {
				b, err := ram.Read8(srcBuf + uint64(i))
				if err != nil {
					state.Regs[cpu.RAX] = EFIStatusInvalidParameter
					return nil
				}
				payload[i] = b
			}
		}
	} else if filePath != 0 {
		name, ok := scanDevicePathFile(ram, filePath)
		if !ok {
			state.Regs[cpu.RAX] = EFIStatusNotFound
			return nil
		}
		data, _, err := fatReadPath(name)
		if err != nil || len(data) == 0 {
			state.Regs[cpu.RAX] = EFIStatusNotFound
			return nil
		}
		payload = data
	} else {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	entry, base, size, err := loadPEImage(ram, payload)
	if err != nil || entry == 0 {
		state.Regs[cpu.RAX] = EFIStatusUnsupported
		return nil
	}
	seq, _ := ram.Read64(HandleSeqAddr)
	h := seq
	_ = ram.Write64(HandleSeqAddr, seq+1)
	img, err := allocBump(ram, 96, 8)
	if err != nil {
		state.Regs[cpu.RAX] = EFIStatusOutOfResources
		return nil
	}
	_ = ram.Write32(img+0, 0x00010000)
	_ = ram.Write64(img+8, state.Regs[cpu.RDX]) // ParentHandle
	_ = ram.Write64(img+16, SystemTableBaseAddr)
	_ = ram.Write64(img+24, HandleBootVol)
	_ = ram.Write64(img+32, filePath)
	_ = ram.Write64(img+64, base)
	_ = ram.Write64(img+72, size)
	fwState.images[h] = entry
	m := map[[16]byte]uint64{loadedImgGUID: img}
	fwState.protos[h] = m
	_ = ram.Write64(outPtr, h)
	bumpMapKey(ram)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// (ImageHandle, ExitDataSize*, ExitData**): transfers control, no return.
func doStartImage(state *cpu.CPUState, ram *mmu.RAM) error {
	h := state.Regs[cpu.RCX]
	entry, ok := fwState.images[h]
	if !ok || entry == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	state.Regs[cpu.RCX] = h
	state.Regs[cpu.RDX] = SystemTableBaseAddr
	state.RIP = entry
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// scanDevicePathFile extracts a UCS2 file path from a guest device path.
func scanDevicePathFile(ram *mmu.RAM, addr uint64) (string, bool) {
	var u16 []uint16
	for i := 0; i < 256; i++ {
		ch, err := ram.Read16(addr + uint64(i*2))
		if err != nil {
			return "", false
		}
		if ch == 0 {
			if len(u16) > 0 {
				break
			}
			continue
		}
		u16 = append(u16, ch)
	}
	best := ""
	for s := 0; s < len(u16); s++ {
		var sb strings.Builder
		for i := s; i < len(u16) && i < s+128; i++ {
			ch := rune(u16[i])
			if ch < 32 || ch > 126 {
				break
			}
			sb.WriteRune(ch)
		}
		cand := sb.String()
		if strings.Contains(cand, ".") && (strings.Contains(cand, "\\") || strings.Contains(cand, "/")) {
			if len(cand) > len(best) {
				best = cand
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// (Protocol*, DevicePath**, Device*): resolve our boot volume path.
func doLocateDevicePath(state *cpu.CPUState, ram *mmu.RAM) error {
	pathPtr := state.Regs[cpu.RDX]
	devPtr := state.Regs[cpu.R8]
	if pathPtr == 0 || devPtr == 0 {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	path, _ := ram.Read64(pathPtr)
	if path != DevicePathAddr {
		state.Regs[cpu.RAX] = EFIStatusNotFound
		return nil
	}
	_ = ram.Write64(devPtr, HandleBootVol)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}
