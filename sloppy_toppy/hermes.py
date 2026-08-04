"""Hermes adapter: read agent sessions from a Hermes state.db (read-only).

Schema facts verified on a live install (2026-08-04):
- `sessions` holds cumulative token counters, estimated/actual cost, model,
  started_at / ended_at / last_activity_at as epoch floats (sometimes stored
  as strings — parse defensively), and a human-readable
  `last_activity_description` that already encodes states like
  "waiting for user approval (80s elapsed)".
- `messages.token_count` is NULL in practice, so rates must be derived by
  poll-diffing the cumulative `sessions` counters.
- Model context windows are cached in `context_length_cache.yaml` as a flat
  map `model@base_url: N` (YAML subset we parse without a yaml dep).
"""

from __future__ import annotations

import os
import sqlite3
from typing import Any

from .discovery import HermesInstall
from .models import Agent


# ---------------------------------------------------------------------------
# context window lookup
# ---------------------------------------------------------------------------


def _parse_ctx_cache(path: str) -> dict[str, int]:
    """Parse the flat `context_length_cache.yaml` map without PyYAML.

    Keys may themselves contain colons (e.g. `model@https://host/v1: N`),
    so split on the LAST colon of each `key: value` line and require the
    value to be a bare integer.
    """
    cache: dict[str, int] = {}
    try:
        with open(path) as fh:
            for line in fh:
                if ":" not in line:
                    continue
                key, _, value = line.rpartition(":")
                key = key.strip()
                value = value.strip()
                if not key or not value.isdigit():
                    continue
                cache[key] = int(value)
    except OSError:
        pass
    return cache


def load_context_cache(hermes_home: str) -> dict[str, int]:
    return _parse_ctx_cache(os.path.join(hermes_home, "context_length_cache.yaml"))


def model_ctx_window(model: str, cache: dict[str, int], default: int = 128_000) -> int:
    """Look up a model's window, tolerating the `@base_url` suffix in cache keys."""
    if model in cache:
        return cache[model]
    for key, value in cache.items():
        if key.startswith(model + "@"):
            return value
    return default


# ---------------------------------------------------------------------------
# session reading
# ---------------------------------------------------------------------------

_SESSION_COLS = (
    "id, source, model, started_at, ended_at, end_reason, "
    "input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, "
    "reasoning_tokens, estimated_cost_usd, last_activity_at, "
    "last_activity_description, title, cwd, git_branch, "
    "api_call_count, tool_call_count"
)

_LAST_TOOL_SQL = """
    SELECT session_id, tool_name FROM messages
    WHERE tool_name IS NOT NULL AND tool_name != ''
      AND id IN (
          SELECT MAX(id) FROM messages
          WHERE tool_name IS NOT NULL AND tool_name != ''
          GROUP BY session_id
      )
"""


def _to_float(value: Any, default: float = 0.0) -> float:
    if value is None:
        return default
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def _to_int(value: Any, default: int = 0) -> int:
    if value is None:
        return default
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def read_sessions(install: HermesInstall, default_ctx: int = 128_000) -> list[Agent]:
    """Read all sessions that have a model (skips unstarted placeholders)."""
    db = sqlite3.connect(f"file:{install.state_db}?mode=ro", uri=True)
    try:
        db.row_factory = sqlite3.Row
        rows = db.execute(
            f"SELECT {_SESSION_COLS} FROM sessions "
            "WHERE model IS NOT NULL AND started_at IS NOT NULL"
        ).fetchall()
        try:
            last_tools = {r[0]: r[1] for r in db.execute(_LAST_TOOL_SQL)}
        except sqlite3.Error:
            last_tools = {}
    finally:
        db.close()

    cache = load_context_cache(install.home)
    source = f"hermes:{install.profile}"
    agents: list[Agent] = []
    for r in rows:
        model = r["model"] or ""
        agents.append(
            Agent(
                source=source,
                session_id=r["id"],
                title=r["title"] or "",
                model=model,
                started_at=_to_float(r["started_at"], default=None) or None,
                last_activity_at=_to_float(r["last_activity_at"], default=None) or None,
                input_tokens=_to_int(r["input_tokens"]),
                output_tokens=_to_int(r["output_tokens"]),
                cache_read_tokens=_to_int(r["cache_read_tokens"]),
                cache_write_tokens=_to_int(r["cache_write_tokens"]),
                reasoning_tokens=_to_int(r["reasoning_tokens"]),
                cost_usd=_to_float(r["estimated_cost_usd"]),
                ctx_window=model_ctx_window(model, cache, default_ctx),
                last_tool=last_tools.get(r["id"], ""),
                last_action_desc=r["last_activity_description"] or "",
                ended_at=_to_float(r["ended_at"], default=None) or None,
                end_reason=r["end_reason"] or "",
                owner_pid=install.gateway_pid,
                cwd=r["cwd"] or "",
                git_branch=r["git_branch"] or "",
                api_call_count=_to_int(r["api_call_count"]),
                tool_call_count=_to_int(r["tool_call_count"]),
            )
        )
    return agents
