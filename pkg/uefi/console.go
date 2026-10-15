package uefi

// EFI_SIMPLE_TEXT_OUTPUT_PROTOCOL members: TestString, QueryMode, SetMode,
// SetAttribute, SetCursorPosition, EnableCursor, Reset.

import (
	"linemu/pkg/cpu"
	"linemu/pkg/mmu"
)

const (
	conColsDefault = 120
	conRowsDefault = 67
)

func conGeometry(ram *mmu.RAM) (cols, rows uint64) {
	if fb, fbW, _, pitch, ok := fbInfo(ram); ok && pitch > 0 {
		cols = fbW / 16
		rows = uint64(len(fb)) / pitch / 16
		if cols == 0 {
			cols = conColsDefault
		}
		if rows == 0 {
			rows = conRowsDefault
		}
		return cols, rows
	}
	return conColsDefault, conRowsDefault
}

// conOutTestString reports SUCCESS when every char is renderable.
func conOutTestString(state *cpu.CPUState, ram *mmu.RAM) error {
	strAddr := state.Regs[cpu.RDX]
	for i := 0; i < 4096; i++ {
		ch, err := ram.Read16(strAddr + uint64(i*2))
		if err != nil {
			state.Regs[cpu.RAX] = EFIStatusInvalidParameter
			return nil
		}
		if ch == 0 {
			break
		}
		switch ch {
		case '\r', '\n', '\b', '\t':
			continue
		}
		if ch < 0x20 || ch >= 0x80 {
			state.Regs[cpu.RAX] = EFIStatusUnsupported
			return nil
		}
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutQueryMode(This, ModeNumber, Columns*, Rows*). Single text mode 0.
func conOutQueryMode(state *cpu.CPUState, ram *mmu.RAM) error {
	if state.Regs[cpu.RDX] != 0 {
		state.Regs[cpu.RAX] = EFIStatusUnsupported
		return nil
	}
	cols, rows := conGeometry(ram)
	if p := state.Regs[cpu.R8]; p != 0 {
		_ = ram.Write64(p, cols)
	}
	if p := state.Regs[cpu.R9]; p != 0 {
		_ = ram.Write64(p, rows)
	}
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutSetMode clears the screen and homes the cursor for mode 0.
func conOutSetMode(state *cpu.CPUState, ram *mmu.RAM) error {
	if state.Regs[cpu.RDX] != 0 {
		state.Regs[cpu.RAX] = EFIStatusUnsupported
		return nil
	}
	_ = ram.Write32(ConOutModeAddr+4, 0)
	return conOutReset(state, ram)
}

// conOutSetAttribute(This, Attribute).
func conOutSetAttribute(state *cpu.CPUState, ram *mmu.RAM) error {
	attr := state.Regs[cpu.RDX]
	if attr > 0x7F {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram.Write32(ConOutModeAddr+8, uint32(attr))
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutSetCursor(This, Column, Row).
func conOutSetCursor(state *cpu.CPUState, ram *mmu.RAM) error {
	col := state.Regs[cpu.RDX]
	row := state.Regs[cpu.R8]
	cols, rows := conGeometry(ram)
	if col >= cols || row >= rows {
		state.Regs[cpu.RAX] = EFIStatusInvalidParameter
		return nil
	}
	_ = ram.Write32(ConOutModeAddr+12, uint32(col))
	_ = ram.Write32(ConOutModeAddr+16, uint32(row))
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutEnableCursor(This, Visible).
func conOutEnableCursor(state *cpu.CPUState, ram *mmu.RAM) error {
	v := uint32(0)
	if state.Regs[cpu.RDX] != 0 {
		v = 1
	}
	_ = ram.Write32(ConOutModeAddr+20, v)
	state.Regs[cpu.RAX] = EFIStatusSuccess
	return nil
}

// conOutReset clears the screen and homes the cursor.
func conOutReset(state *cpu.CPUState, ram *mmu.RAM) error {
	return conOutClearScreen(state, ram)
}
