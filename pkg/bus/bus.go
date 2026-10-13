package bus

// Device represents an I/O or MMIO port-mapped peripheral
type Device interface {
	Read(port uint16, size int) (uint32, error)
	Write(port uint16, val uint32, size int) error
}

type IOBus struct {
	devices map[uint16]Device
}

func NewIOBus() *IOBus {
	return &IOBus{
		devices: make(map[uint16]Device),
	}
}

func (b *IOBus) Register(port uint16, dev Device) {
	b.devices[port] = dev
}

func (b *IOBus) RegisterRange(start, count uint16, dev Device) {
	for i := uint16(0); i < count; i++ {
		b.devices[start+i] = dev
	}
}

func (b *IOBus) In(port uint16, size int) (uint32, error) {
	if dev, ok := b.devices[port]; ok {
		return dev.Read(port, size)
	}
	return 0xFFFFFFFF, nil // Unmapped port returns all 1s on x86
}

func (b *IOBus) Out(port uint16, val uint32, size int) error {
	if dev, ok := b.devices[port]; ok {
		return dev.Write(port, val, size)
	}
	return nil
}

func (b *IOBus) Read(port uint16, size int) (uint32, error) {
	return b.In(port, size)
}

func (b *IOBus) Write(port uint16, val uint32, size int) error {
	return b.Out(port, val, size)
}
