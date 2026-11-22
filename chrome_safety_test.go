package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// These tests check that sharing the person's Chrome takes exactly one
// connection (each one makes Chrome ask "Allow remote debugging?") and
// that nothing shoulder does, however it ends, closes their tabs or
// their Chrome.

// countingProxy stands in for Chrome's DevTools port, forwarding to it
// and counting the connections that say something (a liveness check
// that only connects isn't one). It returns a user data dir whose
// DevToolsActivePort points at it.
func countingProxy(t *testing.T, chromeDir string) (dir string, count func() int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chromeDir, "DevToolsActivePort"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(b))
	upstream := net.JoinHostPort("127.0.0.1", lines[0])
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	dir = t.TempDir()
	port := ln.Addr().(*net.TCPAddr).Port
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(strings.Join([]string{itoa(port), lines[1]}, "\n")), 0o600)
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, err := br.Peek(1); err != nil {
					return
				}
				mu.Lock()
				n++
				mu.Unlock()
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer up.Close()
				go io.Copy(up, br)
				io.Copy(conn, up)
			}()
		}
	}()
	return dir, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func itoa(n int) string { return strconv.Itoa(n) }

// protocolLog records what shoulder says to Chrome and what it answers.
type protocolLog struct {
	mu   sync.Mutex
	sent []map[string]any
	recv []map[string]any
}

