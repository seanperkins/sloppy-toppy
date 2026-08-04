"""Textual TUI: live top-style view of AI agents.

Keys:
    q          quit
    s          cycle sort (state / cost / tokens / ctx)
    a          toggle showing finished sessions
    d / Enter  toggle detail pane for the highlighted row
    r          refresh immediately
    arrows / j/k  move selection
"""

from __future__ import annotations

from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal
from textual.widgets import DataTable, Footer, Header, Static

from .models import Agent, State, fmt_cost_rate, fmt_rate, fmt_tokens, fmt_uptime, fmt_usd
from .monitor import Monitor

SORT_LABELS = {"state": "STATE", "cost": "$/HR", "tokens": "TOK/s", "ctx": "CTX%"}

STATE_STYLES = {
    State.ACTIVE: "bold green",
    State.WAITING: "yellow",
    State.STUCK: "bold red",
    State.IDLE: "dim",
    State.DONE: "grey37",
}


class DetailPane(Static):
    pass


class SloppyToppy(App):
    CSS = """
    #summary {
        height: 1;
        padding: 0 1;
        background: $panel;
        color: $text;
    }
    #table {
        height: 1fr;
    }
    #detail {
        height: auto;
        max-height: 12;
        border-top: solid $primary;
        padding: 0 1;
        display: none;
    }
    #detail.visible {
        display: block;
    }
    DataTable {
        border: none;
    }
    DataTable > .datatable--header {
        color: $text-muted;
    }
    """

    BINDINGS = [
        Binding("q", "quit", "Quit"),
        Binding("s", "cycle_sort", "Sort"),
        Binding("a", "toggle_ended", "Ended"),
        Binding("d,enter", "toggle_detail", "Detail"),
        Binding("r", "refresh_now", "Refresh"),
        Binding("j", "cursor_down", "Down"),
        Binding("k", "cursor_up", "Up"),
    ]

    def __init__(
        self,
        base: str = "~/.hermes",
        stuck_idle: float = 240.0,
        active_idle: float = 60.0,
        show_ended: bool = False,
        refresh_seconds: float = 2.0,
    ):
        super().__init__()
        self._monitor = Monitor(
            base=base,
            stuck_idle=stuck_idle,
            active_idle=active_idle,
            show_ended=show_ended,
        )
        self._refresh_seconds = refresh_seconds
        self._sort = "state"
        self._agents: dict[str, Agent] = {}
        self._timer = None

    # -- app lifecycle ----------------------------------------------------

    def compose(self) -> ComposeResult:
        yield Header(show_clock=True)
        yield Static(id="summary", markup=False)
        yield DataTable(id="table")
        yield DetailPane(id="detail", markup=False)
        yield Footer()

    def on_mount(self) -> None:
        table = self.query_one("#table", DataTable)
        table.add_columns(
            "AGENT",
            "MODEL",
            "TOK/s",
            "CTX%",
            "$/HR",
            "STATE",
            "TOOL",
            "UPTIME",
            "TITLE",
        )
        table.cursor_type = "row"
        self._timer = self.set_interval(self._refresh_seconds, self.refresh_now)
        self.refresh_now()

    def on_unmount(self) -> None:
        if self._timer is not None:
            self._timer.stop()

    # -- data -------------------------------------------------------------

    def refresh_now(self) -> None:
        agents = self._monitor.sorted(self._monitor.snapshot(), self._sort)
        table = self.query_one("#table", DataTable)
        cursor_key = table.coordinate_to_cell_key(table.cursor_coordinate).row_key.value \
            if table.row_count and table.cursor_coordinate.row < table.row_count else None
        table.clear()
        self._agents = {}
        for agent in agents:
            key = f"{agent.source}:{agent.session_id}"
            self._agents[key] = agent
            model = agent.model.split("/")[-1] if agent.model else "—"
            ctx = f"{agent.ctx_pct:.1f}%"
            table.add_row(
                agent.agent_name,
                model,
                fmt_rate(agent.tok_rate),
                ctx,
                fmt_cost_rate(agent.cost_rate),
                agent.state.value,
                (agent.last_tool or "—")[:14],
                fmt_uptime(agent.uptime),
                (agent.title or agent.session_id)[:48],
                key=key,
            )
        if cursor_key and cursor_key in self._agents:
            try:
                table.move_cursor(row=table.get_row_index(cursor_key))
            except Exception:
                pass
        self._update_summary(agents)
        self._render_detail()

    def _update_summary(self, agents: list[Agent]) -> None:
        live = [a for a in agents if a.state is not State.DONE]
        stuck = [a for a in agents if a.state is State.STUCK]
        tok_min = sum(a.tok_rate for a in live) * 60.0
        cost_hr = sum(a.cost_rate for a in live)
        total_ctx = sum(a.ctx_pct for a in live)
        n = len(live)
        flag = f" · {len(stuck)} ⚠" if stuck else ""
        text = (
            f" {n} agent{'s' if n != 1 else ''}"
            f" · {fmt_tokens(int(tok_min))}/min"
            f" · ${fmt_cost_rate(cost_hr)}/hr"
        )
        if n:
            text += f" · ctx {total_ctx / n:.0f}% avg"
        text += (
            f"{flag}   [{self._sort.upper()} sort]"
            " — q quit · s sort · a ended · d detail · r refresh"
        )
        self.query_one("#summary", Static).update(text)

    # -- detail pane ------------------------------------------------------

    def _selected(self) -> Agent | None:
        table = self.query_one("#table", DataTable)
        if not table.row_count:
            return None
        key = table.coordinate_to_cell_key(table.cursor_coordinate).row_key.value
        return self._agents.get(key)

    def _render_detail(self) -> None:
        pane = self.query_one("#detail", DetailPane)
        agent = self._selected()
        if agent is None:
            pane.update("")
            return
        pane.update(self._detail_text(agent))

    @staticmethod
    def _detail_text(a: Agent) -> str:
        lines = [
            f"{a.agent_name} · {a.session_id}",
            f"{(a.title or '(untitled)')[:100]}",
            f"model: {a.model or '—'}  (window {fmt_tokens(a.ctx_window)} "
            f"· ctx {a.ctx_pct:.1f}%)",
            f"tokens: in {fmt_tokens(a.input_tokens)} · out {fmt_tokens(a.output_tokens)}"
            f" · reasoning {fmt_tokens(a.reasoning_tokens)}"
            f" · cache-read {fmt_tokens(a.cache_read_tokens)}",
            f"cost: ${fmt_usd(a.cost_usd)} est · {a.api_call_count} api calls"
            f" · {a.tool_call_count} tool calls"
            f" · burn {fmt_rate(a.tok_rate)} tok/s · ${fmt_cost_rate(a.cost_rate)}/hr",
            f"state: {a.state.value}"
            f"{f' (idle {a.idle_seconds:.0f}s)' if a.idle_seconds else ''}"
            f"{f' · last: {a.last_action_desc}' if a.last_action_desc else ''}"
            f"{f' · tool: {a.last_tool}' if a.last_tool else ''}",
            f"uptime {fmt_uptime(a.uptime)} · owner pid {a.owner_pid or '—'}"
            f"{f' · {a.cwd}' if a.cwd else ''}"
            f"{f' · branch {a.git_branch}' if a.git_branch else ''}"
            f"{f' · ended: {a.end_reason}' if a.end_reason else ''}",
        ]
        return "\n".join(lines)

    # -- actions ----------------------------------------------------------

    def action_cycle_sort(self) -> None:
        order = ["state", "cost", "tokens", "ctx"]
        self._sort = order[(order.index(self._sort) + 1) % len(order)]
        self.refresh_now()

    def action_toggle_ended(self) -> None:
        self._monitor.show_ended = not self._monitor.show_ended
        self.refresh_now()

    def action_toggle_detail(self) -> None:
        pane = self.query_one("#detail", DetailPane)
        if pane.has_class("visible"):
            pane.remove_class("visible")
        else:
            pane.add_class("visible")
            self._render_detail()

    def on_data_table_row_highlighted(self, event: DataTable.RowHighlighted) -> None:
        self._render_detail()

    def action_cursor_down(self) -> None:
        self.query_one("#table", DataTable).action_cursor_down()

    def action_cursor_up(self) -> None:
        self.query_one("#table", DataTable).action_cursor_up()

    def action_refresh_now(self) -> None:
        self.refresh_now()
