# shoulder

```sh
go run github.com/boldsoftware/shoulder@latest
```

\<human\>
Shoulder lets an agent look "over your shoulder" at the same terminal session
you're staring at. 

Say you're doing some ops: `for x in $(cat hosts.txt); do ssh $x some stuff; done`.
The stuff logs a lot, and, well, you've started at it for a minute or two, and now
it's getting boring. You want an agent to keep an eye on it! You started the driving,
but you just want a heads up!

The idea came from our experience with Athena (see
https://blog.exe.dev/athena-deploys-exe) which supervises deployments. I wanted
the same thing for ad-hoc ops work.

The technical idea is that this is just the session sharing part of tmux
or dtach (see also https://blog.exe.dev/exe-scroll), but your agent
can connect to it. We have several connection mechanisms available, since
your agent might be in a very different place than you. Kudos to tailcat
for making this easy. (Tailcat makes this similar to https://github.com/tmate-io/tmate (rip)
or https://upterm.dev/.)

The implementation is vibe-coded.
\</human\>

## Sharing a browser

shoulder can share browser tabs instead of a terminal: an agent reads the
page, takes screenshots, clicks, types, and runs JavaScript over the same
kind of curl API. On the first screen, press `[2]` for Chrome or `[3]` for
Safari, or start with:

```sh
shoulder browser            # your Chrome: the tab you're on
shoulder browser -tabs new  # a new tab, just for the agent
shoulder browser -tabs all  # every tab of the profile, and new ones
shoulder browser -profile Work  # a Chrome profile other than the one you used last
shoulder browser -standalone  # a standalone Chrome: no setup
shoulder browser safari     # Safari (macOS)
```

The agent gets the tab you're on, plus the tabs and popups it opens
itself; it can close only the tabs it opened. Paste the first screen's
text to the agent and it finds out the rest (`GET <url>/` is the guide).
While sharing, your terminal shows the shared tabs and what the agent does;
`[d]` detaches (`shoulder attach` comes back), `[q]` stops sharing.

**Chrome** (version 144 or later) is shared over the DevTools protocol.
Once, open `chrome://inspect/#remote-debugging` and turn on "Allow remote
debugging for this browser instance"; Chrome remembers it. Then each time
sharing starts, Chrome asks "Allow remote debugging?": click Allow. While
it's shared, Chrome shows a "controlled by automated test software" bar.
The first screen tells you if Chrome isn't running or remote debugging is
off. shoulder never closes your tabs or your Chrome: when sharing stops it
lets go of the tabs, and the only tabs it closes are ones the agent opened
and asked it to close.

If you have several Chrome profiles open (Personal, Work), `[p]` on the
first screen (or `-profile`) picks the one to share; by default it's the
one you used last. The agent's tabs open in that profile, and "all tabs"
means all of that profile's tabs. Chrome only tells shoulder which profile
you used last, so if you pick another one, shoulder asks you to click one
of its windows first. (Chrome lets shoulder open tabs directly only in the
profile you used last; otherwise the agent's tab is opened from a shared
tab, in front.)
`$SHOULDER_CHROME_DIR` sets Chrome's user data directory, for a Chrome
other than Google Chrome's usual one.

To skip all that, `-standalone` (or `[o]` on the first screen) has
shoulder start a standalone Chrome instead: no remote debugging to turn
on, no Allow to click. It has its own profile, in shoulder's config
directory (`~/Library/Application Support/shoulder/chrome` on macOS,
`~/.config/shoulder/chrome` on Linux), kept from one share to the next, so
it's signed in to whatever you've signed in to there and nothing else.
shoulder quits it when sharing stops, unless it was already running.
`$SHOULDER_CHROME` sets the Chrome executable to start.

**Safari** is driven with AppleScript. Turn on Develop › "Allow JavaScript
from Apple Events" (Settings › Advanced › "Show features for web
developers" shows the Develop menu); the first time, macOS asks whether
your terminal may control Safari. Safari has no real mouse or keyboard
for scripts, so clicks and typing are simulated in the page, which most
pages accept; it can't see or answer alerts; and screenshots are of the
whole window, tab bar included, and need Screen Recording permission for
your terminal.
