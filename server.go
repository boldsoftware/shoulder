package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// server is a session: it owns the command's PTY, keeps a terminal
// emulator in step with it (so the screen can be rendered for agents and
// repainted for reattaching terminals), and serves both over HTTP. Its own
// Unix socket carries the control API, terminal attachments, and the agent
// API; shares add more listeners for the agent API only.
type server struct {
	cfg      *config
	ctlSock  string
	started  chan struct{}
	quitting chan struct{}

	mu         sync.Mutex
	ptmx       *os.File
	cmd        *exec.Cmd
	vt         *vterm
	attached   atomic.Int32 // len(clients), readable without mu
	out        []byte       // raw output, the most recent maxOutput bytes
	base       int64        // absolute offset of out[0]
	lastOutput time.Time
	lastSend   time.Time
	exited     bool
	exitCode   int
	fg         string // foreground process name
	fgIsShell  bool   // the command itself is in the foreground
	fgChanged  time.Time
	sawBusy    time.Time // last time something other than the command was in the foreground
	changed    chan struct{}
	clients    map[*client]bool
	owner      *client
	shares     map[string]*share
}

const maxOutput = 8 << 20

type config struct {
	Name     string        `json:"name"`
	Dir      string        `json:"dir"`
	Command  []string      `json:"command"`
	Cwd      string        `json:"cwd"`
	Started  time.Time     `json:"started"`
	Linger   time.Duration `json:"linger"`
	Port     int           `json:"port"`
	Host     string        `json:"host"`      // this machine's name, for agents' context
	ReadOnly bool          `json:"read_only"` // never make a code that can type: agents over the network only watch
	Attached bool          `json:"-"`
}

func (c *config) path(name string) string { return filepath.Join(c.Dir, name) }

// serve is the session server's main: `shoulder __serve <dir>`.
func serve(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	cfg := new(config)
	if err := json.Unmarshal(b, cfg); err != nil {
		return err
	}
	s := &server{
		cfg:      cfg,
		ctlSock:  cfg.path("sock"),
		started:  make(chan struct{}),
		quitting: make(chan struct{}),
		changed:  make(chan struct{}),
		clients:  map[*client]bool{},
		shares:   map[string]*share{},
	}
	os.Remove(s.ctlSock)
	ln, err := net.Listen("unix", s.ctlSock)
	if err != nil {
		return err
	}
	defer os.Remove(s.ctlSock)
	os.Chmod(s.ctlSock, 0o600)
	unixShare, err := s.share(context.Background(), "unix")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(unixShare)}
	go srv.Serve(ln)

	// A session nobody starts goes away on its own.
	select {
	case <-s.started:
	case <-s.quitting:
		return nil
	case <-time.After(time.Hour):
		return nil
	}
	go s.watchForeground()
	err = s.cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	s.mu.Lock()
	s.exited, s.exitCode = true, code
	s.notifyLocked()
	for c := range s.clients {
		c.send(frameExit, binary.LittleEndian.AppendUint32(nil, uint32(code)))
	}
	s.mu.Unlock()
	log.Printf("command exited with status %d; lingering %s", code, cfg.Linger)
	select {
	case <-time.After(cfg.Linger):
	case <-s.quitting:
	}
	s.mu.Lock()
	for _, sh := range s.shares {
		sh.close()
	}
	s.mu.Unlock()
	return nil
}

// quit ends the session: it hangs up on the command, as closing its
// terminal would, and stops serving without lingering.
func (s *server) quit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.quitting:
		return
	default:
		close(s.quitting)
	}
	if s.cmd != nil && !s.exited {
		// The command leads its own session (pty.Start setsid), so this
		// reaches its whole process group.
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
	}
}

// notifyLocked wakes everything waiting for output or a state change.
func (s *server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// start runs the command at the given size. Its environment has
// $SHOULDER, the session name, which keeps shoulder from starting a
// session inside itself. (The codes stay out of the environment: they're
// secrets, and children inherit it.)
func (s *server) start(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return errors.New("already started")
	}
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Dir = s.cfg.Cwd
	cmd.Env = append(os.Environ(), "SHOULDER="+s.cfg.Name)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	s.cmd, s.ptmx = cmd, ptmx
	// With a terminal attached, it answers the program's queries itself;
	// with none, the emulator's answers stand in, so programs that ask
	// (where's the cursor? what are you?) don't hang.
	s.vt = newTerm(cols, rows, func(reply []byte) {
		if s.attached.Load() == 0 {
			ptmx.Write(reply)
		}
	})
	go s.readLoop()
	close(s.started)
	return nil
}

