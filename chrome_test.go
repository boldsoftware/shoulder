package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// The Chrome tests run a headless Chrome of their own, on a throwaway
// profile, listening as the person's Chrome does once remote debugging is
// allowed (minus the Allow prompt). They're skipped without Chrome.

const testPage = `<!doctype html>
<title>Shoulder test</title>
<h1>Hello there</h1>
<p id=out>nothing yet</p>
<button id=press onclick="window.n = (window.n || 0) + 1; document.getElementById('out').textContent = 'pressed ' + window.n">Press me</button>
<form action=/done method=get><input name=q aria-label=Query><button>Go</button></form>
<textarea id=notes aria-label=Notes></textarea>
<select id=color aria-label=Color><option>red</option><option>green</option></select>
<a href=/other>Other page</a>
<button id=ask onclick="document.getElementById('out').textContent = confirm('Sure?') ? 'confirmed' : 'declined'">Ask</button>
<button id=pop onclick="window.open('/other', '_blank', 'popup')">Popup</button>
<div style="height: 3000px"></div>
<p id=bottom>the end</p>
<script>
console.log('hello from the page', 42);
console.error('something broke');
fetch('/api?x=1').then(r => r.json()).then(j => console.log('api says', j.ok));
setTimeout(() => { const p = document.createElement('p'); p.textContent = 'late arrival'; document.body.append(p) }, 1500);
</script>
`

func testSite(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, testPage)
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<title>Done</title><p>Query was: %s</p>", r.URL.Query().Get("q"))
	})
	mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Other</title><p>the other page</p>")
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok": true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// testChrome starts a headless Chrome showing url, and returns its
// user data dir.
func testChrome(t *testing.T, url string, extra ...string) string {
	t.Helper()
	return startTestChrome(t, "", append([]string{"--headless=new"}, append(extra, url)...)...)
}

// startTestChrome starts a Chrome on a throwaway user data dir, with
// localState (if any) as its Local State, and returns the dir.
func startTestChrome(t *testing.T, localState string, args ...string) string {
	t.Helper()
	if _, err := chromeBinary(); err != nil {
		t.Skip("no Chrome")
	}
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	dir, err := os.MkdirTemp("", "shoulder-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	if localState != "" {
		os.WriteFile(filepath.Join(dir, "Local State"), []byte(localState), 0o600)
	}
	args = append([]string{"--use-mock-keychain", "--password-store=basic", "--window-size=1000,700"}, args...)
	if goruntime.GOOS == "linux" {
		args = append(args, "--no-sandbox") // CI machines often can't sandbox
	}
	proc, err := launchChrome(dir, args...)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopChrome(proc)
		os.RemoveAll(dir)
	})
	return dir
}

// testCDP is a connection to a test Chrome apart from shoulder's, for
// setting things up and checking on them.
type testCDP struct {
	t   *testing.T
	ctx context.Context
}

func newTestCDP(t *testing.T, dir string) *testCDP {
	t.Helper()
	ws, err := chromeEndpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	actx, cancelAlloc := remote.NewAllocator(context.Background(), ws, remote.NoModifyURL)
	ctx, cancel := chromedp.NewContext(actx, chromedp.WithErrorf(func(string, ...any) {}))
	if err := chromedp.ListenBrowser(ctx, func(any) {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cancelAlloc() })
	return &testCDP{t, ctx}
}

func tcall[P, R any](x *testCDP, cmd cdp.Command[P, R], params P) (R, error) {
	ctx, cancel := context.WithTimeout(x.ctx, 10*time.Second)
	defer cancel()
	return cdp.Call(ctx, chromedp.FromContext(x.ctx).Browser, cmd, params)
}

func (x *testCDP) open(params target.CreateTargetParams) target.ID {
	x.t.Helper()
	r, err := tcall(x, target.CreateTarget, params)
	if err != nil {
		x.t.Fatal(err)
	}
	return r.TargetID
}

// targets lists the browser's tabs or pages.
func (x *testCDP) targets(typ string) []*target.Info {
	x.t.Helper()
	r, err := tcall(x, target.GetTargets, target.GetTargetsParams{Filter: target.Filter{{Type: typ}}})
	if err != nil {
		x.t.Fatal(err)
	}
	return r.TargetInfos
}

