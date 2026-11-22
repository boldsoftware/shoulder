package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// A browser session shares tabs of the person's own browser, instead of
// a terminal: agents read pages, take screenshots, click, type, run
// JavaScript. browser is what a browser (Chrome, Safari) provides; the
// endpoints build on it. Tab ids are shoulder's own (t1, t2, ...), and
// only shared tabs have one: the tab the person picked, tabs the agent
// opens, and popups those open. tab "" means the current tab.
type browser interface {
	label() string // "Chrome", "Safari", ...
	tabs() []tabInfo
	// setCurrent makes tab the one calls without ?tab= use.
	setCurrent(tab string) error

	// run runs one of shoulder's page scripts (pagescripts.go), the body
	// of an async function of args, and returns its value as JSON. Where
	// the browser allows, it runs apart from the page's own scripts.
	run(ctx context.Context, tab, script string, args any) (json.RawMessage, error)
	// js runs the agent's code in the page and returns its value as JSON.
	js(ctx context.Context, tab, code string) (string, error)

	navigate(ctx context.Context, tab, url string) error
	history(ctx context.Context, tab, action string) error // back, forward, reload, stop
	screenshot(ctx context.Context, tab string, png bool) (img []byte, note string, err error)

	// Real input, as if from the mouse and keyboard; errNoRealInput where
	// the browser has none, and the endpoints use page scripts instead.
	// x and y are CSS pixels in the viewport.
	click(ctx context.Context, tab string, x, y float64, count int, mods keyMods) error
	insertText(ctx context.Context, tab, text string) error
	key(ctx context.Context, tab string, k keyPress) error

	open(ctx context.Context, url string, background bool) (string, error)
	closeTab(ctx context.Context, tab string) error
	activate(ctx context.Context, tab string) error
	dialog(ctx context.Context, tab string, accept bool, text string) error

	console(ctx context.Context, tab, level string, clear bool) (string, error)
	network(ctx context.Context, tab, filter string, all, clear bool) (string, error)
	// networkIdle reports whether nothing has been loading for d, and
	// whether the browser can tell at all.
	networkIdle(tab string, d time.Duration) (idle, known bool)

	done() <-chan struct{} // closed when the browser connection ends
	close()
}

type tabInfo struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Current  bool   `json:"current,omitempty"`
	Loading  bool   `json:"loading,omitempty"`
	OpenedBy string `json:"opened_by,omitempty"` // "agent", or the tab whose popup this is
	Dialog   string `json:"dialog,omitempty"`    // an open alert/confirm/prompt
}

type keyMods struct{ alt, ctrl, meta, shift bool }

// bits is the CDP modifiers bitmask.
func (m keyMods) bits() int {
	b := 0
	if m.alt {
		b |= 1
	}
	if m.ctrl {
		b |= 2
	}
	if m.meta {
		b |= 4
	}
	if m.shift {
		b |= 8
	}
	return b
}

var errNoRealInput = errors.New("no real input in this browser")

// httpError is an error with the status the agent should get.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errorf(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