func (s *server) readLoop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.vt.write(data)
			s.out = append(s.out, data...)
			if len(s.out) > maxOutput {
				drop := len(s.out) - maxOutput/2
				s.out = append([]byte(nil), s.out[drop:]...)
				s.base += int64(drop)
			}
			s.lastOutput = time.Now()
			left := s.vt.tookLeftAlt()
			for c := range s.clients {
				c.send(frameData, data)
				if left && c.stale {
					c.send(frameData, s.vt.ansi())
					c.stale = false
				}
			}
			s.notifyLocked()
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// watchForeground tracks which process is in the terminal's foreground, so
// agents can wait for a shell command to finish.
func (s *server) watchForeground() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.exited {
			s.mu.Unlock()
			return
		}
		fd := int(s.ptmx.Fd())
		pid := s.cmd.Process.Pid
		s.mu.Unlock()
		pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if err != nil {
			continue
		}
		name := processName(pgrp)
		s.mu.Lock()
		isShell := pgrp == pid
		if name != s.fg || isShell != s.fgIsShell {
			s.fg, s.fgIsShell, s.fgChanged = name, isShell, time.Now()
			s.notifyLocked()
		}
		if !isShell {
			s.sawBusy = time.Now()
		}
		s.mu.Unlock()
	}
}

func processName(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(filepath.Base(strings.TrimSpace(string(out))), "-")
}

// typeInput writes to the command's terminal.
func (s *server) typeInput(b []byte) error {
	s.mu.Lock()
	ptmx, exited := s.ptmx, s.exited
	s.lastSend = time.Now()
	s.mu.Unlock()
	if ptmx == nil || exited {
		return errors.New("the command is not running")
	}
	_, err := ptmx.Write(b)
	return err
}

// resizeLocked applies a terminal size to the PTY and the emulator.
func (s *server) resizeLocked(cols, rows int) {
	if s.ptmx == nil || cols <= 0 || rows <= 0 {
		return
	}
	if c, r := s.vt.size(); c == cols && r == rows {
		return
	}
	pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	s.vt.resize(cols, rows)
}

// ---------------------------------------------------------------------------
// Terminal attachments. A client upgrades GET /ctl/attach and then speaks
// frames: [type u8][len u32 LE][payload].

const (
	frameData  = 1 // both directions: terminal bytes
	frameWinch = 2 // client→server: cols u16, rows u16
	frameExit  = 4 // server→client: exit status u32
)

type client struct {
	// stale: attached during a full-screen program, so the terminal never
	// saw the screen underneath, and needs it painted when the program
	// leaves the alternate screen.
	stale bool
	conn  net.Conn
	q     chan []byte
	cols  int
	rows  int
}

// send queues a frame without blocking; a client too slow to keep up is
// dropped rather than stalling the command.
func (c *client) send(typ byte, payload []byte) {
	f := make([]byte, 5+len(payload))
	f[0] = typ
	binary.LittleEndian.PutUint32(f[1:], uint32(len(payload)))
	copy(f[5:], payload)
	select {
	case c.q <- f:
	default:
		c.conn.Close()
	}
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[1:])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return hdr[0], p, err
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	f := make([]byte, 5+len(payload))
	f[0] = typ
	binary.LittleEndian.PutUint32(f[1:], uint32(len(payload)))
	copy(f[5:], payload)
	_, err := w.Write(f)
	return err
}

