"""Monitor: poll agent sources, diff cumulative counters into rates,
classify state, and return a sorted snapshot."""

from __future__ import annotations

import time

from . import discovery, hermes
from .models import Agent, State, STATE_RANK, sort_key

# Sources whose sessions legitimately sit idle for a long time (interactive
# chat threads stay open forever). Long idle there = abandoned thread, not a
# stuck worker.
INTERACTIVE_SOURCES = {
    "slack",
    "telegram",
    "discord",
    "whatsapp",
    "signal",
    "teams",
    "matrix",
    "web",
}


class Monitor:
    def __init__(
        self,
        base: str = "~/.hermes",
        stuck_idle: float = 240.0,
        active_idle: float = 15.0,
        show_ended: bool = False,
        default_ctx: int = 128_000,
    ):
        self.base = base
        self.stuck_idle = stuck_idle
        self.active_idle = active_idle
        self.show_ended = show_ended
        self.default_ctx = default_ctx
        self._prev: dict[tuple[str, str], tuple[float, float]] = {}  # key -> (total, cost)
        self._prev_poll_ts: float | None = None
        self._ema: dict[tuple[str, str], tuple[float, float]] = {}  # key -> (tok/s, $/hr)

    # -- public API -------------------------------------------------------

    def snapshot(self, now: float | None = None) -> list[Agent]:
        now = now if now is not None else time.time()
        agents = self._load(now)
        self._diff_rates(agents, now)
        for agent in agents:
            self.classify(agent, now)
        return agents

    def sorted(self, agents: list[Agent], sort: str = "state") -> list[Agent]:
        return sorted(agents, key=sort_key(sort))

    # -- internals --------------------------------------------------------

    def _load(self, now: float) -> list[Agent]:
        agents: list[Agent] = []
        for install in discovery.find_hermes_installs(self.base):
            agents.extend(hermes.read_sessions(install, self.default_ctx))
        if not self.show_ended:
            agents = [a for a in agents if a.ended_at is None]
        return agents

    def _diff_rates(self, agents: list[Agent], now: float) -> None:
        dt = now - self._prev_poll_ts if self._prev_poll_ts is not None else None
        for agent in agents:
            key = (agent.source, agent.session_id)
            total = float(agent.total_burn_tokens)
            cost = agent.cost_usd
            if dt and key in self._prev and dt > 0:
                prev_total, prev_cost = self._prev[key]
                inst_tok = max(0.0, (total - prev_total) / dt)
                inst_cost = max(0.0, (cost - prev_cost) / dt * 3600.0)
                if key in self._ema:
                    etok, ecost = self._ema[key]
                    agent.tok_rate = 0.7 * etok + 0.3 * inst_tok
                    agent.cost_rate = 0.7 * ecost + 0.3 * inst_cost
                else:
                    agent.tok_rate = inst_tok
                    agent.cost_rate = inst_cost
                self._ema[key] = (agent.tok_rate, agent.cost_rate)
            elif agent.started_at is not None and now > agent.started_at:
                # first poll: no diff baseline yet — seed with lifetime
                # averages computed on the same clock as the poll
                elapsed = now - agent.started_at
                agent.tok_rate = total / elapsed
                agent.cost_rate = cost / elapsed * 3600.0
            self._prev[key] = (total, cost)
        self._prev_poll_ts = now

    def classify(self, agent: Agent, now: float | None = None) -> None:
        now = now if now is not None else time.time()
        if agent.ended_at is not None:
            agent.state = State.DONE
            return
        if agent.last_activity_at is None:
            agent.state = State.IDLE
            return
        idle = max(0.0, now - agent.last_activity_at)
        agent.idle_seconds = idle
        desc = (agent.last_action_desc or "").lower()
        if idle <= self.active_idle:
            agent.state = State.ACTIVE
        elif "wait" in desc:
            agent.state = State.WAITING
        elif (
            idle > self.stuck_idle
            and agent.source.split(":", 1)[-1] not in INTERACTIVE_SOURCES
        ):
            agent.state = State.STUCK
        else:
            agent.state = State.IDLE
