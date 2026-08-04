"""Hermetic tests: monitor logic against a synthetic Hermes install."""

import json
import os
import sqlite3

import pytest

from sloppy_toppy.monitor import Monitor
from sloppy_toppy.models import Agent, State


def make_install(tmp_path, ctx_cache: dict | None = None) -> str:
    """Create a fake ~/.hermes with a live-gateway pid and one session."""
    base = tmp_path / "hermes"
    base.mkdir()
    # gateway.pid: point at THIS process so /proc/<pid> exists -> "alive"
    with open(base / "gateway.pid", "w") as fh:
        json.dump({"pid": os.getpid(), "kind": "hermes-gateway", "hermes_home": str(base)}, fh)

    db = sqlite3.connect(base / "state.db")
    db.executescript(
        """
        CREATE TABLE sessions (
            id TEXT, source TEXT, model TEXT, started_at TEXT, ended_at TEXT,
            end_reason TEXT, input_tokens INTEGER, output_tokens INTEGER,
            cache_read_tokens INTEGER, cache_write_tokens INTEGER,
            reasoning_tokens INTEGER, estimated_cost_usd REAL,
            last_activity_at TEXT, last_activity_description TEXT, title TEXT,
            cwd TEXT, git_branch TEXT, api_call_count INTEGER, tool_call_count INTEGER
        );
        CREATE TABLE messages (
            id INTEGER PRIMARY KEY, session_id TEXT, tool_name TEXT, content TEXT
        );
        """
    )
    db.execute(
        """INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
        (
            "sess-a", "cli", "test/model-1", "100.0", None, None,
            100, 50, 40, 0, 10, 0.001, "995.0", "", "Fixture Agent",
            "/tmp", "main", 2, 1,
        ),
    )
    db.execute(
        "INSERT INTO messages (session_id, tool_name, content) VALUES (?,?,?)",
        ("sess-a", "terminal", "{}"),
    )
    db.commit()
    db.close()

    if ctx_cache:
        with open(base / "context_length_cache.yaml", "w") as fh:
            for key, val in ctx_cache.items():
                fh.write(f"  {key}: {val}\n")
    return str(base)


# ---------------------------------------------------------------------------
# classify
# ---------------------------------------------------------------------------

def _agent(**kw) -> Agent:
    defaults = dict(
        source="hermes:default",
        session_id="x",
        started_at=0.0,
        last_activity_at=1000.0,
    )
    defaults.update(kw)
    return Agent(**defaults)


def test_classify_done():
    a = _agent(ended_at=1001.0)
    Monitor().classify(a, now=2000.0)
    assert a.state is State.DONE


def test_classify_active():
    a = _agent(last_activity_at=1995.0)
    Monitor().classify(a, now=2000.0)
    assert a.state is State.ACTIVE


def test_classify_waiting_on_approval():
    a = _agent(last_activity_at=1900.0, last_action_desc="waiting for user approval (80s elapsed)")
    Monitor().classify(a, now=2000.0)
    assert a.state is State.WAITING


def test_classify_stuck_noninteractive():
    a = _agent(source="hermes:default", last_activity_at=1500.0)  # 500s idle > 240
    Monitor(stuck_idle=240.0).classify(a, now=2000.0)
    assert a.state is State.STUCK


def test_classify_idle_chat_thread_is_not_stuck():
    a = _agent(source="hermes:slack", last_activity_at=1500.0)  # long idle, but a chat thread
    Monitor(stuck_idle=240.0).classify(a, now=2000.0)
    assert a.state is State.IDLE


def test_classify_no_activity_is_idle():
    a = _agent(last_activity_at=None)
    Monitor().classify(a, now=2000.0)
    assert a.state is State.IDLE


# ---------------------------------------------------------------------------
# rates via poll-diffing (fixture-backed, hermetic)
# ---------------------------------------------------------------------------

def _bump_session(base, input_tokens, cost):
    db = sqlite3.connect(f"{base}/state.db")
    db.execute("UPDATE sessions SET input_tokens=?, estimated_cost_usd=? WHERE id='sess-a'",
               (input_tokens, cost))
    db.commit()
    db.close()


def test_rates_computed_by_diff(tmp_path):
    base = make_install(tmp_path)
    mon = Monitor(base=base)
    agents = mon.snapshot(now=1000.0)
    assert len(agents) == 1
    # first poll has no diff baseline: rates are seeded with lifetime averages
    # burn = 100 in + 50 out + 10 reasoning = 160 over 900s uptime
    assert agents[0].tok_rate == pytest.approx(160 / 900, abs=1e-3)
    assert agents[0].cost_rate == pytest.approx(0.001 / 900 * 3600, abs=1e-4)

    _bump_session(base, input_tokens=160, cost=0.0011)  # +60 burn tokens, +$0.0001
    agents = mon.snapshot(now=1002.0)
    a = agents[0]
    assert a.tok_rate == pytest.approx(30.0, abs=0.01), "60 tokens / 2s"
    assert a.cost_rate == pytest.approx(0.18, abs=0.01), "$0.0001 / 2s * 3600"
    assert a.state is State.ACTIVE  # 7s idle at now=1002 (last_activity=995)

    # with no further counter movement, rates decay to ~0
    agents = mon.snapshot(now=1004.0)
    assert agents[0].tok_rate < 30.0


def test_ctx_window_from_cache_file(tmp_path):
    base = make_install(tmp_path, ctx_cache={"test/model-1@http://x": 4096})
    agents = Monitor(base=base).snapshot(now=1000.0)
    assert agents[0].ctx_window == 4096
    # ctx_used = in 100 + out 50 + reasoning 10 = 160 (cache-read excluded,
    # it is cumulative billed reads, not current context) -> 3.90625%
    assert agents[0].ctx_pct == pytest.approx(100 * 160 / 4096, abs=0.01)


def test_ctx_pct_capped_at_100(tmp_path):
    from sloppy_toppy.models import Agent

    a = Agent(source="hermes:default", session_id="big", input_tokens=10_000_000,
              ctx_window=128_000)
    assert a.ctx_pct == 100.0


def test_ended_sessions_hidden_by_default(tmp_path):
    base = make_install(tmp_path)
    db = sqlite3.connect(f"{base}/state.db")
    db.execute("UPDATE sessions SET ended_at='1001.0' WHERE id='sess-a'")
    db.commit()
    db.close()
    assert Monitor(base=base).snapshot(now=2000.0) == []
    assert len(Monitor(base=base, show_ended=True).snapshot(now=2000.0)) == 1