func traceProtocol(t *testing.T) *protocolLog {
	l := &protocolLog{}
	chromeTrace = func(msg string) {
		var m map[string]any
		dir, body, _ := strings.Cut(msg, " ")
		if json.Unmarshal([]byte(body), &m) != nil {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if dir == "->" {
			l.sent = append(l.sent, m)
		} else {
			l.recv = append(l.recv, m)
		}
	}
	t.Cleanup(func() { chromeTrace = nil })
	return l
}

// check fails the test if shoulder sent anything that closes a browser,
// a page or a profile, or closed a target it didn't create itself.
func (l *protocolLog) check(t *testing.T) (closed []string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	creates := map[float64]bool{}
	for _, m := range l.sent {
		if m["method"] == "Target.createTarget" {
			creates[m["id"].(float64)] = true
		}
	}
	created := map[string]bool{}
	for _, m := range l.recv {
		if id, ok := m["id"].(float64); ok && creates[id] {
			if r, ok := m["result"].(map[string]any); ok {
				created[r["targetId"].(string)] = true
			}
		}
	}
	for _, m := range l.sent {
		method, _ := m["method"].(string)
		switch method {
		case "Browser.close", "Browser.crash", "Browser.crashGpuProcess", "Page.close", "Page.crash", "Target.disposeBrowserContext":
			t.Errorf("shoulder sent %s", method)
		case "Target.closeTarget":
			id, _ := m["params"].(map[string]any)["targetId"].(string)
			if !created[id] {
				t.Errorf("shoulder closed target %s, which it didn't open", id)
			}
			closed = append(closed, id)
		}
	}
	return closed
}

// said reports whether shoulder sent a message containing s.
func (l *protocolLog) said(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.sent {
		if b, _ := json.Marshal(m); strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}

func (l *protocolLog) count(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.sent {
		if m["method"] == method {
			n++
		}
	}
	return n
}

// pageIDs lists the browser's pages, and whether anyone is attached.
func pageIDs(x *testCDP) map[target.ID]bool {
	out := map[target.ID]bool{}
	for _, p := range x.targets("page") {
		out[p.TargetID] = p.Attached
	}
	return out
}

// A whole share, through to the session ending: one connection, and
// every tab the person had (and every tab the agent left open) is still
// there afterwards.
func TestChromeHandsOff(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	x := newTestCDP(t, dir)
	x.open(target.CreateTargetParams{URL: site.URL + "/other", NewWindow: ptrTo(true)})
	proxy, connections := countingProxy(t, dir)
	log := traceProtocol(t)

	cfg, c, res := browserSession(t, proxy, url.Values{"tabs": {"all"}})
	if len(res.Tabs) != 2 {
		t.Fatalf("start: %+v", res)
	}
	rw, err := c.share("unix", false, true)
	if err != nil {
		t.Fatal(err)
	}
	a := agentFromPaste(t, cfg, rw.Paste)
	a.call("POST", "focus?tab=t1", "")
	for _, call := range []struct{ method, path, body string }{
		{"GET", "page", ""},
		{"GET", "screenshot", ""},
		{"POST", "js", "document.title"},
		{"POST", "click", "#pop"}, // a popup, which joins the share
		{"POST", "open", site.URL + "/done?q=agent-closes"},
		{"POST", "close", ""}, // the tab the agent opened
		{"POST", "open", site.URL + "/done?q=agent-leaves"},
	} {
		if code, out := a.call(call.method, call.path, call.body); code != 200 {
			t.Fatalf("%s %s: %d %s", call.method, call.path, code, out)
		}
	}
	waitUntil(t, "the popup", func() bool { return strings.Count(a.get("state"), `"id"`) == 4 })
	before := pageIDs(x)
	if len(before) != 4 { // the person's two, the popup, and the agent's
		t.Errorf("pages: %v", before)
	}

	// The person stops sharing.
	c.post("/ctl/quit")
	waitUntil(t, "the session to let go", func() bool {
		b, err := c.do("GET", "/ctl/activity")
		return err != nil || strings.Contains(string(b), `"ended": true`)
	})

	if n := connections(); n != 1 {
		t.Errorf("%d connections to Chrome; want 1 (each is an Allow click)", n)
	}
	if closed := log.check(t); len(closed) < 1 || log.count("Target.detachFromTarget") == 0 {
		t.Errorf("closed %v, detached %d times", closed, log.count("Target.detachFromTarget"))
	}
	after := pageIDs(x)
	for u := range before {
		attached, ok := after[u]
		if !ok {
			t.Errorf("%s closed", u)
		}
		if attached {
			t.Errorf("%s still attached", u)
		}
	}
	if len(after) != len(before) {
		t.Errorf("pages before %v, after %v", before, after)
	}
}

// Letting go of a tab (the tab's chromedp context ending) and closing
// the connection detach; neither closes the tab.
func TestChromeLetGo(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	x := newTestCDP(t, dir)
	x.open(target.CreateTargetParams{URL: site.URL + "/other"})
	log := traceProtocol(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connectChrome(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.chooseProfile(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.shareAll(ctx); err != nil {
		t.Fatal(err)
	}
	tabs := c.tabs()
	if len(tabs) != 2 {
		t.Fatalf("tabs: %+v", tabs)
	}
	attached := func() int {
		n := 0
		for _, p := range x.targets("page") {
			if p.Attached {
				n++
			}
		}
		return n
	}
	if n := attached(); n != 2 {
		t.Errorf("%d pages attached; want 2", n)
	}
	// The tab's context, cancelled as chromedp's docs say to.
	c.mu.Lock()
	first := c.shared[0]
	c.mu.Unlock()
	chromedp.Cancel(first.ctx)
	waitUntil(t, "the first tab to be let go", func() bool { return attached() == 1 })
	if n := len(x.targets("page")); n != 2 {
		t.Errorf("%d pages after letting go of one; want 2", n)
	}
	c.close()
	<-c.done()
	waitUntil(t, "the rest to be let go", func() bool { return attached() == 0 })
	if n := len(x.targets("page")); n != 2 {
		t.Errorf("%d pages after closing; want 2", n)
	}
	log.check(t)
	// The only target closed was shoulder's chrome://version page.
	if n := log.count("Target.closeTarget"); n != 1 {
		t.Errorf("%d targets closed; want 1 (the chrome://version page)", n)
	}
}

// If shoulder dies (here: SIGKILL, mid-share), Chrome drops the
// connection and keeps the tabs.
func TestChromeCrash(t *testing.T) {
	if os.Getenv("SHOULDER_CRASH_DIR") != "" {
		t.Skip("helper")
	}
	site := testSite(t)
	dir := testChrome(t, site.URL)
	x := newTestCDP(t, dir)
	x.open(target.CreateTargetParams{URL: site.URL + "/other"})
	cmd := exec.Command(os.Args[0], "-test.run=^TestChromeCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "SHOULDER_CRASH_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	sc := bufio.NewScanner(out)
	shared := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "SHARED") {
			shared = true
			break
		}
	}
	if !shared {
		t.Fatal("the helper didn't share")
	}
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	waitUntil(t, "Chrome to notice", func() bool {
		for _, p := range x.targets("page") {
			if p.Attached {
				return false
			}
		}
		return true
	})
	if n := len(x.targets("page")); n != 2 {
		t.Errorf("%d pages after shoulder died; want 2", n)
	}
}

// TestChromeCrashHelper is TestChromeCrash's shoulder, in a process of
// its own.
func TestChromeCrashHelper(t *testing.T) {
	dir := os.Getenv("SHOULDER_CRASH_DIR")
	if dir == "" {
		t.Skip("run by TestChromeCrash")
	}
	ctx := context.Background()
	c, err := connectChrome(ctx, dir)
	if err == nil {
		err = c.chooseProfile(ctx, "")
	}
	if err == nil {
		err = c.shareAll(ctx)
	}
	if err != nil || len(c.tabs()) != 2 {
		t.Fatal(err, c.tabs())
	}
	os.Stdout.WriteString("SHARED\n")
	time.Sleep(time.Minute)
}

// A connection attempt that's given up on (the person never clicks
// Allow) ends, and isn't retried: each try would ask again.
func TestChromeNoRetry(t *testing.T) {
	// Something that accepts connections and never answers, like Chrome
	// waiting for the person.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if _, err := br.Peek(1); err != nil {
					return
				}
				mu.Lock()
				n++
				mu.Unlock()
				io.Copy(io.Discard, br)
			}()
		}
	}()
	dir := t.TempDir()
	port := ln.Addr().(*net.TCPAddr).Port
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(itoa(port)+"\n/devtools/browser/x\n"), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = connectChrome(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "Allow") {
		t.Errorf("connect: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("gave up after %v", d)
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Errorf("%d handshakes; want 1", n)
	}
}
