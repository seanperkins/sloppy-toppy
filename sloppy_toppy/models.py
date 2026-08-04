"""Unified agent model shared by all adapters, monitors, and UIs."""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from enum import Enum


class State(str, Enum):
    ACTIVE = "ACTIVE"
    WAITING = "WAITING"
    STUCK = "STUCK"
    IDLE = "IDLE"
    DONE = "DONE"


STATE_RANK = {
    State.ACTIVE: 0,
    State.WAITING: 1,
    State.STUCK: 2,
    State.IDLE: 3,
    State.DONE: 4,
}


@dataclass
class Agent:
    """One agent session as seen by sloppy-toppy.

    Token/cost fields are cumulative counters, as recorded by the source
    (Hermes `sessions` table etc.). Rates are derived by poll-diffing.
    """

    source: str  # e.g. "hermes:slack", "hermes:default"
    session_id: str
    title: str = ""
    model: str = ""
    started_at: float | None = None
    last_activity_at: float | None = None
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0
    reasoning_tokens: int = 0
    cost_usd: float = 0.0
    ctx_window: int = 128_000
    last_tool: str = ""
    last_action_desc: str = ""
    ended_at: float | None = None
    end_reason: str = ""
    owner_pid: int | None = None
    cwd: str = ""
    git_branch: str = ""
    api_call_count: int = 0
    tool_call_count: int = 0

    # computed by the monitor
    tok_rate: float = 0.0  # tokens/sec (EMA of poll diffs)
    cost_rate: float = 0.0  # $/hour (EMA of poll diffs)
    state: State = State.IDLE
    idle_seconds: float = 0.0

    @property
    def total_burn_tokens(self) -> int:
        """Tokens that cost real money: input + output + reasoning.

        Cache reads are excluded (cheap, and re-sent context is billed at
        cache rates — we surface them separately via cache_read_tokens).
        """
        return self.input_tokens + self.output_tokens + self.reasoning_tokens

    @property
    def ctx_used(self) -> int:
        """Approximate tokens occupying the context window (lower bound).

        Uses cumulative input + output + reasoning. cache_read_tokens is
        deliberately excluded: it accumulates across API calls (every call
        re-reads the whole prefix), so it is not "what is in the window
        now" — including it produced absurd >100% numbers.
        """
        return self.input_tokens + self.output_tokens + self.reasoning_tokens

    @property
    def ctx_pct(self) -> float:
        if self.ctx_window <= 0:
            return 0.0
        return min(100.0, 100.0 * self.ctx_used / self.ctx_window)

    @property
    def uptime(self) -> float:
        if not self.started_at:
            return 0.0
        end = self.ended_at or time.time()
        return max(0.0, end - self.started_at)

    @property
    def agent_name(self) -> str:
        """Short display name, e.g. 'hermes:slack'."""
        return self.source


# ---------------------------------------------------------------------------
# formatting helpers (shared by CLI batch mode and the TUI)
# ---------------------------------------------------------------------------


def fmt_uptime(seconds: float) -> str:
    if seconds <= 0:
        return "—"
    s = int(seconds)
    if s < 60:
        return f"{s}s"
    if s < 3600:
        return f"{s // 60}m{s % 60:02d}"
    if s < 86400:
        return f"{s // 3600}h{(s % 3600) // 60:02d}"
    return f"{s // 86400}d{(s % 86400) // 3600:02d}h"


def fmt_tokens(n: int) -> str:
    if n >= 1_000_000:
        return f"{n / 1_000_000:.1f}M"
    if n >= 1_000:
        return f"{n / 1_000:.1f}k"
    return str(n)


def fmt_usd(x: float) -> str:
    if x <= 0:
        return "0.00"
    if x < 0.01:
        return f"{x:.5f}"
    if x < 100:
        return f"{x:.2f}"
    return f"{x:.0f}"


def fmt_rate(tok_per_sec: float) -> str:
    if tok_per_sec < 0.05:
        return "0.0"
    return f"{tok_per_sec:.1f}"


def fmt_cost_rate(usd_per_hour: float) -> str:
    return f"{usd_per_hour:.2f}"


def sort_key(sort: str):
    def key(a: Agent):
        if sort == "cost":
            return (STATE_RANK[a.state], -a.cost_rate, -a.tok_rate)
        if sort == "tokens":
            return (STATE_RANK[a.state], -a.tok_rate, -a.cost_rate)
        if sort == "ctx":
            return (STATE_RANK[a.state], -a.ctx_pct, -a.cost_rate)
        return (STATE_RANK[a.state], -a.cost_rate, -a.tok_rate)

    return key
