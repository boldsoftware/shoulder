package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Safari can't be scripted in tests: macOS would ask the person for
// permission. So the JXA program is compiled (which sends Safari
// nothing) and run against a fake Safari object, and shoulder's Safari
// side runs against a stand-in that answers its requests with a headless
// Chrome's tab, running the page scripts in the page's world as Safari
// does.

func TestSafariJXACompiles(t *testing.T) {
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("no osacompile")
	}
	out := filepath.Join(t.TempDir(), "safari.scpt")
	if b, err := exec.Command("osacompile", "-l", "JavaScript", "-o", out, "-e", safariJXA+safariMain).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
}

// fakeSafariJS stands in for Application('Safari'): windows 10 (in
// front) and 20, with tabs whose url, currentTab, and index can be read
// (called) and set, as with the real thing.
const fakeSafariJS = `
function fakeSafari() {
  const s = { _running: true, _windows: [], _events: [] };
  const prop = (o, name, get, set) => Object.defineProperty(o, name, { get() { return get; }, set });
  const mkTab = (url, title) => {
    const t = { _url: url, _title: title, _store: {} };
    prop(t, 'url', () => t._url, v => { t._url = v; t._title = ''; s._events.push('navigate ' + v); });
    t.name = () => t._title;
    t.close = () => { const w = s._windows.find(w => w._tabs.includes(t)); w._tabs.splice(w._tabs.indexOf(t), 1); if (w._cur === t) w._cur = w._tabs[0]; };
    return t;
  };
  const mkWindow = (id, tabs, cur) => {
    const w = { _id: id, _tabs: tabs, _cur: tabs[cur] };
    w.id = () => id;
    w.visible = () => true;
    w.miniaturized = () => false;
    const tabsFn = () => w._tabs.slice();
    tabsFn.push = t => w._tabs.push(t);
    w.tabs = tabsFn;
    prop(w, 'currentTab', () => ({ index: () => w._tabs.indexOf(w._cur) + 1, _t: w._cur }), v => { w._cur = v; });
    prop(w, 'index', () => s._windows.indexOf(w) + 1, v => { s._windows.splice(s._windows.indexOf(w), 1); s._windows.splice(v - 1, 0, w); });
    return w;
  };
  s._windows = [
    mkWindow(10, [mkTab('https://a.example/', 'A'), mkTab('https://b.example/', 'B')], 1),
    mkWindow(20, [mkTab('https://c.example/', 'C')], 0),
  ];
  s.running = () => s._running;
  s.windows = () => s._windows.slice();
  s.activate = () => s._events.push('activate');
  s.Tab = (props) => mkTab(props.url, '');
  s.doJavaScript = (code, opts) => {
    const t = opts.in;
    const poll = code.match(/^\(\(\) => \{ const v = window\["(__shoulder_\w+)"\]/);
    if (poll) {
      const v = t._store[poll[1]];
      if (v === undefined) return '__shoulder_gone__';
      if (v === null) { t._store[poll[1]] = JSON.stringify({ value: 'ran in ' + t._url }); return ''; }
      delete t._store[poll[1]];
      return v;
    }
    const start = code.match(/const KEY = "(__shoulder_\w+)"/);
    if (!start) return undefined;
    t._store[start[1]] = null;
    return 'started';
  };
  return s;
}
function attempt(f) { try { return f(); } catch (e) { return { error: e.message }; } }
function run() {
  const s = fakeSafari();
  const out = {};
  out.list = handle(s, { op: 'list' });
  const code = safariStart;
  out.js = handle(s, { op: 'js', tab: { window: 10, index: 2, url: 'https://b.example/' }, code, key: '__shoulder_0123abcd', timeout: 2 });
  out.badJS = handle(s, { op: 'js', tab: { window: 10, index: 2, url: 'https://b.example/' }, code: 'nonsense', key: '__shoulder_0123abcd', timeout: 2 });
  // The tab before B closes: B is found again by its URL.
  s._windows[0]._tabs[0].close();
  out.tabs = handle(s, { op: 'tabs', tabs: [{ window: 10, index: 2, url: 'https://b.example/' }, { window: 99, index: 1, url: 'https://z.example/' }] });
  // B navigates by itself: same place, same number of tabs, so it's B.
  s._windows[0]._tabs[0]._url = 'https://b2.example/';
  out.moved = handle(s, { op: 'tabs', tabs: [{ window: 10, index: 1, count: 1, url: 'https://b.example/' }, { window: 10, index: 1, count: 2, url: 'https://b.example/' }] });
  s._windows[0]._tabs[0]._url = 'https://b.example/';
  out.nav = handle(s, { op: 'navigate', tab: { window: 10, index: 1, url: 'https://b.example/' }, url: 'https://d.example/' });
  out.open = handle(s, { op: 'open', window: 20, url: 'https://e.example/', background: false });
  out.afterOpen = handle(s, { op: 'list' });
  out.activate = handle(s, { op: 'activate', tab: { window: 20, index: 1, url: 'https://c.example/' } });
  out.front = handle(s, { op: 'front', tab: out.open.tab });
  out.close = handle(s, { op: 'close', tab: out.open.tab });
  out.afterClose = handle(s, { op: 'list' });
  out.gone = attempt(() => handle(s, { op: 'navigate', tab: { window: 99, index: 1, url: '' }, url: 'https://f.example/' }));
  s._running = false;
  out.notRunning = attempt(() => handle(s, { op: 'list' }));
  out.running = handle(s, { op: 'running' });
  out.events = s._events;
  return JSON.stringify(out);
}
`

