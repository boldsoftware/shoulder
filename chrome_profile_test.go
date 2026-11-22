package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
)

const testLocalState = `{
  "profile": {
    "info_cache": {
      "Default": {"name": "Personal", "user_name": "me@example.com", "is_using_default_name": false},
      "Profile 1": {"name": "Work", "user_name": "me@work.example", "is_using_default_name": false},
      "Profile 2": {"name": "Person 1", "gaia_given_name": "Sam", "is_using_default_name": true},
      "Profile 3": {"name": "Old", "is_using_default_name": false}
    },
    "profiles_order": ["Default", "Profile 1", "Profile 2", "Profile 3"],
    "last_active_profiles": ["Profile 1", "Default", "Profile 2"],
    "last_used": "Profile 1"
  }
}`

// chromeLocalState is testLocalState for a Chrome to start with:
// Chrome opens the profile used last, and that's Personal.
var chromeLocalState = strings.Replace(testLocalState, `"last_used": "Profile 1"`, `"last_used": "Default"`, 1)

func TestChromeProfilesLocalState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Local State"), []byte(testLocalState), 0o600)
	p := readChromeProfiles(dir)
	var got []string
	for _, pr := range p.choices() {
		got = append(got, pr.Dir+"="+pr.String())
	}
	want := "Default=Personal (me@example.com),Profile 1=Work (me@work.example),Profile 2=Sam"
	if strings.Join(got, ",") != want {
		t.Errorf("choices:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	if p.lastUsed != "Profile 1" {
		t.Errorf("last used %q", p.lastUsed)
	}
	for in, want := range map[string]string{"work": "Profile 1", "profile 1": "Profile 1", "Old": "Profile 3", "nope": ""} {
		if pr, _ := p.find(in); pr.Dir != want {
			t.Errorf("find(%q) = %q; want %q", in, pr.Dir, want)
		}
	}
	if pr := p.profile("Profile 9"); pr.Name != "Profile 9" {
		t.Errorf("unknown profile: %+v", pr)
	}
	// No Local State yet (a new Chrome writes it lazily): no profiles.
	if p := readChromeProfiles(t.TempDir()); len(p.choices()) != 0 {
		t.Errorf("no Local State: %+v", p)
	}
}