func (s *server) handleAttach(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot attach", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: shoulder\r\nConnection: Upgrade\r\n\r\n")
	rw.Flush()
	c := &client{conn: conn, q: make(chan []byte, 1024)}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	c.cols, c.rows = cols, rows

	s.mu.Lock()
	if s.vt != nil {
		// The newest terminal owns the size; repaint it from the emulator,
		// except for the terminal that just started the session, whose
		// screen is already right (and may hold the paste line).
		s.owner = c
		s.resizeLocked(cols, rows)
		if r.URL.Query().Get("repaint") != "0" {
			c.send(frameData, s.vt.ansi())
			c.stale = s.vt.altScreen()
		}
	}
	if s.exited {
		c.send(frameExit, binary.LittleEndian.AppendUint32(nil, uint32(s.exitCode)))
	}
	s.clients[c] = true
	s.attached.Store(int32(len(s.clients)))
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.attached.Store(int32(len(s.clients)))
		if s.owner == c {
			s.owner = nil
		}
		s.mu.Unlock()
	}()

	go func() {
		for f := range c.q {
			if _, err := conn.Write(f); err != nil {
				conn.Close()
				return
			}
		}
	}()
	defer close(c.q)
	br := bufio.NewReader(rw)
	for {
		typ, p, err := readFrame(br)
		if err != nil {
			return
		}
		switch typ {
		case frameData:
			s.mu.Lock()
			if s.owner != c && c.cols > 0 {
				// Typing claims the size, so the terminal in use fits.
				s.owner = c
				s.resizeLocked(c.cols, c.rows)
			}
			s.mu.Unlock()
			s.typeInput(p)
		case frameWinch:
			if len(p) >= 4 {
				s.mu.Lock()
				c.cols, c.rows = int(binary.LittleEndian.Uint16(p[0:])), int(binary.LittleEndian.Uint16(p[2:]))
				if s.owner == c || s.owner == nil {
					s.owner = c
					s.resizeLocked(c.cols, c.rows)
				}
				s.mu.Unlock()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP.

// handler serves a share's agent API: under /<code>/ on a network share,
// and at the root of the session's own socket, which has no codes and also
// carries the control API under /ctl/.
func (s *server) handler(sh *share) http.Handler {
	mux := http.NewServeMux()
	if sh.transport == "unix" {
		mux.HandleFunc("POST /ctl/start", s.ctlStart)
		mux.HandleFunc("POST /ctl/share", s.ctlShare)
		mux.HandleFunc("POST /ctl/unshare", s.ctlUnshare)
		mux.HandleFunc("GET /ctl/info", s.ctlInfo)
		mux.HandleFunc("POST /ctl/quit", func(w http.ResponseWriter, r *http.Request) { s.quit() })
		mux.HandleFunc("GET /ctl/attach", s.handleAttach)
		mux.HandleFunc("/{rest...}", func(w http.ResponseWriter, r *http.Request) { s.agent(w, r, sh, "", false) })
		return mux
	}
	mux.HandleFunc("/{token}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		readOnly, ok := sh.access(token)
		if !ok {
			http.Error(w, "unknown code", http.StatusForbidden)
			return
		}
		// A read-only session has no read-write code, and would not honor
		// one.
		s.agent(w, r, sh, token, readOnly || s.cfg.ReadOnly)
	})
	return mux
}

func (s *server) ctlStart(w http.ResponseWriter, r *http.Request) {
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	if err := s.start(cols, rows); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
	}
}

// shareInfo is what the launcher shows: the text to paste to an agent.
type shareInfo struct {
	Transport string `json:"transport"`
	ReadOnly  bool   `json:"read_only"`
	Paste     string `json:"paste"`
	Warning   string `json:"warning,omitempty"`
	Note      string `json:"note,omitempty"`
}

// infoReply is what /ctl/info reports: the session's status and its open
// shares.
type infoReply struct {
	Status status   `json:"status"`
	Shares []string `json:"shares"`
}

// ctlShare opens a share if need be and answers with the paste for the
// access asked for. The unix socket has no codes and no read-only access.
// In a read-only session there is no read-write code, so the answer is
// read-only whatever was asked for.
func (s *server) ctlShare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	transport := q.Get("transport")
	readOnly := q.Get("read_only") == "1"
	if transport == "unix" && readOnly {
		http.Error(w, "the unix socket has no read-only access: anyone who can reach it controls the session", http.StatusBadRequest)
		return
	}
	sh, err := s.share(r.Context(), transport)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code := ""
	if transport != "unix" {
		readOnly = readOnly || s.cfg.ReadOnly
		code = sh.rw
		if readOnly {
			code = sh.ro
		}
	}
	writeJSON(w, shareInfo{Transport: transport, ReadOnly: readOnly, Paste: paste(s.cfg, sh, code, readOnly), Warning: sh.warning, Note: sh.note})
}

// share returns the listener for transport, setting it up if need be.
func (s *server) share(ctx context.Context, transport string) (*share, error) {
	s.mu.Lock()
	sh := s.shares[transport]
	s.mu.Unlock()
	if sh != nil {
		return sh, nil
	}
	opened, err := openShare(ctx, s, transport)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shares[transport]; sh != nil {
		opened.close() // opened twice at once; the first one stays
		return sh, nil
	}
	s.shares[transport] = opened
	return opened, nil
}

