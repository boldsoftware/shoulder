package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// testSession runs a session server with bash in it and returns its
// control client.
func testSession(t *testing.T) (*config, *ctl) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	// Unix socket paths are short; t.TempDir can be too long on macOS.
	dir, err := os.MkdirTemp("/tmp", "shoulder-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := &config{Name: "test-session", Dir: dir, Command: []string{bash, "--norc", "--noprofile", "-i"},
		Cwd: dir, Host: "testhost", Linger: time.Minute}
	b, _ := json.Marshal(cfg)
	os.WriteFile(cfg.path("config.json"), b, 0o600)
	os.Setenv("PS1", "$ ")
	done := make(chan error, 1)
	go func() { done <- serve(dir) }()
	c := newCtl(cfg.path("sock"))
	t.Cleanup(func() {
		c.post("/ctl/quit")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	waitUntil(t, "socket", func() bool { _, err := c.do("GET", "/ctl/info"); return err == nil })
	return cfg, c
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// agentClient makes the requests a pasted curl line would.
type agentClient struct {
	t    *testing.T
	http *http.Client
	base string
}

var urlRE = regexp.MustCompile(`(http://\S+)/$`)

func agentFromPaste(t *testing.T, cfg *config, paste string) *agentClient {
	t.Helper()
	m := urlRE.FindStringSubmatch(paste)
	if m == nil {
		t.Fatalf("no URL in paste:\n%s", paste)
	}
	a := &agentClient{t: t, base: m[1], http: http.DefaultClient}
	if strings.Contains(paste, "--unix-socket") {
		a.http = newCtl(cfg.path("sock")).c
	}
	return a
}

func (a *agentClient) call(method, endpoint, body string) (int, string) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+"/"+endpoint, strings.NewReader(body))
	resp, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// shareList is the session's shares with their access, as /ctl/info
// reports them.
func shareList(t *testing.T, c *ctl) []string {
	t.Helper()
	v, err := c.info()
	if err != nil {
		t.Fatal(err)
	}
	return v.Shares
}

func (a *agentClient) get(endpoint string) string {
	a.t.Helper()
	code, body := a.call("GET", endpoint, "")
	if code != 200 {
		a.t.Fatalf("GET %s: %d %s", endpoint, code, body)
	}
	return body
}

func TestSessionAPI(t *testing.T) {
	cfg, c := testSession(t)
	rw, err := c.share("unix", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(rw.Paste, "\n") > 0 || !codeRE.MatchString(rw.Paste) {
		t.Errorf("paste should be one line ending in the code:\n%s", rw.Paste)
	}
	if _, err := c.post("/ctl/start?cols=80&rows=24"); err != nil {
		t.Fatal(err)
	}
	a := agentFromPaste(t, cfg, rw.Paste)
	if g := a.get(""); !strings.Contains(g, "# shoulder") || !strings.Contains(g, "POST send-keys") || !strings.Contains(g, "read-write") {
		t.Errorf("guide:\n%s", g)
	}
	waitUntil(t, "prompt", func() bool { return strings.Contains(a.get("capture-pane"), "$") })

	// run types a command and returns its output once the prompt is back.
	code, out := a.call("POST", "run?timeout=20", "echo hello; sleep 0.5; echo done-$((6*7))")
	if code != 200 || !strings.HasPrefix(out, "[idle") || !strings.Contains(out, "\nhello\n") || !strings.Contains(out, "done-42") {
		t.Fatalf("run: %d\n%s", code, out)
	}
	if s := a.get("capture-pane"); !strings.Contains(s, "done-42") {
		t.Errorf("screen:\n%s", s)
	}

	// send + wait for a pattern.
	if code, out := a.call("POST", "send-keys", "'sleep 0.3; echo MARK-$((1+1))' Enter"); code != 200 {
		t.Fatalf("send: %d %s", code, out)
	}
	if out := a.get("wait?pattern=" + url.QueryEscape(`^MARK-\d`) + "&timeout=10"); !strings.HasPrefix(out, "[matched: MARK-2]") {
		t.Errorf("wait pattern:\n%s", out)
	}

	// output since an offset only has what came after.
	out = a.get("output")
	off := regexp.MustCompile(`\[offset (\d+)\]`).FindStringSubmatch(out)
	if off == nil {
		t.Fatalf("no offset in output:\n%s", out)
	}
	a.call("POST", "run?timeout=10", "echo after")
	if out := a.get("output?since=" + off[1]); strings.Contains(out, "hello") || !strings.Contains(out, "after") {
		t.Errorf("output since %s:\n%s", off[1], out)
	}

	var st status
	json.Unmarshal([]byte(a.get("status")), &st)
	if !st.Running || !st.AtPrompt || st.Cols != 80 || st.ReadOnly {
		t.Errorf("status: %+v", st)
	}

	// A read-only token can look but not type.
	ro, err := c.share("unix", true, false)
	if err != nil {
		t.Fatal(err)
	}
	r := agentFromPaste(t, cfg, ro.Paste)
	if g := r.get(""); strings.Contains(g, "send-keys") || !strings.Contains(g, "read-only") {
		t.Errorf("read-only guide offers typing:\n%s", g)
	}
	if code, _ := r.call("POST", "send-keys", "'echo nope' Enter"); code != http.StatusForbidden {
		t.Errorf("read-only send: %d", code)
	}
	if s := r.get("capture-pane"); !strings.Contains(s, "after") {
		t.Errorf("read-only screen:\n%s", s)
	}
	if code, _ := a.call("GET", "../nope/screen", ""); code == 200 {
		t.Error("bad token accepted")
	}

	// localhost serves the same session over TCP, with its own codes: a
	// share asked for read-only has no read-write code, and the unix
	// socket's read-write code doesn't work on it.
	lh, err := c.share("localhost", true, false)
	if err != nil {
		t.Fatal(err)
	}
	l := agentFromPaste(t, cfg, lh.Paste)
	if !strings.HasPrefix(l.base, "http://127.0.0.1:") {
		t.Errorf("localhost base %s", l.base)
	}
	if s := l.get("capture-pane"); !strings.Contains(s, "after") {
		t.Errorf("localhost screen:\n%s", s)
	}
	if code, _ := l.call("POST", "send-keys", "'echo nope' Enter"); code != http.StatusForbidden {
		t.Errorf("localhost read-only send: %d", code)
	}
	lroot := strings.TrimSuffix(l.base, path.Base(l.base))
	foreign := &agentClient{t: t, http: l.http, base: lroot + path.Base(a.base)}
	if code, _ := foreign.call("GET", "status", ""); code != http.StatusForbidden {
		t.Errorf("unix read-write code on the localhost share: %d", code)
	}
	if shares := shareList(t, c); !slices.Contains(shares, "unix:rw") || !slices.Contains(shares, "localhost:ro") {
		t.Errorf("shares: %v", shares)
	}
	// Asking for read-write access gives the share a read-write code;
	// switching it back to read-only from the launcher revokes it.
	lrw, err := c.share("localhost", false, false)
	if err != nil {
		t.Fatal(err)
	}
	lw := agentFromPaste(t, cfg, lrw.Paste)
	if code, out := lw.call("POST", "run?timeout=10", "echo localhost-$((20+2))"); code != 200 || !strings.Contains(out, "localhost-22") {
		t.Errorf("localhost run: %d\n%s", code, out)
	}
	if shares := shareList(t, c); !slices.Contains(shares, "localhost:rw") {
		t.Errorf("shares: %v", shares)
	}
	if _, err := c.share("localhost", true, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := lw.call("GET", "status", ""); code != http.StatusForbidden {
		t.Errorf("revoked localhost read-write code: %d", code)
	}
	if s := l.get("capture-pane"); !strings.Contains(s, "localhost-22") {
		t.Errorf("localhost screen after revoking read-write:\n%s", s)
	}
	// A wrong code is refused, and the right one keeps working.
	bad := &agentClient{t: t, http: l.http, base: lroot + "NOTACODE"}
	if code, _ := bad.call("GET", "status", ""); code != http.StatusForbidden {
		t.Errorf("wrong code: %d", code)
	}
	if code, _ := l.call("GET", "status", ""); code != 200 {
		t.Errorf("right code after a wrong one: %d", code)
	}

	// A terminal attaching gets the screen repainted.
	conn, err := net.Dial("unix", cfg.path("sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /ctl/attach?cols=80&rows=24 HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: shoulder\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach: %v %v", resp, err)
	}
	typ, p, err := readFrame(br)
	if err != nil || typ != frameData || !strings.Contains(string(p), "done-42") {
		t.Fatalf("repaint: %d %q %v", typ, p, err)
	}
	// Typing in the terminal reaches the command, and its exit reaches the terminal.
	writeFrame(conn, frameData, []byte("exit 3\r"))
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, p, err := readFrame(br)
		if err != nil {
			t.Fatalf("waiting for exit: %v", err)
		}
		if typ == frameExit {
			if len(p) != 4 || p[0] != 3 {
				t.Errorf("exit frame %v", p)
			}
			break
		}
	}
	if out := a.get("wait?timeout=5"); !strings.HasPrefix(out, "[exited with status 3]") {
		t.Errorf("wait after exit:\n%s", out)
	}
	json.Unmarshal([]byte(a.get("status")), &st)
	if st.Running || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Errorf("status after exit: %+v", st)
	}
}

func TestKeyBytes(t *testing.T) {
	for k, want := range map[string]string{
		"Enter": "\r", "C-c": "\x03", "C-C": "\x03", "C-[": "\x1b", "Up": "\x1b[A", "Esc": "\x1b",
		"M-x": "\x1bx", "q": "q", "F5": "\x1b[15~", "PageUp": "\x1b[5~",
	} {
		got, err := keyBytes(k)
		if err != nil || string(got) != want {
			t.Errorf("keyBytes(%q) = %q, %v; want %q", k, got, err, want)
		}
	}
	if _, err := keyBytes("Bogus"); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestCleanLines(t *testing.T) {
	got := cleanLines("a\r\n\x1b[31mred\x1b[0m\nprogress 10%\rprogress 99%\nbell\a!\n")
	if want := []string{"a", "red", "progress 99%", "bell!"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cleanLines = %q, want %q", got, want)
	}
}

func TestCarriageReturnOverwrites(t *testing.T) {
	for in, want := range map[string]string{
		"progress 10%\rprogress 99%": "progress 99%",
		"14:09 prompt line\r":        "14:09 prompt line",
		"abcdef\rXY":                 "XYcdef",
	} {
		if got := cleanLines(in); len(got) != 1 || got[0] != want {
			t.Errorf("cleanLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendKeysBytes(t *testing.T) {
	for in, want := range map[string]string{
		"'git status' Enter":     "git status\r",
		"C-c":                    "\x03",
		"Escape :wq Enter":       "\x1b:wq\r",
		`"say \"hi\"" Enter`:     `say "hi"` + "\r",
		"-l Enter":               "Enter",
		"y":                      "y",
		"ls -la Enter":           "ls-la\r", // like tmux: separate words, no spaces between
		"'ls -la' Enter Up Down": "ls -la\r\x1b[A\x1b[B",
	} {
		got, err := sendKeysBytes(in)
		if err != nil || string(got) != want {
			t.Errorf("sendKeysBytes(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "'unterminated", `"also`} {
		if _, err := sendKeysBytes(bad); err == nil {
			t.Errorf("sendKeysBytes(%q) accepted", bad)
		}
	}
}

// codeRE matches a code at the end of a paste: 26 or more base32
// characters, which is how crypto/rand.Text spells at least 128 bits.
var codeRE = regexp.MustCompile(`/[A-Z2-7]{26,}/$`)

func TestCode(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		c := newCode()
		if !codeRE.MatchString("/" + c + "/") {
			t.Errorf("code %q", c)
		}
		if seen[c] {
			t.Errorf("code %q repeated", c)
		}
		seen[c] = true
	}
}

func TestVtermRepaint(t *testing.T) {
	var replies []string
	v := newTerm(20, 5, func(b []byte) { replies = append(replies, string(b)) })
	v.write([]byte("hello \x1b[31mred\x1b[0m\r\n\x1b[?1049h\x1b[?1h\x1b[?2004h\x1b[Hfull screen\x1b[3;5H"))
	if got := v.text(); !strings.HasPrefix(got, "full screen\n") || strings.Contains(got, "hello") {
		t.Errorf("alt screen text: %q", got)
	}
	out := string(v.ansi())
	for _, want := range []string{"\x1b[?1049h", "full screen", "\x1b[?1h", "\x1b[?2004h", "\x1b[3;5H", "\x1b[?25h"} {
		if !strings.Contains(out, want) {
			t.Errorf("repaint lacks %q:\n%q", want, out)
		}
	}
	v.write([]byte("\x1b[?1049l\x1b[?25l"))
	if got := v.text(); !strings.HasPrefix(got, "hello red\n") {
		t.Errorf("primary screen text: %q", got)
	}
	if out := string(v.ansi()); strings.Contains(out, "\x1b[?1049h") || !strings.Contains(out, "\x1b[31m") || !strings.Contains(out, "\x1b[?25l") {
		t.Errorf("primary repaint:\n%q", out)
	}
}

// A program asking the terminal where the cursor is gets an answer even
// with no terminal attached, from the emulator.
func TestQueriesAnsweredWhileDetached(t *testing.T) {
	cfg, c := testSession(t)
	rw, err := c.share("unix", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.post("/ctl/start?cols=80&rows=24"); err != nil {
		t.Fatal(err)
	}
	a := agentFromPaste(t, cfg, rw.Paste)
	waitUntil(t, "prompt", func() bool { return strings.Contains(a.get("capture-pane"), "$") })
	code, out := a.call("POST", "run?timeout=10", `printf '\033[6n'; IFS= read -rs -d R pos; echo "cursor-reply:${pos#*[}"`)
	if code != 200 || !regexp.MustCompile(`cursor-reply:\d+;\d+`).MatchString(out) {
		t.Errorf("query unanswered: %d\n%s", code, out)
	}
}