func TestSafariJXA(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("no osascript")
	}
	start, _ := safariScript("__shoulder_0123abcd", "return 1;", nil)
	sb, _ := json.Marshal(start)
	script := safariJXA + fakeSafariJS + "\nconst safariStart = " + string(sb) + ";\n"
	b, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	want := map[string]string{
		"list":       `{"windows":[{"id":10,"index":1,"current":2,"tabs":[{"window":10,"index":1,"count":2,"title":"A","url":"https://a.example/"},{"window":10,"index":2,"count":2,"title":"B","url":"https://b.example/"}]},{"id":20,"index":2,"current":1,"tabs":[{"window":20,"index":1,"count":1,"title":"C","url":"https://c.example/"}]}]}`,
		"js":         `{"tab":{"window":10,"index":2,"count":2,"title":"B","url":"https://b.example/"},"value":"{\"value\":\"ran in https://b.example/\"}"}`,
		"badJS":      `{"tab":{"window":10,"index":2,"count":2,"title":"B","url":"https://b.example/"},"failed":true}`,
		"tabs":       `{"tabs":[{"window":10,"index":1,"count":1,"title":"B","url":"https://b.example/","current":true},null]}`,
		"moved":      `{"tabs":[{"window":10,"index":1,"count":1,"title":"B","url":"https://b2.example/","current":true},null]}`,
		"nav":        `{"tab":{"window":10,"index":1,"count":1,"title":"","url":"https://d.example/"}}`,
		"open":       `{"tab":{"window":20,"index":2,"count":2,"title":"","url":"https://e.example/"}}`,
		"afterOpen":  `{"windows":[{"id":10,"index":1,"current":1,"tabs":[{"window":10,"index":1,"count":1,"title":"","url":"https://d.example/"}]},{"id":20,"index":2,"current":2,"tabs":[{"window":20,"index":1,"count":2,"title":"C","url":"https://c.example/"},{"window":20,"index":2,"count":2,"title":"","url":"https://e.example/"}]}]}`,
		"activate":   `{"tab":{"window":20,"index":1,"count":2,"title":"C","url":"https://c.example/"}}`,
		"front":      `{"current":false,"visible":true,"miniaturized":false}`,
		"close":      `{}`,
		"afterClose": `{"windows":[{"id":20,"index":1,"current":1,"tabs":[{"window":20,"index":1,"count":1,"title":"C","url":"https://c.example/"}]},{"id":10,"index":2,"current":1,"tabs":[{"window":10,"index":1,"count":1,"title":"","url":"https://d.example/"}]}]}`,
		"gone":       `{"error":"GONE"}`,
		"notRunning": `{"error":"NOT_RUNNING"}`,
		"running":    `{"running":false}`,
		"events":     `["navigate https://d.example/","activate"]`,
	}
	for k, w := range want {
		if got := string(out[k]); got != w {
			t.Errorf("%s:\n got %s\nwant %s", k, got, w)
		}
	}
}