// ctlUnshare closes a network share. The launcher's menu switches between
// transports by opening the new one and then closing the one it was on.
func (s *server) ctlUnshare(w http.ResponseWriter, r *http.Request) {
	transport := r.URL.Query().Get("transport")
	if transport == "unix" {
		http.Error(w, "the unix socket is the session's own and stays open", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sh := s.shares[transport]
	delete(s.shares, transport)
	s.mu.Unlock()
	if sh != nil {
		sh.close()
	}
}

func (s *server) ctlInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	shares := slices.Sorted(maps.Keys(s.shares))
	s.mu.Unlock()
	writeJSON(w, infoReply{Status: s.status(), Shares: shares})
}

type status struct {
	Name       string   `json:"name"`
	Command    []string `json:"command"`
	Host       string   `json:"host"`
	Cwd        string   `json:"cwd"`
	Started    bool     `json:"started"`
	Running    bool     `json:"running"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Foreground string   `json:"foreground,omitempty"`
	AtPrompt   bool     `json:"at_prompt"` // the command itself is in the foreground (for a shell: at its prompt)
	Cols       int      `json:"cols"`
	Rows       int      `json:"rows"`
	Terminals  int      `json:"attached_terminals"`
	IdleFor    string   `json:"no_output_for,omitempty"`
	Offset     int64    `json:"output_offset"`
	ReadOnly   bool     `json:"read_only"`
}

func (s *server) status() status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := status{
		Name: s.cfg.Name, Command: s.cfg.Command, Host: s.cfg.Host, Cwd: s.cfg.Cwd,
		Started: s.cmd != nil, Running: s.cmd != nil && !s.exited,
		Foreground: s.fg, AtPrompt: s.fgIsShell, Terminals: len(s.clients),
		Offset: s.base + int64(len(s.out)),
	}
	if s.exited {
		code := s.exitCode
		st.ExitCode = &code
	}
	if s.vt != nil {
		st.Cols, st.Rows = s.vt.size()
	}
	if !s.lastOutput.IsZero() {
		st.IdleFor = time.Since(s.lastOutput).Round(100 * time.Millisecond).String()
	}
	return st
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// agent serves the agent API: /<code>/<endpoint> on a network share, or
// /<endpoint> on the unix socket, where token is empty.
func (s *server) agent(w http.ResponseWriter, r *http.Request, sh *share, token string, readOnly bool) {
	switch rest := r.PathValue("rest"); {
	case rest == "" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		io.WriteString(w, guide(s.cfg, sh, token, readOnly))
	case rest == "status" && r.Method == "GET":
		st := s.status()
		st.ReadOnly = readOnly
		writeJSON(w, st)
	case rest == "capture-pane" && r.Method == "GET":
		s.getScreen(w, r)
	case rest == "output" && r.Method == "GET":
		s.getOutput(w, r)
	case rest == "wait" && r.Method == "GET":
		s.getWait(w, r)
	case (rest == "send-keys" || rest == "run") && r.Method == "POST":
		if readOnly {
			http.Error(w, "this code is read-only: you can watch the terminal but not type into it", http.StatusForbidden)
			return
		}
		if rest == "send-keys" {
			s.postSendKeys(w, r)
		} else {
			s.postRun(w, r)
		}
	default:
		http.Error(w, "no such endpoint; GET "+sh.url(token)+"/ describes the API", http.StatusNotFound)
	}
}

func (s *server) getScreen(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.vt == nil {
		s.mu.Unlock()
		http.Error(w, "the command has not started yet", http.StatusServiceUnavailable)
		return
	}
	var body []byte
	switch r.URL.Query().Get("format") {
	case "ansi":
		body = s.vt.ansi()
	default:
		body = []byte(s.vt.text())
	}
	cx, cy := s.vt.cursor()
	cols, rows := s.vt.size()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Cursor", fmt.Sprintf("%d,%d", cx, cy))
	w.Header().Set("X-Size", fmt.Sprintf("%dx%d", cols, rows))
	w.Write(body)
}

// outputSince returns cleaned output from absolute offset off, and the
// offset to pass next time.
func (s *server) outputSince(off int64) (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.base + int64(len(s.out))
	truncated := off < s.base
	off = min(max(off, s.base), end)
	return string(s.out[off-s.base:]), end, truncated
}

func (s *server) getOutput(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	maxLines := intParam(q.Get("lines"), 200)
	var off int64 = -1
	if v := q.Get("since"); v != "" {
		off, _ = strconv.ParseInt(v, 10, 64)
	}
	if wait := durationParam(q.Get("wait"), 0, 5*time.Minute); wait > 0 && off >= 0 {
		// Long-poll: hold the request until there is something new.
		deadline := time.After(wait)
		for {
			s.mu.Lock()
			end, ch, exited := s.base+int64(len(s.out)), s.changed, s.exited
			s.mu.Unlock()
			if end > off || exited {
				break
			}
			select {
			case <-ch:
				continue
			case <-deadline:
			case <-r.Context().Done():
				return
			}
			break
		}
	}
	var text string
	var next int64
	var truncated bool
	if off < 0 {
		text, next, _ = s.outputSince(0)
	} else {
		text, next, truncated = s.outputSince(off)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Offset", strconv.FormatInt(next, 10))
	if q.Get("raw") == "1" {
		w.Write([]byte(text))
		return
	}
	if truncated {
		io.WriteString(w, "[some output was discarded; the session keeps the last 8 MiB]\n")
	}
	if out := tailLines(cleanLines(text), maxLines); out != "" {
		io.WriteString(w, out+"\n")
	}
	fmt.Fprintf(w, "[offset %d]\n", next)
}

// getWait blocks until a condition holds, then reports which, plus the
// output since the wait's starting point.
func (s *server) getWait(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	timeout := durationParam(q.Get("timeout"), 60*time.Second, 30*time.Minute)
	quiet := durationParam(q.Get("quiet"), 0, 30*time.Minute)
	idle := q.Get("idle") == "1"
	var re *regexp.Regexp
	if p := q.Get("pattern"); p != "" {
		var err error
		if re, err = regexp.Compile(p); err != nil {
			http.Error(w, "bad pattern: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	from := s.base + int64(len(s.out))
	since := time.Now()
	if !s.lastSend.IsZero() && time.Since(s.lastSend) < 5*time.Second {
		// Waiting right after typing: count from the keystrokes, so a
		// command that already finished still counts.
		since = s.lastSend
	}
	s.mu.Unlock()
	if v := q.Get("since"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	}
	reason := s.waitFor(r.Context(), from, since, re, quiet, idle, timeout)
	if reason == "" {
		return // client went away
	}
	text, next, _ := s.outputSince(from)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "[%s]\n", reason)
	if out := tailLines(cleanLines(text), intParam(q.Get("lines"), 200)); out != "" {
		io.WriteString(w, out+"\n")
	}
	fmt.Fprintf(w, "[offset %d]\n", next)
}

// waitFor blocks until output after from matches re, there has been no
// output for quiet, the command is back in the foreground (idle), the
// command exits, or timeout. It returns why it stopped.
func (s *server) waitFor(ctx context.Context, from int64, since time.Time, re *regexp.Regexp, quiet time.Duration, idle bool, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	scanned := from
	for {
		s.mu.Lock()
		ch := s.changed
		exited, code := s.exited, s.exitCode
		lastOut := s.lastOutput
		atPrompt, sawBusy := s.fgIsShell, s.sawBusy
		end := s.base + int64(len(s.out))
		s.mu.Unlock()
		if re != nil && end > scanned {
			text, _, _ := s.outputSince(scanned)
			for _, line := range cleanLines(text) {
				if re.MatchString(line) {
					return "matched: " + strings.TrimSpace(line)
				}
			}
			// Resume from the start of the last, possibly partial, line.
			if i := strings.LastIndexByte(text, '\n'); i >= 0 {
				scanned += int64(i + 1)
			}
		}
		if exited {
			return fmt.Sprintf("exited with status %d", code)
		}
		if lastOut.Before(since) {
			lastOut = since
		}
		quietFor := time.Since(lastOut)
		if quiet > 0 && quietFor >= quiet {
			return "quiet for " + quiet.String()
		}
		// Back at the prompt: either something else held the foreground
		// since the wait began, or a command quick enough to slip between
		// polls has printed and gone quiet.
		if idle && atPrompt && (sawBusy.After(since) || (end > from && quietFor >= 300*time.Millisecond)) {
			return "idle: back at the prompt"
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "timed out after " + timeout.String()
		}
		tick := min(remaining, 100*time.Millisecond)
		select {
		case <-ctx.Done():
			return ""
		case <-ch:
		case <-time.After(tick):
		}
	}
}

// postSendKeys types into the terminal. The body is tmux send-keys
// arguments: words naming keys (Enter, C-c, Up, ...) are pressed, other
// words are typed; quote text with spaces. A leading -l types everything
// literally.
func (s *server) postSendKeys(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := sendKeysBytes(string(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.typeInput(b); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_, next, _ := s.outputSince(0)
	fmt.Fprintf(w, "[sent %d bytes] [offset %d]\n", len(b), next)
}

// postRun types the body and Enter, then waits for the shell's prompt to
// come back (or timeout), and returns the output.
func (s *server) postRun(w http.ResponseWriter, r *http.Request) {
	cmdText, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, from, _ := s.outputSince(0)
	since := time.Now()
	if err := s.typeInput(append([]byte(strings.TrimRight(string(cmdText), "\n")), '\r')); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	timeout := durationParam(r.URL.Query().Get("timeout"), 60*time.Second, 30*time.Minute)
	reason := s.waitFor(r.Context(), from, since, nil, 0, true, timeout)
	if reason == "" {
		return
	}
	text, next, _ := s.outputSince(from)
	fmt.Fprintf(w, "[%s]\n", reason)
	if out := tailLines(cleanLines(text), intParam(r.URL.Query().Get("lines"), 200)); out != "" {
		io.WriteString(w, out+"\n")
	}
	fmt.Fprintf(w, "[offset %d]\n", next)
}

// sendKeysBytes turns tmux send-keys arguments into terminal input.
func sendKeysBytes(args string) ([]byte, error) {
	words, err := splitWords(args)
	if err != nil {
		return nil, err
	}
	literal := false
	if len(words) > 0 && words[0] == "-l" {
		literal, words = true, words[1:]
	}
	if len(words) == 0 {
		return nil, errors.New("nothing to send; the body is like tmux send-keys arguments: 'git status' Enter")
	}
	var b []byte
	for _, w := range words {
		if !literal && len([]rune(w)) > 1 {
			if seq, err := keyBytes(w); err == nil {
				b = append(b, seq...)
				continue
			}
		}
		b = append(b, w...)
	}
	return b, nil
}

// splitWords splits like a shell: whitespace separates words, '...' is
// literal, "..." and unquoted text take backslash escapes.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New(`unterminated " quote`)
			}
			inWord = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

var namedKeys = map[string]string{
	"Enter": "\r", "Tab": "\t", "BTab": "\x1b[Z", "Escape": "\x1b", "Esc": "\x1b", "BSpace": "\x7f", "Backspace": "\x7f", "Space": " ",
	"Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
	"Home": "\x1b[H", "End": "\x1b[F", "PageUp": "\x1b[5~", "PageDown": "\x1b[6~", "PPage": "\x1b[5~", "NPage": "\x1b[6~",
	"Delete": "\x1b[3~", "DC": "\x1b[3~", "Insert": "\x1b[2~", "IC": "\x1b[2~",
	"F1": "\x1bOP", "F2": "\x1bOQ", "F3": "\x1bOR", "F4": "\x1bOS",
	"F5": "\x1b[15~", "F6": "\x1b[17~", "F7": "\x1b[18~", "F8": "\x1b[19~",
	"F9": "\x1b[20~", "F10": "\x1b[21~", "F11": "\x1b[23~", "F12": "\x1b[24~",
}

// keyBytes encodes a key name as the bytes a terminal sends: names from
// namedKeys (tmux's names work too), C-x for control characters, M-x for
// meta (ESC prefix).
func keyBytes(k string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(k, "M-"); ok {
		b, err := keyBytes(rest)
		return append([]byte{0x1b}, b...), err
	}
	if rest, ok := strings.CutPrefix(k, "C-"); ok && len(rest) == 1 {
		ch := rest[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return []byte{ch - 'a' + 1}, nil
		case ch >= 'A' && ch <= 'Z':
			return []byte{ch - 'A' + 1}, nil
		case ch >= '@' && ch <= '_':
			return []byte{ch - '@'}, nil
		case ch == '?':
			return []byte{0x7f}, nil
		}
	}
	if s, ok := namedKeys[k]; ok {
		return []byte(s), nil
	}
	if len([]rune(k)) == 1 {
		return []byte(k), nil
	}
	return nil, fmt.Errorf("unknown key %q (try Enter, Tab, Escape, Up, C-c, M-x)", k)
}

func intParam(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return def
}

// durationParam parses seconds ("30", "1.5") or a Go duration ("2m").
func durationParam(v string, def, limit time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return def
		}
		d = time.Duration(f * float64(time.Second))
	}
	return min(limit, d)
}
