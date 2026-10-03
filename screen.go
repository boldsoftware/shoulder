package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/hinshun/vt10x"
)

// Glyph attribute bits, mirroring vt10x's unexported ones.
const (
	attrReverse = 1 << iota
	attrUnderline
	attrBold
	attrGfx
	attrItalic
	attrBlink
)

// screenText renders the emulator's screen as plain text, one line per
// row, trailing blanks trimmed. Callers hold the emulator lock.
func screenText(vt vt10x.Terminal) string {
	cols, rows := vt.Size()
	lines := make([]string, rows)
	var b strings.Builder
	for y := range rows {
		b.Reset()
		for x := range cols {
			r := vt.Cell(x, y).Char
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

// screenANSI renders the emulator's screen as escape sequences that
// repaint a terminal of the same size: what a reattaching client sees.
// Callers hold the emulator lock.
func screenANSI(vt vt10x.Terminal) []byte {
	cols, rows := vt.Size()
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[H\x1b[2J")
	if vt.Mode()&vt10x.ModeAltScreen != 0 {
		// Full-screen programs expect the alternate screen; entering it
		// also means detaching later restores the user's own screen.
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	}
	var cur vt10x.Glyph
	cur.FG, cur.BG = vt10x.DefaultFG, vt10x.DefaultBG
	for y := range rows {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		last := cols
		for last > 0 && blank(vt.Cell(last-1, y)) {
			last--
		}
		for x := range last {
			g := vt.Cell(x, y)
			if g.Mode != cur.Mode || g.FG != cur.FG || g.BG != cur.BG {
				b.WriteString(sgr(g))
				cur = g
			}
			r := g.Char
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
	}
	b.WriteString("\x1b[0m")
	// Input modes the program set, so keys and the mouse keep working.
	mode := vt.Mode()
	for _, m := range []struct {
		flag vt10x.ModeFlag
		seq  string
	}{
		{vt10x.ModeAppCursor, "\x1b[?1h"},
		{vt10x.ModeAppKeypad, "\x1b="},
		{vt10x.ModeMouseButton, "\x1b[?1000h"},
		{vt10x.ModeMouseMotion, "\x1b[?1002h"},
		{vt10x.ModeMouseMany, "\x1b[?1003h"},
		{vt10x.ModeMouseSgr, "\x1b[?1006h"},
	} {
		if mode&m.flag != 0 {
			b.WriteString(m.seq)
		}
	}
	c := vt.Cursor()
	fmt.Fprintf(&b, "\x1b[%d;%dH", c.Y+1, c.X+1)
	if vt.CursorVisible() {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	return []byte(b.String())
}

func blank(g vt10x.Glyph) bool {
	return (g.Char == ' ' || g.Char == 0) && g.BG == vt10x.DefaultBG && g.Mode&attrReverse == 0
}

func sgr(g vt10x.Glyph) string {
	parts := []string{"0"}
	if g.Mode&attrBold != 0 {
		parts = append(parts, "1")
	}
	if g.Mode&attrItalic != 0 {
		parts = append(parts, "3")
	}
	if g.Mode&attrUnderline != 0 {
		parts = append(parts, "4")
	}
	if g.Mode&attrBlink != 0 {
		parts = append(parts, "5")
	}
	if g.Mode&attrReverse != 0 {
		parts = append(parts, "7")
	}
	parts = append(parts, colorSGR(g.FG, 38)...)
	parts = append(parts, colorSGR(g.BG, 48)...)
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

func colorSGR(c vt10x.Color, base int) []string {
	switch {
	case c >= vt10x.DefaultFG:
		return nil
	case c < 256:
		return []string{strconv.Itoa(base), "5", strconv.Itoa(int(c))}
	default:
		return []string{strconv.Itoa(base), "2", strconv.Itoa(int(c >> 16 & 0xff)), strconv.Itoa(int(c >> 8 & 0xff)), strconv.Itoa(int(c & 0xff))}
	}
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