// chromeAsSafari answers safariJXA's requests (one window, one tab) with
// a headless Chrome's tab, running do JavaScript's code in the page's
// own world.
type chromeAsSafari struct {
	t   *testing.T
	ctx context.Context // the page's chromedp context
}

func newChromeAsSafari(t *testing.T, dir string) *chromeAsSafari {
	x := newTestCDP(t, dir)
	pages := x.targets("page")
	if len(pages) == 0 {
		t.Fatal("no page")
	}
	ctx, cancel := chromedp.NewContext(x.ctx, chromedp.WithTargetID(pages[0].TargetID), chromedp.WithDetachOnCancel())
	t.Cleanup(cancel)
	if err := chromedp.Do(ctx); err != nil {
		t.Fatal(err)
	}
	f := &chromeAsSafari{t: t, ctx: ctx}
	// Loaded, so the page's own console messages come before shoulder looks.
	waitUntil(t, "the page to load", func() bool {
		v, _ := f.eval(context.Background(), "document.readyState === 'complete' && document.body.innerText.includes('late arrival')")
		return v == true
	})
	return f
}

func (f *chromeAsSafari) eval(ctx context.Context, code string) (any, error) {
	return chromedp.Run(f.ctx, chromedp.Evaluate[any](code))
}

func (f *chromeAsSafari) tab(ctx context.Context) map[string]any {
	v, _ := f.eval(ctx, "({title: document.title, url: location.href})")
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	m["window"], m["index"] = 1, 1
	return m
}