// browserSession starts a session and shares the Chrome in dir.
func browserSession(t *testing.T, chromeDir string, extra url.Values) (*config, *ctl, browserStart) {
	t.Helper()
	cfg, c := testSession(t)
	if _, err := c.post("/ctl/kind?browser=chrome"); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"browser": {"chrome"}, "dir": {chromeDir}}
	for k, v := range extra {
		q[k] = v
	}
	b, err := c.post("/ctl/browser?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	var res browserStart
	json.Unmarshal(b, &res)
	return cfg, c, res
}

func TestChrome(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	cfg, c, res := browserSession(t, dir, nil)
	if len(res.Tabs) != 1 || res.Tabs[0].ID != "t1" || !strings.HasPrefix(res.Tabs[0].URL, site.URL) {
		t.Fatalf("start: %+v", res)
	}
	rw, err := c.share("unix", false, true)
	if err != nil {
		t.Fatal(err)
	}
	a := agentFromPaste(t, cfg, rw.Paste)

	if g := a.get(""); !strings.Contains(g, "# shoulder: a shared browser") || !strings.Contains(g, "POST click") || !strings.Contains(g, "read-write") {
		t.Errorf("guide:\n%s", g)
	}
	if out := a.get("wait?text=late+arrival"); !strings.HasPrefix(out, "[ready] t1: \"Shoulder test\"") {
		t.Errorf("wait: %s", out)
	}
	var st browserStatus
	if err := json.Unmarshal([]byte(a.get("state")), &st); err != nil || len(st.Tabs) != 1 || !st.Tabs[0].Current || st.Tabs[0].Title != "Shoulder test" {
		t.Fatalf("state: %v %+v", err, st)
	}

	// page: readable text with numbered elements.
	page := a.get("page")
	for _, want := range []string{"# Shoulder test", "# Hello there", `button "Press me"]`, `input:text "Query"]`, `a "Other page" -> ` + site.URL + "/other]", "late arrival"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
	num := func(page, label string) string {
		t.Helper()
		for _, f := range strings.Split(page, "[")[1:] {
			if strings.Contains(f, label) {
				return strings.Fields(f)[0]
			}
		}
		t.Fatalf("no %s in page:\n%s", label, page)
		return ""
	}

	// click, by number and by selector: real clicks.
	if code, out := a.call("POST", "click", num(page, `"Press me"`)); code != 200 || !strings.Contains(out, `clicked [`) {
		t.Fatalf("click: %d %s", code, out)
	}
	if code, out := a.call("POST", "click", "#press"); code != 200 {
		t.Fatalf("click selector: %d %s", code, out)
	}
	if code, _ := a.call("GET", "js", ""); code != 404 {
		t.Errorf("GET js: %d, want 404", code)
	}
	if code, out := a.call("POST", "js", "document.getElementById('out').textContent"); code != 200 || out != "\"pressed 2\"\n" {
		t.Errorf("js expression: %d %q", code, out)
	}
	if code, out := a.call("POST", "js", "const r = await fetch('/api');\nreturn {status: r.status, n: window.n};"); code != 200 || !strings.Contains(out, `"status":200`) || !strings.Contains(out, `"n":2`) {
		t.Errorf("js body: %d %q", code, out)
	}
	if code, out := a.call("POST", "js", "throw new Error('nope')"); code != 400 || !strings.Contains(out, "nope") {
		t.Errorf("js throw: %d %q", code, out)
	}

	// Our scripts run apart from the page's: it can't see them.
	if _, out := a.call("POST", "js", "typeof window.__shoulder"); out != "\"undefined\"\n" {
		t.Errorf("shoulder's helpers leaked into the page: %s", out)
	}

	// type: into the textarea, then edit with keys.
	if code, out := a.call("POST", "type?selector=%23notes", "hello world"); code != 200 || !strings.Contains(out, `now reads "hello world"`) {
		t.Fatalf("type: %d %s", code, out)
	}
	if code, out := a.call("POST", "key", "Backspace Backspace"); code != 200 {
		t.Fatalf("key: %d %s", code, out)
	}
	if _, out := a.call("POST", "js", "document.getElementById('notes').value"); out != "\"hello wor\"\n" {
		t.Errorf("after Backspace: %s", out)
	}
	if code, out := a.call("POST", "type?selector=%23notes&clear=1", "fresh"); code != 200 || !strings.Contains(out, `now reads "fresh"`) {
		t.Errorf("type clear: %d %s", code, out)
	}
	if code, out := a.call("POST", "type?selector=%23color", "green"); code != 200 || !strings.Contains(out, `chose "green"`) {
		t.Errorf("select: %d %s", code, out)
	}

	// screenshot: a JPEG of the viewport in CSS pixels.
	req, _ := http.NewRequest("GET", a.base+"/screenshot", nil)
	resp, err := a.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var img bytes.Buffer
	img.ReadFrom(resp.Body)
	resp.Body.Close()
	im, format, err := image.Decode(&img)
	if err != nil || format != "jpeg" || resp.Header.Get("X-Shoulder-Note") == "" {
		t.Fatalf("screenshot: %v %s %v", err, format, resp.Header)
	}
	var vp struct{ W, H int }
	_, out := a.call("POST", "js", "({W: document.documentElement.clientWidth, H: document.documentElement.clientHeight})")
	json.Unmarshal([]byte(out), &vp)
	if b := im.Bounds(); b.Dx() != vp.W || b.Dy() != vp.H {
		t.Errorf("screenshot is %v, viewport %+v", b, vp)
	}

	// console and network.
	if out := a.get("console"); !strings.Contains(out, "hello from the page 42") || !strings.Contains(out, "something broke") || !strings.Contains(out, "api says true") {
		t.Errorf("console:\n%s", out)
	}
	if out := a.get("console?level=error"); strings.Contains(out, "hello from the page") || !strings.Contains(out, "something broke") {
		t.Errorf("console errors:\n%s", out)
	}
	if out := a.get("network"); !strings.Contains(out, "GET 200 fetch "+site.URL+"/api?x=1") {
		t.Errorf("network:\n%s", out)
	}

	// scroll.
	if code, out := a.call("POST", "scroll", "bottom"); code != 200 || strings.Contains(out, "y=0 ") {
		t.Errorf("scroll: %d %s", code, out)
	}
	if out := a.get("page?viewport_only=1"); !strings.Contains(out, "the end") || strings.Contains(out, "Hello there") {
		t.Errorf("viewport_only after scrolling:\n%s", out)
	}
	a.call("POST", "scroll", "top")

	// A dialog blocks the page until it's answered.
	if code, out := a.call("POST", "click", "#ask"); code != 200 || !strings.Contains(out, "dialog is open") {
		t.Errorf("click opening a dialog: %d %s", code, out)
	}
	if code, out := a.call("GET", "page", ""); code != 409 {
		t.Errorf("page with a dialog open: %d %s", code, out)
	}
	if code, out := a.call("POST", "dialog", "accept"); code != 200 {
		t.Errorf("dialog: %d %s", code, out)
	}
	if _, out := a.call("POST", "js", "document.getElementById('out').textContent"); out != "\"confirmed\"\n" {
		t.Errorf("after the dialog: %s", out)
	}

	// type + submit navigates; back returns.
	if code, out := a.call("POST", "type?element="+num(page, `"Query"`)+"&submit=1", "cats & dogs"); code != 200 || !strings.Contains(out, `"Done"`) {
		t.Fatalf("type+submit: %d %s", code, out)
	}
	if out := a.get("page"); !strings.Contains(out, "Query was: cats & dogs") {
		t.Errorf("after submit:\n%s", out)
	}
	if code, out := a.call("POST", "navigate", "back"); code != 200 || !strings.Contains(out, `"Shoulder test"`) {
		t.Errorf("back: %d %s", code, out)
	}
	if code, out := a.call("POST", "navigate", site.URL+"/other"); code != 200 || !strings.Contains(out, `t1: "Other"`) {
		t.Errorf("navigate: %d %s", code, out)
	}
	if out := a.get("console"); !strings.Contains(out, "navigated to "+site.URL+"/other") {
		t.Errorf("console after navigating:\n%s", out)
	}
	for _, u := range []string{"chrome://settings/passwords", "file:///etc/hosts"} {
		if code, out := a.call("POST", "navigate", u); code != 403 {
			t.Errorf("navigate %s: %d %s", u, code, out)
		}
		if code, out := a.call("POST", "open", u); code != 403 {
			t.Errorf("open %s: %d %s", u, code, out)
		}
	}
	if code, out := a.call("POST", "navigate", "http://127.0.0.1:1/"); code != 502 {
		t.Errorf("navigate nowhere: %d %s", code, out)
	}
	if code, out := a.call("POST", "navigate", site.URL); code != 200 {
		t.Errorf("navigate home: %d %s", code, out)
	}

	// open: a new tab joins the share and becomes current; close.
	code, out := a.call("POST", "open", site.URL+"/other")
	if code != 200 || !strings.Contains(out, `t2: "Other"`) {
		t.Fatalf("open: %d %s", code, out)
	}
	if out := a.get("page"); !strings.Contains(out, "the other page") {
		t.Errorf("page in the new tab:\n%s", out)
	}
	if out := a.get("page?tab=t1"); !strings.Contains(out, "Hello there") {
		t.Errorf("page?tab=t1:\n%s", out)
	}
	if code, out := a.call("POST", "close?tab=t1", ""); code != 403 {
		t.Errorf("closing the person's tab: %d %s", code, out)
	}
	if code, out := a.call("POST", "close?tab=t2", ""); code != 200 {
		t.Errorf("close: %d %s", code, out)
	}
	waitUntil(t, "t2 to go", func() bool { return !strings.Contains(a.get("state"), `"t2"`) })

	// A popup from a shared tab joins the share.
	if code, out := a.call("POST", "click", "#pop"); code != 200 {
		t.Fatalf("popup click: %d %s", code, out)
	}
	waitUntil(t, "the popup", func() bool { return strings.Contains(a.get("state"), `"opened_by": "t1"`) })

	// Read-only codes look but don't act.
	ro, err := c.share("unix", true, false)
	if err != nil {
		t.Fatal(err)
	}
	r := agentFromPaste(t, cfg, ro.Paste)
	if g := r.get(""); !strings.Contains(g, "read-only") || strings.Contains(g, "POST click") {
		t.Errorf("read-only guide:\n%s", g)
	}
	r.get("page?tab=t1")
	for _, ep := range []string{"click", "type", "js", "navigate", "open", "key"} {
		if code, out := r.call("POST", ep, "1"); code != 403 {
			t.Errorf("read-only POST %s: %d %s", ep, code, out)
		}
	}

	// The person's screen sees what happened.
	b, _ := c.do("GET", "/ctl/activity")
	var act browserActivity
	json.Unmarshal(b, &act)
	var lines []string
	for _, l := range act.Activity {
		lines = append(lines, l.Text)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "typed 11 characters into #notes") || strings.Contains(all, "cats") || !strings.Contains(all, "navigate "+site.URL+"/other") {
		t.Errorf("activity:\n%s", all)
	}
	if info, _ := c.do("GET", "/ctl/info"); !strings.Contains(string(info), `"browser": "Chrome"`) {
		t.Errorf("info: %s", info)
	}
}

// With several windows, the person picks the tab.
func TestChromePick(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	newTestCDP(t, dir).open(target.CreateTargetParams{URL: site.URL + "/other", NewWindow: ptrTo(true)})

	_, c, res := browserSession(t, dir, nil)
	if len(res.Choose) != 2 || len(res.Tabs) != 0 {
		t.Fatalf("with two windows: %+v", res)
	}
	var pick candidate
	for _, cand := range res.Choose {
		if cand.Title == "Other" {
			pick = cand
		}
	}
	b, err := c.post("/ctl/browser?" + url.Values{"browser": {"chrome"}, "dir": {dir}, "tabs": {"pick"}, "pick": {pick.ID}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &res)
	if len(res.Tabs) != 1 || res.Tabs[0].Title != "Other" {
		t.Fatalf("picked: %+v", res)
	}
}

// Requests from before shoulder connected come from the page's
// Performance API.
func TestChromeEarlierRequests(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	waitUntil(t, "Chrome to start", func() bool { _, err := chromeEndpoint(dir); return err == nil })
	time.Sleep(time.Second) // the page loads before shoulder looks
	cfg, c, _ := browserSession(t, dir, nil)
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	out := a.get("network")
	if !strings.Contains(out, "Performance API") || !strings.Contains(out, "GET 200 document "+site.URL+"/") || !strings.Contains(out, "GET 200 fetch "+site.URL+"/api?x=1") {
		t.Errorf("network:\n%s", out)
	}
	a.call("POST", "navigate", "reload")
	if out := a.get("network"); strings.Contains(out, "Performance API") || strings.Count(out, "/api?x=1") != 1 {
		t.Errorf("network after reload:\n%s", out)
	}
}

// On a Retina screen, screenshots are still in CSS pixels, so x,y from
// them are click coordinates.
func TestChromeRetina(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL, "--force-device-scale-factor=2")
	cfg, c, _ := browserSession(t, dir, nil)
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	_, out := a.call("POST", "js", "({W: document.documentElement.clientWidth, H: document.documentElement.clientHeight, D: devicePixelRatio})")
	var vp struct{ W, H, D int }
	json.Unmarshal([]byte(out), &vp)
	if vp.D != 2 {
		t.Fatalf("devicePixelRatio: %s", out)
	}
	resp, err := a.http.Get(a.base + "/screenshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	im, _, err := image.Decode(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := im.Bounds(); b.Dx() != vp.W || b.Dy() != vp.H {
		t.Errorf("screenshot is %v, viewport %+v", b, vp)
	}
	// Click where the button is in the screenshot.
	_, out = a.call("POST", "js", "const r = document.getElementById('press').getBoundingClientRect(); return [r.x + r.width/2, r.y + r.height/2];")
	var xy []float64
	json.Unmarshal([]byte(out), &xy)
	if len(xy) != 2 {
		t.Fatalf("button at %s", out)
	}
	if code, out := a.call("POST", fmt.Sprintf("click?x=%g&y=%g", xy[0], xy[1]), ""); code != 200 {
		t.Fatalf("click: %d %s", code, out)
	}
	if _, out := a.call("POST", "js", "document.getElementById('out').textContent"); out != "\"pressed 1\"\n" {
		t.Errorf("after click at x,y: %s", out)
	}
}

// All tabs means the profile's: not another browser context's, such as
// an incognito window. New tabs open in the shared tab's profile.
func TestChromeAllTabs(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	x := newTestCDP(t, dir)
	bc, err := tcall(x, target.CreateBrowserContext, target.CreateBrowserContextParams{})
	if err != nil {
		t.Fatal(err)
	}
	x.open(target.CreateTargetParams{URL: site.URL + "/done?q=private", BrowserContextID: bc.BrowserContextID})
	x.open(target.CreateTargetParams{URL: site.URL + "/other", NewWindow: ptrTo(true)})

	cfg, c, res := browserSession(t, dir, url.Values{"tabs": {"all"}})
	titles := func(tabs []tabInfo) string {
		var ts []string
		for _, tab := range tabs {
			ts = append(ts, tab.Title)
		}
		sort.Strings(ts)
		return strings.Join(ts, ",")
	}
	if got := titles(res.Tabs); got != "Other,Shoulder test" {
		t.Fatalf("shared %q: %+v", got, res)
	}
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	if code, out := a.call("POST", "open", site.URL+"/done?q=agent"); code != 200 {
		t.Fatalf("open: %d %s", code, out)
	}
	// Tabs opened later join, but not in the other context.
	x.open(target.CreateTargetParams{URL: site.URL + "/done?q=private2", BrowserContextID: bc.BrowserContextID})
	x.open(target.CreateTargetParams{URL: site.URL + "/done?q=later"})
	waitUntil(t, "the later tab", func() bool { return strings.Contains(a.get("state"), "q=later") })
	st := a.get("state")
	if strings.Contains(st, "private") || strings.Count(st, `"id"`) != 4 {
		t.Errorf("state: %s", st)
	}
	for _, info := range x.targets("tab") {
		if strings.HasSuffix(info.URL, "q=agent") && info.BrowserContextID == bc.BrowserContextID {
			t.Errorf("the agent's tab opened in the other context")
		}
	}
}

// A Chrome that isn't listening gets instructions, not a hang.
func TestChromeOff(t *testing.T) {
	dir := t.TempDir()
	ok, msg := chromeCheck(dir)
	if ok || !strings.Contains(msg, chromeInspectURL) || !strings.Contains(msg, "isn't running") {
		t.Errorf("%v %s", ok, msg)
	}
	// A stale DevToolsActivePort, left by a Chrome that quit.
	os.WriteFile(dir+"/DevToolsActivePort", []byte("1\n/devtools/browser/x\n"), 0o600)
	if _, err := chromeEndpoint(dir); err == nil {
		t.Error("stale port file accepted")
	}
	// Running (this process holds the lock) but not listening.
	os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), dir+"/SingletonLock")
	if ok, msg := chromeCheck(dir); ok || !strings.Contains(msg, "remote debugging is off") {
		t.Errorf("%v %s", ok, msg)
	}
	if _, err := connectChrome(context.Background(), dir); err == nil || !strings.Contains(err.Error(), chromeInspectURL) {
		t.Errorf("connect: %v", err)
	}
}

