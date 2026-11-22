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
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// Chrome is shared over the DevTools protocol, with chromedp.
//
// The person's own Chrome (since Chrome 136) ignores --remote-debugging-port
// for its default user data dir. Instead, since Chrome 144, they turn on
// "Allow remote debugging for this browser instance" at
// chrome://inspect/#remote-debugging (Chrome remembers it). Chrome then
// listens on 127.0.0.1 (usually port 9222) and writes the port and the
// browser's WebSocket path to DevToolsActivePort in its user data dir.
// There's no HTTP discovery (/json/version is 404) and every new
// connection makes Chrome ask "Allow remote debugging?"; the WebSocket
// handshake waits for the answer. So shoulder reads the file, makes one
// connection, and keeps it for the whole share: one chromedp remote
// allocator and browser, with every tab's chromedp context made from it.
//
// Nothing shoulder does closes the person's tabs or their Chrome. With a
// remote allocator, cancelling chromedp's browser context only hangs up
// (chromedp sends Browser.close only to a browser it started itself).
// Each shared tab's context has WithDetachOnCancel, so cancelling it
// detaches, where chromedp's default would close the tab. The only tabs
// shoulder closes are its own hidden chrome://version pages and, when
// an agent asks, tabs the agent opened. chrome_safety_test.go checks this.
//
// A Chrome started with --remote-debugging-port and its own
// --user-data-dir writes the same file and doesn't ask; that's how the
// tests work.

const chromeInspectURL = "chrome://inspect/#remote-debugging"

// chromeDirs are where Chrome-family browsers keep their profiles.
func chromeDirs() []string {
	home, _ := os.UserHomeDir()
	if goruntime.GOOS == "darwin" {
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Google", "Chrome"),
			filepath.Join(base, "Google", "Chrome Beta"),
			filepath.Join(base, "Google", "Chrome Dev"),
			filepath.Join(base, "Google", "Chrome Canary"),
			filepath.Join(base, "Chromium"),
		}
	}
	base := filepath.Join(home, ".config")
	return []string{
		filepath.Join(base, "google-chrome"),
		filepath.Join(base, "google-chrome-beta"),
		filepath.Join(base, "google-chrome-unstable"),
		filepath.Join(base, "chromium"),
	}
}

// defaultChromeDir picks the Chrome to share: $SHOULDER_CHROME_DIR (its
// user data dir), or else the first that's listening, or else the first
// that's running, or else Chrome's.
func defaultChromeDir() string {
	if d := os.Getenv("SHOULDER_CHROME_DIR"); d != "" {
		return d
	}
	dirs := chromeDirs()
	for _, d := range dirs {
		if _, err := chromeEndpoint(d); err == nil {
			return d
		}
	}
	for _, d := range dirs {
		if chromeRunning(d) {
			return d
		}
	}
	return dirs[0]
}

// chromeEndpoint returns the browser WebSocket URL from a user data dir's
// DevToolsActivePort, if something listens there. The file outlives
// Chrome, so the port is checked. (Connecting to the port doesn't make
// Chrome ask anything; the WebSocket handshake does.)
func chromeEndpoint(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(b))
	if len(lines) < 2 {
		return "", errors.New("DevToolsActivePort is incomplete")
	}
	port, err := strconv.Atoi(lines[0])
	if err != nil || !strings.HasPrefix(lines[1], "/devtools/browser/") {
		return "", errors.New("DevToolsActivePort is malformed")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	c.Close()
	return "ws://" + addr + lines[1], nil
}

// chromeRunning reports whether a Chrome has dir open: SingletonLock
// links to host-pid.
func chromeRunning(dir string) bool {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return false
	}
	i := strings.LastIndex(target, "-")
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

// chromeCheck says whether shoulder can reach the Chrome in dir, and if
// not, what the person should do. It doesn't connect (connecting is what
// makes Chrome ask).
func chromeCheck(dir string) (ok bool, msg string) {
	if _, err := chromeEndpoint(dir); err == nil {
		return true, "When you press enter, Chrome asks \"Allow remote debugging?\": click Allow."
	}
	turnOn := "open " + chromeInspectURL + " in Chrome and tick \"Allow remote debugging for this browser instance\" (Chrome 144 or later; it stays on)"
	if !chromeRunning(dir) {
		return false, "Chrome isn't running (or isn't allowing remote debugging). Start it; if you haven't before, " + turnOn + ". Then press enter."
	}
	return false, "Chrome is running, but remote debugging is off: " + turnOn + ", then press enter."
}

// shoulder can also start a standalone Chrome, for people who'd rather
// not turn on remote debugging in theirs (-standalone, [o]). It has its own
// user data dir, which lasts from one share to the next, so whatever
// the person signs in to there stays signed in. Started with
// --remote-debugging-port, it doesn't ask "Allow remote debugging?".
// shoulder quits it when sharing stops, unless it was already running.

// standaloneChromeDir is the user data dir of the Chrome shoulder starts.
func standaloneChromeDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shoulder", "chrome"), nil
}

// standaloneChromeArgs are extra arguments for the Chrome shoulder starts (the
// tests make it headless).
var standaloneChromeArgs []string

// standaloneChromeCheck is chromeCheck for the Chrome shoulder starts.
func standaloneChromeCheck(dir string) (ok bool, msg string) {
	if _, err := chromeBinary(); err != nil {
		return false, "shoulder can't find Chrome to start: install it, or set $SHOULDER_CHROME to its executable."
	}
	if _, err := chromeEndpoint(dir); err == nil {
		return true, "shoulder shares the standalone Chrome that's running (profile in " + tildePath(dir) + ")."
	}
	if chromeRunning(dir) {
		return false, "The standalone Chrome (profile in " + tildePath(dir) + ") is running without remote debugging: quit it, then press enter."
	}
	return true, "shoulder starts a standalone Chrome, with its own profile in " + tildePath(dir) + " (kept for next time), and quits it when sharing stops. You're signed in there only to what you've signed in to there."
}