func (f *chromeAsSafari) run(ctx context.Context, req []byte) ([]byte, error) {
	var r struct {
		Op, Code, Key, URL string
		Timeout            float64
	}
	json.Unmarshal(req, &r)
	var reply any
	switch r.Op {
	case "running":
		reply = map[string]any{"running": true}
	case "list":
		reply = map[string]any{"windows": []any{map[string]any{"id": 1, "index": 1, "current": 1, "tabs": []any{f.tab(ctx)}}}}
	case "tabs":
		tab := f.tab(ctx)
		tab["current"] = true
		reply = map[string]any{"tabs": []any{tab}}
	case "front":
		reply = map[string]any{"current": true, "visible": true}
	case "navigate":
		chromedp.Run(f.ctx, chromedp.Navigate(r.URL))
		reply = map[string]any{"tab": map[string]any{"window": 1, "index": 1, "url": r.URL}}
	case "js":
		started, err := f.eval(ctx, r.Code)
		if err != nil {
			return nil, err
		}
		if started != "started" {
			reply = map[string]any{"failed": true}
			break
		}
		k, _ := json.Marshal(r.Key)
		for {
			v, err := f.eval(ctx, "window["+string(k)+"]")
			if err != nil {
				return nil, err
			}
			if s, ok := v.(string); ok {
				reply = map[string]any{"tab": f.tab(ctx), "value": s}
				break
			}
			if v == nil {
				if present, _ := f.eval(ctx, "Object.prototype.hasOwnProperty.call(window, "+string(k)+")"); present != true {
					reply = map[string]any{"tab": f.tab(ctx), "navigated": true}
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	default:
		reply = map[string]any{"error": "unsupported in the test: " + r.Op}
	}
	return json.Marshal(reply)
}

func TestSafari(t *testing.T) {
	site := testSite(t)
	dir := testChrome(t, site.URL)
	f := newChromeAsSafari(t, dir)
	oldOSA, oldCapture := safariOSA, safariCapture
	safariOSA = f.run
	safariCapture = func(ctx context.Context, window int, png bool) ([]byte, error) { return []byte("JPEG"), nil }
	t.Cleanup(func() { safariOSA, safariCapture = oldOSA, oldCapture })

	cfg, c := testSession(t)
	c.post("/ctl/kind?browser=safari")
	b, err := c.post("/ctl/browser?" + url.Values{"browser": {"safari"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	var res browserStart
	json.Unmarshal(b, &res)
	if res.Label != "Safari" || len(res.Tabs) != 1 {
		t.Fatalf("start: %s", b)
	}
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	if g := a.get(""); !strings.Contains(g, "## Safari") || !strings.Contains(g, "own Safari") {
		t.Errorf("guide:\n%s", g)
	}
	if out := a.get("wait?text=Hello"); !strings.HasPrefix(out, "[ready]") {
		t.Errorf("wait: %s", out)
	}
	if out := a.get("page"); !strings.Contains(out, "# Hello there") || !strings.Contains(out, `button "Press me"]`) {
		t.Errorf("page:\n%s", out)
	}

	// No real input: page scripts click and type.
	if code, out := a.call("POST", "click", "#press"); code != 200 || !strings.Contains(out, `button "Press me"`) {
		t.Errorf("click: %d %s", code, out)
	}
	if code, out := a.call("POST", "js", "document.getElementById('out').textContent"); code != 200 || out != "\"pressed 1\"\n" {
		t.Errorf("js: %d %q", code, out)
	}
	if code, out := a.call("POST", "js", "const r = await fetch('/api'); console.log('later', 7); return r.status;"); code != 200 || out != "200\n" {
		t.Errorf("js body: %d %q", code, out)
	}
	if code, out := a.call("POST", "js", "nonsense("); code != 400 {
		t.Errorf("js syntax error: %d %q", code, out)
	}
	if code, out := a.call("POST", "type?selector=%23notes", "hello world"); code != 200 || !strings.Contains(out, `now reads "hello world"`) {
		t.Errorf("type: %d %s", code, out)
	}
	if code, out := a.call("POST", "key", "Backspace Backspace"); code != 200 {
		t.Errorf("key: %d %s", code, out)
	}
	if _, out := a.call("POST", "js", "document.getElementById('notes').value"); out != "\"hello wor\"\n" {
		t.Errorf("after Backspace: %s", out)
	}

	// The recorder catches what happens after shoulder first looked.
	if out := a.get("console"); !strings.Contains(out, "later 7") || strings.Contains(out, "hello from the page") {
		t.Errorf("console:\n%s", out)
	}
	if out := a.get("network"); !regexp.MustCompile(`GET 200 fetch http://\S+/api\b`).MatchString(out) || !strings.Contains(out, "document") {
		t.Errorf("network:\n%s", out)
	}

	if code, out := a.call("POST", "navigate", site.URL+"/other"); code != 200 || !strings.Contains(out, "/other") {
		t.Errorf("navigate: %d %s", code, out)
	}
	if out := a.get("page"); !strings.Contains(out, "the other page") {
		t.Errorf("page after navigate:\n%s", out)
	}
	if code, out := a.call("POST", "navigate", "back"); code != 200 || !strings.Contains(out, "Shoulder test") {
		t.Errorf("back: %d %s", code, out)
	}
	if code, out := a.call("GET", "screenshot", ""); code != 200 || out != "JPEG" {
		t.Errorf("screenshot: %d %s", code, out)
	}
	if code, out := a.call("POST", "dialog", "accept"); code != 501 {
		t.Errorf("dialog: %d %s", code, out)
	}
	ro, _ := c.share("unix", true, false)
	r := agentFromPaste(t, cfg, ro.Paste)
	if code, _ := r.call("POST", "click", "#press"); code != 403 {
		t.Errorf("read-only click: %d", code)
	}
}

// Safari's own errors become instructions.
func TestSafariErrors(t *testing.T) {
	for msg, want := range map[string]string{
		"Safari got an error: You must enable the 'Allow JavaScript from Apple Events' option in Safari's Develop menu to use 'do JavaScript'.": "Develop ›",
		"Not authorized to send Apple events to Safari.": "Automation",
		"NOT_RUNNING": "isn't running",
	} {
		if err := safariError(msg, 0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", msg, err)
		}
	}
	osa := func(ctx context.Context, req []byte) ([]byte, error) {
		return []byte(`{"error":"Not authorized to send Apple events to Safari.","number":-1743}`), nil
	}
	if _, err := connectSafari(context.Background(), osa); err == nil || !strings.Contains(err.Error(), "Automation") {
		t.Errorf("connect: %v", err)
	}
}
