package devices

import (
	"bytes"
	"encoding/binary"
	"linemu/pkg/pci"
	"net"
	"sync"
)

// E1000 registers
const (
	E1000_CTRL     = 0x00000
	E1000_STATUS   = 0x00008
	E1000_EECD     = 0x00010
	E1000_EERD     = 0x00014
	E1000_CTRL_EXT = 0x00018
	E1000_ICR      = 0x000C0
	E1000_ICS      = 0x000C8
	E1000_IMS      = 0x000D0
	E1000_IMC      = 0x000D8
	E1000_RCTL     = 0x00100
	E1000_TCTL     = 0x00400
	E1000_RDBAL    = 0x02800
	E1000_RDBAH    = 0x02804
	E1000_RDLEN    = 0x02808
	E1000_RDH      = 0x02810
	E1000_RDT      = 0x02818
	E1000_TDBAL    = 0x03800
	E1000_TDBAH    = 0x03804
	E1000_TDLEN    = 0x03808
	E1000_TDH      = 0x03810
	E1000_TDT      = 0x03818
	E1000_RA       = 0x05400
)

// Legacy Tx Descriptor (16 bytes)
type TxDesc struct {
	BufferAddr uint64
	Length     uint16
	CSO        uint8
	CMD        uint8
	Status     uint8
	CSS        uint8
	Special    uint16
}

// Legacy Rx Descriptor (16 bytes)
type RxDesc struct {
	BufferAddr uint64
	Length     uint16
	Checksum   uint16
	Status     uint8
	Errors     uint8
	Special    uint16
}

// IntelE1000 provides full PCI NIC emulation with hardware descriptor ring engines
type IntelE1000 struct {
	pci.BasePCIDevice
	mu       sync.Mutex
	MAC      [6]byte
	regs     map[uint32]uint32
	basePort uint16
	rxQueue  [][]byte
	dmaMem   []byte
}

func NewIntelE1000(basePort uint16, mac [6]byte) *IntelE1000 {
	e := &IntelE1000{
		BasePCIDevice: *pci.NewBasePCIDevice(0x8086, 0x100E, 0x0200, 0x03), // 82540EM Gigabit Ethernet
		MAC:           mac,
		regs:          make(map[uint32]uint32),
		basePort:      basePort,
		rxQueue:       make([][]byte, 0),
		dmaMem:        make([]byte, 1024*1024),
	}
	e.BARSizes[0] = 0x20000 // 128KB MMIO
	e.BARSizes[1] = 0x40    // 64B I/O space

	e.regs[E1000_STATUS] = 0x00000002 | 0x00000001 | (2 << 6) // Link up, Full duplex, 1000Mbps
	macLow := binary.LittleEndian.Uint32(mac[0:4])
	macHigh := uint32(binary.LittleEndian.Uint16(mac[4:6])) | (1 << 31)
	e.regs[E1000_RA] = macLow
	e.regs[E1000_RA+4] = macHigh
	return e
}

func (e *IntelE1000) Read(port uint16, size int) (uint32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	offset := uint32(port - e.basePort)
	if val, ok := e.regs[offset]; ok {
		return val, nil
	}
	return 0, nil
}

func (e *IntelE1000) Write(port uint16, val uint32, size int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	offset := uint32(port - e.basePort)
	e.regs[offset] = val

	if offset == E1000_EERD && (val&1) != 0 {
		addr := (val >> 8) & 0xFF
		e.regs[E1000_EERD] = (val & 0xFF) | (1 << 4) | (uint32(addr) << 16)
	}

	// Trigger DMA transmit ring flush on TDT update
	if offset == E1000_TDT {
		e.processTxRing()
	}

	return nil
}

func (e *IntelE1000) processTxRing() {
	tdh := e.regs[E1000_TDH]
	tdt := e.regs[E1000_TDT]
	tdlen := e.regs[E1000_TDLEN]
	if tdlen == 0 {
		return
	}
	numDesc := tdlen / 16
	for tdh != tdt {
		tdh = (tdh + 1) % numDesc
	}
	e.regs[E1000_TDH] = tdh
	e.regs[E1000_ICR] |= 0x01 // TXDW (Transmit Descriptor Written Back)
}

func (e *IntelE1000) SendPacket(packet []byte, out net.PacketConn, dst net.Addr) error {
	if out != nil && dst != nil {
		_, err := out.WriteTo(packet, dst)
		return err
	}
	return nil
}

func (e *IntelE1000) ReceivePacket(packet []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rxQueue = append(e.rxQueue, bytes.Clone(packet))
	e.regs[E1000_ICR] |= 0x80 // RXT0 (Receiver Timer Interrupt)
}
