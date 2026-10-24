package cpu

import "testing"

type probeMem struct{ e []uint64 }

func (m *probeMem) Read8(a uint64) (uint8, error)    { return byte(m.e[a>>3] >> (8 * uint(a&7))), nil }
func (m *probeMem) Read16(a uint64) (uint16, error)  { return 0, nil }
func (m *probeMem) Read32(a uint64) (uint32, error)  { return 0, nil }
func (m *probeMem) Read64(a uint64) (uint64, error)  { return m.e[a>>3], nil }
func (m *probeMem) Write8(a uint64, v uint8) error   { return nil }
func (m *probeMem) Write16(a uint64, v uint16) error { return nil }
func (m *probeMem) Write32(a uint64, v uint32) error { return nil }
func (m *probeMem) Write64(a uint64, v uint64) error { return nil }

func TestTranslateLargePages(t *testing.T) {
	c := NewCPUState()
	c.CR0 = CR0PE | CR0PG
	c.CR3 = 0x1000 // PML4 base
	m := &probeMem{e: make([]uint64, 0x20000)}

	// PML4(0x1000) -> PDPT(0x2000) -> PDE(0x3000) -> PTE(0x4000) -> phys 0x800000.
	m.e[0x1000>>3] = 0x2000 | 1   // PML4[0] -> PDPT at 0x2000
	m.e[0x2000>>3] = 0x3000 | 1   // PDPT[0] -> PDE at 0x3000
	m.e[0x3000>>3] = 0x4000 | 1   // PDE[0]  -> PTE at 0x4000
	m.e[0x4000>>3] = 0x800000 | 1 // PTE[0]  -> phys 0x800000
	if p, ok := c.Translate(0, m); !ok || p != 0x800000 {
		t.Fatalf("4KB translate = 0x%x ok=%v, want 0x800000 ok=true", p, ok)
	}

	// 2MB large page: PDE[0] = physBase | PSE(0x80); covers vaddr [0, 2MB).
	m.e[0x3000>>3] = 0x800000 | 1 | 0x80
	if p, ok := c.Translate(0x1234, m); !ok || p != 0x801234 {
		t.Fatalf("2MB translate = 0x%x ok=%v, want 0x801234 ok=true", p, ok)
	}

	// 1GB large page: PDPT[0] = physBase | PSE(0x80); covers vaddr [0, 1GB).
	m.e[0x2000>>3] = 0x80000000 | 1 | 0x80
	if p, ok := c.Translate(0x12345678, m); !ok || p != 0x92345678 {
		t.Fatalf("1GB translate = 0x%x ok=%v, want 0x92345678 ok=true", p, ok)
	}
}
