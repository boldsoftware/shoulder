package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The person's side of a browser session: choosing it on the first
// screen, connecting, and the screen that shows what agents do.

// choice is what the first screen will share.
type choice struct {
	kind       string // "" for the terminal, or "chrome", "safari"
	tabs       string // active, new, all
	profile    string // Chrome: the profile's dir, or "" for the one used last
	standalone bool   // Chrome: a standalone one, started if need be, not the person's
}

var kinds = []struct{ key, name, label string }{
	{"1", "", "terminal"},
	{"2", "chrome", "Chrome"},
	{"3", "safari", "Safari"},
}

var tabModes = []struct{ name, label string }{
	{"active", "the tab you're on"},
	{"new", "a new tab"},
	{"all", "all tabs"},
}

func availableKinds() []struct{ key, name, label string } {
	if runtime.GOOS != "darwin" {
		return kinds[:2]
	}
	return kinds
}

// readKeys waits up to d for keys and returns them (none on timeout).
// It only reads when there's input, so nothing is left reading the
// terminal when the command starts. (poll(2) doesn't work on macOS
// terminals; select(2) does.)
func readKeys(d time.Duration) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	var set unix.FdSet
	set.Set(fd)
	tv := unix.NsecToTimeval(d.Nanoseconds())
	n, err := unix.Select(fd+1, &set, nil, nil, &tv)
	if errors.Is(err, unix.EINTR) {
		return nil, nil
	}
	if err != nil || n == 0 {
		return nil, err
	}
	buf := make([]byte, 256)
	n, err = os.Stdin.Read(buf)
	if n == 0 && err == nil {
		err = io.EOF
	}
	return buf[:n], err
}

// screen redraws the whole terminal, only when what's on it changes.
type screen struct{ last string }

func (s *screen) draw(text string) {
	if text == s.last {
		return
	}
	s.last = text
	os.Stdout.WriteString("\x1b[H\x1b[2J" + strings.ReplaceAll(text, "\n", "\r\n"))
}

