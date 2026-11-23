// Command shoulder runs a command in a terminal session you can hand
// to an agent. The command runs in a PTY that shoulder owns, your terminal
// shows it, and agents read the screen, wait for output, and type, over
// plain curl. The session ends when the command does.
//
//	shoulder [flags] [--] [command [args...]]   # default $SHELL
//	shoulder browser [flags] [chrome|safari]
//	shoulder share [-t transport] [-read-only] [name]
//	shoulder ls
//
// A session can share browser tabs instead: your own Chrome, over the
// DevTools protocol (see chrome.go), or Safari, over AppleScript. Agents
// read pages, take screenshots, click and type over the same kind of API.
//
// Before the command starts, a first screen shows the text to paste to an
// agent and lets you choose what to share ([1] the terminal, [2] Chrome,
// [3] Safari) and how the agent connects: [u] the session's Unix
// socket (default; for agents on this machine), [h] localhost TCP, [*] all
// interfaces, [s] this machine's Tailscale address, or [t] an ephemeral
// tailcat node, which reaches across networks with nothing but the
// tailcat binary on the other side. [r] switches between read-write and
// read-only access; each has its own short code in the URL.
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// version is set at release time (see .goreleaser.yml).
var version = "dev"

func main() {
	var err error
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "browser":
		err = run(args[1:], true)
	case "share":
		err = shareCmd(args[1:])
	case "ls":
		err = listCmd()
	case "version", "-version", "--version":
		fmt.Println("shoulder", version)
	default:
		err = run(args, false)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shoulder:", err)
		os.Exit(1)
	}
}

func sessionsDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "shoulder"), nil
}

