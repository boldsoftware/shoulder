package main

import (
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

// share is one way into the agent API.
type share struct {
	mu     sync.Mutex
	fails  []time.Time // recent wrong codes
	locked time.Time   // wrong codes are refused until then

	transport string
	curl      string // command prefix that reaches base, e.g. "curl -s --unix-socket /x"
	base      string // URL the paths hang off, e.g. "http://127.0.0.1:7357"
	warning   string
	ln        net.Listener
	srv       *http.Server
	proc      *exec.Cmd // tailcat, when it carries the share
}

func (sh *share) url(token string) string { return sh.base + "/" + token }

// Codes are short enough to guess, so a network share that sees too many
// wrong ones stops answering for a while: 10 guesses per 10 minutes makes
// finding one of ~6.5M codes take years. The session's own Unix socket is
// protected by file permissions instead.
const (
	maxFails   = 10
	failWindow = 10 * time.Minute
)

func (sh *share) failed() {
	if sh.transport == "unix" {
		return
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
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
	if sh.proc != nil && sh.proc.Process != nil {
		sh.proc.Process.Kill()
		sh.proc.Wait()
	}
}

func openShare(ctx context.Context, s *server, transport string) (*share, error) {
	switch transport {
	case "unix":
		// The session's own socket; always open.
		return &share{transport: "unix", curl: "curl -s --unix-socket " + shellPath(s.ctlSock), base: "http://shoulder"}, nil
	case "localhost":
		return listenShare(s, transport, "127.0.0.1", "127.0.0.1", "")
	case "lan":
		host := lanAddress()
		return listenShare(s, transport, "", host,
			"Listening on every interface over plain HTTP: anyone who can reach this machine and has the URL can use the session.")
	case "tailscale":
		ip, name, err := tailscaleAddress(ctx)
		if err != nil {
			return nil, err
		}
		return listenShare(s, transport, ip, cmpOr(name, ip), "")
	case "tailcat":
		return tailcatShare(ctx, s)
	}
	return nil, fmt.Errorf("unknown transport %q", transport)
}

// listenShare serves the agent API on bind:port, reachable as host:port.
func listenShare(s *server, transport, bind, host, warning string) (*share, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(s.cfg.Port)))
	if err != nil && s.cfg.Port != 0 {
		ln, err = net.Listen("tcp", net.JoinHostPort(bind, "0")) // the port is taken; take any
	}
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	sh := &share{transport: transport, curl: "curl -s", base: "http://" + net.JoinHostPort(host, strconv.Itoa(port)), warning: warning, ln: ln}
	sh.srv = &http.Server{Handler: s.handler(sh)}
	go sh.srv.Serve(ln)
	return sh, nil
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

// tailcatShare puts the API on an ephemeral tailcat node. Agents reach it
// with `tailcat socks curl http://<address>:<port>/...`.
func tailcatShare(ctx context.Context, s *server) (*share, error) {
	bin, err := exec.LookPath("tailcat")
	if err != nil {
		return nil, errors.New("tailcat isn't installed: go install github.com/tailscale/tailcat/cmd/tailcat@latest")
	}
	sh, err := listenShare(s, "tailcat", "127.0.0.1", "127.0.0.1", "")
	if err != nil {
		return nil, err
	}
	port := sh.ln.Addr().(*net.TCPAddr).Port
	addrFile := s.cfg.path("tailcat.addr")
	os.Remove(addrFile)
	cmd := exec.Command(bin, "--serve="+strconv.Itoa(port), "--key=new")
	cmd.Env = append(os.Environ(), "TAILCAT_ADDR_FILE="+addrFile)
	if logf, err := os.OpenFile(s.cfg.path("tailcat.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		sh.close()
		return nil, err
	}
	sh.proc = cmd
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(addrFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			addr := strings.TrimSpace(string(b))
			sh.curl = "tailcat socks curl -s"
			sh.base = "http://" + addr + ":" + strconv.Itoa(port)
			sh.warning = "The agent's machine needs tailcat: go install github.com/tailscale/tailcat/cmd/tailcat@latest"
			return sh, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			sh.close()
			return nil, errors.New("tailcat didn't come up; see " + s.cfg.path("tailcat.log"))
		}
		time.Sleep(100 * time.Millisecond)
	}
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

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
