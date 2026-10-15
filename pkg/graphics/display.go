package graphics

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
)

// DisplayMode defines where graphics render
type DisplayMode string

const (
	DisplayTerminal DisplayMode = "terminal" // ANSI truecolor terminal output
	DisplayWindow   DisplayMode = "window"   // Native window or external viewer (DRM/X11/Cocoa/ffplay/open)
	DisplayHeadless DisplayMode = "none"     // Background headless mode
)

// InputDevice defines interface for keyboard and mouse event consumption
type InputDevice interface {
	SendScancode(code uint8)
	SendKey(r rune, pressed bool)
	SendMouseEvent(dx, dy int, buttons uint8)
}

// DisplayRenderer handles live onscreen presentation of guest framebuffer
type DisplayRenderer struct {
	mu        sync.Mutex
	Mode      DisplayMode
	Width     uint32
	Height    uint32
	Running   bool
	writer    io.Writer
	reader    io.Reader
	input     InputDevice
	stopInput chan struct{}
	extCmd    *exec.Cmd
	cmdStdin  io.WriteCloser
	frameChan chan []byte
}

func NewDisplayRenderer(mode DisplayMode, width, height uint32) *DisplayRenderer {
	if mode == "" {
		mode = DisplayWindow
	}
	return &DisplayRenderer{
		Mode:      mode,
		Width:     width,
		Height:    height,
		writer:    os.Stdout,
		frameChan: make(chan []byte, 2),
	}
}

func (d *DisplayRenderer) SetOutput(w io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writer = w
}

func (d *DisplayRenderer) SetInputSource(r io.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reader = r
}

func (d *DisplayRenderer) AttachInput(input InputDevice) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.input = input
}

func (d *DisplayRenderer) Input() InputDevice {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.input
}

func (d *DisplayRenderer) SendKey(r rune, pressed bool) {
	d.mu.Lock()
	in := d.input
	d.mu.Unlock()
	if in != nil {
		in.SendKey(r, pressed)
	}
}

func (d *DisplayRenderer) SendScancode(code uint8) {
	d.mu.Lock()
	in := d.input
	d.mu.Unlock()
	if in != nil {
		in.SendScancode(code)
	}
}

func (d *DisplayRenderer) SendMouseEvent(dx, dy int, buttons uint8) {
	d.mu.Lock()
	in := d.input
	d.mu.Unlock()
	if in != nil {
		in.SendMouseEvent(dx, dy, buttons)
	}
}

func (d *DisplayRenderer) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.Mode == DisplayHeadless {
		return nil
	}

	d.Running = true

	// Check if external window viewer pipe (ffplay/mpv/custom) is feasible
	if d.Mode == DisplayWindow {
		// Spawn real-time raw frame player if available; fallback to terminal truecolor
		var cmd *exec.Cmd
		if _, err := exec.LookPath("ffplay"); err == nil {
			cmd = exec.Command("ffplay",
				"-loglevel", "quiet",
				"-f", "rawvideo",
				"-pixel_format", "bgra",
				"-video_size", fmt.Sprintf("%dx%d", d.Width, d.Height),
				"-framerate", "30",
				"-window_title", "LinEMU Display",
				"-i", "pipe:0",
			)
		} else if _, err := exec.LookPath("mpv"); err == nil {
			cmd = exec.Command("mpv",
				"--demuxer=rawvideo",
				fmt.Sprintf("--demuxer-rawvideo-w=%d", d.Width),
				fmt.Sprintf("--demuxer-rawvideo-h=%d", d.Height),
				"--demuxer-rawvideo-format=bgra",
				"--demuxer-rawvideo-fps=30",
				"--title=LinEMU Display",
				"-",
			)
		}

		if cmd != nil {
			stdin, err := cmd.StdinPipe()
			if err == nil {
				if err := cmd.Start(); err == nil {
					d.extCmd = cmd
					d.cmdStdin = stdin
					go d.streamLoop()
					return nil
				}
			}
		}
		// Fallback to ANSI truecolor terminal presentation when window pipe tool not installed
		d.Mode = DisplayTerminal
	}

	go d.streamLoop()

	if d.input != nil && d.reader != nil {
		d.stopInput = make(chan struct{})
		go d.inputLoop()
	}
	return nil
}

func (d *DisplayRenderer) inputLoop() {
	buf := make([]byte, 128)
	for {
		d.mu.Lock()
		running := d.Running
		r := d.reader
		stopCh := d.stopInput
		d.mu.Unlock()

		if !running || r == nil {
			return
		}

		select {
		case <-stopCh:
			return
		default:
		}

		n, err := r.Read(buf)
		if err != nil || n == 0 {
			return
		}
		d.handleInputBytes(buf[:n])
	}
}

