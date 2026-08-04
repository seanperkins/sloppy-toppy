# sloppy-toppy

`top`, for the sloppy ones. A live terminal monitor for your AI agents —
token burn rate, context fill, cost, and a stuck-agent heartbeat, across
every agent runtime on the box.

The resources `top` measures (CPU, RAM) are mostly meaningless for an agent
process that's 95% idle waiting on an API call. What actually matters:

- **TOK/s** — how fast it's burning tokens
- **CTX%** — how full its context window is (the thing that makes agents go stupid)
- **$/HR** — what it's costing right now
- **STATE** — ACTIVE / WAITING / STUCK / IDLE, with a heartbeat detector that
  flags agents that stopped making progress

```
AGENT             MODEL                      TOK/s    CTX%    $/HR STATE  TOOL          UPTIME  TITLE
claude:api-server claude-opus-5              128.9   43.2%   63.23 ACTIVE bash           27m04  Refactor the auth middleware
claude:etl        claude-sonnet-5             12.1   88.4%    4.02 STUCK  read            3h11  Nightly ingest run
codex:parser      gpt-5.6-sol                  1.4   39.7%       ? IDLE   —              23h03  Track down the flaky test
hermes:default    deepseek-v4-flash-0731       2.6   ~4.4%    0.00 IDLE   web_search      4h52  Draft the release notes
```

## Install

Single static binary, no runtime required. This downloads to a temp file and
verifies it against the release checksums before installing — piping an
unverified download straight into `/usr/local/bin` is not a good trade for one
saved line:

```sh
REPO=seanperkins/sloppy-toppy
ASSET="sloppy-toppy_$(uname -s)_$(uname -m).tar.gz"
BASE="https://github.com/$REPO/releases/latest/download"

tmp=$(mktemp -d) && cd "$tmp" &&
curl -fsSL -O "$BASE/$ASSET" &&
curl -fsSL -O "$BASE/checksums.txt" &&
shasum -a 256 --ignore-missing -c checksums.txt &&
tar -xzf "$ASSET" sloppy-toppy &&
sudo install -m 0755 sloppy-toppy /usr/local/bin/sloppy-toppy
```

`curl -f` matters: without it a 404 writes GitHub's error page to the file and
exits 0, handing the failure to `tar` instead of reporting it.

Or build from source (Go 1.26+, matching `go.mod`):

```sh
go install github.com/seanperkins/sloppy-toppy/cmd/sloppy-toppy@latest
```

## Usage

```sh
sloppy-toppy                    # live TUI
sloppy-toppy --once             # one snapshot as a table (cron-friendly)
sloppy-toppy --once --json      # one snapshot as JSON lines
sloppy-toppy -a                 # include finished sessions
sloppy-toppy --sort cost        # sort by $/hr
sloppy-toppy --providers codex  # poll one runtime only
```

TUI keys: `q` quit · `s` cycle sort · `a` toggle ended sessions · `d`/`Enter`
detail pane · `r` refresh · `j`/`k` move.

Piping to a file or a pipe automatically behaves like `--once`, so
`sloppy-toppy > snapshot.txt` works without a TTY.

## Supported runtimes

| | **Hermes** | **Claude Code** | **Codex** |
|---|---|---|---|
| Storage | sqlite `state.db` | JSONL transcripts | JSONL rollouts |
| Token counters | cumulative | per-call, summed | cumulative |
| Cost | reported by the runtime | derived from pricing | derived from pricing |
| Context window | `context_length_cache.yaml` | model table | self-reported |
| Context fill | lower bound (`~`) | measured | measured |
| Liveness | `gateway.pid` | file mtime | file mtime |

**OpenRouter** is different in kind: a remote billing API with no per-session
heartbeat, no tool calls, and no context window. Forcing it into agent rows
would leave most columns meaningless, so it surfaces as a separate spend
strip on its own slow poll (60s by default), enabled by setting
`OPENROUTER_API_KEY`. Without that variable the source stays silent rather
than reporting a permanent error.

