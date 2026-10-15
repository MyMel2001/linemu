package mmu

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// MMIORegion maps a physical address range to an external byte slice (e.g. GPU framebuffer)
type MMIORegion struct {
	Base uint64
	Size uint64
	Data []byte // backing store owned by the device
}

// RAM represents guest physical memory with optional MMIO regions
type RAM struct {
	mu      sync.RWMutex
	data    []byte
	size    uint64
	mmio    []MMIORegion
}

func NewRAM(size uint64) *RAM {
	return &RAM{
		data: make([]byte, size),
		size: size,
	}
}

// MapMMIO registers a memory-mapped I/O region backed by an external byte slice.
// Reads/writes to [base, base+len(data)) go to data instead of guest RAM.
func (r *RAM) MapMMIO(base uint64, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mmio = append(r.mmio, MMIORegion{Base: base, Size: uint64(len(data)), Data: data})
}

// resolve returns the backing slice and offset for addr, checking MMIO regions first.
// Caller must hold at least r.mu.RLock.
func (r *RAM) resolve(addr uint64) ([]byte, uint64, error) {
	for i := range r.mmio {
		m := &r.mmio[i]
		if addr >= m.Base && addr < m.Base+m.Size {
			return m.Data, addr - m.Base, nil
		}
	}
	if addr >= r.size {
		return nil, 0, fmt.Errorf("access out of bounds: 0x%x", addr)
	}
	return r.data, addr, nil
}

func (r *RAM) Size() uint64 {
	return r.size
}

func (r *RAM) Read8(addr uint64) (uint8, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return 0, err
	}
	return buf[off], nil
}

func (r *RAM) Read16(addr uint64) (uint16, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return 0, err
	}
	if off+2 > uint64(len(buf)) {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint16(buf[off : off+2]), nil
}

func (r *RAM) Read32(addr uint64) (uint32, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return 0, err
	}
	if off+4 > uint64(len(buf)) {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint32(buf[off : off+4]), nil
}

func (r *RAM) Read64(addr uint64) (uint64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return 0, err
	}
	if off+8 > uint64(len(buf)) {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint64(buf[off : off+8]), nil
}

func (r *RAM) Write8(addr uint64, val uint8) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return err
	}
	buf[off] = val
	return nil
}

func (r *RAM) Write16(addr uint64, val uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return err
	}
	if off+2 > uint64(len(buf)) {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint16(buf[off:off+2], val)
	return nil
}

func (r *RAM) Write32(addr uint64, val uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return err
	}
	if off+4 > uint64(len(buf)) {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint32(buf[off:off+4], val)
	return nil
}

func (r *RAM) Write64(addr uint64, val uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return err
	}
	if off+8 > uint64(len(buf)) {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint64(buf[off:off+8], val)
	return nil
}

// LoadBytes copies guest image or code directly to target physical address
func (r *RAM) LoadBytes(addr uint64, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return err
	}
	if off+uint64(len(b)) > uint64(len(buf)) {
		return fmt.Errorf("image exceeds region size: 0x%x + 0x%x", addr, len(b))
	}
	copy(buf[off:], b)
	return nil
}

// RawBuffer returns guest memory slice
func (r *RAM) RawBuffer(addr, length uint64) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	buf, off, err := r.resolve(addr)
	if err != nil {
		return nil, err
	}
	if off+length > uint64(len(buf)) {
		return nil, fmt.Errorf("slice out of bounds: 0x%x + 0x%x", addr, length)
	}
	return buf[off : off+length], nil
}
