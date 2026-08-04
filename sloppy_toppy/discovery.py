"""Discover installed agent runtimes on this host.

v0.1 covers Hermes installations (multi-profile aware). The adapter list is
the extension point for claude/codex/opencode later.
"""

from __future__ import annotations

import glob
import json
import os
from dataclasses import dataclass


@dataclass
class HermesInstall:
    home: str  # hermes_home (profile dir, or ~/.hermes for default)
    state_db: str
    gateway_pid_file: str
    profile: str = "default"
    gateway_pid: int | None = None


def _read_pid_json(path: str) -> int | None:
    try:
        with open(path) as fh:
            data = json.load(fh)
        return int(data.get("pid"))
    except (OSError, ValueError, TypeError, json.JSONDecodeError):
        return None


def _pid_alive(pid: int | None) -> bool:
    return pid is not None and os.path.exists(f"/proc/{pid}")


def find_hermes_installs(base: str = "~/.hermes") -> list[HermesInstall]:
    """Return Hermes installs that have a state DB *and* a live gateway.

    Dead gateways are skipped: their sessions are not "currently running".
    """
    base = os.path.expanduser(base)
    installs: list[HermesInstall] = []

    # default install: <base>/state.db + <base>/gateway.pid
    default_db = os.path.join(base, "state.db")
    if os.path.exists(default_db):
        pid = _read_pid_json(os.path.join(base, "gateway.pid"))
        if _pid_alive(pid):
            installs.append(
                HermesInstall(
                    home=base,
                    state_db=default_db,
                    gateway_pid_file=os.path.join(base, "gateway.pid"),
                    profile="default",
                    gateway_pid=pid,
                )
            )

    # profile installs: <base>/profiles/*/state.db + gateway.pid
    for pid_file in sorted(glob.glob(os.path.join(base, "profiles", "*", "gateway.pid"))):
        home_dir = os.path.dirname(pid_file)
        db = os.path.join(home_dir, "state.db")
        pid = _read_pid_json(pid_file)
        if os.path.exists(db) and _pid_alive(pid):
            installs.append(
                HermesInstall(
                    home=home_dir,
                    state_db=db,
                    gateway_pid_file=pid_file,
                    profile=os.path.basename(home_dir),
                    gateway_pid=pid,
                )
            )

    return installs
