package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Safari is driven with AppleScript (as JavaScript for Automation):
// Safari's "do JavaScript" runs code in a tab, and its scripting
// dictionary lists, opens, closes, and switches tabs. That needs two
// things from the person, once: Develop › "Allow JavaScript from Apple
// Events", and macOS's Automation permission for their terminal to
// control Safari (macOS asks the first time).
//
// There's no protocol beneath the page: no real mouse or keyboard (page
// scripts simulate them), no isolated world (shoulder's helpers live in
// the page's), no console or network events (a recorder in the page
// keeps them from when shoulder first looks), no dialogs. Screenshots
// are of the window, with screencapture, which needs Screen Recording.
// Tabs have no ids either, so a shared tab is its window's id and its
// place there, checked against the URL it last had.

const safariHelp = "Safari needs Develop › \"Allow JavaScript from Apple Events\" (turn on Settings › Advanced › \"Show features for web developers\" to get the Develop menu), and macOS will ask whether this terminal may control Safari: click OK."

// safariRunning says whether Safari is open, without asking Safari
// (which would need the person's permission).
func safariRunning() bool {
	return exec.Command("pgrep", "-x", "Safari").Run() == nil
}

// osaRunner runs safariJXA with one request and returns its reply.
type osaRunner func(ctx context.Context, req []byte) ([]byte, error)

// How shoulder reaches Safari; tests replace these.
var (
	safariOSA     osaRunner = osascript
	safariCapture           = screencaptureWindow
)

