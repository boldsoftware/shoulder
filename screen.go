package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// vterm is the session's model of the screen: a charmbracelet/x/vt
// emulator fed everything the command prints, plus the bits of mode state
// a repaint has to restore. Callers serialize access.
type vterm struct {
	emu           *vt.Emulator
	cursorVisible bool
	modes         map[ansi.DECMode]bool
	leftAlt       bool // the program left the alternate screen since the last check
}

// replayModes are the input modes a reattaching terminal needs restored for
// keys, the mouse, and pasting to keep working.
var replayModes = []ansi.DECMode{
	ansi.ModeCursorKeys,
	ansi.ModeMouseNormal, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent, ansi.ModeMouseExtSgr,
	ansi.ModeFocusEvent, ansi.ModeBracketedPaste,
}

// newTerm makes an emulator of the given size. The emulator answers
// terminal queries (cursor position, device attributes) through replies,
// which must be drained, or writing to the emulator blocks.
func newTerm(cols, rows int, replies func([]byte)) *vterm {
	t := &vterm{emu: vt.NewEmulator(cols, rows), cursorVisible: true, modes: map[ansi.DECMode]bool{}}
	t.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { t.cursorVisible = v },
		AltScreen: func(on bool) {
			if !on {
				t.leftAlt = true
			}
		},
		EnableMode: func(m ansi.Mode) {
			if d, ok := m.(ansi.DECMode); ok {
				t.modes[d] = true
			}
		},
		DisableMode: func(m ansi.Mode) {
			if d, ok := m.(ansi.DECMode); ok {
				delete(t.modes, d)
			}
		},
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := t.emu.Read(buf)
			if n > 0 {
				replies(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	return t
}

func (t *vterm) write(b []byte)  { t.emu.Write(b) }
func (t *vterm) altScreen() bool { return t.emu.IsAltScreen() }

// tookLeftAlt reports, once, that the program has left the alternate
// screen.
func (t *vterm) tookLeftAlt() bool {
	left := t.leftAlt
	t.leftAlt = false
	return left
}
func (t *vterm) size() (cols, rows int) { return t.emu.Width(), t.emu.Height() }
func (t *vterm) resize(cols, rows int)  { t.emu.Resize(cols, rows) }

func (t *vterm) cursor() (x, y int) {
	p := t.emu.CursorPosition()
	return p.X, p.Y
}

// text renders the screen as plain text, one line per row, trailing blanks
// trimmed.
func (t *vterm) text() string {
	lines := strings.Split(t.emu.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

// ansi renders the screen as escape sequences that repaint a terminal of
// the same size: what a reattaching terminal sees.
func (t *vterm) ansi() []byte {
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[H\x1b[2J")
	if t.emu.IsAltScreen() {
		// Full-screen programs expect the alternate screen; entering it
		// also means leaving later restores the user's own screen.
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	}
	for y, line := range strings.Split(t.emu.Render(), "\n") {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[0m", y+1, strings.TrimRight(line, " "))
	}
	for _, m := range replayModes {
		if t.modes[m] {
			fmt.Fprintf(&b, "\x1b[?%dh", m)
		}
	}
	x, y := t.cursor()
	fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
	if t.cursorVisible {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	return []byte(b.String())
}

// cleanLines turns a stream of terminal output into lines as a person
// would have seen them: escape sequences removed, and a carriage return
// rewinding the line so that what follows overwrites it; a progress bar
// collapses to its final state.
func cleanLines(s string) []string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.IndexByte(line, '\r') >= 0 {
			var cur []rune
			for _, seg := range strings.Split(line, "\r") {
				r := []rune(seg)
				if len(r) >= len(cur) {
					cur = r
				} else {
					cur = append(r, cur[len(r):]...)
				}
			}
			line = string(cur)
		}
		lines[i] = strings.Map(func(r rune) rune {
			if r < ' ' && r != '\t' {
				return -1
			}
			return r
		}, line)
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// tailLines keeps the last max lines, saying how many were left out.
func tailLines(lines []string, max int) string {
	if len(lines) <= max {
		return strings.Join(lines, "\n")
	}
	return fmt.Sprintf("[%d earlier lines omitted]\n%s", len(lines)-max, strings.Join(lines[len(lines)-max:], "\n"))
}
