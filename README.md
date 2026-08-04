# sloppy-toppy

`top`, for the sloppy ones. A live terminal monitor for your AI agents —
token burn rate, context fill, cost, and a stuck-agent heartbeat. The
resources `top` measures (CPU, RAM) are mostly meaningless for an agent
process that's 95% idle waiting on an API call. What actually matters:

- **TOK/s** — how fast it's burning tokens
- **CTX%** — how full its context window is (the thing that makes agents go stupid)
- **$/HR** — what it's costing right now
- **STATE** — ACTIVE / WAITING / STUCK / IDLE, with a heartbeat detector that
  flags agents that stopped making progress

```
AGENT           MODEL                          TOK/s   CTX%   $/HR STATE  TOOL           UPTIME  TITLE
hermes:slack    deepseek-v4-flash-0731          57.7  13.5%   0.00 ACTIVE patch           40m50  What should sloppy-toppy do
hermes:default  deepseek-v4-flash-0731          51.1 100.0%   0.02 STUCK  terminal        1d02h  GitHub Notification Setup for Autonomous Replies
```

## Install

```sh
python3 -m venv .venv
.venv/bin/pip install -e ".[dev]"
ln -sf "$PWD/.venv/bin/sloppy-toppy" ~/.local/bin/sloppy-toppy   # optional
```

## Usage

```sh
sloppy-toppy                 # live TUI (needs a TTY)
sloppy-toppy --once          # one snapshot as a table (cron-friendly)
sloppy-toppy --once --json   # one snapshot as JSON lines
sloppy-toppy -a              # TUI including finished sessions
sloppy-toppy --sort cost     # sort by $/hr
```

TUI keys: `q` quit · `s` cycle sort (state/cost/tokens/ctx) · `a` toggle
ended sessions · `d`/`Enter` detail pane · `r` refresh · `j`/`k` move.

Flags: `--base` (Hermes home, default `~/.hermes`), `--stuck-idle` (default
240s), `--active-idle` (default 60s), `--refresh` (default 2s).

## How it reads agents

v0.1 monitors **Hermes** (multi-profile: it discovers every live gateway
under `~/.hermes` and `~/.hermes/profiles/*` via their `gateway.pid` files
and reads each `state.db` read-only). Claude Code / Codex / OpenCode
adapters are the planned extension points.

- Token/cost counters are cumulative, so rates are derived by **poll-diffing**
  between refreshes (the same trick `top` uses on `/proc`), smoothed with an
  EMA; the first poll seeds with lifetime averages.
- Context window sizes come from each profile's `context_length_cache.yaml`
  (fallback 128k). CTX% is a lower bound on peak context: cumulative
  `cache_read_tokens` are excluded because they accumulate per API call and
  would blow past 100%.
- **STUCK** = non-interactive source (cron/CLI) idle past `--stuck-idle`.
  Interactive chat threads (slack/telegram/…) idle for a long time are just
  abandoned threads → shown as IDLE, not stuck.

All DB access is `mode=ro`; sloppy-toppy never writes to a Hermes state DB.

## Test

```sh
.venv/bin/pytest -q
```

Hermetic tests use a synthetic Hermes install; golden tests assert against
the live box's real state DB (they skip if no Hermes gateway is running).