// ponytail: Terminal ANSI & SGR mouse parser; upgrade path: full VT100 / termios raw mode integration.
func (d *DisplayRenderer) handleInputBytes(buf []byte) {
	i := 0
	for i < len(buf) {
		if buf[i] == 0x1B && i+2 < len(buf) && buf[i+1] == '[' {
			switch buf[i+2] {
			case 'A': // Up
				d.SendScancode(0x48)
				d.SendScancode(0x48 | 0x80)
				i += 3
				continue
			case 'B': // Down
				d.SendScancode(0x50)
				d.SendScancode(0x50 | 0x80)
				i += 3
				continue
			case 'C': // Right
				d.SendScancode(0x4D)
				d.SendScancode(0x4D | 0x80)
				i += 3
				continue
			case 'D': // Left
				d.SendScancode(0x4B)
				d.SendScancode(0x4B | 0x80)
				i += 3
				continue
			case '<': // SGR mouse mode: \033[<btn;x;yM or m
				end := i + 3
				for end < len(buf) && buf[end] != 'M' && buf[end] != 'm' {
					end++
				}
				if end < len(buf) {
					var btn, x, y int
					n, _ := fmt.Sscanf(string(buf[i+3:end]), "%d;%d;%d", &btn, &x, &y)
					if n == 3 {
						isRelease := buf[end] == 'm'
						var buttons uint8
						if !isRelease {
							switch btn & 3 {
							case 0:
								buttons |= 1 // Left
							case 1:
								buttons |= 4 // Middle
							case 2:
								buttons |= 2 // Right
							}
						}
						d.SendMouseEvent(x, y, buttons)
					}
					i = end + 1
					continue
				}
			}
		}

		ch := rune(buf[i])
		d.SendKey(ch, true)
		d.SendKey(ch, false)
		i++
	}
}

func (d *DisplayRenderer) streamLoop() {
	for frame := range d.frameChan {
		d.mu.Lock()
		running := d.Running
		cmdStdin := d.cmdStdin
		mode := d.Mode
		w := d.writer
		d.mu.Unlock()

		if !running {
			break
		}

		if cmdStdin != nil {
			if _, err := cmdStdin.Write(frame); err != nil {
				break
			}
			continue
		}

		if mode == DisplayTerminal && w != nil {
			d.renderTerminal(frame, w)
		}
	}
}

func (d *DisplayRenderer) HasExternalViewer() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.extCmd != nil
}

func (d *DisplayRenderer) Wait() error {
	d.mu.Lock()
	cmd := d.extCmd
	d.mu.Unlock()
	if cmd != nil {
		return cmd.Wait()
	}
	return nil
}

// renderTerminal downscales framebuffer to ANSI 24-bit truecolor escape blocks
func (d *DisplayRenderer) renderTerminal(fb []byte, out io.Writer) {
	termW := 80
	termH := 24
	if termW <= 0 || termH <= 0 {
		return
	}

	scaleX := int(d.Width) / termW
	if scaleX <= 0 {
		scaleX = 1
	}
	scaleY := int(d.Height) / termH
	if scaleY <= 0 {
		scaleY = 1
	}

	buf := make([]byte, 0, termW*termH*24)
	buf = append(buf, "\033[H"... /* cursor home */)

	for ty := 0; ty < termH; ty++ {
		srcY := ty * scaleY
		for tx := 0; tx < termW; tx++ {
			srcX := tx * scaleX
			offset := (srcY*int(d.Width) + srcX) * 4
			if offset+3 < len(fb) {
				b := fb[offset]
				g := fb[offset+1]
				r := fb[offset+2]
				buf = append(buf, fmt.Sprintf("\033[48;2;%d;%d;%dm ", r, g, b)...)
			}
		}
		buf = append(buf, "\033[0m\n"...)
	}

	_, _ = out.Write(buf)
}

// PresentFrame delivers raw BGRA framebuffer to renderer
func (d *DisplayRenderer) PresentFrame(fb []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.Running || d.Mode == DisplayHeadless {
		return
	}

	frameCopy := make([]byte, len(fb))
	copy(frameCopy, fb)

	select {
	case d.frameChan <- frameCopy:
	default:
		// Drop frame if display loop congested
	}
}

func (d *DisplayRenderer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Running = false
	if d.stopInput != nil {
		close(d.stopInput)
		d.stopInput = nil
	}
	if d.cmdStdin != nil {
		_ = d.cmdStdin.Close()
	}
	if d.extCmd != nil && d.extCmd.Process != nil {
		_ = d.extCmd.Process.Kill()
	}
	if d.frameChan != nil {
		close(d.frameChan)
		d.frameChan = nil
	}
	_ = runtime.GOOS
}