// The standalone Chrome: started for a share without asking anything,
// shared again if it's running, and quit when the share that started
// it stops.
func TestStandaloneChrome(t *testing.T) {
	if _, err := chromeBinary(); err != nil {
		t.Skip("no Chrome")
	}
	if testing.Short() {
		t.Skip("starts Chrome")
	}
	dir := t.TempDir()
	standaloneChromeArgs = []string{"--headless=new", "--use-mock-keychain", "--password-store=basic"}
	if goruntime.GOOS == "linux" {
		standaloneChromeArgs = append(standaloneChromeArgs, "--no-sandbox")
	}
	t.Cleanup(func() { standaloneChromeArgs = nil })
	if ok, msg := standaloneChromeCheck(dir); !ok || !strings.Contains(msg, "starts a standalone Chrome") {
		t.Errorf("before: %v %s", ok, msg)
	}

	_, c, res := browserSession(t, dir, url.Values{"standalone": {"1"}})
	if res.Label != "standalone Chrome" || len(res.Tabs) != 1 {
		t.Fatalf("start: %+v", res)
	}
	if !chromeRunning(dir) {
		t.Fatal("the standalone Chrome isn't running")
	}
	if ok, msg := standaloneChromeCheck(dir); !ok || !strings.Contains(msg, "that's running") {
		t.Errorf("running: %v %s", ok, msg)
	}

	// Another share uses the running one, and leaves it running.
	p, err := openBrowser(context.Background(), "chrome", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.(*chromeBrowser).launched != nil {
		t.Error("started a second Chrome")
	}
	p.close()
	if !chromeRunning(dir) {
		t.Fatal("letting go of a Chrome shoulder didn't start quit it")
	}

	c.post("/ctl/quit")
	deadline := time.Now().Add(15 * time.Second)
	for chromeRunning(dir) {
		if time.Now().After(deadline) {
			t.Fatal("the standalone Chrome still running after sharing stopped")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The standalone Chrome's profile open without remote debugging (the person
// started Chrome on it by hand) gets instructions.
func TestStandaloneChromeNotListening(t *testing.T) {
	dir := t.TempDir()
	os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), dir+"/SingletonLock")
	if _, err := chromeBinary(); err != nil {
		t.Skip("no Chrome")
	}
	if ok, msg := standaloneChromeCheck(dir); ok || !strings.Contains(msg, "quit it") {
		t.Errorf("%v %s", ok, msg)
	}
	if _, err := openBrowser(context.Background(), "chrome", dir, true); err == nil || !strings.Contains(err.Error(), "quit it") {
		t.Errorf("open: %v", err)
	}
}