// startStandaloneChrome makes sure the standalone Chrome is running on dir. It
// returns the process if it started it, nil if one was running already.
func startStandaloneChrome(dir string) (*os.Process, error) {
	if _, err := chromeEndpoint(dir); err == nil {
		return nil, nil
	}
	if ok, msg := standaloneChromeCheck(dir); !ok {
		return nil, errors.New(msg)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return launchChrome(dir, standaloneChromeArgs...)
}

// chromeBinary finds Chrome's executable: $SHOULDER_CHROME, or an
// installed Chrome.
func chromeBinary() (string, error) {
	if p := os.Getenv("SHOULDER_CHROME"); p != "" {
		return p, nil
	}
	if goruntime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		for _, app := range []string{"Google Chrome", "Google Chrome Beta", "Chromium"} {
			for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
				p := filepath.Join(root, app+".app", "Contents", "MacOS", app)
				if _, err := os.Stat(p); err == nil {
					return p, nil
				}
			}
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome (set $SHOULDER_CHROME to its executable)")
}

// launchChrome starts a Chrome on its own user data dir (the standalone
// Chrome's, or a test's), listening on a free port, and waits for it to say where.
// It runs in its own session, so it outlives shoulder if need be.
func launchChrome(dir string, extra ...string) (*os.Process, error) {
	bin, err := chromeBinary()
	if err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(dir, "DevToolsActivePort"))
	args := append([]string{"--remote-debugging-port=0", "--user-data-dir=" + dir, "--no-first-run", "--no-default-browser-check"}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for range 150 {
		if _, err := chromeEndpoint(dir); err == nil {
			return cmd.Process, nil
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("chrome exited: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	stopChrome(cmd.Process)
	return nil, errors.New("chrome didn't start listening")
}

// stopChrome stops a Chrome launchChrome started, and its helpers.
func stopChrome(p *os.Process) {
	syscall.Kill(-p.Pid, syscall.SIGTERM)
	for range 50 {
		if syscall.Kill(p.Pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// ---------------------------------------------------------------------------
// Profiles.
//
// The person's Chrome can have several profiles open (Personal, Work),
// each signed in to its own accounts. In the DevTools protocol each is a
// browser context with a random id, and nothing names it. What there
// is: Chrome's default browser context is the profile it used last (the
// one whose window was in front most recently; never an incognito one),
// and chrome://version, opened in a context, shows its profile's dir.
// Local State names the dirs. So shoulder learns which profile a context
// is while it's the default, and when the person chose another profile,
// asks them to click one of its windows.

// chromeProfile is one of Chrome's profiles, from Local State.
type chromeProfile struct {
	Dir   string `json:"dir"`  // "Default", "Profile 1"
	Name  string `json:"name"` // as Chrome shows it: "Work"
	Email string `json:"email,omitempty"`
}

func (p chromeProfile) String() string {
	if p.Email != "" {
		return p.Name + " (" + p.Email + ")"
	}
	return p.Name
}

// chromeProfiles is what Local State says about the profiles. Chrome
// writes it lazily, within seconds of a change.
type chromeProfiles struct {
	all      map[string]chromeProfile
	order    []string // as Chrome lists them
	open     []string // with windows open
	lastUsed string
}

func readChromeProfiles(userDataDir string) chromeProfiles {
	p := chromeProfiles{all: map[string]chromeProfile{}}
	b, err := os.ReadFile(filepath.Join(userDataDir, "Local State"))
	if err != nil {
		return p
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name               string `json:"name"`
				UserName           string `json:"user_name"`
				GaiaName           string `json:"gaia_name"`
				GaiaGivenName      string `json:"gaia_given_name"`
				IsUsingDefaultName bool   `json:"is_using_default_name"`
			} `json:"info_cache"`
			LastUsed   string   `json:"last_used"`
			LastActive []string `json:"last_active_profiles"`
			Order      []string `json:"profiles_order"`
		} `json:"profile"`
	}
	if json.Unmarshal(b, &ls) != nil {
		return p
	}
	for dir, info := range ls.Profile.InfoCache {
		name := info.Name
		// A profile still called "Person 1" goes by its Google name.
		if gaia := cmpOr(info.GaiaGivenName, info.GaiaName); info.IsUsingDefaultName && gaia != "" {
			name = gaia
		}
		p.all[dir] = chromeProfile{Dir: dir, Name: cmpOr(name, dir), Email: info.UserName}
	}
	for _, dir := range ls.Profile.Order {
		if _, ok := p.all[dir]; ok && !slices.Contains(p.order, dir) {
			p.order = append(p.order, dir)
		}
	}
	var rest []string
	for dir := range p.all {
		if !slices.Contains(p.order, dir) {
			rest = append(rest, dir)
		}
	}
	sort.Strings(rest)
	p.order = append(p.order, rest...)
	for _, dir := range ls.Profile.LastActive {
		if _, ok := p.all[dir]; ok {
			p.open = append(p.open, dir)
		}
	}
	p.lastUsed = ls.Profile.LastUsed
	return p
}

// profile describes a profile dir, by name if Local State knows it.
func (p chromeProfiles) profile(dir string) chromeProfile {
	if pr, ok := p.all[dir]; ok {
		return pr
	}
	return chromeProfile{Dir: dir, Name: dir}
}

// choices are the profiles to offer: the open ones, in Chrome's order
// (all of them if Local State doesn't say which are open).
func (p chromeProfiles) choices() []chromeProfile {
	var out []chromeProfile
	for _, dir := range p.order {
		if len(p.open) == 0 || slices.Contains(p.open, dir) {
			out = append(out, p.all[dir])
		}
	}
	return out
}

// find matches what the person typed (-profile) to a profile: its name
// or its dir, ignoring case.
func (p chromeProfiles) find(s string) (chromeProfile, bool) {
	for _, dir := range p.order {
		if pr := p.all[dir]; strings.EqualFold(pr.Name, s) || strings.EqualFold(pr.Dir, s) {
			return pr, true
		}
	}
	return chromeProfile{}, false
}

// waitError says the person has to do something in the browser before
// sharing can go on. The launcher shows it and asks again.
type waitError struct{ msg string }

func (e *waitError) Error() string { return e.msg }

// ---------------------------------------------------------------------------

type chromeBrowser struct {
	dir        string      // Chrome's user data dir
	standalone bool        // a standalone Chrome, not the person's
	launched   *os.Process // the standalone Chrome, if this share started it
	root       context.Context
	shutdown   func() // see connectChrome

	// relatedMu serialises pageOf, which tells its attachment from
	// others only by being the one waiting.
	relatedMu sync.Mutex

	mu         sync.Mutex
	profiles   map[cdp.BrowserContextID]string // learned: context -> profile dir
	profile    cdp.BrowserContextID            // the shared profile
	profileDir string
	all        bool // every tab of the profile is shared
	shared     []*chromeTab
	targets    map[target.ID]*target.Info // tabs and pages, from discovery
	attaching  map[target.ID]bool         // pages being attached to now
	helpers    map[target.ID]bool         // shoulder's hidden chrome://version pages
	related    chan *target.EventAttachedToTarget
	opening    map[target.ID]chan target.ID // window.open calls waiting, by opener
	current    *chromeTab
	next       int
	changed    chan struct{}
}

type chromeTab struct {
	id       string
	page     target.ID // page target, whose id is also its main frame's
	ctx      context.Context
	cancel   context.CancelFunc // detaches (WithDetachOnCancel)
	t        *chromedp.Target
	title    string
	url      string
	openedBy string
	loading  bool
	dialog   string
	world    int64            // shoulder's isolated world in the page, 0 if none yet
	worlds   map[string]int64 // shoulder's worlds by unique id, to see them go

	console        []consoleEntry
	consoleDropped int
	net            []*netEntry
	netByID        map[network.RequestID]*netEntry
	netMark        int // where the current document's requests start in net
	lastNet        time.Time
	// recording is when shoulder started recording requests; earlier
	// ones come from the page's Performance API until the next page.
	recording time.Time
	newPage   bool
}

type consoleEntry struct {
	time   time.Time
	level  string
	source string
	text   string
}

type netEntry struct {
	id               network.RequestID
	method, url, typ string
	status           int
	size             int64
	start, end       time.Time
	failed           string
	done             bool
}

// chromeTrace, if set (by tests), gets every protocol message as chromedp
// logs it: "-> {...}" sent, "<- {...}" received.
var chromeTrace func(msg string)

// chromeLogf is where chromedp's own complaints go: nowhere, unless
// $SHOULDER_DEBUG is set.
func chromeLogf(format string, args ...any) {
	if os.Getenv("SHOULDER_DEBUG") != "" {
		log.Printf("chromedp: "+format, args...)
	}
}

// connectChrome connects to the Chrome whose user data dir is dir.
// Against the person's Chrome, this waits for them to click Allow.
func connectChrome(ctx context.Context, dir string) (*chromeBrowser, error) {
	ws, err := chromeEndpoint(dir)
	if err != nil {
		_, msg := chromeCheck(dir)
		return nil, errors.New(msg)
	}
	c := &chromeBrowser{
		dir: dir, profiles: map[cdp.BrowserContextID]string{},
		targets: map[target.ID]*target.Info{}, attaching: map[target.ID]bool{}, helpers: map[target.ID]bool{},
		opening: map[target.ID]chan target.ID{}, changed: make(chan struct{}),
	}
	// No dial timeout: the handshake waits for the person's answer.
	// Cancelling root ends a handshake that's still waiting.
	actx, cancelAlloc := remote.NewAllocator(context.Background(), ws, remote.NoModifyURL, remote.WithDialTimeout(0))
	opts := []chromedp.ContextOption{chromedp.WithErrorf(chromeLogf), chromedp.WithLogf(chromeLogf)}
	if trace := chromeTrace; trace != nil {
		opts = append(opts, chromedp.WithDebugf(func(format string, args ...any) { trace(fmt.Sprintf(format, args...)) }))
	}
	root, cancel := chromedp.NewContext(actx, opts...)
	c.root = root
	var once sync.Once
	c.shutdown = func() {
		once.Do(func() {
			c.mu.Lock()
			tabs := slices.Clone(c.shared)
			c.mu.Unlock()
			// Detach from each tab (they stay open), then hang up.
			var wg sync.WaitGroup
			for _, t := range tabs {
				wg.Go(func() { chromedp.Cancel(t.ctx) })
			}
			wg.Wait()
			chromedp.Cancel(root)
			cancel()
			cancelAlloc()
			if a := chromedp.FromContext(root).Allocator; a != nil {
				a.Wait()
			}
			if c.launched != nil {
				stopChrome(c.launched)
			}
		})
	}
	// The first browser call connects. It's given root itself: the
	// connection lives as long as the context it was made with.
	connected := make(chan error, 1)
	go func() { connected <- chromedp.ListenBrowser(root, c.browserEvent) }()
	select {
	case err = <-connected:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		// root is never tried again: each try would be another prompt.
		c.shutdown()
		if ctx.Err() != nil {
			return nil, errors.New("Chrome didn't allow the connection in time (it asks \"Allow remote debugging?\")")
		}
		return nil, fmt.Errorf("couldn't connect to Chrome (was Allow clicked?): %v", err)
	}
	filter := target.Filter{{Type: "tab"}, {Type: "page"}}
	if _, err := browserCall(c, ctx, target.SetDiscoverTargets, target.SetDiscoverTargetsParams{Discover: true, Filter: filter}); err != nil {
		c.shutdown()
		return nil, err
	}
	return c, nil
}

func (c *chromeBrowser) close() { c.shutdown() }

func (c *chromeBrowser) label() string {
	if c.standalone {
		return "standalone Chrome"
	}
	if name := c.profileName(); name != "" {
		return "Chrome (" + name + ")"
	}
	return "Chrome"
}

// profileName is the shared profile's name as Chrome shows it, if
// Local State says.
func (c *chromeBrowser) profileName() string {
	c.mu.Lock()
	dir := c.profileDir
	c.mu.Unlock()
	return readChromeProfiles(c.dir).all[dir].Name
}

func (c *chromeBrowser) done() <-chan struct{} { return c.root.Done() }

var errBrowserGone = errors.New("the connection to the browser has closed")

// gone turns errors after the connection ended into errBrowserGone.
func (c *chromeBrowser) gone(err error) error {
	if err != nil && c.root.Err() != nil {
		return errBrowserGone
	}
	return err
}

func (c *chromeBrowser) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// browserCall makes a browser-level call.
func browserCall[P, R any](c *chromeBrowser, ctx context.Context, cmd cdp.Command[P, R], params P) (R, error) {
	r, err := cdp.Call(ctx, chromedp.FromContext(c.root).Browser, cmd, params)
	return r, c.gone(err)
}

// defaultProfile returns Chrome's default browser context and its
// profile dir, finding the dir out the first time.
func (c *chromeBrowser) defaultProfile(ctx context.Context) (cdp.BrowserContextID, string, error) {
	for attempt := 0; ; attempt++ {
		r, err := browserCall(c, ctx, target.GetBrowserContexts, cdp.Empty{})
		if err != nil {
			return "", "", err
		}
		id := r.DefaultBrowserContextID
		c.mu.Lock()
		dir, ok := c.profiles[id]
		c.mu.Unlock()
		if ok {
			return id, dir, nil
		}
		dir, err = c.readProfileDir(ctx, id)
		if err != nil && attempt == 0 && contextNotFound(err) {
			continue // the default changed under us
		}
		if err != nil {
			return "", "", fmt.Errorf("couldn't tell which Chrome profile is in use: %w", err)
		}
		c.mu.Lock()
		c.profiles[id] = dir
		c.mu.Unlock()
		return id, dir, nil
	}
}

// contextNotFound is Chrome refusing a browser context it doesn't let
// the protocol name: anything but the default and ones the protocol made.
func contextNotFound(err error) bool {
	var ce *cdproto.Error
	return errors.As(err, &ce) && strings.Contains(ce.Message, "Failed to find browser context")
}

// readProfileDir reads a browser context's profile dir from
// chrome://version, in a hidden page (in no window, and closed after).
func (c *chromeBrowser) readProfileDir(ctx context.Context, id cdp.BrowserContextID) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	yes := true
	r, err := browserCall(c, ctx, target.CreateTarget, target.CreateTargetParams{URL: "chrome://version/", BrowserContextID: id, Background: &yes, Hidden: &yes})
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.helpers[r.TargetID] = true
	c.mu.Unlock()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		browserCall(c, ctx, target.CloseTarget, target.CloseTargetParams{TargetID: r.TargetID})
	}()
	tctx, tcancel := chromedp.NewContext(c.root, chromedp.WithTargetID(r.TargetID), chromedp.WithDetachOnCancel())
	defer tcancel()
	attached := make(chan error, 1)
	go func() { attached <- chromedp.Do(tctx) }()
	select {
	case err = <-attached:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return "", c.gone(err)
	}
	read := chromedp.Evaluate[string](`(document.getElementById("profile_path") || {}).textContent || ""`)
	for {
		path, err := read(ctx, chromedp.FromContext(tctx).Target)
		if path != "" {
			return filepath.Base(strings.TrimSpace(path)), nil
		}
		if ctx.Err() != nil {
			return "", cmp.Or(err, ctx.Err())
		}
		sleepCtx(ctx, 50*time.Millisecond)
	}
}

// chooseProfile settles which profile is shared: dir ("Default",
// "Profile 1"), or "" for the one Chrome used last.
func (c *chromeBrowser) chooseProfile(ctx context.Context, dir string) error {
	id, defDir, err := c.defaultProfile(ctx)
	if err != nil {
		return err
	}
	if dir != "" && dir != defDir {
		c.mu.Lock()
		id = ""
		for cid, d := range c.profiles {
			if d == dir {
				id = cid
			}
		}
		c.mu.Unlock()
		if id == "" {
			profiles := readChromeProfiles(c.dir)
			name, last := profiles.profile(dir).Name, profiles.profile(defDir).Name
			return &waitError{fmt.Sprintf("Click a %[1]s window in Chrome (open one if there isn't one).\n\nChrome tells shoulder only which profile you used last, and that's %[2]s; once you've clicked into %[1]s, shoulder knows which windows are %[1]s's.", name, last)}
		}
	}
	c.mu.Lock()
	c.profile, c.profileDir = id, cmpOr(dir, defDir)
	c.mu.Unlock()
	return nil
}

// candidates lists the shared profile's tabs, for the person to pick from.
func (c *chromeBrowser) candidates(ctx context.Context) ([]candidate, error) {
	r, err := browserCall(c, ctx, target.GetTargets, target.GetTargetsParams{Filter: target.Filter{{Type: "tab"}}})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	profile := c.profile
	c.mu.Unlock()
	var out []candidate
	index := map[string]int{}
	for _, t := range r.TargetInfos {
		var ed struct {
			TabActive     bool `json:"tabActive"`
			TabStripIndex int  `json:"tabStripIndex"`
		}
		// Extensions' pages have no embedder data; Chrome's own pages
		// aren't offered, nor other profiles' tabs.
		if len(t.EmbedderData) == 0 || json.Unmarshal(t.EmbedderData, &ed) != nil || !shareableURL(t.URL) || t.BrowserContextID != profile {
			continue
		}
		w, _ := browserCall(c, ctx, cdpbrowser.GetWindowForTarget, cdpbrowser.GetWindowForTargetParams{TargetID: t.TargetID})
		out = append(out, candidate{ID: string(t.TargetID), Title: t.Title, URL: t.URL, Active: ed.TabActive, Window: int(w.WindowID)})
		index[string(t.TargetID)] = ed.TabStripIndex
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window
		}
		return index[out[i].ID] < index[out[j].ID]
	})
	return out, nil
}

// shareableURL leaves out the browser's own pages (settings, passwords,
// extensions, devtools), which are no web page's and no agent's
// business. A new tab page is shareable, to go somewhere from.
func shareableURL(u string) bool {
	return !browserPage(u) || u == "chrome://newtab/"
}

func browserPage(u string) bool {
	for _, p := range []string{"chrome:", "chrome-extension:", "chrome-untrusted:", "chrome-search:", "devtools:", "edge:"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// pageOf finds a tab's page. Chrome's protocol has tab targets (what
// the tab strip shows) and page targets (what's in a tab, and what can
// be worked with), and nothing in either's info links them. What does:
// Target.autoAttachRelated on the tab attaches to its page (now, or when
// a new tab gets one). That attachment is only used to learn the page's
// id, and then dropped.
func (c *chromeBrowser) pageOf(ctx context.Context, tab target.ID) (target.ID, error) {
	c.mu.Lock()
	info := c.targets[tab]
	c.mu.Unlock()
	if info == nil {
		r, err := browserCall(c, ctx, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: tab})
		if err != nil {
			return "", errorf(http.StatusNotFound, "that tab is gone")
		}
		info = r.TargetInfo
	}
	c.relatedMu.Lock()
	defer c.relatedMu.Unlock()
	ch := make(chan *target.EventAttachedToTarget, 8)
	c.mu.Lock()
	c.related = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.related = nil
		c.mu.Unlock()
		// Stop Chrome watching the tab for pages to attach.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		browserCall(c, ctx, target.SetAutoAttach, target.SetAutoAttachParams{AutoAttach: false, Flatten: ptrTo(true)})
	}()
	if _, err := browserCall(c, ctx, target.AutoAttachRelated, target.AutoAttachRelatedParams{TargetID: tab, Filter: target.Filter{{Type: "page"}}}); err != nil {
		return "", err
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case ev := <-ch:
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				browserCall(c, ctx, target.DetachFromTarget, target.DetachFromTargetParams{SessionID: ev.SessionID})
			}()
			if p := ev.TargetInfo; p.Type == "page" && p.Subtype == "" && p.BrowserContextID == info.BrowserContextID {
				return p.TargetID, nil
			}
		case <-timeout.C:
			return "", errorf(http.StatusServiceUnavailable, "that tab has no page right now (Chrome may have put it to sleep: click it, then try again)")
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.root.Done():
			return "", errBrowserGone
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

// shareTab adds a page to the share, once.
func (c *chromeBrowser) shareTab(ctx context.Context, page target.ID, openedBy string) (*chromeTab, error) {
	c.mu.Lock()
	for {
		var found *chromeTab
		for _, t := range c.shared {
			if t.page == page {
				found = t
			}
		}
		if found != nil {
			if openedBy == "agent" && found.openedBy != "agent" {
				found.openedBy = openedBy
				c.notifyLocked()
			}
			c.mu.Unlock()
			return found, nil
		}
		if !c.attaching[page] {
			break
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
	}
	c.attaching[page] = true
	c.mu.Unlock()
	t, err := c.attachPage(ctx, page, openedBy)
	c.mu.Lock()
	delete(c.attaching, page)
	c.notifyLocked()
	c.mu.Unlock()
	return t, err
}

// attachPage attaches to a page with chromedp, which turns on what
// shoulder records (console, network, dialogs, loading), and adds it to
// the share.
func (c *chromeBrowser) attachPage(ctx context.Context, page target.ID, openedBy string) (*chromeTab, error) {
	t := &chromeTab{page: page, openedBy: openedBy, netByID: map[network.RequestID]*netEntry{}, worlds: map[string]int64{}}
	c.mu.Lock()
	if info := c.targets[page]; info != nil {
		t.title, t.url = info.Title, info.URL
	}
	c.mu.Unlock()
	// WithDetachOnCancel: when shoulder lets go of the tab, it stays open.
	t.ctx, t.cancel = chromedp.NewContext(c.root, chromedp.WithTargetID(page), chromedp.WithDetachOnCancel(),
		chromedp.WithListener(func(ev any) { c.pageEvent(t, ev) }))
	// The tab's chromedp target lives as long as the context of its
	// first call: t.ctx, not ctx.
	attached := make(chan error, 1)
	go func() { attached <- chromedp.Do(t.ctx) }()
	var err error
	select {
	case err = <-attached:
	case <-ctx.Done():
		err = ctx.Err()
	case <-time.After(15 * time.Second):
		err = errorf(http.StatusGatewayTimeout, "the tab didn't answer (is a dialog open in it? answer it, then try again)")
	}
	if err == nil {
		t.t = chromedp.FromContext(t.ctx).Target
		// chromedp turned network recording on; this caps what Chrome
		// keeps of bodies for shoulder.
		_, err = cdp.Call(ctx, t.t, network.Enable, network.EnableParams{MaxTotalBufferSize: ptrTo[int64](1 << 20), MaxResourceBufferSize: ptrTo[int64](1 << 18)})
	}
	if err != nil {
		t.cancel()
		return nil, fmt.Errorf("couldn't attach to the tab: %w", c.gone(err))
	}
	// It may have closed while shoulder attached.
	if _, err := browserCall(c, ctx, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: page}); err != nil {
		t.cancel()
		return nil, errorf(http.StatusNotFound, "the tab closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	t.id = "t" + strconv.Itoa(c.next)
	t.recording = time.Now()
	c.shared = append(c.shared, t)
	if c.current == nil || t.openedBy != "" {
		c.current = t
	}
	c.notifyLocked()
	return t, nil
}

func (c *chromeBrowser) shareCandidate(ctx context.Context, id string) error {
	cands, err := c.candidates(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(cands, func(cand candidate) bool { return cand.ID == id }) {
		return errorf(http.StatusNotFound, "that tab isn't one of the profile's tabs any more")
	}
	page, err := c.pageOf(ctx, target.ID(id))
	if err != nil {
		return err
	}
	_, err = c.shareTab(ctx, page, "")
	return err
}

// shareAll shares every tab of the profile, and tabs opened in it later.
func (c *chromeBrowser) shareAll(ctx context.Context) error {
	c.mu.Lock()
	c.all = true // before listing, so new tabs are shared too
	c.mu.Unlock()
	cands, err := c.candidates(ctx)
	if err != nil {
		return err
	}
	for _, cand := range cands {
		page, err := c.pageOf(ctx, target.ID(cand.ID))
		if err != nil {
			continue // asleep, or closed since
		}
		if _, err := c.shareTab(ctx, page, ""); err != nil && ctx.Err() != nil {
			return err
		}
	}
	return nil
}

// shareNewTab shares a tab opened in the profile, with all tabs shared.
func (c *chromeBrowser) shareNewTab(tab target.ID) {
	ctx, cancel := context.WithTimeout(c.root, 15*time.Second)
	defer cancel()
	page, err := c.pageOf(ctx, tab)
	c.mu.Lock()
	helper := c.helpers[page]
	c.mu.Unlock()
	if err == nil && !helper {
		c.shareTab(ctx, page, "")
	}
}

// adoptPopup shares a page opened by a shared one.
func (c *chromeBrowser) adoptPopup(page target.ID, opener string) {
	ctx, cancel := context.WithTimeout(c.root, 15*time.Second)
	defer cancel()
	c.shareTab(ctx, page, opener)
}

// browserEvent handles what Chrome tells shoulder about its targets, on
// chromedp's reading goroutine: it updates the tabs, and calls nothing.
func (c *chromeBrowser) browserEvent(ev any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev := ev.(type) {
	case *target.EventTargetCreated:
		c.targetLocked(ev.TargetInfo, true)
	case *target.EventTargetInfoChanged:
		c.targetLocked(ev.TargetInfo, false)
	case *target.EventTargetDestroyed:
		delete(c.targets, ev.TargetID)
		delete(c.helpers, ev.TargetID)
		for _, t := range slices.Clone(c.shared) {
			if t.page == ev.TargetID {
				c.removeLocked(t)
			}
		}
	case *target.EventAttachedToTarget:
		// Wanted only by a waiting pageOf, and not shoulder's own
		// attachments (chromedp's, to tabs and helpers).
		id := ev.TargetInfo.TargetID
		if c.related != nil && !c.attaching[id] && !c.helpers[id] {
			select {
			case c.related <- ev:
			default:
			}
		}
	}
}

func (c *chromeBrowser) targetLocked(info *target.Info, created bool) {
	if info == nil {
		return
	}
	_, known := c.targets[info.TargetID]
	c.targets[info.TargetID] = info
	for _, t := range c.shared {
		if t.page == info.TargetID {
			t.title, t.url = info.Title, info.URL
		}
	}
	if known || !created || c.profile == "" {
		return
	}
	switch {
	case info.Type == "page" && info.OpenerID != "" && c.opening[info.OpenerID] != nil:
		select { // the agent's window.open (see openFrom)
		case c.opening[info.OpenerID] <- info.TargetID:
		default:
		}
	case info.Type == "page" && info.OpenerID != "" && !c.all:
		for _, t := range c.shared {
			if t.page == info.OpenerID {
				go c.adoptPopup(info.TargetID, t.id)
			}
		}
	case info.Type == "tab" && c.all && info.BrowserContextID == c.profile:
		go c.shareNewTab(info.TargetID)
	}
}

func (c *chromeBrowser) removeLocked(t *chromeTab) {
	i := slices.Index(c.shared, t)
	if i < 0 {
		return
	}
	c.shared = slices.Delete(c.shared, i, i+1)
	go t.cancel()
	if c.current == t {
		c.current = nil
		if n := len(c.shared); n > 0 {
			c.current = c.shared[n-1]
		}
	}
	c.notifyLocked()
}

// ---------------------------------------------------------------------------
// What a shared page says.

const maxConsole, maxNet = 1000, 1000

// pageEvent records a shared page's events, on chromedp's reading
// goroutine: it updates the tab, and calls nothing.
func (c *chromeBrowser) pageEvent(t *chromeTab, ev any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := func(level, source, text string) {
		t.console = append(t.console, consoleEntry{time.Now(), level, source, text})
		if len(t.console) > maxConsole {
			t.console = t.console[1:]
			t.consoleDropped++
		}
	}
	switch ev := ev.(type) {
	case *runtime.EventConsoleAPICalled:
		var parts []string
		for _, a := range ev.Args {
			parts = append(parts, objectString(a))
		}
		typ := string(ev.Type)
		level := map[string]string{"warning": "warn", "assert": "error", "trace": "log", "dir": "log", "table": "log"}[typ]
		log(cmpOr(level, typ), "console", strings.Join(parts, " ")+where(ev.StackTrace))
	case *runtime.EventExceptionThrown:
		d := ev.ExceptionDetails
		if d == nil {
			break
		}
		text := d.Text
		if d.Exception != nil {
			text = objectString(d.Exception)
		}
		if d.URL != "" {
			text += fmt.Sprintf(" (%s:%d)", d.URL, d.LineNumber+1)
		}
		log("error", "exception", text)
	case *cdplog.EventEntryAdded:
		e := ev.Entry
		if e == nil {
			break
		}
		level := map[string]string{"warning": "warn", "verbose": "debug"}[string(e.Level)]
		text := e.Text
		if e.URL != "" && !strings.Contains(text, e.URL) {
			text += " (" + e.URL + ")"
		}
		log(cmpOr(level, string(e.Level)), string(e.Source), text)
	case *runtime.EventExecutionContextCreated:
		if ctx := ev.Context; ctx != nil && ctx.Name == "shoulder" {
			t.worlds[ctx.UniqueID] = int64(ctx.ID)
		}
	case *runtime.EventExecutionContextDestroyed:
		if id, ok := t.worlds[ev.ExecutionContextUniqueID]; ok {
			delete(t.worlds, ev.ExecutionContextUniqueID)
			if id == t.world {
				t.world = 0
			}
		}
	case *runtime.EventExecutionContextsCleared:
		t.world = 0
		clear(t.worlds)
	case *page.EventFrameNavigated:
		if f := ev.Frame; f != nil && f.ParentID == "" {
			t.url = f.URL
			log("info", "shoulder", "navigated to "+f.URL)
		}
	case *page.EventFrameStartedLoading:
		if ev.FrameID == cdp.FrameID(t.page) {
			t.loading = true
		}
	case *page.EventFrameStoppedLoading:
		if ev.FrameID == cdp.FrameID(t.page) {
			t.loading = false
		}
	case *page.EventJavascriptDialogOpening:
		t.dialog = fmt.Sprintf("%s %q", ev.Type, short(ev.Message, 200))
		c.notifyLocked()
	case *page.EventJavascriptDialogClosed:
		t.dialog = ""
		c.notifyLocked()
	case *network.EventRequestWillBeSent:
		if old := t.netByID[ev.RequestID]; old != nil && ev.RedirectResponse != nil {
			old.status, old.done, old.end = int(ev.RedirectResponse.Status), true, time.Now()
		}
		if ev.Type == network.ResourceTypeDocument && ev.FrameID == cdp.FrameID(t.page) {
			t.netMark, t.newPage = len(t.net), true
		}
		e := &netEntry{id: ev.RequestID, typ: strings.ToLower(string(ev.Type)), start: time.Now()}
		if ev.Request != nil {
			e.method, e.url = ev.Request.Method, ev.Request.URL
		}
		t.net = append(t.net, e)
		t.netByID[ev.RequestID] = e
		if len(t.net) > maxNet {
			if old := t.net[0]; t.netByID[old.id] == old {
				delete(t.netByID, old.id)
			}
			t.net = t.net[1:]
			t.netMark = max(t.netMark-1, 0)
		}
		t.lastNet = time.Now()
	case *network.EventResponseReceived:
		if e := t.netByID[ev.RequestID]; e != nil && ev.Response != nil {
			e.status = int(ev.Response.Status)
		}
		t.lastNet = time.Now()
	case *network.EventLoadingFinished:
		if e := t.netByID[ev.RequestID]; e != nil {
			e.done, e.end, e.size = true, time.Now(), int64(ev.EncodedDataLength)
			delete(t.netByID, ev.RequestID)
		}
		t.lastNet = time.Now()
	case *network.EventLoadingFailed:
		if e := t.netByID[ev.RequestID]; e != nil {
			e.done, e.end = true, time.Now()
			e.failed = cmpOr(ev.ErrorText, "failed")
			if ev.Canceled {
				e.failed = "canceled"
			}
			if ev.BlockedReason != "" {
				e.failed += " (blocked: " + string(ev.BlockedReason) + ")"
			}
			delete(t.netByID, ev.RequestID)
		}
		t.lastNet = time.Now()
	}
}

// objectString is how the console shows a value.
func objectString(o *runtime.RemoteObject) string {
	switch {
	case o == nil:
		return ""
	case o.Type == "string":
		var s string
		json.Unmarshal(o.Value, &s)
		return s
	case o.Type == "undefined":
		return "undefined"
	case o.UnserializableValue != "":
		return string(o.UnserializableValue)
	case len(o.Value) > 0 && o.Type != "object":
		return string(o.Value)
	case o.Subtype == "null":
		return "null"
	}
	return o.Description
}

func where(s *runtime.StackTrace) string {
	if s == nil || len(s.CallFrames) == 0 || s.CallFrames[0] == nil || s.CallFrames[0].URL == "" {
		return ""
	}
	return fmt.Sprintf(" (%s:%d)", s.CallFrames[0].URL, s.CallFrames[0].LineNumber+1)
}

// ---------------------------------------------------------------------------
// The browser interface.

func (c *chromeBrowser) tabs() []tabInfo {
	// Chrome doesn't always announce new titles, so ask.
	ctx, cancel := context.WithTimeout(c.root, 2*time.Second)
	r, err := browserCall(c, ctx, target.GetTargets, target.GetTargetsParams{Filter: target.Filter{{Type: "page"}}})
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		for _, info := range r.TargetInfos {
			for _, t := range c.shared {
				if t.page == info.TargetID {
					t.title, t.url = info.Title, info.URL
				}
			}
		}
	}
	out := []tabInfo{}
	for _, t := range c.shared {
		out = append(out, tabInfo{ID: t.id, Title: t.title, URL: t.url, Current: t == c.current, Loading: t.loading, OpenedBy: t.openedBy, Dialog: t.dialog})
	}
	return out
}

func (c *chromeBrowser) findLocked(id string) (*chromeTab, error) {
	if id == "" {
		if c.current == nil {
			return nil, errorf(http.StatusNotFound, "no tabs are shared now (the person closed them?); POST open starts a new one")
		}
		return c.current, nil
	}
	for _, t := range c.shared {
		if t.id == id {
			return t, nil
		}
	}
	return nil, errorf(http.StatusNotFound, "no shared tab %s; GET state lists them", id)
}

func (c *chromeBrowser) setCurrent(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.findLocked(id)
	if err == nil {
		c.current = t
	}
	return err
}

// pageUse is what page is wanted for: acting in it (not in Chrome's own
// pages), navigating it, or answering its dialog.
type pageUse int

const (
	pageAct pageUse = iota
	pageNav
	pageDialog
)

// page finds a shared tab. Unless it's for answering one, an open
// dialog is an error: the page can't run anything until it's answered.
func (c *chromeBrowser) page(ctx context.Context, id string, use pageUse) (*chromeTab, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.findLocked(id)
	switch {
	case err != nil:
		return nil, err
	case c.root.Err() != nil:
		return nil, errBrowserGone
	case use == pageAct && !shareableURL(t.url):
		return nil, errorf(http.StatusForbidden, "%s is showing one of Chrome's own pages (%s), which shoulder leaves alone; POST navigate takes it somewhere else", t.id, short(t.url, 100))
	case t.dialog != "" && use != pageDialog:
		return nil, errorf(http.StatusConflict, "%s has a dialog open (%s); nothing runs in the page until it's answered: POST dialog with accept or dismiss", t.id, t.dialog)
	case t.dialog == "" && use == pageDialog:
		return nil, errorf(http.StatusConflict, "%s has no dialog open", t.id)
	}
	return t, nil
}

// world returns shoulder's isolated world in the tab's page: the page's
// DOM, but not its JavaScript globals.
func (c *chromeBrowser) world(ctx context.Context, t *chromeTab) (runtime.ExecutionContextID, error) {
	c.mu.Lock()
	w := t.world
	c.mu.Unlock()
	if w != 0 {
		return runtime.ExecutionContextID(w), nil
	}
	r, err := cdp.Call(ctx, t.t, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{FrameID: cdp.FrameID(t.page), WorldName: "shoulder", GrantUniveralAccess: ptrTo(false)})
	if err != nil {
		return 0, c.gone(err)
	}
	c.mu.Lock()
	t.world = int64(r.ExecutionContextID)
	c.mu.Unlock()
	return r.ExecutionContextID, nil
}

// pageCall makes a call to a tab's page that could open a dialog: an
// alert in a click handler, say. The page stops until the dialog is
// answered, and so would the call, so it returns a dialogError then.
func pageCall[P, R any](c *chromeBrowser, ctx context.Context, t *chromeTab, cmd cdp.Command[P, R], params P) (R, error) {
	type result struct {
		r   R
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := cdp.Call(ctx, t.t, cmd, params)
		done <- result{r, err}
	}()
	for {
		select {
		case res := <-done:
			return res.r, c.gone(res.err)
		default:
		}
		c.mu.Lock()
		dialog, ch := t.dialog, c.changed
		c.mu.Unlock()
		var zero R
		if dialog != "" {
			return zero, &dialogError{t.id, dialog}
		}
		select {
		case res := <-done:
			return res.r, c.gone(res.err)
		case <-ch:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

// dialogError says a dialog opened while shoulder was doing something.
type dialogError struct{ tab, dialog string }

func (e *dialogError) Error() string {
	return fmt.Sprintf("%s opened a dialog (%s); the page is stopped until it's answered: POST dialog with accept or dismiss", e.tab, e.dialog)
}

// opened treats a dialog an action opened as the action's result.
func opened(err error) error {
	var de *dialogError
	if errors.As(err, &de) {
		return nil
	}
	return err
}

// thrown is the error for an exception thrown by code shoulder ran.
func thrown(d *runtime.ExceptionDetails) error {
	if d == nil {
		return nil
	}
	msg := d.Text
	if d.Exception != nil {
		msg = objectString(d.Exception)
	}
	msg = strings.TrimPrefix(msg, "Uncaught ")
	return errorf(http.StatusBadRequest, "the page threw: %s", msg)
}

func (c *chromeBrowser) run(ctx context.Context, id, script string, args any) (json.RawMessage, error) {
	a, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	expr := "(async (args) => {\n" + script + "\n})(" + string(a) + ")"
	for attempt := 0; ; attempt++ {
		t, err := c.page(ctx, id, pageAct)
		if err != nil {
			return nil, err
		}
		w, err := c.world(ctx, t)
		if err == nil {
			var r runtime.EvaluateResult
			r, err = pageCall(c, ctx, t, runtime.Evaluate, runtime.EvaluateParams{Expression: expr, ContextID: w, AwaitPromise: ptrTo(true), ReturnByValue: ptrTo(true)})
			if err == nil {
				if err := thrown(r.ExceptionDetails); err != nil {
					return nil, err
				}
				if r.Result == nil || len(r.Result.Value) == 0 {
					return json.RawMessage("null"), nil
				}
				return json.RawMessage(r.Result.Value), nil
			}
		}
		// The page navigated away under the call: its world went with it.
		var ce *cdproto.Error
		if !errors.As(err, &ce) || attempt >= 2 {
			return nil, err
		}
		c.mu.Lock()
		if t.world == int64(w) {
			t.world = 0
		}
		c.mu.Unlock()
		sleepCtx(ctx, 200*time.Millisecond)
	}
}

func (c *chromeBrowser) js(ctx context.Context, id, code string) (string, error) {
	t, err := c.page(ctx, id, pageAct)
	if err != nil {
		return "", err
	}
	if browserPage(t.url) {
		return "", errorf(http.StatusForbidden, "js doesn't run in Chrome's own pages; POST navigate takes %s to a web page", t.id)
	}
	// A lone expression is returned; anything else is a function body.
	compiled, err := cdp.Call(ctx, t.t, runtime.CompileScript, runtime.CompileScriptParams{Expression: userCode(code, true)})
	if err != nil {
		return "", c.gone(err)
	}
	expr := userCode(code, compiled.ExceptionDetails == nil)
	r, err := pageCall(c, ctx, t, runtime.Evaluate, runtime.EvaluateParams{Expression: expr, AwaitPromise: ptrTo(true), ReturnByValue: ptrTo(true), UserGesture: ptrTo(true)})
	if err != nil {
		return "", err
	}
	if err := thrown(r.ExceptionDetails); err != nil {
		return "", err
	}
	var s string
	if r.Result != nil {
		json.Unmarshal(r.Result.Value, &s)
	}
	return s, nil
}

func (c *chromeBrowser) navigate(ctx context.Context, id, u string) error {
	t, err := c.page(ctx, id, pageNav)
	if err != nil {
		return err
	}
	r, err := pageCall(c, ctx, t, page.Navigate, page.NavigateParams{URL: u})
	if err != nil {
		return opened(err) // "Leave site?"
	}
	if r.ErrorText != "" {
		return errorf(http.StatusBadGateway, "Chrome couldn't load %s: %s", u, r.ErrorText)
	}
	return nil
}

func (c *chromeBrowser) history(ctx context.Context, id, action string) error {
	t, err := c.page(ctx, id, pageNav)
	if err != nil {
		return err
	}
	switch action {
	case "reload":
		_, err := pageCall(c, ctx, t, page.Reload, page.ReloadParams{})
		return opened(err)
	case "stop":
		_, err := cdp.Call(ctx, t.t, page.StopLoading, cdp.Empty{})
		return c.gone(err)
	}
	h, err := cdp.Call(ctx, t.t, page.GetNavigationHistory, cdp.Empty{})
	if err != nil {
		return c.gone(err)
	}
	i := int(h.CurrentIndex) - 1
	if action == "forward" {
		i = int(h.CurrentIndex) + 1
	}
	if i < 0 || i >= len(h.Entries) {
		return errorf(http.StatusConflict, "there's nothing to go %s to", action)
	}
	_, err = pageCall(c, ctx, t, page.NavigateToHistoryEntry, page.NavigateToHistoryEntryParams{EntryID: h.Entries[i].ID})
	return opened(err)
}

func (c *chromeBrowser) screenshot(ctx context.Context, id string, png bool) ([]byte, string, error) {
	t, err := c.page(ctx, id, pageAct)
	if err != nil {
		return nil, "", err
	}
	var vp struct {
		Width, Height, Dpr, X, Y float64
	}
	raw, err := c.run(ctx, id, viewportScript, nil)
	if err != nil {
		return nil, "", err
	}
	json.Unmarshal(raw, &vp)
	m, err := cdp.Call(ctx, t.t, page.GetLayoutMetrics, cdp.Empty{})
	if err != nil {
		return nil, "", c.gone(err)
	}
	v := m.CSSVisualViewport
	if v == nil {
		return nil, "", errors.New("Chrome didn't say where the viewport is")
	}
	// Scaled so one image pixel is one CSS pixel, the unit click takes.
	params := page.CaptureScreenshotParams{
		Format: page.CaptureScreenshotFormatJpeg, Quality: ptrTo[int64](80),
		Clip: &page.Viewport{X: v.PageX, Y: v.PageY, Width: v.ClientWidth, Height: v.ClientHeight, Scale: 1 / max(vp.Dpr, 1)},
	}
	if png {
		params.Format, params.Quality = page.CaptureScreenshotFormatPng, nil
	}
	shotCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := cdp.Call(shotCtx, t.t, page.CaptureScreenshot, params)
	if err != nil {
		if shotCtx.Err() != nil && ctx.Err() == nil {
			return nil, "", errorf(http.StatusGatewayTimeout, "Chrome didn't draw the tab in time; tabs in the background or minimized windows may not draw (POST focus brings the tab to the front)")
		}
		return nil, "", c.gone(err)
	}
	note := fmt.Sprintf("the viewport, %gx%g CSS pixels, scrolled to %g,%g; image pixels are CSS pixels, as click?x=&y= takes them", v.ClientWidth, v.ClientHeight, v.PageX, v.PageY)
	return r.Data, note, nil
}

func (c *chromeBrowser) click(ctx context.Context, id string, x, y float64, count int, mods keyMods) error {
	t, err := c.page(ctx, id, pageAct)
	if err != nil {
		return err
	}
	ev := func(typ input.DispatchMouseEventType, n, buttons int64) error {
		p := input.DispatchMouseEventParams{Type: typ, X: x, Y: y, Modifiers: int64(mods.bits()), Buttons: buttons}
		if typ != input.DispatchMouseEventTypeMouseMoved {
			p.Button, p.ClickCount = input.MouseButtonLeft, n
		}
		_, err := pageCall(c, ctx, t, input.DispatchMouseEvent, p)
		return err
	}
	if err := ev(input.DispatchMouseEventTypeMouseMoved, 0, 0); err != nil {
		return opened(err)
	}
	for n := int64(1); n <= int64(count); n++ {
		if err := ev(input.DispatchMouseEventTypeMousePressed, n, 1); err != nil {
			return opened(err)
		}
		if err := ev(input.DispatchMouseEventTypeMouseReleased, n, 0); err != nil {
			return opened(err)
		}
	}
	return nil
}

func (c *chromeBrowser) insertText(ctx context.Context, id, text string) error {
	t, err := c.page(ctx, id, pageAct)
	if err != nil {
		return err
	}
	_, err = pageCall(c, ctx, t, input.InsertText, input.InsertTextParams{Text: text})
	return opened(err)
}

// macEditingCommands are what the keys do in text on a Mac. There, the
// window's text system turns keys into editing commands, and synthetic
// key events skip it, so they carry the commands themselves.
var macEditingCommands = map[string]string{
	"Backspace": "deleteBackward", "Delete": "deleteForward", "Alt+Backspace": "deleteWordBackward",
	"Meta+Backspace": "deleteToBeginningOfLine", "Alt+Delete": "deleteWordForward",
	"ArrowLeft": "moveLeft", "ArrowRight": "moveRight", "ArrowUp": "moveUp", "ArrowDown": "moveDown",
	"Shift+ArrowLeft": "moveLeftAndModifySelection", "Shift+ArrowRight": "moveRightAndModifySelection",
	"Shift+ArrowUp": "moveUpAndModifySelection", "Shift+ArrowDown": "moveDownAndModifySelection",
	"Alt+ArrowLeft": "moveWordLeft", "Alt+ArrowRight": "moveWordRight",
	"Meta+ArrowLeft": "moveToLeftEndOfLine", "Meta+ArrowRight": "moveToRightEndOfLine",
	"Meta+ArrowUp": "moveToBeginningOfDocument", "Meta+ArrowDown": "moveToEndOfDocument",
	"Home": "scrollToBeginningOfDocument", "End": "scrollToEndOfDocument",
	"PageUp": "scrollPageUp", "PageDown": "scrollPageDown",
	"Meta+a": "selectAll", "Meta+c": "copy", "Meta+x": "cut", "Meta+v": "paste", "Meta+z": "undo", "Shift+Meta+z": "redo",
}

func (c *chromeBrowser) key(ctx context.Context, id string, k keyPress) error {
	t, err := c.page(ctx, id, pageAct)
	if err != nil {
		return err
	}
	down := input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeRawKeyDown, Key: k.key, Code: k.code, WindowsVirtualKeyCode: int64(k.vk), Modifiers: int64(k.mods.bits())}
	if k.text != "" {
		down.Type, down.Text, down.UnmodifiedText = input.DispatchKeyEventTypeKeyDown, k.text, k.text
	}
	if goruntime.GOOS == "darwin" {
		name := k.key
		if k.mods.alt {
			name = "Alt+" + name
		}
		if k.mods.meta {
			name = "Meta+" + strings.ToLower(name)
		}
		if k.mods.shift && k.code != "" && !strings.HasPrefix(k.code, "Key") {
			name = "Shift+" + name
		} else if k.mods.shift && k.mods.meta {
			name = "Shift+" + name
		}
		if cmd, ok := macEditingCommands[name]; ok {
			down.Commands = []string{cmd}
		}
	}
	if _, err := pageCall(c, ctx, t, input.DispatchKeyEvent, down); err != nil {
		return opened(err)
	}
	up := input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyUp, Key: k.key, Code: k.code, WindowsVirtualKeyCode: int64(k.vk), Modifiers: int64(k.mods.bits())}
	_, err = pageCall(c, ctx, t, input.DispatchKeyEvent, up)
	return opened(err)
}

// open opens a tab in the shared profile. Chrome lets the protocol
// open tabs only in the profile it used last; for another, the page
// opens it, from a shared tab of the profile (window.open), which can't
// keep it in the background.
func (c *chromeBrowser) open(ctx context.Context, u string, background bool) (string, error) {
	c.mu.Lock()
	profile := c.profile
	c.mu.Unlock()
	r, err := browserCall(c, ctx, target.CreateTarget, target.CreateTargetParams{URL: u, BrowserContextID: profile, Background: ptrTo(background)})
	page := r.TargetID
	if err != nil && contextNotFound(err) {
		page, err = c.openFrom(ctx, u)
	}
	if err != nil {
		return "", err
	}
	t, err := c.shareTab(ctx, page, "agent")
	if err != nil {
		return "", err
	}
	return t.id, nil
}

// openFrom opens a tab with window.open in a shared tab of the profile.
func (c *chromeBrowser) openFrom(ctx context.Context, u string) (target.ID, error) {
	c.mu.Lock()
	var from *chromeTab
	for _, t := range append([]*chromeTab{c.current}, c.shared...) {
		if t != nil && t.dialog == "" && shareableURL(t.url) && !browserPage(t.url) {
			from = t
			break
		}
	}
	if from == nil {
		c.mu.Unlock()
		name := readChromeProfiles(c.dir).profile(c.profileDir).Name
		return "", &waitError{fmt.Sprintf("Click a %[1]s window in Chrome. (Chrome opens tabs for shoulder only in the profile you used last, or from a shared %[1]s tab showing a web page, and there's none.)", name)}
	}
	if c.opening[from.page] != nil {
		c.mu.Unlock()
		return "", errorf(http.StatusConflict, "another tab is being opened from %s; try again in a moment", from.id)
	}
	ch := make(chan target.ID, 1)
	c.opening[from.page] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.opening, from.page)
		c.mu.Unlock()
	}()
	w, err := c.world(ctx, from)
	if err != nil {
		return "", err
	}
	q, _ := json.Marshal(u)
	_, err = pageCall(c, ctx, from, runtime.Evaluate, runtime.EvaluateParams{Expression: "window.open(" + string(q) + ", '_blank', 'noopener'), true", ContextID: w, UserGesture: ptrTo(true), ReturnByValue: ptrTo(true)})
	if err != nil {
		return "", err
	}
	select {
	case id := <-ch:
		return id, nil
	case <-time.After(5 * time.Second):
		return "", errorf(http.StatusBadGateway, "Chrome didn't open the tab (a popup blocker?)")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *chromeBrowser) closeTab(ctx context.Context, id string) error {
	c.mu.Lock()
	t, err := c.findLocked(id)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if t.openedBy == "" {
		return errorf(http.StatusForbidden, "%s is the person's own tab; shoulder only closes tabs the agent opened", t.id)
	}
	_, err = browserCall(c, ctx, target.CloseTarget, target.CloseTargetParams{TargetID: t.page})
	return err
}

func (c *chromeBrowser) activate(ctx context.Context, id string) error {
	c.mu.Lock()
	t, err := c.findLocked(id)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = browserCall(c, ctx, target.ActivateTarget, target.ActivateTargetParams{TargetID: t.page})
	return err
}

func (c *chromeBrowser) dialog(ctx context.Context, id string, accept bool, text string) error {
	t, err := c.page(ctx, id, pageDialog)
	if err != nil {
		return err
	}
	_, err = cdp.Call(ctx, t.t, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: accept, PromptText: text})
	return c.gone(err)
}
func (c *chromeBrowser) console(ctx context.Context, id, level string, clear bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.findLocked(id)
	if err != nil {
		return "", err
	}
	out := formatConsole(t.console, t.consoleDropped, level)
	if clear {
		t.console, t.consoleDropped = nil, 0
	}
	return out, nil
}

func formatConsole(entries []consoleEntry, dropped int, level string) string {
	rank := map[string]int{"error": 3, "warn": 2}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if rank[e.level] < rank[level] && e.source != "shoulder" {
			continue
		}
		n++
		fmt.Fprintf(&b, "[%s %s, %s] %s\n", e.time.Format("15:04:05.000"), e.level, e.source, e.text)
	}
	head := fmt.Sprintf("%d messages", n)
	if level != "" {
		head = fmt.Sprintf("%d messages at level %s or above", n, level)
	}
	if dropped > 0 {
		head += fmt.Sprintf(" (%d older ones dropped)", dropped)
	}
	return head + "\n" + b.String()
}

func (c *chromeBrowser) network(ctx context.Context, id, filter string, all, clear bool) (string, error) {
	c.mu.Lock()
	t, err := c.findLocked(id)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	tab, newPage, since := t.id, t.newPage, t.recording
	if since.IsZero() {
		since = time.Now()
	}
	c.mu.Unlock()
	// Requests from before shoulder looked: the page's own record.
	var earlier []*netEntry
	if !newPage {
		if raw, err := c.run(ctx, tab, perfNetworkScript, nil); err == nil {
			for _, e := range perfEntries(raw) {
				if e.start.Before(since) {
					earlier = append(earlier, e)
				}
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, err = c.findLocked(id); err != nil {
		return "", err
	}
	entries := t.net[min(t.netMark, len(t.net)):]
	if all {
		entries = t.net
	}
	note := ""
	if !t.newPage && len(earlier) > 0 {
		// Around when recording started, a request can be in both.
		seen := map[string]bool{}
		for _, e := range entries {
			seen[e.url] = true
		}
		earlier = slices.DeleteFunc(earlier, func(e *netEntry) bool { return seen[e.url] })
		entries = append(earlier, entries...)
		note = "(requests from before shoulder connected are from the page's Performance API, which doesn't record failures)\n"
	}
	out := note + formatNetwork(entries, filter, all)
	if clear {
		t.net, t.netMark, t.newPage = nil, 0, true
		for id, e := range t.netByID {
			if e.done {
				delete(t.netByID, id)
			}
		}
	}
	return out, nil
}

// perfNetworkScript reads a page's Performance API record of its loads.
const perfNetworkScript = `
const at = e => Math.round(performance.timeOrigin + e.startTime);
const nav = performance.getEntriesByType("navigation")[0];
const res = performance.getEntriesByType("resource").map(e => ({
  url: e.name, type: e.initiatorType, t: at(e), ms: Math.round(e.duration), size: e.transferSize || 0, status: e.responseStatus || 0 }));
if (nav) res.unshift({ url: nav.name, type: "document", t: at(nav), ms: Math.round(nav.duration), size: nav.transferSize || 0, status: nav.responseStatus || 0 });
return res;
`

func perfEntries(raw json.RawMessage) []*netEntry {
	var res []struct {
		URL, Type string
		T, MS     float64
		Size      int64
		Status    int
	}
	json.Unmarshal(raw, &res)
	var out []*netEntry
	for _, r := range res {
		typ := r.Type
		switch typ {
		case "xmlhttprequest":
			typ = "xhr"
		case "link", "css":
			typ = "stylesheet"
		case "img":
			typ = "image"
		}
		start := time.UnixMilli(int64(r.T))
		out = append(out, &netEntry{method: "GET", url: r.URL, typ: typ, status: r.Status, size: r.Size, done: true, start: start, end: start.Add(time.Duration(r.MS) * time.Millisecond)})
	}
	return out
}

func formatNetwork(entries []*netEntry, filter string, all bool) string {
	var b strings.Builder
	others := map[string]int{}
	shown := 0
	for _, e := range entries {
		if filter != "" && !strings.Contains(e.url, filter) {
			continue
		}
		interesting := e.typ == "document" || e.typ == "fetch" || e.typ == "xhr" || e.failed != "" || e.status >= 400
		if !all && filter == "" && !interesting {
			others[e.typ]++
			continue
		}
		shown++
		status := strconv.Itoa(e.status)
		switch {
		case e.status == 0 && e.done && e.failed == "":
			status = "-"
		case e.failed != "":
			status = "failed"
		case !e.done:
			status = "pending"
		}
		fmt.Fprintf(&b, "%s %s %s %s", e.method, status, cmpOr(e.typ, "other"), short(e.url, 300))
		switch {
		case e.failed != "":
			fmt.Fprintf(&b, " (%s)", e.failed)
		case e.done:
			fmt.Fprintf(&b, " (%s, %dms)", formatSize(e.size), e.end.Sub(e.start).Milliseconds())
		}
		b.WriteString("\n")
	}
	head := fmt.Sprintf("%d requests since this page started loading", shown)
	if all {
		head = fmt.Sprintf("%d requests since sharing began", shown)
	}
	out := head + "\n" + b.String()
	if len(others) > 0 {
		var kinds []string
		total := 0
		for k, n := range others {
			kinds = append(kinds, fmt.Sprintf("%d %s", n, cmpOr(k, "other")))
			total += n
		}
		sort.Strings(kinds)
		out += fmt.Sprintf("and %d more that loaded fine (%s): network?all=1 lists them\n", total, strings.Join(kinds, ", "))
	}
	return out
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (c *chromeBrowser) networkIdle(id string, d time.Duration) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.findLocked(id)
	if err != nil {
		return true, false
	}
	for _, e := range t.netByID {
		// Long polls and streams never finish; they don't count.
		if !e.done && time.Since(e.start) < 10*time.Second && e.typ != "eventsource" && e.typ != "websocket" {
			return false, true
		}
	}
	return time.Since(t.lastNet) >= d, true
}