// Which profile Chrome's default browser context is comes from
// chrome://version; its name, from Local State. A profile that isn't
// the default (and wasn't while connected) needs a click.
func TestChromeProfileMapping(t *testing.T) {
	site := testSite(t)
	dir := startTestChrome(t, chromeLocalState, "--headless=new", site.URL)
	x := newTestCDP(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connectChrome(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	id, profileDir, err := c.defaultProfile(ctx)
	if err != nil || profileDir != "Default" || id == "" {
		t.Fatalf("default profile: %q %q %v", id, profileDir, err)
	}
	// Shoulder's chrome://version page is gone, and was never a tab.
	for _, p := range x.targets("page") {
		if strings.HasPrefix(p.URL, "chrome://version") {
			t.Errorf("left behind: %+v", p)
		}
	}
	for _, tab := range x.targets("tab") {
		if strings.HasPrefix(tab.URL, "chrome://version") {
			t.Errorf("a chrome://version tab: %+v", tab)
		}
	}
	var we *waitError
	if err := c.chooseProfile(ctx, "Profile 1"); !errors.As(err, &we) || !strings.Contains(we.msg, "Click a Work window") || !strings.Contains(we.msg, "Personal") {
		t.Errorf("choosing Work: %v", err)
	}
	if err := c.chooseProfile(ctx, "Default"); err != nil || c.label() != "Chrome (Personal)" {
		t.Errorf("choosing Personal: %v %q", err, c.label())
	}
	// Another browser context's tabs (an incognito-like one, made over
	// the protocol) aren't the profile's.
	bc, err := tcall(x, target.CreateBrowserContext, target.CreateBrowserContextParams{})
	if err != nil {
		t.Fatal(err)
	}
	x.open(target.CreateTargetParams{URL: site.URL + "/other", BrowserContextID: bc.BrowserContextID})
	cands, err := c.candidates(ctx)
	if err != nil || len(cands) != 1 || cands[0].URL != site.URL+"/" {
		t.Errorf("candidates: %v %+v", err, cands)
	}
}

// A session in a named profile says so, to the person and the agent.
func TestChromeProfileSession(t *testing.T) {
	site := testSite(t)
	dir := startTestChrome(t, chromeLocalState, "--headless=new", site.URL)
	cfg, c, res := browserSession(t, dir, url.Values{"profile": {"Profile 1"}})
	if !strings.Contains(res.Wait, "Click a Work window") || len(res.Tabs) != 0 {
		t.Errorf("sharing Work: %+v", res)
	}
	b, err := c.post("/ctl/browser?" + url.Values{"browser": {"chrome"}, "dir": {dir}, "profile": {"Default"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &res)
	if res.Label != "Chrome (Personal)" || len(res.Tabs) != 1 {
		t.Fatalf("sharing Personal: %+v", res)
	}
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	if g := a.get(""); !strings.Contains(g, `in their "Personal" profile`) {
		t.Errorf("guide:\n%s", g)
	}
	if info, _ := c.do("GET", "/ctl/info"); !strings.Contains(string(info), `"browser": "Chrome (Personal)"`) {
		t.Errorf("info: %s", info)
	}
}

// With two profiles open in one (headful) Chrome: the profile shared is
// the one chosen, and tabs the agent opens go into it, whichever
// profile the person is using. Chrome's windows show up on the screen,
// so this runs only with SHOULDER_TEST_HEADFUL=1.
func TestChromeTwoProfiles(t *testing.T) {
	if os.Getenv("SHOULDER_TEST_HEADFUL") == "" {
		t.Skip("set SHOULDER_TEST_HEADFUL=1 to run (opens Chrome windows)")
	}
	site := testSite(t)
	dir := startTestChrome(t, chromeLocalState, site.URL+"/done?q=personal")
	// A second launch on the same dir hands the window to the running
	// Chrome, in the other profile.
	bin, _ := chromeBinary()
	if err := runQuiet(bin, "--user-data-dir="+dir, "--profile-directory=Profile 1", "--no-first-run", site.URL+"/done?q=work"); err != nil {
		t.Fatal(err)
	}
	x := newTestCDP(t, dir)
	contexts := func() map[string]cdp.BrowserContextID {
		out := map[string]cdp.BrowserContextID{}
		for _, tab := range x.targets("tab") {
			if strings.Contains(tab.URL, "q=") {
				out[tab.URL[strings.Index(tab.URL, "q=")+2:]] = tab.BrowserContextID
			}
		}
		return out
	}
	waitUntil(t, "the Work window", func() bool { return len(contexts()) == 2 })
	byURL := contexts()
	personal, work := byURL["personal"], byURL["work"]
	if personal == work {
		t.Fatalf("one context for both profiles: %v", byURL)
	}
	activate := func(q string) {
		for _, p := range x.targets("page") {
			if strings.HasSuffix(p.URL, "q="+q) {
				tcall(x, target.ActivateTarget, target.ActivateTargetParams{TargetID: p.TargetID})
			}
		}
	}
	log := traceProtocol(t)
	// The person is in Work (its window opened last); they share Personal.
	activate("work")
	cfg, c, res := browserSession(t, dir, url.Values{"profile": {"Default"}, "tabs": {"all"}})
	if !strings.Contains(res.Wait, "Click a Personal window") {
		t.Fatalf("sharing Personal from Work: %+v", res)
	}
	// They click a Personal window, and the launcher asks again.
	activate("personal")
	waitUntil(t, "Personal to be shared", func() bool {
		b, _ := c.post("/ctl/browser?" + url.Values{"browser": {"chrome"}, "dir": {dir}, "profile": {"Default"}, "tabs": {"all"}}.Encode())
		json.Unmarshal(b, &res)
		return len(res.Tabs) > 0
	})
	if res.Label != "Chrome (Personal)" || len(res.Tabs) != 1 || !strings.HasSuffix(res.Tabs[0].URL, "q=personal") {
		t.Fatalf("shared: %+v", res)
	}
	rw, _ := c.share("unix", false, true)
	a := agentFromPaste(t, cfg, rw.Paste)
	// Personal is Chrome's default now: the protocol opens the tab.
	if code, out := a.call("POST", "open?background=1", site.URL+"/done?q=agent1"); code != 200 {
		t.Fatalf("open: %d %s", code, out)
	}
	// The person goes back to Work; the agent's next tab still goes
	// into Personal (opened from the shared Personal tab).
	activate("work")
	time.Sleep(500 * time.Millisecond)
	if code, out := a.call("POST", "open", site.URL+"/done?q=agent2"); code != 200 {
		t.Fatalf("open from Work: %d %s", code, out)
	}
	// A tab the person opens in Work isn't shared.
	activate("work")
	runQuiet(bin, "--user-data-dir="+dir, "--profile-directory=Profile 1", site.URL+"/done?q=work2")
	waitUntil(t, "the tabs", func() bool {
		cs := contexts()
		return cs["agent1"] != "" && cs["agent2"] != "" && cs["work2"] != ""
	})
	cs := contexts()
	if cs["agent1"] != personal || cs["agent2"] != personal || cs["work2"] != work {
		t.Errorf("contexts: personal %s work %s: %v", personal, work, cs)
	}
	// The second tab came from the page, Chrome having refused to
	// open one in Personal while Work was in use.
	if !log.said("window.open(") {
		t.Error("agent2 wasn't opened by the page")
	}
	time.Sleep(time.Second)
	st := a.get("state")
	if !strings.Contains(st, "q=agent1") || !strings.Contains(st, "q=agent2") || strings.Contains(st, "q=work") {
		t.Errorf("state: %s", st)
	}
}

func runQuiet(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	return cmd.Run()
}
