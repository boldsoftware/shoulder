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