func osascript(ctx context.Context, req []byte) ([]byte, error) {
	// It goes on the command line, which macOS limits to 1 MB in all.
	if len(req) > 512<<10 {
		return nil, errorf(http.StatusRequestEntityTooLarge, "too much for Safari at once (it goes through osascript's command line): keep it under 500 KB")
	}
	cmd := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", safariJXA+safariMain, string(req))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errorf(http.StatusBadGateway, "osascript: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// safariJXA does what shoulder asks of Safari: handle(safari, req), where
// req.op is one of the cases below. Tabs are {window, index, url}: the
// window's id, the tab's place in it (from 1), and the URL it had when
// shoulder last looked, which finds it again if tabs before it closed.
const safariJXA = `
function tabInfo(w, t, i) {
  return { window: w.id(), index: i, count: w.tabs().length, title: t.name() || '', url: t.url() || '' };
}
function windowsOf(safari) {
  const out = [];
  for (const w of safari.windows()) {
    let tabs;
    try { tabs = w.tabs(); } catch (e) { continue; }
    if (!tabs || !tabs.length) continue;
    out.push({ w, tabs });
  }
  return out;
}
// find finds a shared tab: where it was, if it still has its URL; or
// by its URL nearby, if tabs moved; or where it was, if the window has
// the same tabs as before and only that one changed (it navigated).
// Otherwise it's gone, rather than risk acting on a tab nobody shared.
function find(safari, ref) {
  for (const { w, tabs } of windowsOf(safari)) {
    if (w.id() !== ref.window) continue;
    const at = tabs[ref.index - 1];
    if (at && (!ref.url || at.url() === ref.url)) return { w, t: at, index: ref.index };
    let best = -1;
    if (ref.url) for (let i = 0; i < tabs.length; i++) {
      if (tabs[i].url() === ref.url && (best < 0 || Math.abs(i + 1 - ref.index) < Math.abs(best + 1 - ref.index))) best = i;
    }
    if (best >= 0) return { w, t: tabs[best], index: best + 1 };
    if (at && ref.count === tabs.length) return { w, t: at, index: ref.index };
    return null;
  }
  return null;
}
function mustFind(safari, ref) {
  const f = find(safari, ref);
  if (!f) throw new Error('GONE');
  return f;
}
function handle(safari, req) {
  if (req.op === 'running') return { running: safari.running() };
  if (!safari.running()) throw new Error('NOT_RUNNING');
  switch (req.op) {
  case 'list': {
    const out = [];
    for (const { w, tabs } of windowsOf(safari)) {
      const current = w.currentTab().index();
      const win = { id: w.id(), index: w.index(), current, tabs: [] };
      tabs.forEach((t, i) => win.tabs.push(tabInfo(w, t, i + 1)));
      out.push(win);
    }
    return { windows: out };
  }
  case 'tabs':
    return { tabs: req.tabs.map(ref => {
      const f = find(safari, ref);
      if (!f) return null;
      const info = tabInfo(f.w, f.t, f.index);
      info.current = f.w.currentTab().index() === f.index;
      return info;
    }) };
  case 'js': {
    const f = mustFind(safari, req.tab);
    const started = safari.doJavaScript(req.code, { in: f.t });
    if (started !== 'started') return { tab: tabInfo(f.w, f.t, f.index), failed: true };
    const poll = '(() => { const v = window[' + JSON.stringify(req.key) + ']; if (v === undefined) return "__shoulder_gone__"; if (v === null) return ""; delete window[' + JSON.stringify(req.key) + ']; return v; })()';
    const deadline = Date.now() + req.timeout * 1000;
    for (;;) {
      const r = safari.doJavaScript(poll, { in: f.t });
      if (r === '__shoulder_gone__' || r === undefined || r === null) return { tab: tabInfo(f.w, f.t, f.index), navigated: true };
      if (r) return { tab: tabInfo(f.w, f.t, f.index), value: r };
      if (Date.now() > deadline) return { tab: tabInfo(f.w, f.t, f.index), timeout: true };
      delay(0.05);
    }
  }
  case 'navigate': {
    const f = mustFind(safari, req.tab);
    f.t.url = req.url;
    return { tab: { window: f.w.id(), index: f.index, count: f.w.tabs().length, title: '', url: req.url } };
  }
  case 'open': {
    let w = null;
    if (req.window) w = windowsOf(safari).map(x => x.w).find(x => x.id() === req.window) || null;
    if (!w) {
      const all = windowsOf(safari);
      if (all.length) w = all[0].w;
      else { // No windows: make one, and use its tab.
        safari.Document().make();
        for (let i = 0; i < 30 && !windowsOf(safari).length; i++) delay(0.1);
        const made = windowsOf(safari);
        if (!made.length) throw new Error('NO_WINDOWS');
        w = made[0].w;
        made[0].tabs[0].url = req.url;
        return { tab: { window: w.id(), index: 1, count: 1, title: '', url: req.url } };
      }
    }
    const t = safari.Tab({ url: req.url });
    w.tabs.push(t);
    const tabs = w.tabs();
    const nt = tabs[tabs.length - 1];
    if (!req.background) w.currentTab = nt;
    return { tab: { window: w.id(), index: tabs.length, count: tabs.length, title: '', url: req.url } };
  }
  case 'close': {
    const f = mustFind(safari, req.tab);
    f.t.close();
    return {};
  }
  case 'activate': {
    const f = mustFind(safari, req.tab);
    f.w.currentTab = f.t;
    f.w.index = 1;
    safari.activate();
    return { tab: tabInfo(f.w, f.t, f.index) };
  }
  case 'front': {
    const f = mustFind(safari, req.tab);
    return { current: f.w.currentTab().index() === f.index, visible: f.w.visible(), miniaturized: f.w.miniaturized() };
  }
  }
  throw new Error('unknown op ' + req.op);
}
`

const safariMain = `
function run(argv) {
  const safari = Application('Safari');
  try { return JSON.stringify(handle(safari, JSON.parse(argv[0]))); }
  catch (e) { return JSON.stringify({ error: String((e && e.message) || e), number: (e && e.errorNumber) || 0 }); }
}
`

// safariRef is where a shared tab is: its window, its place there, how
// many tabs the window had, and its URL, when shoulder last looked.
type safariRef struct {
	Window int    `json:"window"`
	Index  int    `json:"index"`
	Count  int    `json:"count,omitempty"`
	URL    string `json:"url"`
}

type safariTabReply struct {
	Window  int    `json:"window"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Current bool   `json:"current"`
}

type safariTab struct {
	id       string
	ref      safariRef
	title    string
	openedBy string
	inFront  bool
}

type safariBrowser struct {
	osa osaRunner
	// capture screenshots a window by its id (screencapture -l).
	capture func(ctx context.Context, window int, png bool) ([]byte, error)

	mu      sync.Mutex
	all     bool
	shared  []*safariTab
	current *safariTab
	next    int
	gone    chan struct{}
	stop    chan struct{}
	stopped bool
}

// connectSafari checks Safari is running and shoulder may script it (the
// first time, macOS asks the person).
func connectSafari(ctx context.Context, osa osaRunner) (*safariBrowser, error) {
	s := &safariBrowser{osa: osa, capture: safariCapture, gone: make(chan struct{}), stop: make(chan struct{})}
	if _, err := s.candidates(ctx); err != nil {
		return nil, err
	}
	go s.watch()
	return s, nil
}

// watch notices Safari quitting.
func (s *safariBrowser) watch() {
	for {
		select {
		case <-s.stop:
			return
		case <-time.After(3 * time.Second):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var r struct{ Running bool }
		err := s.call(ctx, map[string]any{"op": "running"}, &r)
		cancel()
		if err == nil && !r.Running {
			close(s.gone)
			return
		}
	}
}

// call sends Safari one request.
func (s *safariBrowser) call(ctx context.Context, req map[string]any, reply any) error {
	b, _ := json.Marshal(req)
	out, err := s.osa(ctx, b)
	if err != nil {
		if ctx.Err() != nil {
			return errorf(http.StatusGatewayTimeout, "Safari didn't answer in time (an open alert or dialog in the page stops it; ask the person)")
		}
		return err
	}
	var e struct {
		Error  *string
		Number int
	}
	if err := json.Unmarshal(out, &e); err != nil {
		return fmt.Errorf("odd reply from osascript: %q", short(string(out), 200))
	}
	if e.Error != nil {
		return safariError(*e.Error, e.Number)
	}
	if reply != nil {
		return json.Unmarshal(out, reply)
	}
	return nil
}

func safariError(msg string, number int) error {
	switch {
	case msg == "NOT_RUNNING":
		return errorf(http.StatusServiceUnavailable, "Safari isn't running")
	case msg == "GONE":
		return errorf(http.StatusNotFound, "that tab is gone (closed, or moved to another window)")
	case msg == "NO_WINDOWS":
		return errorf(http.StatusConflict, "Safari has no windows open")
	case number == -1743 || strings.Contains(msg, "Not authorized"):
		return errorf(http.StatusForbidden, "macOS doesn't let shoulder control Safari: allow it in System Settings › Privacy & Security › Automation, under the terminal app, then try again")
	case strings.Contains(msg, "Allow JavaScript from Apple Events"):
		return errorf(http.StatusForbidden, "Safari doesn't allow JavaScript from Apple Events: the person needs to turn on Develop › \"Allow JavaScript from Apple Events\" (Settings › Advanced › \"Show features for web developers\" shows the Develop menu)")
	case number == -600 || number == -609:
		return errBrowserGone
	}
	return errorf(http.StatusBadGateway, "Safari: %s", msg)
}

func (s *safariBrowser) label() string         { return "Safari" }
func (s *safariBrowser) done() <-chan struct{} { return s.gone }

func (s *safariBrowser) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
}

type safariWindow struct {
	ID      int              `json:"id"`
	Index   int              `json:"index"`
	Current int              `json:"current"`
	Tabs    []safariTabReply `json:"tabs"`
}

func (s *safariBrowser) list(ctx context.Context) ([]safariWindow, error) {
	var r struct{ Windows []safariWindow }
	err := s.call(ctx, map[string]any{"op": "list"}, &r)
	return r.Windows, err
}

// candidates lists Safari's tabs. Only the front window's current tab is
// active: Safari says which window is in front, so that's the tab the
// person was on.
func (s *safariBrowser) candidates(ctx context.Context) ([]candidate, error) {
	wins, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, w := range wins {
		for _, t := range w.Tabs {
			out = append(out, candidate{
				ID: fmt.Sprintf("%d:%d:%d:%s", w.ID, t.Index, len(w.Tabs), t.URL), Title: t.Title, URL: t.URL,
				Active: w.Index == 1 && t.Index == w.Current, Window: w.ID,
			})
		}
	}
	return out, nil
}

func parseSafariID(id string) (safariRef, error) {
	parts := strings.SplitN(id, ":", 4)
	if len(parts) == 4 {
		w, err1 := strconv.Atoi(parts[0])
		i, err2 := strconv.Atoi(parts[1])
		n, err3 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil && err3 == nil {
			return safariRef{w, i, n, parts[3]}, nil
		}
	}
	return safariRef{}, fmt.Errorf("no Safari tab %q", id)
}

func (s *safariBrowser) shareCandidate(ctx context.Context, id string) error {
	ref, err := parseSafariID(id)
	if err != nil {
		return err
	}
	t := s.add(ref, "", "")
	// Check now that Safari runs JavaScript for shoulder.
	if _, err := s.run(ctx, t.id, "return 1;", nil); err != nil {
		s.remove(t)
		return err
	}
	return nil
}

func (s *safariBrowser) shareAll(ctx context.Context) error {
	cands, err := s.candidates(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.all = true
	s.mu.Unlock()
	for _, c := range cands {
		ref, _ := parseSafariID(c.ID)
		t := s.add(ref, c.Title, "")
		if c.Active {
			s.mu.Lock()
			s.current = t
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *safariBrowser) add(ref safariRef, title, openedBy string) *safariTab {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	t := &safariTab{id: "t" + strconv.Itoa(s.next), ref: ref, title: title, openedBy: openedBy}
	s.shared = append(s.shared, t)
	if s.current == nil || openedBy != "" {
		s.current = t
	}
	return t
}

func (s *safariBrowser) remove(t *safariTab) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.shared {
		if x == t {
			s.shared = append(s.shared[:i], s.shared[i+1:]...)
			break
		}
	}
	if s.current == t {
		s.current = nil
		if n := len(s.shared); n > 0 {
			s.current = s.shared[n-1]
		}
	}
}

func (s *safariBrowser) find(id string) (*safariTab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		if s.current == nil {
			return nil, errorf(http.StatusNotFound, "no tabs are shared now (the person closed them?); POST open starts a new one")
		}
		return s.current, nil
	}
	for _, t := range s.shared {
		if t.id == id {
			return t, nil
		}
	}
	return nil, errorf(http.StatusNotFound, "no shared tab %s; GET state lists them", id)
}

// update records where a tab is now, from a reply.
func (s *safariBrowser) update(t *safariTab, r *safariTabReply) {
	if r == nil || r.Window == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.ref = safariRef{r.Window, r.Index, r.Count, r.URL}
	if r.Title != "" {
		t.title = r.Title
	}
}

// tabCall makes a request about one tab, and drops the tab if it's gone.
func (s *safariBrowser) tabCall(ctx context.Context, t *safariTab, req map[string]any, reply any) error {
	s.mu.Lock()
	req["tab"] = t.ref
	s.mu.Unlock()
	err := s.call(ctx, req, reply)
	var he *httpError
	if errors.As(err, &he) && he.code == http.StatusNotFound {
		s.remove(t)
		return errorf(http.StatusNotFound, "%s is gone (closed, or moved to another window)", t.id)
	}
	return err
}

func (s *safariBrowser) tabs() []tabInfo {
	s.mu.Lock()
	shared := append([]*safariTab(nil), s.shared...)
	all := s.all
	refs := make([]safariRef, len(shared))
	for i, t := range shared {
		refs[i] = t.ref
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r struct{ Tabs []*safariTabReply }
	if err := s.call(ctx, map[string]any{"op": "tabs", "tabs": refs}, &r); err == nil && len(r.Tabs) == len(shared) {
		for i, t := range shared {
			if r.Tabs[i] == nil {
				s.remove(t)
				continue
			}
			s.update(t, r.Tabs[i])
			s.mu.Lock()
			t.inFront = r.Tabs[i].Current
			s.mu.Unlock()
		}
	}
	if all { // Safari doesn't say when tabs open: look.
		if cands, err := s.candidates(ctx); err == nil {
			for _, c := range cands {
				ref, _ := parseSafariID(c.ID)
				if !s.has(ref) {
					s.add(ref, c.Title, "")
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []tabInfo{}
	for _, t := range s.shared {
		out = append(out, tabInfo{ID: t.id, Title: t.title, URL: t.ref.URL, Current: t == s.current, OpenedBy: t.openedBy})
	}
	return out
}

func (s *safariBrowser) has(ref safariRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.shared {
		if t.ref.Window == ref.Window && t.ref.Index == ref.Index {
			return true
		}
	}
	return false
}

func (s *safariBrowser) setCurrent(id string) error {
	t, err := s.find(id)
	if err == nil {
		s.mu.Lock()
		s.current = t
		s.mu.Unlock()
	}
	return err
}

func randomKey() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "__shoulder_" + hex.EncodeToString(b)
}

// safariScript wraps an async function body for do JavaScript, which
// doesn't wait for promises: it starts the function, which leaves its
// result in the page for safariJXA to collect. It also makes sure the
// console and network recorder is there.
func safariScript(key, body string, args any) (string, error) {
	a, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	k, _ := json.Marshal(key)
	return "(() => {\n" + pageRecorder + "\nconst KEY = " + string(k) + ";\nwindow[KEY] = null;\n" +
		"(async (args) => {\n" + body + "\n})(" + string(a) + ").then(\n" +
		"  v => { window[KEY] = JSON.stringify({ value: v === undefined ? null : v }); },\n" +
		"  e => { window[KEY] = JSON.stringify({ error: String((e && e.message) || e) }); });\n" +
		"return 'started';\n})()", nil
}

func (s *safariBrowser) run(ctx context.Context, id, script string, args any) (json.RawMessage, error) {
	t, err := s.find(id)
	if err != nil {
		return nil, err
	}
	key := randomKey()
	code, err := safariScript(key, script, args)
	if err != nil {
		return nil, err
	}
	timeout := 30.0
	if d, ok := ctx.Deadline(); ok {
		timeout = max(time.Until(d).Seconds()-1, 0.5)
	}
	var r struct {
		Tab       *safariTabReply
		Value     *string
		Navigated bool
		Timeout   bool
		Failed    bool
	}
	if err := s.tabCall(ctx, t, map[string]any{"op": "js", "code": code, "key": key, "timeout": timeout}, &r); err != nil {
		return nil, err
	}
	s.update(t, r.Tab)
	switch {
	case r.Failed:
		return nil, errorf(http.StatusBadRequest, "Safari couldn't run that: a syntax error? (Safari doesn't say)")
	case r.Navigated:
		return json.RawMessage("null"), nil // the page went away mid-call
	case r.Timeout:
		return nil, errorf(http.StatusGatewayTimeout, "timed out: the page didn't answer in time")
	case r.Value == nil:
		return json.RawMessage("null"), nil
	}
	var v struct {
		Value json.RawMessage
		Error *string
	}
	if err := json.Unmarshal([]byte(*r.Value), &v); err != nil {
		return nil, fmt.Errorf("odd reply from the page: %q", short(*r.Value, 200))
	}
	if v.Error != nil {
		return nil, errorf(http.StatusBadRequest, "the page threw: %s", *v.Error)
	}
	return v.Value, nil
}

func (s *safariBrowser) js(ctx context.Context, id, code string) (string, error) {
	raw, err := s.run(ctx, id, "return await "+userCode(code, isExpression(code))+";", nil)
	if err != nil {
		return "", err
	}
	var out string
	json.Unmarshal(raw, &out)
	return out, nil
}

func (s *safariBrowser) navigate(ctx context.Context, id, u string) error {
	t, err := s.find(id)
	if err != nil {
		return err
	}
	var r struct{ Tab *safariTabReply }
	if err := s.tabCall(ctx, t, map[string]any{"op": "navigate", "url": u}, &r); err != nil {
		return err
	}
	s.update(t, r.Tab)
	return nil
}

func (s *safariBrowser) history(ctx context.Context, id, action string) error {
	js := map[string]string{"back": "history.back()", "forward": "history.forward()", "reload": "location.reload()", "stop": "window.stop()"}[action]
	_, err := s.run(ctx, id, "setTimeout(() => "+js+", 0); return null;", nil)
	return err
}

func (s *safariBrowser) screenshot(ctx context.Context, id string, png bool) ([]byte, string, error) {
	t, err := s.find(id)
	if err != nil {
		return nil, "", err
	}
	var r struct{ Current, Visible, Miniaturized bool }
	if err := s.tabCall(ctx, t, map[string]any{"op": "front"}, &r); err != nil {
		return nil, "", err
	}
	if !r.Current || !r.Visible || r.Miniaturized {
		return nil, "", errorf(http.StatusConflict, "%s isn't showing (another tab is in front of it, or its window is hidden), and Safari can only be photographed as a window: POST focus?tab=%s brings it to the front, which changes what the person sees; GET page reads it without that", t.id, t.id)
	}
	s.mu.Lock()
	win := t.ref.Window
	s.mu.Unlock()
	img, err := s.capture(ctx, win, png)
	if err != nil {
		return nil, "", err
	}
	return img, "the whole Safari window, toolbar included, in screen pixels (2 per CSS pixel on Retina screens); click by element number from page rather than by x,y", nil
}

// screencaptureWindow photographs a window. Safari's window ids are the
// window server's. It needs Screen Recording permission; without it
// macOS asks, or the picture is only the desktop.
func screencaptureWindow(ctx context.Context, window int, png bool) ([]byte, error) {
	f, err := os.CreateTemp("", "shoulder-shot-*")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	format := "jpg"
	if png {
		format = "png"
	}
	out, err := exec.CommandContext(ctx, "screencapture", "-x", "-o", "-t", format, "-l", strconv.Itoa(window), f.Name()).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("screencapture: %v: %s", err, strings.TrimSpace(string(out)))
	}
	img, err := os.ReadFile(f.Name())
	if err == nil && len(img) == 0 {
		err = errorf(http.StatusForbidden, "no screenshot: shoulder needs Screen Recording permission (System Settings › Privacy & Security › Screen Recording, for the terminal app)")
	}
	return img, err
}

func (s *safariBrowser) click(context.Context, string, float64, float64, int, keyMods) error {
	return errNoRealInput
}
func (s *safariBrowser) insertText(context.Context, string, string) error { return errNoRealInput }
func (s *safariBrowser) key(context.Context, string, keyPress) error      { return errNoRealInput }

func (s *safariBrowser) open(ctx context.Context, u string, background bool) (string, error) {
	win := 0
	if cur, err := s.find(""); err == nil {
		s.mu.Lock()
		win = cur.ref.Window
		s.mu.Unlock()
	}
	var r struct{ Tab *safariTabReply }
	if err := s.call(ctx, map[string]any{"op": "open", "url": u, "window": win, "background": background}, &r); err != nil {
		return "", err
	}
	if r.Tab == nil {
		return "", errors.New("Safari didn't say where the tab is")
	}
	t := s.add(safariRef{r.Tab.Window, r.Tab.Index, r.Tab.Count, r.Tab.URL}, "", "agent")
	return t.id, nil
}

func (s *safariBrowser) closeTab(ctx context.Context, id string) error {
	t, err := s.find(id)
	if err != nil {
		return err
	}
	if t.openedBy == "" {
		return errorf(http.StatusForbidden, "%s is the person's own tab; shoulder only closes tabs the agent opened", t.id)
	}
	if err := s.tabCall(ctx, t, map[string]any{"op": "close"}, nil); err != nil {
		return err
	}
	s.remove(t)
	return nil
}

func (s *safariBrowser) activate(ctx context.Context, id string) error {
	t, err := s.find(id)
	if err != nil {
		return err
	}
	var r struct{ Tab *safariTabReply }
	if err := s.tabCall(ctx, t, map[string]any{"op": "activate"}, &r); err != nil {
		return err
	}
	s.update(t, r.Tab)
	return nil
}

func (s *safariBrowser) dialog(context.Context, string, bool, string) error {
	return errorf(http.StatusNotImplemented, "shoulder can't see or answer Safari's dialogs; ask the person to")
}

func (s *safariBrowser) console(ctx context.Context, id, level string, clear bool) (string, error) {
	raw, err := s.run(ctx, id, `const f = window.__shoulder_devtools; return f ? f("logs", args.clear) : null;`, map[string]any{"clear": clear})
	if err != nil {
		return "", err
	}
	var r struct {
		Entries []struct {
			T                   float64
			Level, Source, Text string
		}
		Dropped int
	}
	json.Unmarshal(raw, &r)
	var entries []consoleEntry
	for _, e := range r.Entries {
		entries = append(entries, consoleEntry{time.UnixMilli(int64(e.T)), e.Level, e.Source, e.Text})
	}
	return "(Safari: recorded since shoulder first looked at this page)\n" + formatConsole(entries, r.Dropped, level), nil
}

func (s *safariBrowser) network(ctx context.Context, id, filter string, all, clear bool) (string, error) {
	raw, err := s.run(ctx, id, "const f = window.__shoulder_devtools;\nconst calls = f ? f(\"requests\", args.clear).entries : [];\nconst res = (async () => {"+perfNetworkScript+"})();\nreturn { calls, res: await res };", map[string]any{"clear": clear})
	if err != nil {
		return "", err
	}
	var r struct {
		Calls []struct {
			T                 float64
			Kind, Method, URL string
			Status            int
			MS                *float64
			Error             string
		}
		Res json.RawMessage
	}
	json.Unmarshal(raw, &r)
	var entries []*netEntry
	seen := map[string]bool{}
	for _, c := range r.Calls {
		seen[c.URL] = true
	}
	for _, e := range perfEntries(r.Res) {
		if (e.typ == "fetch" || e.typ == "xhr") && seen[e.url] {
			continue
		}
		entries = append(entries, e)
	}
	for _, c := range r.Calls {
		e := &netEntry{method: c.Method, url: c.URL, typ: c.Kind, status: c.Status, failed: c.Error, start: time.UnixMilli(int64(c.T))}
		if c.MS != nil {
			e.done, e.end = true, e.start.Add(time.Duration(*c.MS)*time.Millisecond)
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].start.Before(entries[j].start) })
	return "(Safari: fetch and XHR calls are recorded from when shoulder first looked at this page; other loads come from the page's Performance API)\n" + formatNetwork(entries, filter, all), nil
}

func (s *safariBrowser) networkIdle(string, time.Duration) (bool, bool) { return true, false }

// pageRecorder keeps console messages, errors, and fetch/XHR calls in
// the page, for browsers with no protocol to report them (Safari). It's
// installed once per page, the first time shoulder runs anything there.
// After Eyebrows' PageDevTools.
const pageRecorder = `
if (!Object.prototype.hasOwnProperty.call(window, "__shoulder_devtools")) (() => {
  const MAX = 1000, logs = [], requests = [];
  let droppedLogs = 0, droppedRequests = 0;
  const short = (s, n) => s.length > n ? s.slice(0, n) + "…" : s;
  const describe = (v, depth) => {
    try {
      if (typeof v === "string") return short(v, 2000);
      if (v === undefined) return "undefined";
      if (v === null || typeof v !== "object" && typeof v !== "function") return String(v);
      if (typeof v === "function") return "ƒ " + (v.name || "anonymous") + "()";
      if (v instanceof Error) return short((v.name || "Error") + ": " + v.message, 3000);
      if (typeof Element !== "undefined" && v instanceof Element) return "<" + v.tagName.toLowerCase() + (v.id ? " id=\"" + v.id + "\"" : "") + ">";
      if (depth > 1) return Array.isArray(v) ? "[…]" : "{…}";
      if (Array.isArray(v)) return "[" + v.slice(0, 20).map(x => describe(x, depth + 1)).join(", ") + (v.length > 20 ? ", …" : "") + "]";
      const keys = Object.keys(v);
      return short("{" + keys.slice(0, 20).map(k => k + ": " + describe(v[k], depth + 1)).join(", ") + (keys.length > 20 ? ", …" : "") + "}", 2000);
    } catch (e) { return "[unprintable]"; }
  };
  const push = (level, text, source) => {
    logs.push({ t: Date.now(), level, source, text: short(text, 4000) });
    if (logs.length > MAX) { logs.shift(); droppedLogs++; }
  };
  for (const level of ["log", "info", "warn", "error", "debug"]) {
    const original = console[level];
    if (typeof original !== "function") continue;
    console[level] = function (...args) {
      try { push(level, args.map(a => describe(a, 0)).join(" "), "console"); } catch (e) {}
      return original.apply(this, args);
    };
  }
  addEventListener("error", e => {
    const t = e.target;
    if (t && t !== window && t.nodeType === 1) push("error", "Failed to load " + t.tagName.toLowerCase() + " " + (t.currentSrc || t.src || t.href || ""), "network");
    else push("error", "Uncaught " + (e.error ? describe(e.error, 0) : e.message) + (e.filename ? " (" + e.filename + ":" + e.lineno + ")" : ""), "exception");
  }, true);
  addEventListener("unhandledrejection", e => push("error", "Unhandled promise rejection: " + describe(e.reason, 0), "exception"));
  const record = r => { requests.push(r); if (requests.length > MAX) { requests.shift(); droppedRequests++; } };
  const originalFetch = window.fetch;
  if (typeof originalFetch === "function") {
    window.fetch = function (input, init) {
      const start = performance.now();
      let method = "GET", url = "";
      try {
        method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
        url = new URL(String(input && typeof input === "object" && "url" in input ? input.url : input), location.href).href;
      } catch (e) {}
      const r = { t: Date.now(), kind: "fetch", method, url };
      record(r);
      const p = originalFetch.apply(this, arguments);
      p.then(res => { r.status = res.status; r.ms = Math.round(performance.now() - start); },
             err => { r.error = String((err && err.message) || err); r.ms = Math.round(performance.now() - start); });
      return p;
    };
  }
  const X = window.XMLHttpRequest && XMLHttpRequest.prototype;
  if (X) {
    const open = X.open, send = X.send;
    X.open = function (method, url) {
      try { this.__shoulder = { kind: "xhr", method: String(method).toUpperCase(), url: new URL(String(url), location.href).href }; } catch (e) {}
      return open.apply(this, arguments);
    };
    X.send = function () {
      const r = this.__shoulder;
      if (r) {
        const start = performance.now();
        r.t = Date.now();
        record(r);
        this.addEventListener("loadend", () => {
          r.ms = Math.round(performance.now() - start);
          if (this.status) r.status = this.status; else r.error = "failed (network error, CORS, or aborted)";
        });
      }
      return send.apply(this, arguments);
    };
  }
  try { performance.setResourceTimingBufferSize(2000); } catch (e) {}
  Object.defineProperty(window, "__shoulder_devtools", { enumerable: false, value: (what, clear) => {
    const out = what === "requests" ? { entries: requests.slice(), dropped: droppedRequests } : { entries: logs.slice(), dropped: droppedLogs };
    if (clear) { if (what === "requests") { requests.length = 0; droppedRequests = 0; } else { logs.length = 0; droppedLogs = 0; } }
    return out;
  } });
})();
`
