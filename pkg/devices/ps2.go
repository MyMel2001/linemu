package devices

import (
	"sync"
)

// Status register bits (port 0x64)
const (
	PS2StatusOBF     = 1 << 0 // Output buffer full
	PS2StatusIBF     = 1 << 1 // Input buffer full
	PS2StatusSYS     = 1 << 2 // System flag (1 = POST passed)
	PS2StatusA2      = 1 << 3 // Command/data (1 = port 0x64, 0 = port 0x60)
	PS2StatusINH     = 1 << 4 // Inhibit switch (1 = keyboard enabled)
	PS2StatusAUXData = 1 << 5 // Mouse output buffer full
	PS2StatusTimeout = 1 << 6 // Timeout error
	PS2StatusParity  = 1 << 7 // Parity error
)

type ps2Byte struct {
	val   uint8
	isAux bool
}

// PS2Controller emulates standard i8042 keyboard and mouse controller
type PS2Controller struct {
	mu sync.Mutex

	status uint8
	ccb    uint8

	pendingCmd       uint8
	kbdWaitingParam  uint8
	mouseWaitingParam uint8

	outQueue []ps2Byte

	kbdEnabled bool
	kbdLEDs    uint8

	mouseEnabled    bool
	mouseStreaming  bool
	mouseSampleRate uint8
	mouseResolution uint8
	mouseScaling21  bool
	mouseButtons    uint8
}

func NewPS2Controller() *PS2Controller {
	p := &PS2Controller{
		status:          PS2StatusSYS | PS2StatusINH,
		ccb:             0x47, // IRQ1 + IRQ12 enabled, SYS set, translation enabled
		kbdEnabled:      true,
		mouseEnabled:    true,
		mouseSampleRate: 100,
		mouseResolution: 4,
	}
	return p
}

func (p *PS2Controller) pushOutput(val uint8, isAux bool) {
	p.outQueue = append(p.outQueue, ps2Byte{val: val, isAux: isAux})
	p.updateStatus()
}

func (p *PS2Controller) updateStatus() {
	if len(p.outQueue) > 0 {
		p.status |= PS2StatusOBF
		if p.outQueue[0].isAux {
			p.status |= PS2StatusAUXData
		} else {
			p.status &= ^uint8(PS2StatusAUXData)
		}
	} else {
		p.status &= ^uint8(PS2StatusOBF | PS2StatusAUXData)
	}
}

func (p *PS2Controller) Read(port uint16, size int) (uint32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch port {
	case 0x64:
		return uint32(p.status), nil
	case 0x60:
		if len(p.outQueue) == 0 {
			return 0, nil
		}
		item := p.outQueue[0]
		p.outQueue = p.outQueue[1:]
		p.updateStatus()
		return uint32(item.val), nil
	default:
		return 0, nil
	}
}

func (p *PS2Controller) Write(port uint16, val uint32, size int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	val8 := uint8(val)
	switch port {
	case 0x64:
		p.status |= PS2StatusA2
		p.handleCommand(val8)
	case 0x60:
		p.status &= ^uint8(PS2StatusA2)
		p.handleData(val8)
	}
	return nil
}

func (p *PS2Controller) handleCommand(cmd uint8) {
	switch cmd {
	case 0x20: // Read CCB
		p.pushOutput(p.ccb, false)
	case 0x60: // Write CCB
		p.pendingCmd = 0x60
	case 0xA7: // Disable mouse
		p.mouseEnabled = false
		p.ccb |= 0x20
	case 0xA8: // Enable mouse
		p.mouseEnabled = true
		p.ccb &= ^uint8(0x20)
	case 0xA9: // Test mouse interface
		p.pushOutput(0x00, false)
	case 0xAA: // Controller self-test
		p.status |= PS2StatusSYS
		p.pushOutput(0x55, false)
	case 0xAB: // Test keyboard interface
		p.pushOutput(0x00, false)
	case 0xAD: // Disable keyboard
		p.kbdEnabled = false
		p.ccb |= 0x10
	case 0xAE: // Enable keyboard
		p.kbdEnabled = true
		p.ccb &= ^uint8(0x10)
	case 0xC0: // Read input port
		p.pushOutput(0x00, false)
	case 0xD0: // Read output port
		p.pushOutput(0x03, false) // System reset high, A20 gate high
	case 0xD1: // Write output port
		p.pendingCmd = 0xD1
	case 0xD4: // Write to mouse device
		p.pendingCmd = 0xD4
	default:
		// Unsupported or no-op command
	}
}

