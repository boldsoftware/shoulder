package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Transports: how an agent reaches the session.
var transports = []struct {
	name, key, label string
}{
	{"unix", "u", "unix socket"},
	{"localhost", "h", "localhost"},
	{"lan", "*", "all interfaces"},
	{"tailscale", "s", "tailscale"},
	{"tailcat", "t", "tailcat"},
}

// share is one way into the agent API. Each share has its own codes: a
// read-only one from the start, and a read-write one only once read-write
// access is asked for on it. A share only ever shared read-only has no code
// that can type, and a code never works on a share it wasn't issued for.
type share struct {
	mu     sync.Mutex
	ro     string      // code that can watch
	rw     string      // code that can also type; empty until asked for
	fails  []time.Time // recent wrong codes
	locked time.Time   // wrong codes are refused until then

	transport string
	curl      string // command prefix that reaches base, e.g. "curl -s --unix-socket /x"
	base      string // URL the paths hang off, e.g. "http://127.0.0.1:7357"
	warning   string // a risk the user should know about
	note      string // how the agent's side works
	ln        net.Listener
	srv       *http.Server
	tc        *tailcat.Server // when tailcat carries the share
}

func (sh *share) url(token string) string { return sh.base + "/" + token }

// access says what a code is good for on this share. A wrong code counts
// against the share's rate limit.
func (sh *share) access(code string) (readOnly, ok bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	switch {
	case sh.rw != "" && code == sh.rw:
		return false, true
	case code == sh.ro:
		return true, true
	}
	sh.failedLocked()
	return false, false
}

// readWrite reports whether the share has a read-write code.
func (sh *share) readWrite() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.rw != ""
}

// Codes are short enough to guess, so a network share that sees too many
// wrong ones stops answering for a while: 10 guesses per 10 minutes makes
// finding one of ~6.5M codes take years. The session's own Unix socket is
// protected by file permissions instead.
const (
	maxFails   = 10
	failWindow = 10 * time.Minute
)

func (sh *share) failedLocked() {
	if sh.transport == "unix" {
		return
	}
	now := time.Now()
	recent := sh.fails[:0]
	for _, t := range sh.fails {
		if now.Sub(t) < failWindow {
			recent = append(recent, t)
		}
	}
	sh.fails = append(recent, now)
	if len(sh.fails) >= maxFails {
		sh.locked = now.Add(failWindow)
		sh.fails = nil
		log.Printf("%s share: %d wrong codes; locked until %s", sh.transport, maxFails, sh.locked.Format(time.TimeOnly))
	}
}

// lockedUntil is when a lock ends, or zero if the share isn't locked.
func (sh *share) lockedUntil() time.Time {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if time.Now().Before(sh.locked) {
		return sh.locked
	}
	return time.Time{}
}

func (sh *share) close() {
	if sh.srv != nil {
		sh.srv.Close()
	}
	if sh.tc != nil {
		sh.tc.Close()
	}
}

// openShare sets up a share: its listener, its read-only code, and then,
// with the code in place, its server.
func openShare(ctx context.Context, s *server, transport string) (*share, error) {
	var sh *share
	var err error
	switch transport {
	case "unix":
		// The session's own socket, which serve listens on; always open.
		sh = &share{transport: "unix", curl: "curl -s --unix-socket " + shellPath(s.ctlSock), base: "http://shoulder"}
	case "localhost":
		sh, err = listenShare(s, transport, "127.0.0.1", "127.0.0.1", "")
	case "lan":
		host := lanAddress()
		sh, err = listenShare(s, transport, "", host,
			"Listening on every interface over plain HTTP: anyone who can reach this machine and has the URL can use the session.")
	case "tailscale":
		var ip, name string
		if ip, name, err = tailscaleAddress(ctx); err != nil {
			return nil, err
		}
		sh, err = listenShare(s, transport, ip, cmp.Or(name, ip), "")
	case "tailcat":
		sh, err = tailcatShare(ctx, s)
	default:
		return nil, fmt.Errorf("unknown transport %q", transport)
	}
	if err != nil {
		return nil, err
	}
	sh.ro = newCode()
	if sh.ln != nil {
		sh.srv = &http.Server{Handler: s.handler(sh)}
		go sh.srv.Serve(sh.ln)
	}
	return sh, nil
}

