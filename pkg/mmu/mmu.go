package mmu

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// RAM represents guest physical memory
type RAM struct {
	mu   sync.RWMutex
	data []byte
	size uint64
}

func NewRAM(size uint64) *RAM {
	return &RAM{
		data: make([]byte, size),
		size: size,
	}
}

func (r *RAM) Size() uint64 {
	return r.size
}

func (r *RAM) Read8(addr uint64) (uint8, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if addr >= r.size {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return r.data[addr], nil
}

func (r *RAM) Read16(addr uint64) (uint16, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if addr+2 > r.size {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint16(r.data[addr : addr+2]), nil
}

func (r *RAM) Read32(addr uint64) (uint32, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if addr+4 > r.size {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint32(r.data[addr : addr+4]), nil
}

func (r *RAM) Read64(addr uint64) (uint64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if addr+8 > r.size {
		return 0, fmt.Errorf("read out of bounds: 0x%x", addr)
	}
	return binary.LittleEndian.Uint64(r.data[addr : addr+8]), nil
}

func (r *RAM) Write8(addr uint64, val uint8) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if addr >= r.size {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	r.data[addr] = val
	return nil
}

func (r *RAM) Write16(addr uint64, val uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if addr+2 > r.size {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint16(r.data[addr:addr+2], val)
	return nil
}

func (r *RAM) Write32(addr uint64, val uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if addr+4 > r.size {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint32(r.data[addr:addr+4], val)
	return nil
}

func (r *RAM) Write64(addr uint64, val uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if addr+8 > r.size {
		return fmt.Errorf("write out of bounds: 0x%x", addr)
	}
	binary.LittleEndian.PutUint64(r.data[addr:addr+8], val)
	return nil
}

// LoadBytes copies guest image or code directly to target physical address
func (r *RAM) LoadBytes(addr uint64, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if addr+uint64(len(b)) > r.size {
		return fmt.Errorf("image exceeds memory size: 0x%x > 0x%x", addr+uint64(len(b)), r.size)
	}
	copy(r.data[addr:], b)
	return nil
}

// RawBuffer returns guest memory slice
func (r *RAM) RawBuffer(addr, length uint64) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if addr+length > r.size {
		return nil, fmt.Errorf("slice out of bounds: 0x%x + 0x%x", addr, length)
	}
	return r.data[addr : addr+length], nil
}
