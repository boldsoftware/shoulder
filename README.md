# shoulder

Let an AI agent look over your shoulder, and lend a hand, in a terminal
you're already using.

`shoulder` runs a command (your shell, by default) in a session you can
detach from and reattach to, like `dtach`. It also serves that session over
HTTP, so an agent such as Claude Code or Codex, on this machine or another
one, can read the screen, wait for things to finish, and type. All the
agent needs is `curl`, and you give it one line to paste.

## Install

```sh
go install github.com/boldsoftware/shoulder@latest
```

Or run it without installing:

```sh
go run github.com/boldsoftware/shoulder@latest
```

It runs on macOS and Linux.

## Use

```sh
shoulder                 # share your $SHELL
shoulder -- make -j8     # or any command
```

Before the command starts, shoulder shows a line to paste to your agent:

```
shoulder · session cosmic-quail · bash

Paste this to an agent (Claude Code, Codex, or one on another machine):

Run this to use my terminal: curl -s --unix-socket ~/Library/Caches/shoulder/cosmic-quail/sock http://shoulder/77-beach-globe/

share  [u] unix socket  [h] localhost  [*] all interfaces  [s] tailscale  [t] tailcat
access [r] read-write  (r switches to read-only)

[enter] start bash   [c] copy   [q] quit
```

Paste the line to the agent and press Enter. The agent fetches the URL,
which returns a guide to everything else. Then you keep working in your
terminal, and the agent works in it too.

- **Ctrl-\\** detaches. The command keeps running. `shoulder attach`
  reattaches, with the screen repainted.
- `shoulder ls` lists sessions.
- `shoulder share [-t transport] [-read-only] [name]` prints a line to
  paste for a running session, adding a transport if needed.

### How the agent connects

| Key | `-t`        | The agent runs                                   | Good for |
|-----|-------------|--------------------------------------------------|----------|
| `u` | `unix`      | `curl --unix-socket …/sock http://shoulder/…`    | Agents on this machine (default) |
| `h` | `localhost` | `curl http://127.0.0.1:PORT/…`                   | Agents on this machine that can't use Unix sockets |
| `*` | `lan`       | `curl http://192.168.x.y:PORT/…`                 | Your local network. Plain HTTP: see Security |
| `s` | `tailscale` | `curl http://your-machine.tailnet.ts.net:PORT/…` | Agents anywhere on your tailnet |
| `t` | `tailcat`   | `tailcat socks curl http://tc…:PORT/…`           | Agents anywhere, through an ephemeral [tailcat](https://github.com/tailscale/tailcat) node. Both machines need `tailcat`. |

`r` (or `-read-only`) switches between a read-write code and a read-only
code. An agent with the read-only code can watch, but can't type.

To skip the first screen, use `-y`, which prints the line and starts right
away:

```sh
shoulder -y -t tailscale -read-only -- tail -f /var/log/system.log
```

## What the agent gets

Everything lives under the pasted URL. `GET /` is the guide, written using
that agent's own curl command.

| Endpoint | Does |
|----------|------|
| `GET capture-pane` | What the screen shows now, as text, like `tmux capture-pane -p` |
| `GET status` | JSON: running, exit code, foreground process, at the prompt, size |
| `GET output?since=N` | What the program printed, as clean lines. Each reply ends with `[offset N]` for the next call. |
| `GET wait?idle=1` | Blocks until the shell is back at its prompt. It also accepts `pattern=REGEX`, `quiet=SECONDS` and `timeout=SECONDS`, and returns early if the command exits. |
| `POST run` | The body is a command line. It's typed with Enter, and the reply is the command's output once the prompt returns. |
| `POST send-keys` | The body uses `tmux send-keys` syntax: `-d "'git status' Enter"`, `-d C-c`, `-d 'Escape :wq Enter'` |

`run` and `send-keys` need the read-write code.

## Security

- **Codes** look like `77-beach-globe`, wormhole-style, and can be read
  aloud. They are short, so network shares rate-limit wrong guesses. After
  10 wrong codes, a share locks for 10 minutes, even against the right
  code. At that rate, guessing a code would take years.
- **The Unix socket** is protected by file permissions: only you can reach
  it. It never locks.
- **`lan`** is plain HTTP on every interface. Anyone on the network who
  sees the URL can use the session. Prefer `tailscale` or `tailcat`, which
  are encrypted.
- **Read-write access** lets an agent type anything you could type.
  Consider `-read-only` when you only want it to watch.

## Flags

```
-t transport   unix (default), localhost, lan, tailscale, tailcat
-read-only     share read-only access
-y             skip the first screen
-port N        TCP port for network transports (default: any free port)
-name NAME     session name (default: two random words)
-linger D      keep the final screen readable this long after exit (default 10m)
```

## How it works

A background session server owns the command's terminal (a PTY). It keeps
a terminal emulator ([vt10x](https://github.com/hinshun/vt10x)) in step with
the output, so it can render the screen as text for agents and repaint it
when you reattach. Your terminal attaches over the session's Unix socket.
The same socket, and any extra listeners you choose, serve the HTTP API.
"At the prompt" means the command itself, usually your shell, owns the
terminal's foreground process group again.

Sessions live in `~/Library/Caches/shoulder/` on macOS, or `~/.cache/shoulder/`
on Linux.

## Limits

- **Screen accuracy:** vt10x is a simple emulator. Shells, pagers and most
  tools render fine, but elaborate full-screen programs may come through
  imperfectly.
- **Reattaching** repaints only the current screen; earlier output isn't
  replayed into your terminal's scrollback. Agents can still read all of it
  with `output`.
- **Output kept:** the last 8 MiB.
