package uefi

// EFI_SIMPLE_TEXT_INPUT_PROTOCOL: real ReadKeyStroke driven by PS2 scancodes.
// Scancode set 1 (with E0 extensions) translates to EFI_INPUT_KEY
// {ScanCode u16, UnicodeChar u16}. Modifier state persists in guest cells.

import (
	"linemu/pkg/cpu"
	"linemu/pkg/devices"
	"linemu/pkg/mmu"
)

const (
	ShiftStateAddr uint64 = 0x101B08 // bitmask: bit0 LShift, bit1 RShift
	E0PendingAddr  uint64 = 0x101B10 // 1 = previous byte was 0xE0 prefix
)

// EFI scan codes (EFI_SIMPLE_TEXT_INPUT_EX / EFI_INPUT_KEY ScanCode)
const (
	efiScanNull  = 0x00
	efiScanUp    = 0x01
	efiScanDown  = 0x02
	efiScanRight = 0x03
	efiScanLeft  = 0x04
	efiScanHome  = 0x05
	efiScanEnd   = 0x06
	efiScanIns   = 0x07
	efiScanDel   = 0x08
	efiScanPgUp  = 0x09
	efiScanPgDn  = 0x0A
	efiScanEsc   = 0x17
)

// keyPress is a translated key event; scan==0 means printable unicode.
type keyPress struct {
	scan uint16
	uni  uint16
}

// shiftedPairs maps unshifted ASCII punctuation to its shifted form.
var shiftedPairs = map[byte]byte{
	'1': '!', '2': '@', '3': '#', '4': '$', '5': '%',
	'6': '^', '7': '&', '8': '*', '9': '(', '0': ')',
	'-': '_', '=': '+', '[': '{', ']': '}', '\\': '|',
	';': ':', '\'': '"', ',': '<', '.': '>', '/': '?', '`': '~',
}

// plainKeys maps set-1 make codes to unshifted ASCII (0 = non-printable).
var plainKeys = map[uint8]byte{
	0x02: '1', 0x03: '2', 0x04: '3', 0x05: '4', 0x06: '5',
	0x07: '6', 0x08: '7', 0x09: '8', 0x0A: '9', 0x0B: '0',
	0x0C: '-', 0x0D: '=', 0x0E: '\b', 0x0F: '\t',
	0x10: 'q', 0x11: 'w', 0x12: 'e', 0x13: 'r', 0x14: 't',
	0x15: 'y', 0x16: 'u', 0x17: 'i', 0x18: 'o', 0x19: 'p',
	0x1A: '[', 0x1B: ']', 0x1C: '\r',
	0x1E: 'a', 0x1F: 's', 0x20: 'd', 0x21: 'f', 0x22: 'g',
	0x23: 'h', 0x24: 'j', 0x25: 'k', 0x26: 'l', 0x27: ';',
	0x28: '\'', 0x29: '`',
	0x2B: '\\', 0x2C: 'z', 0x2D: 'x', 0x2E: 'c', 0x2F: 'v',
	0x30: 'b', 0x31: 'n', 0x32: 'm', 0x33: ',', 0x34: '.',
	0x35: '/', 0x39: ' ',
}

// extKeys maps E0-prefixed make codes to EFI scan codes.
var extKeys = map[uint8]uint16{
	0x48: efiScanUp, 0x50: efiScanDown, 0x4D: efiScanRight, 0x4B: efiScanLeft,
	0x47: efiScanHome, 0x4F: efiScanEnd, 0x52: efiScanIns, 0x53: efiScanDel,
	0x49: efiScanPgUp, 0x51: efiScanPgDn, 0x1C: 0, // Numpad Enter -> handled as \r
}

// fnKeys maps plain make codes to EFI function-key scan codes.
var fnKeys = map[uint8]uint16{
	0x01: efiScanEsc,
	0x3B: 0x0B, 0x3C: 0x0C, 0x3D: 0x0D, 0x3E: 0x0E, 0x3F: 0x0F,
	0x40: 0x10, 0x41: 0x11, 0x42: 0x12, 0x43: 0x13, 0x44: 0x14,
	0x57: 0x15, 0x58: 0x16, // F11, F12
}

// conInReset drains pending keyboard bytes.
func conInReset(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller) error {
	if ps2 != nil {
		for {
			if _, ok := ps2.PollScancode(); !ok {
				break
			}
		}
	}
	_ = ram.Write64(E0PendingAddr, 0)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conInWaitForKey: no event/interrupt path exists, so report ready and let the
// guest poll ReadKeyStroke. Always success; guest spins until a key arrives.
func conInWaitForKey(state *cpu.CPUState, ram *mmu.RAM) error {
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conInReadKeyStroke implements ReadKeyStroke(This, Key*).
func conInReadKeyStroke(state *cpu.CPUState, ram *mmu.RAM, ps2 *devices.PS2Controller) error {
	keyOut := state.Regs[cpu.RDX]
	kp, ok := pollKey(ram, ps2)
	if !ok {
		state.Regs[cpu.RAX] = EFIStatusNotReady
		return nil
	}
	if keyOut != 0 {
		_ = ram.Write16(keyOut, kp.scan)
		_ = ram.Write16(keyOut+2, kp.uni)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// pollKey consumes scancodes until a full key press translates or the queue drains.
func pollKey(ram *mmu.RAM, ps2 *devices.PS2Controller) (keyPress, bool) {
	if ps2 == nil {
		return keyPress{}, false
	}
	for {
		code, ok := ps2.PollScancode()
		if !ok {
			return keyPress{}, false
		}
		if code == 0xE0 {
			_ = ram.Write64(E0PendingAddr, 1)
			continue
		}
		e0, _ := ram.Read64(E0PendingAddr)
		_ = ram.Write64(E0PendingAddr, 0)
		release := code&0x80 != 0
		make := code & 0x7F

		shift, _ := ram.Read64(ShiftStateAddr)
		switch {
		case !release && make == 0x2A:
			_ = ram.Write64(ShiftStateAddr, shift|1)
			continue
		case release && make == 0x2A:
			_ = ram.Write64(ShiftStateAddr, shift&^1)
			continue
		case !release && make == 0x36:
			_ = ram.Write64(ShiftStateAddr, shift|2)
			continue
		case release && make == 0x36:
			_ = ram.Write64(ShiftStateAddr, shift&^2)
			continue
		}
		if release {
			continue
		}
		shifted := shift != 0

		if e0 != 0 {
			if make == 0x1C { // Numpad Enter
				return keyPress{uni: '\r'}, true
			}
			if scan, ok := extKeys[make]; ok {
				return keyPress{scan: scan}, true
			}
			continue
		}
		if scan, ok := fnKeys[make]; ok {
			return keyPress{scan: scan}, true
		}
		ch, ok := plainKeys[make]
		if !ok || ch == 0 {
			continue
		}
		b := ch
		switch {
		case b >= 'a' && b <= 'z' && shifted:
			b -= 'a' - 'A'
		case shifted:
			if s, ok := shiftedPairs[b]; ok {
				b = s
			}
		}
		return keyPress{uni: uint16(b)}, true
	}
}