## How it reads agents

- **Rates are poll-diffed.** Token and cost counters are cumulative, so rates
  come from differencing between refreshes (the same trick `top` uses on
  `/proc`), smoothed with an EMA. The first poll seeds from lifetime averages
  because there is no baseline yet — so `--once` reports a lifetime average,
  not an instantaneous rate.

- **Context fill is honest about its accuracy.** Claude Code and Codex expose
  the real prefix a call sent, so their CTX% is a measurement. Hermes can only
  offer a lower bound (its cumulative `cache_read_tokens` accumulate per call
  and would blow past 100%), so those figures are prefixed with `~`.

- **Cost distinguishes three cases.** Reported by the runtime, estimated from
  the local pricing table, or genuinely unknown — shown as `?`, never as
  `$0.00`. A model we ship no rates for is not free, and saying otherwise is a
  lie a cost monitor can't afford. Add your own rates for unlisted models (any
  OpenRouter or self-hosted model) in `~/.config/sloppy-toppy/pricing.json`:

  ```json
  { "gpt-5.6-sol": { "input_per_mtok": 2.5, "output_per_mtok": 10 } }
  ```

- **STUCK keys off the session's own origin, not the profile name.** A cron
  job is worth flagging when it goes quiet; an idle Slack thread is just an
  abandoned conversation. That distinction comes from each runtime's own
  source field (Hermes `sessions.source`, Claude Code `entrypoint`, Codex
  `source`) — so a profile *named* "slack" that runs cron jobs is still
  eligible for STUCK.

- **A finished session is only inferred where that's safe.** Claude Code and
  Codex write no end marker — there is no terminal stop record — so silence
  cannot distinguish a completed run from a wedged one. The inference is
  therefore applied only to sessions a human was driving: those go quiet
  because someone walked away, and hiding them keeps the table readable
  (`--assume-ended-after`, `end_reason` records that the end was inferred).

  Unattended sessions are deliberately excluded. Calling one DONE on nothing
  but silence would retire exactly the alert this tool exists to raise, so they
  stay live and classify STUCK. Bound how far back they're considered with
  `--lookback` rather than by guessing that quiet means finished.

- **Counters are never silently partial.** A transcript line too large to read
  is skipped rather than ending the scan, and the session is flagged
  `incomplete` in `--json`. Stopping at that line — what an unchecked
  `bufio.Scanner` does — would freeze a session's reported spend for the rest
  of its life while still looking authoritative.

- **Provider-supplied text is sanitized before it reaches your terminal.**
  Titles are user prompts and action descriptions are runtime text, so they can
  carry ANSI escapes. Rendered raw, a monitored agent could repaint its own row
  — showing STUCK as ACTIVE, or a $40/hr burn as $0.02 — and OSC 52 would write
  your clipboard. Control sequences are stripped at the render boundary.

- **Polling is proportional to live work, not to accumulated history.**
  File-based runtimes are filtered on modification time before anything is
  parsed (`--lookback`, default 12h). On a box with 2541 Claude Code
  transcripts totalling 903 MB, only 7 had been touched in the last hour —
  parsing them all would cost ~2.2s out of a 2s refresh interval.

- **Read-only, always.** Hermes databases are opened with `mode=ro`, and no
  adapter writes to any runtime's state.

## Development

```sh
go test ./...
go test -race ./...
```

Tests are fully hermetic: every adapter runs against synthetic fixtures in a
temp directory, so the suite passes on any machine with no agent runtime
installed.

## Building a release

```sh
goreleaser release --clean
```

All four targets — `darwin/arm64`, `darwin/amd64`, `linux/amd64`,
`linux/arm64` — cross-compile from a single machine because the sqlite driver
is pure Go (`modernc.org/sqlite`) and `CGO_ENABLED=0`. Swapping in a cgo
sqlite binding would break that and force a per-platform build matrix.
