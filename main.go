// Command shoulder runs a command in a terminal session you can hand
// to an agent. It is dtach with an HTTP API: the command
// runs in a PTY owned by a background session server, your terminal
// attaches to it (`shoulder attach` reattaches if it goes away), and
// agents read the screen, wait for output, and type, over plain curl.
//
//	shoulder [flags] [--] [command [args...]]   # default $SHELL
//	shoulder attach [name]
//	shoulder share [-t transport] [-read-only] [name]
//	shoulder ls
//
// Before the command starts, a first screen shows the text to paste to an
// agent and lets you choose how the agent connects: [u] the session's Unix
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

func main() {
	var err error
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "__serve":
		err = serve(args[1])
	case "attach":
		err = attachCmd(args[1:])
	case "share":
		err = shareCmd(args[1:])
	case "ls":
		err = listCmd()
	default:
		err = run(args)
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

func run(args []string) error {
	fs := flag.NewFlagSet("shoulder", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: shoulder [flags] [--] [command [args...]]   run command (default $SHELL) in a shareable session
       shoulder attach [name]                           reattach a terminal to a running session
       shoulder share [-t transport] [-read-only] [name] print the text to paste to an agent
       shoulder ls                                      list sessions

flags:
`)
		fs.PrintDefaults()
	}
	transport := fs.String("t", "unix", "how agents connect: unix, localhost, lan (all interfaces), tailscale, tailcat")
	readOnly := fs.Bool("read-only", false, "share read-only access (agents can watch but not type)")
	yes := fs.Bool("y", false, "skip the first screen: share, print the paste text, and start")
	port := fs.Int("port", 0, "TCP port for localhost/lan/tailscale/tailcat shares (default: any free port)")
	name := fs.String("name", "", "session name (default: two random words)")
	linger := fs.Duration("linger", 10*time.Minute, "keep serving the final screen this long after the command exits")
	fs.Parse(args)

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("shoulder needs a terminal")
	}
	if !validTransport(*transport) {
		return fmt.Errorf("unknown transport %q", *transport)
	}
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	cfg := &config{Command: fs.Args(), Linger: *linger, Port: *port, Started: time.Now()}
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
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfg.path("config.json"), b, 0o600); err != nil {
		return err
	}
	if err := spawnServer(cfg); err != nil {
		return err
	}
	ctl := newCtl(cfg.path("sock"))

	info, err := ctl.share(*transport, *readOnly, true)
	if err != nil {
		ctl.post("/ctl/quit")
		return err
	}
	if *yes {
		fmt.Printf("%s\n", info.Paste)
	} else {
		var ok bool
		if info, ok, err = firstScreen(cfg, ctl, info); err != nil || !ok {
			ctl.post("/ctl/quit")
			return err
		}
	}
	cols, rows := termSize()
	q := url.Values{"cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)}, "share": {info.Paste}}
	if _, err := ctl.post("/ctl/start?" + q.Encode()); err != nil {
		return err
	}
	return attach(cfg, false)
}

func validTransport(t string) bool {
	for _, tr := range transports {
		if tr.name == t {
			return true
		}
	}
	return false
}

// spawnServer starts the session server in the background, detached from
// this terminal, and waits for its socket.
func spawnServer(cfg *config) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(cfg.path("server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "__serve", cfg.Dir)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()
	sock := cfg.path("sock")
	for range 100 {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session server didn't start; see %s", cfg.path("server.log"))
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
	req, err := http.NewRequest(method, "http://shoulder"+path, nil)
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
// once the user starts the command (ok) or quits.
func firstScreen(cfg *config, c *ctl, info shareInfo) (shareInfo, bool, error) {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return info, false, err
	}
	defer term.Restore(int(os.Stdin.Fd()), old)
	in := bufio.NewReader(os.Stdin)
	note := ""
	for {
		drawFirstScreen(cfg, info, note)
		note = ""
		k, err := in.ReadByte()
		if err != nil {
			return info, false, err
		}
		switch k {
		case '\r', '\n':
			fmt.Print("\x1b[H\x1b[2J")
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
					drawFirstScreen(cfg, info, "setting up "+tr.label+"…")
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

const (
	bold  = "\x1b[1m"
	rev   = "\x1b[7m"
	reset = "\x1b[0m"
)

func drawFirstScreen(cfg *config, info shareInfo, note string) {
	var b strings.Builder
	p := func(format string, args ...any) {
		b.WriteString(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", "\r\n"))
	}
	p("\x1b[H\x1b[2J%sshoulder%s · session %s%s%s · %s\n\n", bold, reset, bold, cfg.Name, reset, displayCommand(cfg.Command))
	p("Paste this to an agent (Claude Code, Codex, or one on another machine):\n\n%s%s%s\n\n", bold, info.Paste, reset)
	if info.Note != "" {
		p("%s\n", info.Note)
	}
	if info.Warning != "" {
		p("%s⚠ %s%s\n", "\x1b[33m", info.Warning, reset)
	}
	p("\n%sshare%s  ", bold, reset)
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
	p("\n%s[enter]%s start %s   %s[c]%s copy   %s[q]%s quit\n", bold, reset, filepath.Base(cfg.Command[0]), bold, reset, bold, reset)
	if note != "" {
		p("\n%s\n", note)
	}
	os.Stdout.WriteString(b.String())
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
		if st := si.status; st != nil {
			switch {
			case !st.Started:
				state = "not started"
			case st.ExitCode != nil:
				state = fmt.Sprintf("exited %d", *st.ExitCode)
			default:
				state = fmt.Sprintf("running, %d attached", st.Terminals)
			}
			state += " · " + strings.Join(si.shares, ",")
		}
		fmt.Printf("%-18s %-34s %s  (%s)\n", si.cfg.Name, state, displayCommand(si.cfg.Command), tildePath(si.cfg.Cwd))
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
	if info.Note != "" {
		fmt.Fprintln(os.Stderr, info.Note)
	}
	if info.Warning != "" {
		fmt.Fprintln(os.Stderr, "⚠", info.Warning)
	}
	fmt.Print(info.Paste)
	return nil
}

func attachCmd(args []string) error {
	fs := flag.NewFlagSet("shoulder attach", flag.ExitOnError)
	fs.Parse(args)
	cfg, err := findSession(fs.Arg(0))
	if err != nil {
		return err
	}
	return attach(cfg, true)
}

// ---------------------------------------------------------------------------
// Attaching a terminal.

// terminalReset undoes modes a full-screen program may have left on, so
// the terminal is usable after the session ends. Leaving the alternate
// screen can restore a stale saved cursor, so it ends at the bottom row.
const terminalReset = "\x1b[0m\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1l\x1b>\x1b[999;1H"

// attach connects this terminal to a session. A terminal that just
// started the session skips the repaint, as with dtach.
func attach(cfg *config, repaint bool) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("attaching needs a terminal")
	}
	conn, err := net.Dial("unix", cfg.path("sock"))
	if err != nil {
		return fmt.Errorf("session %s isn't running: %w", cfg.Name, err)
	}
	defer conn.Close()
	cols, rows := termSize()
	rp := "1"
	if !repaint {
		rp = "0"
	}
	fmt.Fprintf(conn, "GET /ctl/attach?cols=%d&rows=%d&repaint=%s HTTP/1.1\r\nHost: shoulder\r\nConnection: Upgrade\r\nUpgrade: shoulder\r\n\r\n", cols, rows, rp)
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
		fmt.Printf("\r\n[%s exited with status %s; agents can still read the session for %s]\n",
			filepath.Base(cfg.Command[0]), strconv.Itoa(exitCode), cfg.Linger)
		return nil
	}
	fmt.Printf("\r\n[lost the session %s]\n", cfg.Name)
	return nil
}