func (p *PS2Controller) handleData(data uint8) {
	if p.pendingCmd == 0x60 {
		p.ccb = data
		p.pendingCmd = 0
		return
	}
	if p.pendingCmd == 0xD1 {
		p.pendingCmd = 0
		return
	}
	if p.pendingCmd == 0xD4 {
		p.pendingCmd = 0
		p.handleMouseCommand(data)
		return
	}

	if p.kbdWaitingParam != 0 {
		p.kbdWaitingParam = 0
		p.pushOutput(0xFA, false)
		return
	}

	switch data {
	case 0xFF: // Reset keyboard
		p.pushOutput(0xFA, false)
		p.pushOutput(0xAA, false)
	case 0xFE: // Resend
		p.pushOutput(0xFA, false)
	case 0xF2: // Identify keyboard
		p.pushOutput(0xFA, false)
		p.pushOutput(0xAB, false)
		p.pushOutput(0x83, false)
	case 0xF4: // Enable scanning
		p.kbdEnabled = true
		p.pushOutput(0xFA, false)
	case 0xF5: // Disable scanning
		p.kbdEnabled = false
		p.pushOutput(0xFA, false)
	case 0xF6: // Set defaults
		p.kbdLEDs = 0
		p.kbdEnabled = true
		p.pushOutput(0xFA, false)
	case 0xED, 0xF0, 0xF3: // Commands expecting one parameter byte
		p.kbdWaitingParam = data
		p.pushOutput(0xFA, false)
	case 0xEE: // Echo
		p.pushOutput(0xEE, false)
	default:
		p.pushOutput(0xFA, false)
	}
}

func (p *PS2Controller) handleMouseCommand(cmd uint8) {
	if p.mouseWaitingParam != 0 {
		p.mouseWaitingParam = 0
		p.pushOutput(0xFA, true)
		return
	}

	switch cmd {
	case 0xFF: // Reset mouse
		p.mouseStreaming = false
		p.mouseSampleRate = 100
		p.mouseResolution = 4
		p.mouseScaling21 = false
		p.pushOutput(0xFA, true)
		p.pushOutput(0xAA, true)
		p.pushOutput(0x00, true) // Standard PS/2 mouse ID
	case 0xF2: // Get device ID
		p.pushOutput(0xFA, true)
		p.pushOutput(0x00, true)
	case 0xF3, 0xE8: // Set sample rate / resolution
		p.mouseWaitingParam = cmd
		p.pushOutput(0xFA, true)
	case 0xE6: // Set scaling 1:1
		p.mouseScaling21 = false
		p.pushOutput(0xFA, true)
	case 0xE7: // Set scaling 2:1
		p.mouseScaling21 = true
		p.pushOutput(0xFA, true)
	case 0xF4: // Enable reporting
		p.mouseStreaming = true
		p.pushOutput(0xFA, true)
	case 0xF5: // Disable reporting
		p.mouseStreaming = false
		p.pushOutput(0xFA, true)
	case 0xF6: // Set defaults
		p.mouseSampleRate = 100
		p.mouseResolution = 4
		p.mouseStreaming = false
		p.mouseScaling21 = false
		p.pushOutput(0xFA, true)
	case 0xEB: // Read data
		p.pushOutput(0xFA, true)
		p.sendMousePacketLocked(0, 0, p.mouseButtons)
	default:
		p.pushOutput(0xFA, true)
	}
}