func run(args []string, browserCmd bool) error {
	fs := flag.NewFlagSet("shoulder", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: shoulder [flags] [--] [command [args...]]   run command (default $SHELL) in a shareable session
       shoulder browser [flags] [chrome|safari]         share browser tabs instead (default chrome)
       shoulder share [-t transport] [-read-only] [name] print the text to paste to an agent
       shoulder ls                                      list sessions

flags:
`)
		fs.PrintDefaults()
	}
	ch := choice{tabs: "active"}
	var tabs *string
	var profile *string
	var standalone *bool
	if browserCmd {
		fs = flag.NewFlagSet("shoulder browser", flag.ExitOnError)
		fs.Usage = func() {
			fmt.Fprint(fs.Output(), `usage: shoulder browser [flags] [chrome|safari]

Shares browser tabs with an agent, as shoulder shares a terminal. Chrome
is your own, running Chrome: allow remote debugging once at
chrome://inspect/#remote-debugging, and click Allow when Chrome asks.
Or -standalone: shoulder starts a Chrome of its own, which needs neither.

flags:
`)
			fs.PrintDefaults()
		}
		ch.kind = "chrome"
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			ch.kind, args = args[0], args[1:]
		}
		tabs = fs.String("tabs", "current", "which tabs to share: current (the tab you're on), new (a new tab), all (every tab of the profile, and new ones)")
		standalone = fs.Bool("standalone", false, "Chrome: start a standalone Chrome (its own profile, kept for next time) instead of sharing yours; no remote debugging to allow")
		profile = fs.String("profile", "", "Chrome: the profile to share, by name as Chrome shows it (\"Work\") or dir (\"Profile 1\") (default: the one you used last)")
	}
	transport := fs.String("t", "unix", "how agents connect: unix, localhost, lan (all interfaces), tailscale, tailcat")
	readOnly := fs.Bool("read-only", false, "share read-only access (agents can watch but not type)")
	yes := fs.Bool("y", false, "skip the first screen: share, print the paste text, and start")
	port := fs.Int("port", 0, "TCP port for localhost/lan/tailscale/tailcat shares (default: any free port)")
	name := fs.String("name", "", "session name (default: two random words)")
	nested := fs.Bool("nested", false, "allow starting a session inside another shoulder session")
	fs.Parse(args)
	if browserCmd {
		if fs.NArg() > 0 {
			ch.kind = fs.Arg(0)
		}
		if browserLabels[ch.kind] == "" || fs.NArg() > 1 {
			return fmt.Errorf("shoulder browser shares chrome or safari, not %q", strings.Join(fs.Args(), " "))
		}
		ch.tabs = map[string]string{"current": "active", "active": "active", "new": "new", "all": "all"}[*tabs]
		if ch.tabs == "" {
			return fmt.Errorf("-tabs is current, new or all, not %q", *tabs)
		}
		if *standalone {
			if ch.kind != "chrome" {
				return errors.New("-standalone is for Chrome")
			}
			if *profile != "" {
				return errors.New("-profile is for your Chrome, not a standalone one")
			}
			ch.standalone = true
		}
		if *profile != "" {
			if ch.kind != "chrome" {
				return errors.New("-profile is for Chrome")
			}
			profiles := readChromeProfiles(defaultChromeDir())
			pr, ok := profiles.find(*profile)
			if !ok {
				var names []string
				for _, dir := range profiles.order {
					names = append(names, strconv.Quote(profiles.all[dir].Name))
				}
				return fmt.Errorf("Chrome has no profile %q (it has %s)", *profile, cmpOr(strings.Join(names, ", "), "none that shoulder can see"))
			}
			ch.profile = pr.Dir
		}
	}

	if outer := os.Getenv("SHOULDER"); outer != "" && !*nested && !browserCmd {
		return fmt.Errorf("already inside shoulder session %s; `shoulder share %s` prints its line for an agent (-nested starts one anyway)", outer, outer)
	}

	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if !interactive && !(browserCmd && *yes) {
		return errors.New("shoulder needs a terminal (shoulder browser -y doesn't)")
	}
	if !validTransport(*transport) {
		return fmt.Errorf("unknown transport %q", *transport)
	}
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	cfg := &config{Command: fs.Args(), Port: *port, Started: time.Now(), Browser: ch.kind}
	if browserCmd {
		cfg.Command = nil
	}
	if len(cfg.Command) == 0 {
		cfg.Command = []string{cmp.Or(os.Getenv("SHELL"), "/bin/sh")}
	}
	cfg.Name = cmp.Or(*name, newID(dir))
	cfg.Dir = filepath.Join(dir, cfg.Name)
	if cfg.Cwd, err = os.Getwd(); err != nil {
		return err
	}
	cfg.Host, _ = os.Hostname()
	cfg.Host, _, _ = strings.Cut(cfg.Host, ".")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return err
	}
	if err := cfg.save(); err != nil {
		return err
	}
	stop, err := startServer(cfg)
	if err != nil {
		return err
	}
	defer stop()
	ctl := newCtl(cfg.path("sock"))

	info, err := ctl.share(*transport, *readOnly, true)
	if err != nil {
		return err
	}
	if *yes {
		fmt.Printf("%s\n", info.Paste)
	}
	for note := ""; ; {
		if !*yes {
			var ok bool
			if info, ok, err = firstScreen(cfg, ctl, info, &ch, note); err != nil || !ok {
				return err
			}
		}
		if ch.kind == "" {
			break
		}
		ok, err := connectBrowser(cfg, ctl, ch, !*yes)
		if ok {
			return browserScreen(cfg, ctl, info.Paste)
		}
		if *yes || err != nil && !interactive {
			return err
		}
		note = ""
		if err != nil {
			note = fmt.Sprintf("%s⚠ %s%s", "\x1b[33m", err, reset)
		}
	}
	cols, rows := termSize()
	q := url.Values{"cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)}}
	if _, err := ctl.post("/ctl/start?" + q.Encode()); err != nil {
		return err
	}
	return attach(cfg)
}

func validTransport(t string) bool {
	for _, tr := range transports {
		if tr.name == t {
			return true
		}
	}
	return false
}

// startServer serves the session from this process, logging to its
// server.log. stop ends the session, as do SIGHUP and SIGTERM (the
// terminal closing, say), and waits for it to clean up: closing shares,
// letting go of the browser.
func startServer(cfg *config) (stop func(), err error) {
	logf, err := os.OpenFile(cfg.path("server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	log.SetOutput(logf)
	sc := *cfg // the server's own, which it updates
	s, err := listen(&sc)
	if err != nil {
		logf.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		if err := s.run(); err != nil {
			log.Printf("session: %v", err)
		}
		close(done)
	}()
	wait := func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	tstate, _ := term.GetState(int(os.Stdin.Fd()))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigs:
			log.Printf("%v: ending the session", sig)
			s.quit()
			wait()
			if tstate != nil {
				term.Restore(int(os.Stdin.Fd()), tstate)
			}
			os.Exit(1)
		case <-done:
		}
	}()
	return func() {
		s.quit()
		wait()
		signal.Stop(sigs)
		logf.Close()
	}, nil
}

func termSize() (int, int) {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80, 24
	}
	return cols, rows
}

// ctl talks to a session server's control API over its Unix socket.
type ctl struct {
	c *http.Client
}

func newCtl(sock string) *ctl {
	return &ctl{c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

func (c *ctl) do(method, path string) ([]byte, error) {
	return c.doContext(context.Background(), method, path)
}

func (c *ctl) doContext(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://shoulder"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(strings.TrimSpace(string(b)))
	}
	return b, err
}

func (c *ctl) post(path string) ([]byte, error) { return c.do("POST", path) }

func (c *ctl) share(transport string, readOnly, exclusive bool) (shareInfo, error) {
	q := url.Values{"transport": {transport}}
	if readOnly {
		q.Set("read_only", "1")
	}
	if exclusive {
		q.Set("exclusive", "1")
	}
	var info shareInfo
	b, err := c.post("/ctl/share?" + q.Encode())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// firstScreen shows the paste text and the sharing choices, and returns
// once the user starts the command or shares the browser (ok) or quits.
func firstScreen(cfg *config, c *ctl, info shareInfo, ch *choice, note string) (shareInfo, bool, error) {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return info, false, err
	}
	defer term.Restore(int(os.Stdin.Fd()), old)
	var scr screen
	var keys []byte
	for {
		status := ""
		ready := true
		if ch.kind != "" {
			ready, status = browserReady(*ch)
		}
		if ready && strings.HasPrefix(note, notYet) {
			note = ""
		}
		if len(keys) == 0 {
			scr.draw(drawFirstScreen(cfg, info, *ch, ready, status, note))
			// The browser's state can change while this is up: check again.
			if keys, err = readKeys(time.Second); err != nil {
				return info, false, err
			}
			continue
		}
		k := keys[0]
		keys = keys[1:]
		note = ""
		if handled, n := browserKey(c, cfg, ch, k); handled {
			note = n
			continue
		}
		switch k {
		case '\r', '\n':
			if !ready {
				note = notYet + browserLabels[ch.kind] + " isn't reachable (see above)."
				continue
			}
			return info, true, nil
		case 'q', 3, 4: // q, C-c, C-d
			fmt.Print("\x1b[H\x1b[2J")
			return info, false, nil
		case 'c':
			if err := copyToClipboard(info.Paste); err != nil {
				note = "couldn't copy: " + err.Error()
			} else {
				note = "copied to the clipboard"
			}
		case 'r':
			next, err := c.share(info.Transport, !info.ReadOnly, true)
			if err != nil {
				note = err.Error()
			} else {
				info = next
			}
		default:
			for _, tr := range transports {
				if string(k) == tr.key && tr.name != info.Transport {
					scr.draw(drawFirstScreen(cfg, info, *ch, ready, status, "setting up "+tr.label+"…"))
					next, err := c.share(tr.name, info.ReadOnly, true)
					if err != nil {
						note = err.Error()
					} else {
						info = next
					}
				}
			}
		}
	}
}

const notYet = "Not yet: "

const (
	bold  = "\x1b[1m"
	rev   = "\x1b[7m"
	dim   = "\x1b[2m"
	reset = "\x1b[0m"
)

func drawFirstScreen(cfg *config, info shareInfo, ch choice, ready bool, status, note string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	what := displayCommand(cfg.Command)
	if ch.kind != "" {
		what = browserLabels[ch.kind]
	}
	p("%sshoulder%s · session %s%s%s · %s\n\n", bold, reset, bold, cfg.Name, reset, what)
	p("Paste this to an agent (Claude Code, Codex, or one on another machine):\n\n%s%s%s\n\n", bold, info.Paste, reset)
	if info.Warning != "" {
		p("%s⚠ %s%s\n", "\x1b[33m", info.Warning, reset)
	}
	if info.Hint != "" {
		p("%s%s%s\n", dim, info.Hint, reset)
	}
	p("\n%sshare%s  ", bold, reset)
	for _, kd := range availableKinds() {
		label := "[" + kd.key + "] " + kd.label
		if kd.name == ch.kind {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
	browserRows(p, ch)
	p("\n%svia%s    ", bold, reset)
	for _, tr := range transports {
		label := "[" + tr.key + "] " + tr.label
		if tr.name == info.Transport {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
	p("\n%saccess%s ", bold, reset)
	for _, ro := range []bool{false, true} {
		label := "[r] read-write"
		if ro {
			label = "[r] read-only"
		}
		if ro == info.ReadOnly {
			label = rev + label + reset
		}
		p("%s  ", label)
	}
	p("\n")
	start := "start " + filepath.Base(cfg.Command[0])
	if ch.kind != "" {
		mark := "\x1b[32m✓\x1b[0m"
		if !ready {
			mark = "\x1b[33m⚠\x1b[0m"
		}
		p("\n%s %s\n", mark, status)
		start = "share " + browserLabels[ch.kind]
	}
	p("\n%s[enter]%s %s   %s[c]%s copy   %s[q]%s quit\n", bold, reset, start, bold, reset, bold, reset)
	if note != "" {
		p("\n%s\n", note)
	}
	return b.String()
}

func copyToClipboard(s string) error {
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin":
		cmd = exec.Command("pbcopy")
	case os.Getenv("WAYLAND_DISPLAY") != "":
		cmd = exec.Command("wl-copy")
	default:
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	if cmd == nil {
		return errors.New("no clipboard tool (pbcopy, wl-copy, xclip)")
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Other subcommands.

type sessionInfo struct {
	cfg    *config
	status *status
	shares []string
}

// sessions lists sessions newest first, with live status where the
// server answers.
func sessions() ([]sessionInfo, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []sessionInfo
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "config.json"))
		if err != nil {
			continue
		}
		cfg := new(config)
		if json.Unmarshal(b, cfg) != nil {
			continue
		}
		si := sessionInfo{cfg: cfg}
		if b, err := newCtl(cfg.path("sock")).do("GET", "/ctl/info"); err == nil {
			var v struct {
				Status status   `json:"status"`
				Shares []string `json:"shares"`
			}
			if json.Unmarshal(b, &v) == nil {
				si.status, si.shares = &v.Status, v.Shares
			}
		}
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.Started.After(out[j].cfg.Started) })
	return out, nil
}

// findSession resolves a name, or with none the newest live session.
func findSession(name string) (*config, error) {
	all, err := sessions()
	if err != nil {
		return nil, err
	}
	for _, si := range all {
		if (name == "" && si.status != nil) || si.cfg.Name == name {
			if si.status == nil {
				return nil, fmt.Errorf("session %s has ended", si.cfg.Name)
			}
			return si.cfg, nil
		}
	}
	if name == "" {
		return nil, errors.New("no live sessions")
	}
	return nil, fmt.Errorf("no session %q (shoulder ls lists them)", name)
}

func listCmd() error {
	all, err := sessions()
	if err != nil {
		return err
	}
	for _, si := range all {
		state := "ended"
		what := displayCommand(si.cfg.Command)
		if si.cfg.Browser != "" {
			what = browserLabels[si.cfg.Browser]
		}
		if st := si.status; st != nil {
			switch {
			case !st.Started:
				state = "not started"
			case st.Browser != "":
				what = st.Browser
				state = fmt.Sprintf("sharing %d tab", st.SharedTabs)
				if st.SharedTabs != 1 {
					state += "s"
				}
				if !st.Running {
					state = "ended"
				}
			case st.ExitCode != nil:
				state = fmt.Sprintf("exited %d", *st.ExitCode)
			default:
				state = "running"
			}
			state += " · " + strings.Join(si.shares, ",")
		}
		fmt.Printf("%-18s %-34s %s  (%s)\n", si.cfg.Name, state, what, tildePath(si.cfg.Cwd))
	}
	return nil
}

func shareCmd(args []string) error {
	fs := flag.NewFlagSet("shoulder share", flag.ExitOnError)
	transport := fs.String("t", "unix", "unix, localhost, lan, tailscale, tailcat")
	readOnly := fs.Bool("read-only", false, "read-only access")
	fs.Parse(args)
	cfg, err := findSession(fs.Arg(0))
	if err != nil {
		return err
	}
	info, err := newCtl(cfg.path("sock")).share(*transport, *readOnly, false)
	if err != nil {
		return err
	}
	if info.Warning != "" {
		fmt.Fprintln(os.Stderr, "⚠", info.Warning)
	}
	if info.Hint != "" {
		fmt.Fprintln(os.Stderr, info.Hint)
	}
	fmt.Print(info.Paste)
	return nil
}

// ---------------------------------------------------------------------------
// The terminal.

// terminalReset undoes modes a full-screen program may have left on, so
// the terminal is usable after the session ends. Leaving the alternate
// screen can restore a stale saved cursor, so it ends at the bottom row.
const terminalReset = "\x1b[0m\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1l\x1b>\x1b[999;1H"

// attach connects this terminal to the session's command, until it exits.
func attach(cfg *config) error {
	conn, err := net.Dial("unix", cfg.path("sock"))
	if err != nil {
		return err
	}
	defer conn.Close()
	cols, rows := termSize()
	fmt.Fprintf(conn, "GET /ctl/attach?cols=%d&rows=%d HTTP/1.1\r\nHost: shoulder\r\nConnection: Upgrade\r\nUpgrade: shoulder\r\n\r\n", cols, rows)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("attach: %s", resp.Status)
	}

	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	restore := func() { term.Restore(int(os.Stdin.Fd()), old) }
	defer restore()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			c, r := termSize()
			p := binary.LittleEndian.AppendUint16(nil, uint16(c))
			writeFrame(conn, frameWinch, binary.LittleEndian.AppendUint16(p, uint16(r)))
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if writeFrame(conn, frameData, buf[:n]) != nil {
				return
			}
		}
	}()

	exitCode := -1
	for {
		typ, p, err := readFrame(br)
		if err != nil {
			break
		}
		switch typ {
		case frameData:
			os.Stdout.Write(p)
		case frameExit:
			if len(p) >= 4 {
				exitCode = int(binary.LittleEndian.Uint32(p))
			}
		}
		if exitCode >= 0 {
			break
		}
	}
	restore()
	os.Stdout.WriteString(terminalReset)
	if exitCode >= 0 {
		fmt.Printf("\r\n[%s exited with status %s]\n", filepath.Base(cfg.Command[0]), strconv.Itoa(exitCode))
		return nil
	}
	fmt.Printf("\r\n[lost the session %s]\n", cfg.Name)
	return nil
}
