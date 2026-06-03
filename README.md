# quack

<p align="center">
  <img src="assets/quack.png" alt="quack — a brain-duck" width="180">
</p>

Start Claude/Codex agent sessions from Discord. quack runs on your machine,
connects out to the Discord Gateway, and on a mention resolves a repo, creates a
git worktree, and launches the agent in a tmux session with your prompt.

No inbound port forwarding, dynamic DNS, or Tailscale exposure is required for
the bot itself.

## Usage

Just talk to quack. By default the **whole message is a natural-language
request** — a quick read-only agent reads it (plus recent channel context) and
figures out the repo/path, agent, effort, base branch, name, and whether to run
headless, then launches the session for you:

```text
@quack investigate the directory cache pin bug in dagger/dagger and reproduce it with a failing test
```

It posts a one-line note echoing how it interpreted the request so you can see
(and correct) its choices.

### Explicit directive (`!`)

Prefix the mention with **`!`** to skip inference and spell the directive out
yourself. The **first line** is the directive line (it may be empty); the prompt
is whatever follows the first newline:

```text
@quack ! [repo-or-path] [codex] [no-headless] [no-wt] [effort=high] [name=fix-x] [base=main]
<your multiline prompt, starting on the next line>
```

Defaults:

- **claude** is the default agent; the bare keyword **`codex`** switches to Codex.
- **headless** is the default (a two-way Discord conversation, below); the bare
  keyword **`no-headless`** gives you a plain interactive tmux session instead.
- **claude effort** defaults to `xhigh`.
- **no repo/path** → runs in the shared scratch workspace (`scratch_dir`,
  default `~/dev/work`) for quick questions; pass the literal target
  **`temp-dir`** for a fresh throwaway directory instead.
- **no `name=`** → the agent suggests a short task-based name from your prompt
  (e.g. `readme-suggestions`); falls back to `<repo>-<base>-<rand>` if it can't.
- **`no-wt`** (advanced, dangerous) → skip the worktree and run **directly in the
  repo checkout**. Parallel sessions on the same repo can clash — use sparingly.

Example:

```text
@quack ! dagger/dagger effort=high
Investigate the directory cache pin bug; reproduce with a failing test.
```

quack replies in a per-session thread. In **headless** mode (the default) it
runs the agent non-interactively, posting the answer in segments as it's
produced (tool steps inline), and you reply in-thread to send another turn. Status shows as a reaction on your
message (👀 working → ✅ answered · ❌ error). Post **`/attach`** to promote the
session to a local tmux session you can jump into — same conversation, with
terminal + files; post `/stop` or archive the thread to end it. With
**`no-headless`** it starts an interactive tmux session from the start, with no
Discord back-channel:

```sh
tmux attach -t quack/<name>
```

## Setup

1. Create a Discord application and bot; enable the Message Content intent.
   Upload `assets/quack.png` as the application icon / bot avatar.
2. Invite the bot to your private server.
3. Copy `config.example.toml` to `~/.config/quack/config.toml` and fill in IDs.
4. Put `DISCORD_BOT_TOKEN=...` in `~/.config/quack/env` and `chmod 600` it.
5. Build the binary:

```sh
go build -o ~/.local/bin/quack ./cmd/quack
```

6. Install the systemd user unit and start it:

```sh
mkdir -p ~/.config/systemd/user
cp quack.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now quack
```

7. On a home server, allow the service to run without an active login session
   (start at boot, survive logout):

```sh
loginctl enable-linger "$USER"
```

Check it connected:

```sh
systemctl --user status quack
journalctl --user -u quack -f   # look for "quack connected"
```

## Design

See `hack/designs/2026-05-31-quack-design.md`.

## Tests

```sh
go test ./...
QUACK_INTEGRATION=1 go test ./...
```

The integration run needs `git` and `tmux` installed.

## License

[MIT](LICENSE)
