"""Headless TUI test: the app must mount, load agents, and render."""

import asyncio

from textual.widgets import DataTable, Static

from sloppy_toppy.tui import SloppyToppy

from test_monitor import make_install


def test_tui_renders_snapshot(tmp_path):
    base = make_install(tmp_path)

    async def run():
        app = SloppyToppy(base=base, refresh_seconds=0.05)
        async with app.run_test() as pilot:
            await pilot.pause()
            table = app.query_one("#table", DataTable)
            assert table.row_count >= 1, "table must show at least the fixture agent"

            summary = str(app.query_one("#summary", Static).content)
            assert "1 agent" in summary, f"summary mismatch: {summary!r}"

            # detail pane renders for the selected row
            app.action_toggle_detail()
            await pilot.pause()
            detail = str(app.query_one("#detail", Static).content)
            assert "Fixture Agent" in detail
            assert "sess-a" in detail

            # second refresh keeps rendering (no crash on repeated polls)
            app.refresh_now()
            await pilot.pause()
            assert table.row_count >= 1

    asyncio.run(run())


def test_tui_empty_install_renders(tmp_path):
    base = tmp_path / "hermes"
    base.mkdir()

    async def run():
        app = SloppyToppy(base=str(base), refresh_seconds=0.05)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert app.query_one("#table", DataTable).row_count == 0
            summary = str(app.query_one("#summary", Static).content)
            assert "0 agents" in summary

    asyncio.run(run())