// ponytail: Set 1 scancodes; upgrade path: add scancode set 2 translation if guest requests mode 2.
var runeToScancode = map[rune]uint8{
	'\n': 0x1C,
	'\r': 0x1C,
	'\t': 0x0F,
	'\b': 0x0E,
	' ':  0x39,
	0x1B: 0x01,
	'a': 0x1E, 'b': 0x30, 'c': 0x2E, 'd': 0x20, 'e': 0x12, 'f': 0x21,
	'g': 0x22, 'h': 0x23, 'i': 0x17, 'j': 0x24, 'k': 0x25, 'l': 0x26,
	'm': 0x32, 'n': 0x31, 'o': 0x18, 'p': 0x19, 'q': 0x10, 'r': 0x13,
	's': 0x1F, 't': 0x14, 'u': 0x16, 'v': 0x2F, 'w': 0x11, 'x': 0x2D,
	'y': 0x15, 'z': 0x2C,
	'A': 0x1E, 'B': 0x30, 'C': 0x2E, 'D': 0x20, 'E': 0x12, 'F': 0x21,
	'G': 0x22, 'H': 0x23, 'I': 0x17, 'J': 0x24, 'K': 0x25, 'L': 0x26,
	'M': 0x32, 'N': 0x31, 'O': 0x18, 'P': 0x19, 'Q': 0x10, 'R': 0x13,
	'S': 0x1F, 'T': 0x14, 'U': 0x16, 'V': 0x2F, 'W': 0x11, 'X': 0x2D,
	'Y': 0x15, 'Z': 0x2C,
	'1': 0x02, '2': 0x03, '3': 0x04, '4': 0x05, '5': 0x06,
	'6': 0x07, '7': 0x08, '8': 0x09, '9': 0x0A, '0': 0x0B,
	'-': 0x0C, '=': 0x0D, '[': 0x1A, ']': 0x1B, ';': 0x27,
	'\'': 0x28, '`': 0x29, '\\': 0x2B, ',': 0x33, '.': 0x34,
	'/': 0x35,
}

func (p *PS2Controller) SendScancode(code uint8) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushOutput(code, false)
}

// PeekScancode returns the oldest pending keyboard byte without consuming it.
func (p *PS2Controller) PeekScancode() (uint8, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.outQueue {
		if !b.isAux {
			return b.val, true
		}
	}
	return 0, false
}

// PollScancode consumes and returns the oldest pending keyboard byte.
func (p *PS2Controller) PollScancode() (uint8, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, b := range p.outQueue {
		if !b.isAux {
			p.outQueue = append(p.outQueue[:i], p.outQueue[i+1:]...)
			p.updateStatus()
			return b.val, true
		}
	}
	return 0, false
}

func (p *PS2Controller) KeyDown(scancode uint8) {
	p.SendScancode(scancode)
}

func (p *PS2Controller) KeyUp(scancode uint8) {
	p.SendScancode(scancode | 0x80)
}

func (p *PS2Controller) SendKey(r rune, pressed bool) {
	code, ok := runeToScancode[r]
	if !ok {
		return
	}
	if pressed {
		p.KeyDown(code)
	} else {
		p.KeyUp(code)
	}
}

// ponytail: Standard 3-byte PS/2 packet; upgrade path: add 4-byte wheel packet for Intellimouse.
func (p *PS2Controller) sendMousePacketLocked(dx, dy int, buttons uint8) {
	if dx > 127 {
		dx = 127
	} else if dx < -128 {
		dx = -128
	}
	if dy > 127 {
		dy = 127
	} else if dy < -128 {
		dy = -128
	}

	b0 := uint8(0x08) // bit 3 always 1
	if (buttons & 0x01) != 0 {
		b0 |= 0x01
	}
	if (buttons & 0x02) != 0 {
		b0 |= 0x02
	}
	if (buttons & 0x04) != 0 {
		b0 |= 0x04
	}
	if dx < 0 {
		b0 |= 0x10
	}
	if dy < 0 {
		b0 |= 0x20
	}

	p.pushOutput(b0, true)
	p.pushOutput(uint8(int8(dx)), true)
	p.pushOutput(uint8(int8(dy)), true)
}

func (p *PS2Controller) SendMouseEvent(dx, dy int, buttons uint8) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mouseButtons = buttons
	p.sendMousePacketLocked(dx, dy, buttons)
}
