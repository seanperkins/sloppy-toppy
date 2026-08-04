"""sloppy-toppy CLI: live TUI when on a TTY, batch table otherwise.

Usage:
    sloppy-toppy                 # live TUI (needs a TTY)
    sloppy-toppy --once          # one snapshot, plain table (cron-friendly)
    sloppy-toppy --once --json   # one snapshot as JSON lines
"""

from __future__ import annotations

import argparse
import json
import sys

from .monitor import Monitor
from .models import Agent, State, fmt_cost_rate, fmt_rate, fmt_uptime, fmt_usd


def build_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(
        prog="sloppy-toppy",
        description="top for your AI agents — live token / context / cost monitor",
    )
    ap.add_argument("--once", action="store_true", help="print one snapshot and exit")
    ap.add_argument("--json", action="store_true", help="JSON lines output (implies --once)")
    ap.add_argument("--base", default="~/.hermes", help="Hermes home (default: ~/.hermes)")
    ap.add_argument("--stuck-idle", type=float, default=240.0,
                    help="idle seconds before a non-interactive agent is STUCK (default 240)")
    ap.add_argument("--active-idle", type=float, default=60.0,
                    help="idle seconds that still count as ACTIVE (default 60)")
    ap.add_argument("-a", "--show-ended", action="store_true", help="include finished sessions")
    ap.add_argument("--sort", choices=["state", "cost", "tokens", "ctx"], default="state",
                    help="sort column (default: state, then cost rate)")
    ap.add_argument("--refresh", type=float, default=2.0,
                    help="TUI refresh interval in seconds (default 2)")
    ap.add_argument("--version", action="version", version="sloppy-toppy 0.1.0")
    return ap


# ---------------------------------------------------------------------------
# batch rendering
# ---------------------------------------------------------------------------

def render_table(agents: list[Agent], sort: str = "state") -> str:
    mon = Monitor()
    agents = mon.sorted(agents, sort)
    lines = [
        f"{'AGENT':<16}{'MODEL':<30}{'TOK/s':>6}{'CTX%':>7}{'$/HR':>7} "
        f"{'STATE':<7}{'TOOL':<14}{'UPTIME':>7}  TITLE"
    ]
    for a in agents:
        model = a.model.split("/")[-1] if a.model else "—"
        ctx = f"{a.ctx_pct:.1f}%"
        lines.append(
            f"{a.agent_name:<16}{model:<30}{fmt_rate(a.tok_rate):>6}"
            f"{ctx:>7}{fmt_cost_rate(a.cost_rate):>7} "
            f"{a.state.value:<7}{(a.last_tool or '—')[:14]:<14}{fmt_uptime(a.uptime):>7}"
            f"  {(a.title or a.session_id)[:48]}"
        )
    return "\n".join(lines)


def _agent_json(a: Agent) -> dict:
    return {
        "agent": a.agent_name,
        "session_id": a.session_id,
        "title": a.title,
        "model": a.model,
        "state": a.state.value,
        "idle_seconds": round(a.idle_seconds, 1),
        "tokens_per_sec": round(a.tok_rate, 2),
        "cost_per_hour": round(a.cost_rate, 4),
        "ctx_pct": round(a.ctx_pct, 2),
        "ctx_window": a.ctx_window,
        "total_tokens": a.total_burn_tokens,
        "cost_usd": round(a.cost_usd, 6),
        "uptime_seconds": round(a.uptime, 1),
        "last_tool": a.last_tool,
        "last_action": a.last_action_desc,
        "owner_pid": a.owner_pid,
    }


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    use_json = args.json
    use_once = args.once or use_json

    monitor = Monitor(
        base=args.base,
        stuck_idle=args.stuck_idle,
        active_idle=args.active_idle,
        show_ended=args.show_ended,
    )

    if not use_once:
        if not sys.stdout.isatty():
            # piped output: behave like --once rather than crash on a missing TTY
            use_once = True
        else:
            from .tui import SloppyToppy

            SloppyToppy(
                base=args.base,
                stuck_idle=args.stuck_idle,
                active_idle=args.active_idle,
                show_ended=args.show_ended,
                refresh_seconds=args.refresh,
            ).run()
            return 0

    agents = monitor.snapshot()
    if use_json:
        for a in agents:
            print(json.dumps(_agent_json(a), ensure_ascii=False))
    else:
        print(render_table(agents, args.sort))
    return 0


if __name__ == "__main__":
    sys.exit(main())
