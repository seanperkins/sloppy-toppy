package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
)

// These are pure arithmetic on m.width / m.height, so they need no terminal.
// The package previously had no tests at all, which is why a row 9 columns
// too wide shipped.

func testAgents(n int) []*agent.Agent {
	out := make([]*agent.Agent, n)
	for i := range out {
		out[i] = &agent.Agent{
			Provider:  "claude",
			Instance:  "some-project",
			SessionID: fmt.Sprintf("session-%03d", i),
			Title:     fmt.Sprintf("title-%03d for a session that needs truncating", i),
			Model:     "claude-opus-5",
			State:     agent.StateActive,
			CtxUsed:   500, CtxWindow: 1000, CtxAccurate: true,
			CostSource: agent.CostEstimated,
			StartedAt:  time.Unix(1000, 0),
		}
	}
	return out
}

// TestRowNeverExceedsTerminalWidth is the regression that matters: the title
// budget was hardcoded to width-84 while the real fixed width is 93, so every
// row overflowed by 9 columns and wrapped, breaking the whole viewport.
func TestRowNeverExceedsTerminalWidth(t *testing.T) {
	for _, w := range []int{80, 100, 120, 200} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := &model{width: w, height: 40, now: time.Unix(5000, 0)}
			m.agents = testAgents(1)

			row := m.row(m.agents[0])
			if got := lipgloss.Width(row); got > w {
				t.Fatalf("row is %d columns wide, terminal is %d — it will wrap", got, w)
			}
		})
	}
}

// TestHeaderFitsAtEightyColumns covers the case where the header wrapped
// before a single agent row was drawn.
func TestHeaderFitsAtEightyColumns(t *testing.T) {
	m := &model{width: 80, height: 24}
	if got := lipgloss.Width(m.columnHeader()); got > 80 {
		t.Fatalf("column header is %d columns wide at an 80-column terminal", got)
	}
}

// TestFixedWidthMatchesRenderedRow pins the constants to reality: the layout
// is expressed both in rowFormat and in the split left/right render, and those
// two encodings drifted apart once already.
func TestFixedWidthMatchesRenderedRow(t *testing.T) {
	m := &model{width: 500, height: 40, now: time.Unix(5000, 0)}
	a := testAgents(1)[0]
	a.Title = "" // fall back to session id, whose length we know
	a.SessionID = "T"

	row := m.row(a)
	// Everything before the title plus the one-character title.
	l := m.layoutFor()
	if got, want := lipgloss.Width(row), l.fixed+1; got != want {
		t.Fatalf("rendered row width = %d, want fixed+1 = %d — the column "+
			"constants no longer describe the row", got, want)
	}
}

// TestCursorStaysInsideDrawnRegion pins the missing viewport: rendering always
// started at index 0 while the cursor could reach the end of the slice, so `G`
// selected a row that was never drawn.
func TestCursorStaysInsideDrawnRegion(t *testing.T) {
	m := &model{width: 120, height: 24, now: time.Unix(5000, 0)}
	m.agents = testAgents(50)

	rows := m.visibleRows()
	if rows >= len(m.agents) {
		t.Fatalf("test needs more agents than rows: %d rows, %d agents", rows, len(m.agents))
	}

	// Jump to the end, as `G` does.
	m.cursor = len(m.agents) - 1
	m.clampScroll()

	if m.cursor < m.offset || m.cursor >= m.offset+rows {
		t.Fatalf("cursor %d outside drawn window [%d,%d)", m.cursor, m.offset, m.offset+rows)
	}

	// And back to the top.
	m.cursor = 0
	m.clampScroll()
	if m.offset != 0 {
		t.Fatalf("offset = %d after returning to the first row, want 0", m.offset)
	}
}

// TestSelectedRowIsActuallyRendered walks the cursor down the whole list and
// checks the highlighted agent appears in the drawn output every time.
func TestSelectedRowIsActuallyRendered(t *testing.T) {
	m := &model{width: 140, height: 20, now: time.Unix(5000, 0)}
	m.agents = testAgents(40)

	for i := range m.agents {
		m.cursor = i
		m.clampScroll()
		out := m.View()
		marker := fmt.Sprintf("title-%03d", i)
		if !strings.Contains(out, marker) {
			t.Fatalf("cursor at %d: %q is not in the drawn output (offset %d, rows %d)",
				i, marker, m.offset, m.visibleRows())
		}
	}
}

// TestVisibleRowsLeavesRoomForChrome guards the budget that decides how many
// rows are drawn; if it over-counts, the footer scrolls off screen.
func TestVisibleRowsLeavesRoomForChrome(t *testing.T) {
	m := &model{width: 120, height: 24}
	m.agents = testAgents(100)

	rows := m.visibleRows()
	if rows <= 0 {
		t.Fatalf("visibleRows = %d", rows)
	}
	if rows >= m.height {
		t.Fatalf("visibleRows = %d leaves no room for chrome in %d lines", rows, m.height)
	}

	// With the detail pane open the budget must shrink.
	m.showDetail = true
	if withDetail := m.visibleRows(); withDetail >= rows {
		t.Fatalf("detail pane open did not reduce the row budget: %d -> %d", rows, withDetail)
	}
}

// TestRowSanitizesTitle checks the render path cannot emit a control sequence
// even though the agent carries one.
func TestRowSanitizesTitle(t *testing.T) {
	m := &model{width: 120, height: 24, now: time.Unix(5000, 0)}
	a := testAgents(1)[0]
	a.Title = "\x1b[2K\rSPOOFED"
	m.agents = []*agent.Agent{a}

	row := m.row(a)
	if strings.ContainsRune(row, 0x1b) {
		// lipgloss adds its own styling escapes, so check for the payload.
		if strings.Contains(row, "[2K") {
			t.Fatalf("row leaked a control sequence: %q", row)
		}
	}
	if !strings.Contains(row, "SPOOFED") {
		t.Fatalf("sanitizing removed the visible text too: %q", row)
	}
}

// TestTitleFallsBackWhenSanitizedEmpty covers a title made entirely of control
// characters: blanking the column is less useful than the session id.
func TestTitleFallsBackWhenSanitizedEmpty(t *testing.T) {
	m := &model{width: 120, height: 24, now: time.Unix(5000, 0)}
	a := testAgents(1)[0]
	a.Title = "\x1b[2K"
	a.SessionID = "fallback-id"

	if row := m.row(a); !strings.Contains(row, "fallback-id") {
		t.Fatalf("row did not fall back to the session id: %q", row)
	}
}

func TestUnknownCostRendersAsQuestionMark(t *testing.T) {
	a := testAgents(1)[0]
	a.CostSource = agent.CostUnknown
	if got := costCell(a); got != "?" {
		t.Fatalf("unknown cost rendered as %q, want ?", got)
	}
}

func TestApproximateContextIsMarked(t *testing.T) {
	a := testAgents(1)[0]
	a.CtxAccurate = false
	if got := ctxCell(a); !strings.HasPrefix(got, "~") {
		t.Fatalf("approximate context = %q, want a leading ~", got)
	}
}
