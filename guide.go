package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// paste is what the user hands to an agent: one command, whose output
// (the guide) tells the agent everything else.
func paste(cfg *config, sh *share, token string, readOnly bool) string {
	return "Run this to use my terminal: " + sh.curl + " " + sh.url(token) + "/"
}

// guide is the full description, served at the base URL.
func guide(cfg *config, sh *share, token string, readOnly bool) string {
	call := sh.curl + " " + sh.url(token)
	access := "read-write: you can type into it"
	if readOnly {
		access = "read-only: you can watch it, but not type into it"
	}
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("# shoulder: a shared terminal\n\n")
	p("You're connected to a live terminal on %s: session %q, running `%s` in %s.\n", cfg.Host, cfg.Name, displayCommand(cfg.Command), tildePath(cfg.Cwd))
	p("A person uses this terminal too, possibly right now. Your access is %s.\n\n", access)
	p("Every call is:\n\n    %s/ENDPOINT\n\n", call)
	p(`## Look

- **GET capture-pane** - what the screen shows now, as text, like
  `+"`tmux capture-pane -p`"+`. Start here, and look again before you type.
- **GET status** - JSON: running, exit_code, foreground (the process in the
  terminal's foreground, e.g. "make"), at_prompt (the command itself, e.g.
  the shell, is in the foreground), cols/rows, attached_terminals.
- **GET output** - what the program printed, as lines with escape codes
  stripped and progress bars collapsed. Each reply ends with `+"`[offset N]`"+`;
  `+"`output?since=N`"+` returns only what came after. It can be long, so save
  it to a file and search that:

      %s/output > /tmp/shoulder-output.txt

## Wait

**GET wait** blocks until something happens, then says what (first line,
in brackets), followed by the output since the wait began and `+"`[offset N]`"+`.

- `+"`wait?idle=1`"+` - the shell is back at its prompt: the job finished.
- `+"`wait?pattern=REGEX`"+` - a line of output matches (Go syntax; URL-encode it).
- `+"`wait?quiet=5`"+` - nothing printed for 5 seconds.
- Always: the command exits, or `+"`timeout`"+` seconds pass (default 60, max 1800).

Combine them, e.g. `+"`wait?idle=1&timeout=600`"+`. `+"`since=N`"+` counts output from
offset N. A wait that starts within a few seconds of your own typing counts
from the keystrokes, so typing and then waiting can't miss a quick command.
`, call)
	if !readOnly {
		p(`
## Type

- **POST run** - the body is a command line for the shell. It is typed
  and Enter pressed; the reply comes once the prompt is back (or after
  `+"`run?timeout=SECONDS`"+`, default 60) and holds the command's output.
  Use it when the terminal is sitting at a shell prompt.

      %[1]s/run -d 'go test ./...'

- **POST send-keys** - the body is like `+"`tmux send-keys`"+` arguments: words that
  name keys are pressed, other words are typed; quote text that has
  spaces. A leading `+"`-l`"+` types everything literally. Returns at once;
  follow it with wait or capture-pane.

      %[1]s/send-keys -d "'git status' Enter"
      %[1]s/send-keys -d 'C-c'
      %[1]s/send-keys -d 'Escape :wq Enter'

  Keys: Enter Tab BTab Escape BSpace Space Up Down Left Right Home End
  PageUp PageDown (PPage NPage) Delete Insert F1-F12, C-x for control,
  M-x for meta.

## Manners

This is someone's terminal. Look at the screen before you type; they may
be in the middle of something. Ask before anything destructive, never
type secrets, and leave things as you'd want to find them.
`, call)
	}
	return b.String()
}

// displayCommand shows argv with the program's directory dropped:
// "bash" rather than "/opt/homebrew/bin/bash".
func displayCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return strings.Join(append([]string{filepath.Base(argv[0])}, argv[1:]...), " ")
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			return "~/" + rel
		}
		if p == home {
			return "~"
		}
	}
	return p
}