func writeError(w http.ResponseWriter, err error) {
	var he *httpError
	var de *dialogError
	var we *waitError
	switch {
	case errors.As(err, &he):
		http.Error(w, he.msg, he.code)
	case errors.As(err, &we):
		http.Error(w, we.msg, http.StatusConflict)
	case errors.As(err, &de):
		http.Error(w, de.Error(), http.StatusConflict)
	case errors.Is(err, errBrowserGone):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "timed out: the page didn't answer in time", http.StatusGatewayTimeout)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// browserLabels names the browsers a session can share.
var browserLabels = map[string]string{"chrome": "Chrome", "safari": "Safari"}

// browserAgent serves the agent API of a browser session.
func (s *server) browserAgent(w http.ResponseWriter, r *http.Request, sh *share, token string, readOnly bool) {
	rest := r.PathValue("rest")
	if rest == "" && r.Method == "GET" {
		s.mu.Lock()
		cfg := *s.cfg
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		io.WriteString(w, browserGuide(&cfg, sh, token, readOnly))
		return
	}
	get := map[string]func(context.Context, browser, string, url.Values) (string, error){
		"page": getPage, "html": getHTML, "console": getConsole, "network": getNetwork, "wait": getWait,
	}
	post := map[string]func(context.Context, browser, string, url.Values, string) (string, error){
		"navigate": postNavigate, "click": postClick, "type": postType, "key": postKey, "scroll": postScroll,
		"js": postJS, "open": postOpen, "close": postClose, "focus": postFocus, "dialog": postDialog,
	}
	getFn, isGet := get[rest]
	postFn, isPost := post[rest]
	if readOnly && r.URL.Query().Has("clear") {
		// Clearing is for whoever acts; a read-only code only looks.
		q := r.URL.Query()
		q.Del("clear")
		r.URL.RawQuery = q.Encode()
	}
	switch {
	case r.Method == "GET" && (isGet || rest == "state" || rest == "screenshot"):
	case r.Method == "POST" && isPost:
		if readOnly {
			http.Error(w, "this code is read-only: you can look at the shared tabs but not act in them", http.StatusForbidden)
			return
		}
	default:
		http.Error(w, "no such endpoint; GET "+sh.url(token)+"/ describes the API", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	b := s.browser
	s.mu.Unlock()
	if rest == "state" {
		writeJSON(w, s.browserState(b, readOnly))
		return
	}
	if b == nil {
		http.Error(w, "the browser isn't shared yet: the person is still choosing on shoulder's first screen; try again shortly", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	tab := q.Get("tab")
	var body string
	if r.Method == "POST" {
		bb, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = string(bb)
	}
	s.logActivity(readOnly, describeCall(r.Method, rest, tab, q, body))

	limit := 30 * time.Second
	if rest == "wait" || rest == "js" {
		limit = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	defer cancel()
	if rest == "screenshot" {
		img, note, err := b.screenshot(ctx, tab, q.Get("format") == "png")
		if err != nil {
			writeError(w, err)
			return
		}
		if note != "" {
			w.Header().Set("X-Shoulder-Note", note)
		}
		w.Header().Set("Content-Type", http.DetectContentType(img))
		w.Write(img)
		return
	}
	var out string
	var err error
	if isGet {
		out, err = getFn(ctx, b, tab, q)
	} else {
		out, err = postFn(ctx, b, tab, q, body)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, strings.TrimRight(out, "\n")+"\n")
}

type browserStatus struct {
	Browser  string    `json:"browser"`
	Name     string    `json:"session"`
	Host     string    `json:"host"`
	Shared   bool      `json:"shared"` // false until the person has picked a tab
	Tabs     []tabInfo `json:"tabs"`
	ReadOnly bool      `json:"read_only"`
}

func (s *server) browserState(b browser, readOnly bool) browserStatus {
	s.mu.Lock()
	st := browserStatus{Browser: browserLabels[s.cfg.Browser], Name: s.cfg.Name, Host: s.cfg.Host, ReadOnly: readOnly, Tabs: []tabInfo{}}
	s.mu.Unlock()
	if b != nil {
		st.Browser, st.Shared = b.label(), true
		st.Tabs = b.tabs()
		s.mu.Lock()
		s.browserTabs = len(st.Tabs)
		s.mu.Unlock()
	}
	return st
}

// ---------------------------------------------------------------------------
// Look.

type pageText struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Scroll struct {
		X, Y     int
		Height   int
		Viewport [2]int
	} `json:"scroll"`
	Elements  int    `json:"elements"`
	Total     int    `json:"total"`
	Start     int    `json:"start"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text"`
}

func getPage(ctx context.Context, b browser, tab string, q url.Values) (string, error) {
	args := map[string]any{
		"start":         intParam(q.Get("start"), 0),
		"max_chars":     intParam(q.Get("max_chars"), 20000),
		"viewport_only": q.Get("viewport_only") == "1",
	}
	var p pageText
	if err := runInto(ctx, b, tab, readScript, args, &p); err != nil {
		return "", err
	}
	var o strings.Builder
	fmt.Fprintf(&o, "# %s\n%s\n", cmpOr(p.Title, "(untitled)"), p.URL)
	fmt.Fprintf(&o, "[scrolled to y=%d of %d; viewport %dx%d; %d elements]\n\n", p.Scroll.Y, p.Scroll.Height, p.Scroll.Viewport[0], p.Scroll.Viewport[1], p.Elements)
	o.WriteString(p.Text)
	if p.Truncated {
		next := p.Start + jsLen(p.Text)
		fmt.Fprintf(&o, "\n\n[characters %d-%d of %d: page?start=%d for more]", p.Start, next, p.Total, next)
	}
	return o.String(), nil
}

func getHTML(ctx context.Context, b browser, tab string, q url.Values) (string, error) {
	args := map[string]any{
		"selector": q.Get("selector"),
		"start":    intParam(q.Get("start"), 0),
		"max":      intParam(q.Get("max_chars"), 50000),
	}
	var h *struct {
		HTML  string `json:"html"`
		Total int    `json:"total"`
	}
	if err := runInto(ctx, b, tab, htmlScript, args, &h); err != nil {
		return "", err
	}
	if h == nil {
		return "", errorf(http.StatusNotFound, "no element matches %s", q.Get("selector"))
	}
	out := h.HTML
	if end := args["start"].(int) + jsLen(h.HTML); end < h.Total {
		out += fmt.Sprintf("\n[characters %d-%d of %d: html?start=%d for more]", args["start"], end, h.Total, end)
	}
	return out, nil
}

// jsLen is a string's length in JavaScript: UTF-16 code units, which
// the page scripts' start and total count in.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func getConsole(ctx context.Context, b browser, tab string, q url.Values) (string, error) {
	return b.console(ctx, tab, q.Get("level"), q.Get("clear") == "1")
}

func getNetwork(ctx context.Context, b browser, tab string, q url.Values) (string, error) {
	return b.network(ctx, tab, q.Get("filter"), q.Get("all") == "1", q.Get("clear") == "1")
}

// getWait polls until the page is loaded (always), and the text, selector,
// gone, and idle conditions hold, or the timeout passes.
func getWait(ctx context.Context, b browser, tab string, q url.Values) (string, error) {
	timeout := durationParam(q.Get("timeout"), 10*time.Second, 2*time.Minute)
	deadline := time.Now().Add(timeout)
	args := map[string]any{"text": q.Get("text"), "selector": q.Get("selector"), "gone": q.Get("gone")}
	idle := q.Get("idle") == "1"
	for {
		ok, err := pageReady(ctx, b, tab)
		if err != nil {
			return "", err
		}
		if ok {
			var met bool
			if err := runInto(ctx, b, tab, checkScript, args, &met); err != nil && !transient(err) {
				return "", err
			}
			if met && idle {
				if quiet, known := b.networkIdle(tab, 500*time.Millisecond); known {
					met = quiet
				}
			}
			if met {
				return "[ready] " + nowLine(b, tab), nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("[timeout after %s] %s", timeout, nowLine(b, tab)), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// pageReady reports whether the tab's document has loaded.
func pageReady(ctx context.Context, b browser, tab string) (bool, error) {
	var st struct{ State string }
	if err := runInto(ctx, b, tab, loadStateScript, nil, &st); err != nil {
		if transient(err) {
			return false, nil
		}
		return false, err
	}
	return st.State == "complete", nil
}

// transient errors come from a page in the middle of navigating.
func transient(err error) bool {
	var he *httpError
	var de *dialogError
	if errors.As(err, &he) || errors.As(err, &de) || errors.Is(err, errBrowserGone) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// settle gives an action's effects a moment, then waits (briefly) for a
// navigation it started.
func settle(ctx context.Context, b browser, tab string) {
	sleepCtx(ctx, 350*time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok, err := pageReady(ctx, b, tab); ok || err != nil {
			return
		}
		sleepCtx(ctx, 250*time.Millisecond)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// nowLine says where the tab is: its title and URL.
func nowLine(b browser, tab string) string {
	for _, t := range b.tabs() {
		if t.ID == tab || (tab == "" && t.Current) {
			s := fmt.Sprintf("%s: %q %s", t.ID, t.Title, t.URL)
			if t.Dialog != "" {
				s += "\n[a dialog is open: " + t.Dialog + "; POST dialog answers it]"
			}
			return s
		}
	}
	return "(the tab has closed)"
}

func runInto(ctx context.Context, b browser, tab, script string, args, v any) error {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := b.run(ctx, tab, script, args)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// ---------------------------------------------------------------------------
// Act.

func postNavigate(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	target := strings.TrimSpace(body)
	switch target {
	case "":
		return "", errorf(http.StatusBadRequest, "the body is the URL to go to, or back, forward, reload, or stop")
	case "back", "forward", "reload", "stop":
		if err := b.history(ctx, tab, target); err != nil {
			return "", err
		}
	default:
		u, err := interpretURL(target)
		if err != nil {
			return "", err
		}
		if err := b.navigate(ctx, tab, u); err != nil {
			return "", err
		}
	}
	settle(ctx, b, tab)
	return nowLine(b, tab), nil
}

// interpretURL turns what an agent might type into an address bar into a
// URL: example.com is https, localhost:8080 is http.
func interpretURL(s string) (string, error) {
	if i := strings.Index(s, ":"); i > 0 && !strings.ContainsAny(s[:i], "/.") {
		scheme := strings.ToLower(s[:i])
		if _, err := strconv.Atoi(strings.SplitN(s[i+1:], "/", 2)[0]); err != nil || scheme == "about" {
			// It has a scheme (it isn't host:port). Only web pages:
			// file: would read the person's files, chrome: their
			// settings and passwords.
			if scheme == "http" || scheme == "https" || strings.EqualFold(s, "about:blank") {
				return s, nil
			}
			return "", errorf(http.StatusForbidden, "shoulder opens only http and https URLs (and about:blank), not %s:", scheme)
		}
	}
	if strings.ContainsAny(s, " \t\n") {
		return "", errorf(http.StatusBadRequest, "%q isn't a URL", s)
	}
	host := s
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	hostname := host
	hasPort := false
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasPrefix(host, "[") {
		hostname, hasPort = host[:i], true
	}
	local := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") ||
		strings.HasSuffix(hostname, ".test") || strings.HasPrefix(hostname, "[") || regexp.MustCompile(`^[0-9.]+$`).MatchString(hostname)
	if local || (hasPort && !strings.Contains(hostname, ".")) {
		return "http://" + s, nil
	}
	if !strings.Contains(hostname, ".") {
		return "", errorf(http.StatusBadRequest, "%q isn't a URL", s)
	}
	return "https://" + s, nil
}

// elementArgs is what click, type, and scroll act on: an element number from
// page, or a CSS selector.
func elementArgs(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	if n, err := strconv.Atoi(strings.Trim(s, "[]")); err == nil {
		return map[string]any{"element": n}
	}
	return map[string]any{"selector": s}
}

func mergeArgs(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

type located struct {
	X, Y          float64
	Width, Height float64
	Description   string
	Covered       *string
}

func postClick(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	args := elementArgs(body)
	hasTarget := len(args) > 0
	count := 1
	if q.Get("double") == "1" {
		count = 2
	}
	mods, err := parseMods(q.Get("modifiers"))
	if err != nil {
		return "", err
	}
	args["count"], args["meta"], args["ctrl"], args["shift"], args["alt"] = count, mods.meta, mods.ctrl, mods.shift, mods.alt
	var what, note string
	switch {
	case hasTarget:
		var loc located
		if err := runInto(ctx, b, tab, locateScript, args, &loc); err != nil {
			return "", err
		}
		what = loc.Description
		if loc.Covered != nil {
			note = "\n[something else was on top of it, so the click went to " + *loc.Covered + "]"
		}
		err = errNoRealInput
		if loc.Width > 0 && loc.Height > 0 {
			err = b.click(ctx, tab, loc.X, loc.Y, count, mods)
		}
		if errors.Is(err, errNoRealInput) {
			note = ""
			err = runInto(ctx, b, tab, clickScript, args, nil)
		}
		if err != nil {
			return "", err
		}
	case q.Has("x") && q.Has("y"):
		x, errx := strconv.ParseFloat(q.Get("x"), 64)
		y, erry := strconv.ParseFloat(q.Get("y"), 64)
		if errx != nil || erry != nil {
			return "", errorf(http.StatusBadRequest, "x and y are numbers: CSS pixels from the viewport's top left")
		}
		what = fmt.Sprintf("(%g, %g)", x, y)
		err := b.click(ctx, tab, x, y, count, mods)
		if errors.Is(err, errNoRealInput) {
			args["x"], args["y"] = x, y
			var d string
			err = runInto(ctx, b, tab, clickScript, args, &d)
			what += " " + d
		}
		if err != nil {
			return "", err
		}
	default:
		return "", errorf(http.StatusBadRequest, "say what to click: the body is an element number from page or a CSS selector, or use click?x=X&y=Y")
	}
	settle(ctx, b, tab)
	verb := "clicked"
	if count == 2 {
		verb = "double-clicked"
	}
	return verb + " " + what + note + "\nnow " + nowLine(b, tab), nil
}

func parseMods(s string) (keyMods, error) {
	var m keyMods
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == '+' || r == ' ' }) {
		switch w {
		case "alt", "option", "opt":
			m.alt = true
		case "ctrl", "control":
			m.ctrl = true
		case "meta", "cmd", "command":
			m.meta = true
		case "shift":
			m.shift = true
		default:
			return m, errorf(http.StatusBadRequest, "unknown modifier %q (alt, ctrl, meta, shift)", w)
		}
	}
	return m, nil
}

type typed struct {
	Described string `json:"described"`
	Value     string `json:"value"`
	Select    bool   `json:"select"`
}

// postType types the body into an element (?element=N or ?selector=),
// or the focused one.
func postType(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	args := map[string]any{"clear": q.Get("clear") == "1", "prepare_only": true, "text": body}
	if e := q.Get("element"); e != "" {
		args = mergeArgs(args, elementArgs(e))
	} else if sel := q.Get("selector"); sel != "" {
		args["selector"] = sel
	}
	var t typed
	if err := runInto(ctx, b, tab, typeScript, args, &t); err != nil {
		return "", err
	}
	if t.Select {
		args["prepare_only"] = false
		if err := runInto(ctx, b, tab, typeScript, args, &t); err != nil {
			return "", err
		}
		return fmt.Sprintf("chose %q in %s", t.Value, t.Described), nil
	}
	if body != "" {
		err := b.insertText(ctx, tab, body)
		if errors.Is(err, errNoRealInput) {
			err = runInto(ctx, b, tab, typeScript, map[string]any{"text": body}, nil)
		}
		if err != nil {
			return "", err
		}
	}
	if err := runInto(ctx, b, tab, valueScript, nil, &t); err != nil && !transient(err) {
		return "", err
	}
	out := fmt.Sprintf("typed %d characters into %s; it now reads %q", len([]rune(body)), t.Described, t.Value)
	if q.Get("submit") == "1" {
		if err := pressKeys(ctx, b, tab, "Enter", 1); err != nil {
			return "", err
		}
		settle(ctx, b, tab)
		out += ", and pressed Enter\nnow " + nowLine(b, tab)
	}
	return out, nil
}

func postKey(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", errorf(http.StatusBadRequest, "the body names keys, e.g. Enter, Shift+Tab, Meta+a")
	}
	if err := pressKeys(ctx, b, tab, body, intParam(q.Get("repeat"), 1)); err != nil {
		return "", err
	}
	settle(ctx, b, tab)
	return "pressed " + strings.Join(strings.Fields(body), " ") + "\nnow " + nowLine(b, tab), nil
}

func pressKeys(ctx context.Context, b browser, tab, keys string, repeat int) error {
	var presses []keyPress
	for _, w := range strings.Fields(keys) {
		k, err := parseKey(w)
		if err != nil {
			return err
		}
		presses = append(presses, k)
	}
	for range min(max(repeat, 1), 100) {
		for _, k := range presses {
			err := b.key(ctx, tab, k)
			if errors.Is(err, errNoRealInput) {
				args := map[string]any{"key": k.key, "code": k.code, "shift": k.mods.shift, "ctrl": k.mods.ctrl, "alt": k.mods.alt, "meta": k.mods.meta}
				err = runInto(ctx, b, tab, keyScript, args, nil)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func postScroll(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	body = strings.TrimSpace(strings.ToLower(body))
	args := map[string]any{"amount": intParam(q.Get("amount"), 0)}
	switch body {
	case "", "down", "up", "left", "right":
		args["direction"] = cmpOr(body, "down")
	case "top", "bottom":
		args["to"] = body
	default:
		for k, v := range elementArgs(body) {
			args[k] = v
		}
	}
	var pos struct {
		X, Y, Height int
		Viewport     [2]int
	}
	if err := runInto(ctx, b, tab, scrollScript, args, &pos); err != nil {
		return "", err
	}
	sleepCtx(ctx, 200*time.Millisecond)
	return fmt.Sprintf("scrolled to y=%d of %d (viewport %dx%d)", pos.Y, pos.Height, pos.Viewport[0], pos.Viewport[1]), nil
}

func postJS(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", errorf(http.StatusBadRequest, "the body is JavaScript: an expression, or the body of an async function that returns a value")
	}
	return b.js(ctx, tab, body)
}

func postOpen(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	u := "about:blank"
	if s := strings.TrimSpace(body); s != "" {
		var err error
		if u, err = interpretURL(s); err != nil {
			return "", err
		}
	}
	id, err := b.open(ctx, u, q.Get("background") == "1")
	if err != nil {
		return "", err
	}
	settle(ctx, b, id)
	return "opened " + nowLine(b, id) + "\nIt's the current tab now.", nil
}

func postClose(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	if err := b.closeTab(ctx, tab); err != nil {
		return "", err
	}
	return "closed " + cmpOr(tab, "the tab"), nil
}

func postFocus(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	if tab == "" {
		tab = strings.TrimSpace(body)
	}
	if err := b.setCurrent(tab); err != nil {
		return "", err
	}
	if err := b.activate(ctx, tab); err != nil {
		return "", err
	}
	return "current tab: " + nowLine(b, tab), nil
}

func postDialog(ctx context.Context, b browser, tab string, q url.Values, body string) (string, error) {
	var accept bool
	switch strings.TrimSpace(strings.ToLower(body)) {
	case "accept", "ok", "yes":
		accept = true
	case "dismiss", "cancel", "no":
	default:
		return "", errorf(http.StatusBadRequest, "the body is accept or dismiss")
	}
	if err := b.dialog(ctx, tab, accept, q.Get("text")); err != nil {
		return "", err
	}
	settle(ctx, b, tab)
	return "answered the dialog\nnow " + nowLine(b, tab), nil
}

// ---------------------------------------------------------------------------
// Keys.

type keyPress struct {
	key  string // DOM key: "Enter", "a", "A"
	code string // DOM code: "Enter", "KeyA"
	vk   int    // Windows virtual key code
	text string // what it types, if anything
	mods keyMods
}

var namedBrowserKeys = map[string]keyPress{
	"enter":      {key: "Enter", code: "Enter", vk: 13, text: "\r"},
	"tab":        {key: "Tab", code: "Tab", vk: 9},
	"escape":     {key: "Escape", code: "Escape", vk: 27},
	"backspace":  {key: "Backspace", code: "Backspace", vk: 8},
	"delete":     {key: "Delete", code: "Delete", vk: 46},
	"space":      {key: " ", code: "Space", vk: 32, text: " "},
	"arrowup":    {key: "ArrowUp", code: "ArrowUp", vk: 38},
	"arrowdown":  {key: "ArrowDown", code: "ArrowDown", vk: 40},
	"arrowleft":  {key: "ArrowLeft", code: "ArrowLeft", vk: 37},
	"arrowright": {key: "ArrowRight", code: "ArrowRight", vk: 39},
	"home":       {key: "Home", code: "Home", vk: 36},
	"end":        {key: "End", code: "End", vk: 35},
	"pageup":     {key: "PageUp", code: "PageUp", vk: 33},
	"pagedown":   {key: "PageDown", code: "PageDown", vk: 34},
}

var keyAliases = map[string]string{
	"return": "enter", "esc": "escape", "bspace": "backspace", "del": "delete",
	"up": "arrowup", "down": "arrowdown", "left": "arrowleft", "right": "arrowright",
	"pgup": "pageup", "pgdn": "pagedown", "ppage": "pageup", "npage": "pagedown",
}

// parseKey reads a key with modifiers: Enter, Shift+Tab, Meta+a, F5.
func parseKey(s string) (keyPress, error) {
	parts := strings.Split(s, "+")
	name := parts[len(parts)-1]
	if name == "" && len(parts) > 1 { // "Shift++"
		name, parts = "+", parts[:len(parts)-1]
	}
	mods, err := parseMods(strings.Join(parts[:len(parts)-1], ","))
	if err != nil {
		return keyPress{}, err
	}
	lower := strings.ToLower(name)
	if a, ok := keyAliases[lower]; ok {
		lower = a
	}
	k, ok := namedBrowserKeys[lower]
	switch {
	case ok:
	case len(lower) >= 2 && lower[0] == 'f' && lower[1] >= '1' && lower[1] <= '9':
		n, err := strconv.Atoi(lower[1:])
		if err != nil || n > 12 {
			return keyPress{}, errorf(http.StatusBadRequest, "unknown key %q", name)
		}
		k = keyPress{key: "F" + lower[1:], code: "F" + lower[1:], vk: 111 + n}
	case len([]rune(name)) == 1:
		r := []rune(name)[0]
		k = keyPress{key: name, text: name}
		switch {
		case r >= 'a' && r <= 'z':
			k.code, k.vk = "Key"+strings.ToUpper(name), int(r-'a'+'A')
		case r >= 'A' && r <= 'Z':
			k.code, k.vk = "Key"+name, int(r)
			mods.shift = true
		case r >= '0' && r <= '9':
			k.code, k.vk = "Digit"+name, int(r)
		}
		if mods.shift && r >= 'a' && r <= 'z' {
			k.key, k.text = strings.ToUpper(name), strings.ToUpper(name)
		}
	default:
		return keyPress{}, errorf(http.StatusBadRequest, "unknown key %q: use Enter, Tab, Escape, Backspace, Delete, Space, ArrowUp/Down/Left/Right, Home, End, PageUp, PageDown, F1-F12, or one character, with modifiers like Shift+Tab or Meta+a", name)
	}
	k.mods = mods
	if mods.ctrl || mods.meta || (mods.alt && k.code != "") {
		k.text = "" // a shortcut, not typing
	}
	return k, nil
}

// ---------------------------------------------------------------------------
// Activity: what agents did, for the person's screen.

type activity struct {
	Time     time.Time `json:"time"`
	Text     string    `json:"text"`
	ReadOnly bool      `json:"read_only"`
	Count    int       `json:"count"`
}

const maxActivity = 200

func (s *server) logActivity(readOnly bool, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.activity); n > 0 {
		if last := &s.activity[n-1]; last.Text == text && last.ReadOnly == readOnly && time.Since(last.Time) < time.Minute {
			last.Count++
			last.Time = time.Now()
			s.notifyLocked()
			return
		}
	}
	s.activity = append(s.activity, activity{Time: time.Now(), Text: text, ReadOnly: readOnly, Count: 1})
	if len(s.activity) > maxActivity {
		s.activity = s.activity[len(s.activity)-maxActivity:]
	}
	s.notifyLocked()
}

// describeCall is one line about a request, for the person: what the
// agent is doing, without what it types.
func describeCall(method, endpoint, tab string, q url.Values, body string) string {
	s := endpoint
	if method == "GET" {
		s = "looked at " + endpoint
		switch endpoint {
		case "wait":
			s = "waited"
			for _, k := range []string{"text", "selector", "gone"} {
				if v := q.Get(k); v != "" {
					s += fmt.Sprintf(" for %s %q", k, short(v, 40))
				}
			}
		case "page":
			s = "read the page"
		case "screenshot":
			s = "took a screenshot"
		}
	} else {
		body = strings.TrimSpace(body)
		switch endpoint {
		case "navigate", "open":
			s += " " + short(body, 80)
		case "click", "scroll", "key", "dialog":
			if body != "" {
				s += " " + short(body, 60)
			}
			if q.Has("x") {
				s += fmt.Sprintf(" at %s,%s", q.Get("x"), q.Get("y"))
			}
		case "type":
			s = fmt.Sprintf("typed %d characters", len([]rune(body)))
			if e := cmpOr(q.Get("element"), q.Get("selector")); e != "" {
				s += " into " + short(e, 40)
			}
		case "js":
			s = fmt.Sprintf("ran JavaScript (%d lines)", strings.Count(body, "\n")+1)
		}
	}
	if tab != "" {
		s += " in " + tab
	}
	return s
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Starting a browser session (control API).

// A sharer is a connected browser that hasn't shared anything yet.
type sharer interface {
	browser
	candidates(ctx context.Context) ([]candidate, error)
	shareCandidate(ctx context.Context, id string) error
	shareAll(ctx context.Context) error
}

// candidate is a tab the person might share.
type candidate struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"` // the selected tab of its window
	Window int    `json:"window"`
}

// ctlKind sets what the session will share before it starts: a browser
// ("chrome", "safari"), or "" for the terminal. Agents reading the guide
// early get the right one.
func (s *server) ctlKind(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("browser")
	if kind != "" && browserLabels[kind] == "" {
		http.Error(w, "unknown browser "+kind, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil || s.browser != nil {
		http.Error(w, "already started", http.StatusConflict)
		return
	}
	s.cfg.Browser = kind
	s.cfg.save()
	// A browser connected for choosing a tab isn't wanted now.
	if p := s.pending; p != nil && !strings.HasPrefix(s.pendingKind, kind+"/") {
		s.pending = nil
		go p.close()
	}
}

// browserStart is the reply to /ctl/browser: the shared tabs; or, when
// it isn't clear which tab to share, the tabs to choose from; or what
// the person has to do in the browser first (then ask again).
type browserStart struct {
	Label  string      `json:"label"`
	Choose []candidate `json:"choose,omitempty"`
	Tabs   []tabInfo   `json:"tabs,omitempty"`
	Wait   string      `json:"wait,omitempty"`
}

// A profiler is a browser with profiles (Chrome) that shares one of them.
type profiler interface {
	chooseProfile(ctx context.Context, dir string) error
}

// ctlBrowser connects to the browser and shares tabs, per tabs=: active
// (the selected tab, if there's one obvious one), new (a new tab), all,
// or pick (pick=ID, a candidate). For Chrome, profile= is the profile's
// dir ("Default", "Profile 1"; none for the one used last), dir=
// Chrome's user data dir, and standalone=1 means a standalone Chrome,
// started if need be.
func (s *server) ctlBrowser(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-s.quitting:
			cancel()
		case <-ctx.Done():
		}
	}()
	res, err := s.startBrowser(ctx, r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, res)
}

func (s *server) startBrowser(ctx context.Context, q url.Values) (browserStart, error) {
	kind, mode := q.Get("browser"), cmpOr(q.Get("tabs"), "active")
	if browserLabels[kind] == "" {
		return browserStart{}, fmt.Errorf("unknown browser %q", kind)
	}
	s.connecting.Lock()
	defer s.connecting.Unlock()
	s.mu.Lock()
	if s.cmd != nil || s.browser != nil {
		s.mu.Unlock()
		return browserStart{}, errors.New("already started")
	}
	p, pkind := s.pending, s.pendingKind
	s.pending = nil
	s.mu.Unlock()
	// A connection is kept until it shares (or isn't wanted): making
	// another can mean the person clicking Allow again.
	standalone := q.Get("standalone") == "1"
	want := kind + "/" + q.Get("dir") + "/" + q.Get("standalone")
	if p != nil && pkind != want {
		p.close()
		p = nil
	}
	if p == nil {
		var err error
		if p, err = openBrowser(ctx, kind, q.Get("dir"), standalone); err != nil {
			return browserStart{}, err
		}
	}
	keep := func(res browserStart) (browserStart, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case <-s.quitting:
			p.close()
			return browserStart{}, errors.New("the session is ending")
		default:
		}
		s.pending, s.pendingKind = p, want
		res.Label = p.label()
		return res, nil
	}
	var err error
	if pr, ok := p.(profiler); ok {
		err = pr.chooseProfile(ctx, q.Get("profile"))
	}
	if err == nil {
		switch mode {
		case "all":
			err = p.shareAll(ctx)
		case "new":
			var id string
			id, err = p.open(ctx, "about:blank", false)
			if _, ok := p.(*safariBrowser); ok && err == nil {
				// Check now that Safari runs JavaScript for shoulder.
				_, err = p.run(ctx, id, "return 1;", nil)
			}
		case "pick":
			err = p.shareCandidate(ctx, q.Get("pick"))
		case "active":
			var cands, active []candidate
			cands, err = p.candidates(ctx)
			for _, c := range cands {
				if c.Active {
					active = append(active, c)
				}
			}
			if err == nil && len(cands) == 0 {
				// Nothing to pick from (Chrome showing only its own
				// pages, or just started): share a new tab.
				_, err = p.open(ctx, "about:blank", false)
				break
			}
			if err == nil && len(active) != 1 {
				return keep(browserStart{Choose: cands})
			}
			if err == nil {
				err = p.shareCandidate(ctx, active[0].ID)
			}
		default:
			err = fmt.Errorf("unknown tabs=%s", mode)
		}
	}
	var we *waitError
	if errors.As(err, &we) && len(p.tabs()) == 0 {
		return keep(browserStart{Wait: we.msg})
	}
	if err != nil {
		p.close()
		return browserStart{}, err
	}
	tabs := p.tabs()
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.quitting:
		p.close()
		return browserStart{}, errors.New("the session is ending")
	default:
	}
	s.browser, s.browserTabs = p, len(tabs)
	s.cfg.Browser, s.cfg.AllTabs, s.cfg.Profile = kind, mode == "all", ""
	if c, ok := p.(*chromeBrowser); ok {
		s.cfg.Profile = c.profileName()
	}
	s.cfg.save()
	close(s.started)
	s.notifyLocked()
	return browserStart{Label: p.label(), Tabs: tabs}, nil
}

// openBrowser connects to a browser: Safari, or the Chrome whose user
// data dir is dir (by default, the person's), or with standalone, a
// standalone Chrome (on dir, by default standaloneChromeDir), started if
// need be.
func openBrowser(ctx context.Context, kind, dir string, standalone bool) (sharer, error) {
	if kind == "safari" {
		s, err := connectSafari(ctx, safariOSA)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	if standalone {
		return openStandaloneChrome(ctx, dir)
	}
	c, err := connectChrome(ctx, cmpOr(dir, defaultChromeDir()))
	if err != nil {
		return nil, err
	}
	return c, nil
}

func openStandaloneChrome(ctx context.Context, dir string) (sharer, error) {
	if dir == "" {
		var err error
		if dir, err = standaloneChromeDir(); err != nil {
			return nil, err
		}
	}
	proc, err := startStandaloneChrome(dir)
	if err != nil {
		return nil, err
	}
	c, err := connectChrome(ctx, dir)
	if err != nil {
		if proc != nil {
			stopChrome(proc)
		}
		return nil, err
	}
	c.standalone, c.launched = true, proc
	return c, nil
}

func (s *server) closePending() {
	s.mu.Lock()
	p := s.pending
	s.pending = nil
	s.mu.Unlock()
	if p != nil {
		p.close()
	}
}

// serveBrowser runs a started browser session until the browser goes
// away or the person stops sharing.
func (s *server) serveBrowser() error {
	b := s.browser
	select {
	case <-b.done():
		log.Printf("the browser connection ended")
	case <-s.quitting:
	}
	b.close()
	s.mu.Lock()
	s.browserDone = true
	s.notifyLocked()
	s.mu.Unlock()
	time.Sleep(time.Second) // for the person's screen to hear
	s.mu.Lock()
	for _, sh := range s.shares {
		sh.close()
	}
	s.mu.Unlock()
	return nil
}

// browserActivity is what the person's screen shows while sharing.
type browserActivity struct {
	Label    string     `json:"label"`
	Tabs     []tabInfo  `json:"tabs"`
	Activity []activity `json:"activity"`
	Ended    bool       `json:"ended"`
}

func (s *server) ctlActivity(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b := s.browser
	a := browserActivity{Activity: append([]activity(nil), s.activity...), Ended: s.browserDone}
	s.mu.Unlock()
	if b != nil {
		a.Label, a.Tabs = b.label(), b.tabs()
		s.mu.Lock()
		s.browserTabs = len(a.Tabs)
		s.mu.Unlock()
	}
	writeJSON(w, a)
}