// listenShare listens for the agent API on bind:port, reachable as host:port.
func listenShare(s *server, transport, bind, host, warning string) (*share, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(s.cfg.Port)))
	if err != nil && s.cfg.Port != 0 {
		ln, err = net.Listen("tcp", net.JoinHostPort(bind, "0")) // the port is taken; take any
	}
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return &share{transport: transport, curl: "curl -s", base: "http://" + net.JoinHostPort(host, strconv.Itoa(port)), warning: warning, ln: ln}, nil
}

// lanAddress is this machine's address on its primary network.
func lanAddress() string {
	conn, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET; nothing is sent
	if err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	h, _ := os.Hostname()
	return h
}

func tailscaleBinary() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	if _, err := os.Stat(app); err == nil {
		return app, nil
	}
	return "", errors.New("tailscale isn't installed (or not on $PATH)")
}

// tailscaleAddress is this machine's tailnet IPv4 address and MagicDNS name.
func tailscaleAddress(ctx context.Context) (ip, name string, err error) {
	bin, err := tailscaleBinary()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--self", "--peers=false", "--json").Output()
	if err != nil {
		return "", "", fmt.Errorf("tailscale status: %w", err)
	}
	var st struct {
		BackendState string
		Self         struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", "", err
	}
	if st.BackendState != "Running" {
		return "", "", fmt.Errorf("tailscale is %s, not running", st.BackendState)
	}
	for _, a := range st.Self.TailscaleIPs {
		if !strings.Contains(a, ":") {
			ip = a
		}
	}
	if ip == "" {
		return "", "", errors.New("tailscale has no IPv4 address")
	}
	return ip, strings.TrimSuffix(st.Self.DNSName, "."), nil
}

// tailcatShare puts the API on an ephemeral tailcat node, run in this
// process: an encrypted WireGuard tunnel that reaches across NATs, with no
// account and nothing listening on this machine's network. Agents reach
// it with the stock tailcat client: `tailcat socks curl`.
func tailcatShare(ctx context.Context, s *server) (*share, error) {
	logf := logger.Discard
	if f, err := os.OpenFile(s.cfg.path("tailcat.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		logf = log.New(f, "", log.LstdFlags).Printf
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Pick the nearest public relay region ourselves, as the tailcat CLI
	// does, so the address can name it by ID instead of embedding it.
	pick := &tailcat.ConnInfo{RegionID: -1}
	if err := pick.Expand(ctx, tailcat.ExpandForServer); err != nil {
		return nil, fmt.Errorf("tailcat: choosing a relay: %w", err)
	}
	region := pick.Region[0]
	priv := key.NewNode()
	tc := &tailcat.Server{Key: priv, DisablePresharedKey: true, Region: region, Logf: logf}
	ln, err := tc.Listen(ctx, "tcp", ":80")
	if err != nil {
		tc.Close()
		return nil, fmt.Errorf("tailcat: %w", err)
	}
	// The shortest address current clients accept: no pre-shared key (the
	// access code guards the session; WireGuard still encrypts), and the
	// relay named by region ID.
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: priv.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(priv),
		RegionID:          region.RegionID,
	}
	return &share{transport: "tailcat", curl: "tailcat socks curl -s", base: "http://" + string(ci.Addr()), ln: ln, tc: tc,
		note: "Nothing to install here; the agent needs tailcat: go install github.com/tailscale/tailcat/cmd/tailcat@latest"}, nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellPath is path for a shell command line, with ~ for the home
// directory when that needs no quoting.
func shellPath(path string) string {
	if t := tildePath(path); strings.HasPrefix(t, "~/") && shellQuote(t[2:]) == t[2:] {
		return t
	}
	return shellQuote(path)
}