// fit cuts s to n columns (roughly: it counts runes).
func fit(s string, n int) string {
	if n < 2 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// browserReady says whether shoulder can connect to the chosen browser,
// and what the person should know first.
func browserReady(ch choice) (bool, string) {
	switch {
	case ch.kind == "safari":
		if !safariRunning() {
			return false, "Safari isn't running: open it, with the tab you want to share in front."
		}
		return true, safariHelp
	case ch.standalone:
		dir, err := standaloneChromeDir()
		if err != nil {
			return false, err.Error()
		}
		return standaloneChromeCheck(dir)
	default:
		return chromeCheck(defaultChromeDir())
	}
}

// browserRows are the first screen's browser settings.
func browserRows(p func(string, ...any), ch choice) {
	if ch.kind == "" {
		return
	}
	p("\n%stabs%s   ", bold, reset)
	for _, m := range tabModes {
		label := "[a] " + m.label
		if m.name == ch.tabs {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
	if ch.kind != "chrome" {
		return
	}
	p("\n%schrome%s ", bold, reset)
	for _, sa := range []bool{false, true} {
		label := "[o] yours (allow remote debugging)"
		if sa {
			label = "[o] standalone (no setup)"
		}
		if sa == ch.standalone {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
	if ch.standalone {
		return
	}
	profiles := readChromeProfiles(defaultChromeDir())
	choices := profiles.choices()
	if len(choices) < 2 {
		return
	}
	selected := cmpOr(ch.profile, profiles.lastUsed)
	p("\n%sprofile%s ", bold, reset)
	for _, pr := range choices {
		label := "[p] " + pr.Name
		if pr.Dir == selected {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
}

// nextProfile is the profile after ch's, for [p].
func nextProfile(ch choice) string {
	profiles := readChromeProfiles(defaultChromeDir())
	choices := profiles.choices()
	if len(choices) == 0 {
		return ""
	}
	selected := cmpOr(ch.profile, profiles.lastUsed)
	for i, pr := range choices {
		if pr.Dir == selected {
			return choices[(i+1)%len(choices)].Dir
		}
	}
	return choices[0].Dir
}

// browserKey handles the first screen's browser keys.
func browserKey(c *ctl, cfg *config, ch *choice, k byte) (handled bool, note string) {
	for _, kd := range availableKinds() {
		if string(k) == kd.key {
			if kd.name != ch.kind {
				if _, err := c.post("/ctl/kind?browser=" + kd.name); err != nil {
					return true, err.Error()
				}
				ch.kind, cfg.Browser = kd.name, kd.name
			}
			return true, ""
		}
	}
	if ch.kind == "" {
		return false, ""
	}
	switch k {
	case 'a':
		for i, m := range tabModes {
			if m.name == ch.tabs {
				ch.tabs = tabModes[(i+1)%len(tabModes)].name
				break
			}
		}
		return true, ""
	case 'o':
		if ch.kind == "chrome" {
			ch.standalone = !ch.standalone
			return true, ""
		}
	case 'p':
		if ch.kind == "chrome" && !ch.standalone {
			ch.profile = nextProfile(*ch)
			return true, ""
		}
	}
	return false, ""
}

// connectBrowser connects the session to the chosen browser and shares
// tabs, asking which if need be. It returns false if the person backed
// out.
func connectBrowser(cfg *config, c *ctl, ch choice, interactive bool) (bool, error) {
	q := url.Values{"browser": {ch.kind}, "tabs": {ch.tabs}}
	if ch.profile != "" {
		q.Set("profile", ch.profile)
	}
	if ch.standalone {
		q.Set("standalone", "1")
	}
	label := browserLabels[ch.kind]
	waiting := "Connecting to " + label + "…"
	switch {
	case ch.kind == "safari":
		waiting += "\n\nIf macOS asks whether this terminal may control Safari, click OK."
	case ch.standalone:
		waiting = "Starting a standalone Chrome…"
	default:
		waiting += "\n\nChrome asks \"Allow remote debugging?\": click " + bold + "Allow" + reset + ".\n(It's Chrome's own question; it asks each time sharing starts.)"
	}
	var scr screen
	lastWait := ""
	for {
		var res browserStart
		b, err := waitFor(c, &scr, "/ctl/browser?"+q.Encode(), cfg.Name, waiting, interactive)
		if err != nil || b == nil {
			return false, err
		}
		if err := json.Unmarshal(b, &res); err != nil {
			return false, err
		}
		if res.Wait != "" {
			// The person has to do something in the browser; then ask again.
			waiting = res.Label + " is connected. " + res.Wait
			if !interactive && res.Wait != lastWait {
				fmt.Fprintln(os.Stderr, res.Wait)
			}
			lastWait = res.Wait
			if !pause(&scr, cfg.Name, waiting, interactive) {
				return false, nil
			}
			continue
		}
		if len(res.Choose) == 0 {
			return true, nil
		}
		if !interactive {
			return false, fmt.Errorf("%s has %d windows, so which tab you're on isn't clear: use -tabs new or -tabs all, or leave out -y to pick one", res.Label, countWindows(res.Choose))
		}
		mode, pick, ok := pickTab(cfg, res)
		if !ok {
			return false, nil
		}
		scr = screen{}
		q.Set("tabs", mode)
		q.Set("pick", pick)
		waiting = "Sharing…"
	}
}

// pause shows waiting for a second before asking the browser again. It
// returns false if the person gave up ([q]).
func pause(scr *screen, name, waiting string, interactive bool) bool {
	if !interactive {
		sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		select {
		case <-time.After(time.Second):
			return true
		case <-sig.Done():
			return false
		}
	}
	drawWaiting(scr, name, waiting)
	keys, err := readKeys(time.Second)
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(keys, isBack)
}

func isBack(k byte) bool { return k == 'q' || k == 3 || k == 4 || k == 27 }

func drawWaiting(scr *screen, name, waiting string) {
	scr.draw(fmt.Sprintf("%sshoulder%s · session %s%s%s\n\n%s\n\n%s[q]%s back\n", bold, reset, bold, name, reset, waiting, bold, reset))
}

func countWindows(cands []candidate) int {
	w := map[int]bool{}
	for _, c := range cands {
		w[c.Window] = true
	}
	return len(w)
}

// waitFor makes a control call that can take a while (the person has to
// click Allow), showing what's going on. [q] gives up: it returns nil.
func waitFor(c *ctl, scr *screen, path, name, waiting string, interactive bool) ([]byte, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := c.doContext(ctx, "POST", path)
		done <- result{b, err}
	}()
	if !interactive {
		if scr.last == "" {
			scr.last = waiting
			fmt.Fprintln(os.Stderr, strings.ReplaceAll(strings.ReplaceAll(waiting, bold, ""), reset, ""))
		}
		sig, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		select {
		case r := <-done:
			return r.b, r.err
		case <-sig.Done():
			return nil, errors.New("interrupted")
		}
	}
	drawWaiting(scr, name, waiting)
	for {
		select {
		case r := <-done:
			return r.b, r.err
		default:
		}
		keys, err := readKeys(100 * time.Millisecond)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(keys, isBack) {
			return nil, nil
		}
	}
}

// pickTab asks which tab to share, when there's no one obvious tab. It
// returns the tabs= mode and, for "pick", the tab.
func pickTab(cfg *config, res browserStart) (mode, pick string, ok bool) {
	cands := res.Choose
	if len(cands) > 9 {
		cands = cands[:9]
	}
	var scr screen
	for {
		cols, _ := termSize()
		var b strings.Builder
		fmt.Fprintf(&b, "%sshoulder%s · session %s%s%s · %s\n\n", bold, reset, bold, cfg.Name, reset, res.Label)
		if n := countWindows(res.Choose); n > 1 {
			fmt.Fprintf(&b, "%s has %d windows. Which tab should the agent see?\n\n", res.Label, n)
		} else {
			fmt.Fprintf(&b, "Which tab should the agent see?\n\n")
		}
		for i, c := range cands {
			mark := " "
			if c.Active {
				mark = "•"
			}
			line := fmt.Sprintf("%s[%d]%s %s %s — %s", bold, i+1, reset, mark, cmpOr(c.Title, "(untitled)"), c.URL)
			fmt.Fprintln(&b, fit(line, cols+len(bold)+len(reset)))
		}
		if len(res.Choose) > len(cands) {
			fmt.Fprintf(&b, "    and %d more\n", len(res.Choose)-len(cands))
		}
		fmt.Fprintf(&b, "\n• is the tab in front in its window.\n\n%s[n]%s a new tab   %s[a]%s all tabs   %s[q]%s back\n", bold, reset, bold, reset, bold, reset)
		scr.draw(b.String())
		keys, err := readKeys(time.Second)
		if err != nil {
			return "", "", false
		}
		for _, k := range keys {
			switch {
			case k >= '1' && int(k-'1') < len(cands):
				return "pick", cands[k-'1'].ID, true
			case k == 'n':
				return "new", "", true
			case k == 'a':
				return "all", "", true
			case k == 'q' || k == 3 || k == 4 || k == 27:
				return "", "", false
			}
		}
	}
}

// browserScreen shows a sharing browser session: the shared tabs and
// what agents do in them, until the person stops sharing or the browser
// goes away.
func browserScreen(cfg *config, c *ctl, paste string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return browserLog(c)
	}
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	restore := func() { term.Restore(int(os.Stdin.Fd()), old); fmt.Print("\x1b[H\x1b[2J") }
	var scr screen
	note := ""
	for {
		var a browserActivity
		b, err := c.do("GET", "/ctl/activity")
		if err == nil {
			err = json.Unmarshal(b, &a)
		}
		if err != nil || a.Ended {
			restore()
			if err != nil {
				fmt.Println("The session ended.")
			} else {
				fmt.Printf("%s went away (closed, or the connection was turned off), so sharing stopped.\n", cmpOr(a.Label, "The browser"))
			}
			return nil
		}
		scr.draw(drawBrowserScreen(cfg, paste, a, note))
		keys, err := readKeys(time.Second)
		if err != nil {
			restore()
			return err
		}
		for _, k := range keys {
			switch k {
			case 'q', 3, 4:
				c.post("/ctl/quit")
				restore()
				fmt.Printf("Stopped sharing %s.\n", a.Label)
				return nil
			case 'c':
				if err := copyToClipboard(paste); err != nil {
					note = "couldn't copy: " + err.Error()
				} else {
					note = "copied to the clipboard"
				}
			}
		}
	}
}

func drawBrowserScreen(cfg *config, paste string, a browserActivity, note string) string {
	cols, rows := termSize()
	var lines []string
	p := func(format string, args ...any) {
		lines = append(lines, strings.Split(fmt.Sprintf(format, args...), "\n")...)
	}
	p("%sshoulder%s · session %s%s%s · sharing %s\n", bold, reset, bold, cfg.Name, reset, a.Label)
	p("Paste this to an agent (Claude Code, Codex, or one on another machine):\n\n%s%s%s\n", bold, paste, reset)
	if len(a.Tabs) == 0 {
		p("%stabs%s   none: the shared tabs were closed (an agent can still open one)", bold, reset)
	}
	for i, t := range a.Tabs {
		head := "      "
		if i == 0 {
			head = bold + "tabs" + reset + "  "
		}
		var notes []string
		if t.Current && len(a.Tabs) > 1 {
			notes = append(notes, "the agent's current tab")
		}
		if t.OpenedBy != "" {
			notes = append(notes, "opened by the agent")
		}
		if t.Dialog != "" {
			notes = append(notes, "dialog open")
		}
		extra := ""
		if len(notes) > 0 {
			extra = " (" + strings.Join(notes, ", ") + ")"
		}
		p("%s", fit(fmt.Sprintf("%s %s %s — %s%s", head, t.ID, cmpOr(t.Title, "(untitled)"), t.URL, extra), cols+len(bold)+len(reset)))
	}
	p("")
	foot := []string{"", fmt.Sprintf("%s[c]%s copy   %s[q]%s stop sharing", bold, reset, bold, reset)}
	if note != "" {
		foot = append(foot, note)
	}
	room := rows - len(lines) - len(foot) - 2
	if len(a.Activity) == 0 {
		p("%sagent%s  nothing yet", bold, reset)
	} else if room > 0 {
		acts := a.Activity
		if len(acts) > room {
			acts = acts[len(acts)-room:]
		}
		for i, act := range acts {
			head := "       "
			if i == 0 {
				head = bold + "agent" + reset + "  "
			}
			p("%s", fit(head+formatActivity(act), cols+len(bold)+len(reset)))
		}
	}
	lines = append(lines, foot...)
	return strings.Join(lines, "\n")
}

func formatActivity(a activity) string {
	s := a.Time.Local().Format("15:04:05") + "  " + a.Text
	if a.Count > 1 {
		s += fmt.Sprintf(" (×%d)", a.Count)
	}
	if a.ReadOnly {
		s += " [read-only code]"
	}
	return s
}

// browserLog is browserScreen without a terminal: activity as lines on
// stdout, until the browser goes away or shoulder is interrupted.
func browserLog(c *ctl) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var seen time.Time
	for first := true; ; first = false {
		var a browserActivity
		b, err := c.do("GET", "/ctl/activity")
		if err == nil {
			err = json.Unmarshal(b, &a)
		}
		if err != nil {
			return err
		}
		if first {
			fmt.Fprintf(os.Stderr, "sharing %s: %d %s; interrupt (^C) stops\n", a.Label, len(a.Tabs), plural(len(a.Tabs), "tab", "tabs"))
		}
		for _, act := range a.Activity {
			if act.Time.After(seen) {
				fmt.Println(formatActivity(act))
				seen = act.Time
			}
		}
		if a.Ended {
			fmt.Fprintf(os.Stderr, "%s went away; sharing stopped\n", a.Label)
			return nil
		}
		select {
		case <-ctx.Done():
			c.post("/ctl/quit")
			fmt.Fprintf(os.Stderr, "stopped sharing %s\n", a.Label)
			return nil
		case <-time.After(time.Second):
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
