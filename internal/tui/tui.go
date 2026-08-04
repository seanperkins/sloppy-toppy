// Package tui renders the live top-style view with bubbletea.
//
// Polling happens in a command goroutine rather than in Update, so provider
// I/O never blocks the render loop. That is a structural property of the
// bubbletea model here, not a discipline the code has to remember.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/monitor"
	"github.com/seanperkins/sloppy-toppy/internal/provider"
)

// Options configures the TUI.
type Options struct {
	Monitor *monitor.Monitor
	Sort    agent.SortMode
	Refresh time.Duration
	Version string
}

// Run starts the interactive UI and blocks until the user quits.
func Run(o Options) error {
	if o.Refresh <= 0 {
		o.Refresh = monitor.DefaultRefresh
	}
	m := &model{
		mon:     o.Monitor,
		sort:    o.Sort,
		refresh: o.Refresh,
		version: o.Version,
		// Seed from the monitor, or `sloppy-toppy -a` starts showing ended
		// sessions while the model believes it is not, so the first press of
		// `a` turns them on again instead of off.
		showEnded: o.Monitor.Config().ShowEnded,
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ---------------------------------------------------------------------------
// styles
// ---------------------------------------------------------------------------

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).
			Background(lipgloss.Color("236"))
	summaryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).
			Background(lipgloss.Color("238")).Padding(0, 1)
	colHeadStyle = lipgloss.NewStyle().Faint(true)
	selectedRow  = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	detailStyle  = lipgloss.NewStyle().BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	footerStyle = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

var stateStyles = map[agent.State]lipgloss.Style{
	agent.StateActive:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")),
	agent.StateWaiting: lipgloss.NewStyle().Foreground(lipgloss.Color("220")),
	agent.StateStuck:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
	agent.StateIdle:    lipgloss.NewStyle().Faint(true),
	agent.StateDone:    lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
}

// ---------------------------------------------------------------------------
// model
// ---------------------------------------------------------------------------

type snapshotMsg struct {
	agents []*agent.Agent
	spends []*provider.Snapshot
	errs   []error
	at     time.Time
}

type tickMsg time.Time

type model struct {
	mon     *monitor.Monitor
	sort    agent.SortMode
	refresh time.Duration
	version string

	agents []*agent.Agent
	spends []*provider.Snapshot
	errs   []error
	now    time.Time

	cursor int
	// offset is the index of the first drawn row. Without it the view always
	// started at 0 while the cursor could reach the end of the slice, so on
	// any list longer than the viewport the highlight simply vanished and the
	// detail pane described a row that was never on screen.
	offset     int
	showDetail bool
	showEnded  bool
	width      int
	height     int
	loading    bool
}

func (m *model) Init() tea.Cmd {
	// Mark the first poll in flight before it starts. Otherwise a poll slower
	// than the refresh interval lets the first tick launch a second one, and
	// two concurrent sweeps can land out of order — the older snapshot
	// overwriting the newer, and a spend source being billed twice.
	m.loading = true
	return tea.Batch(m.poll(), tick(m.refresh))
}

// clampScroll keeps the cursor inside the drawn region.
func (m *model) clampScroll() {
	rows := m.visibleRows()
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.agents) {
		m.cursor = maxInt(0, len(m.agents)-1)
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	if max := len(m.agents) - rows; m.offset > max {
		m.offset = maxInt(0, max)
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// poll runs a provider sweep off the render loop. bubbletea executes the
// returned function on its own goroutine, so sqlite reads and JSONL parsing
// never stall keystroke handling.
func (m *model) poll() tea.Cmd {
	mon := m.mon
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		now := time.Now()
		agents, errs := mon.Snapshot(ctx, now)
		spends := mon.SpendSnapshots(ctx, now)
		return snapshotMsg{agents: agents, spends: spends, errs: errs, at: now}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// A resize changes how many rows fit, so the window may need moving.
		m.clampScroll()
		return m, nil

	case tickMsg:
		if m.loading {
			// A poll is still in flight; skip this beat rather than piling
			// up concurrent sweeps on a slow provider.
			return m, tick(m.refresh)
		}
		m.loading = true
		return m, tea.Batch(m.poll(), tick(m.refresh))

	case snapshotMsg:
		m.loading = false
		m.now = msg.at
		m.spends = msg.spends
		m.errs = msg.errs
		agent.Sort(msg.agents, m.sort)
		m.preserveCursor(msg.agents)
		m.agents = msg.agents
		m.clampScroll()
		return m, nil

	case tea.KeyMsg:
		model, cmd := m.handleKey(msg)
		m.clampScroll()
		return model, cmd
	}
	return m, nil
}

// preserveCursor keeps the highlight on the same session across refreshes,
// so a row that moves in the sort order does not steal the selection.
func (m *model) preserveCursor(next []*agent.Agent) {
	if m.cursor < 0 || m.cursor >= len(m.agents) {
		m.cursor = 0
		return
	}
	key := m.agents[m.cursor].Key()
	for i, a := range next {
		if a.Key() == key {
			m.cursor = i
			return
		}
	}
	if m.cursor >= len(next) {
		m.cursor = maxInt(0, len(next)-1)
	}
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(m.agents)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = maxInt(0, len(m.agents)-1)
	case "s":
		m.cycleSort()
		agent.Sort(m.agents, m.sort)
	case "a":
		m.showEnded = !m.showEnded
		m.mon.SetShowEnded(m.showEnded)
		m.loading = true
		return m, m.poll()
	case "d", "enter":
		m.showDetail = !m.showDetail
	case "r":
		if !m.loading {
			m.loading = true
			return m, m.poll()
		}
	}
	return m, nil
}

func (m *model) cycleSort() {
	for i, s := range agent.SortModes {
		if s == m.sort {
			m.sort = agent.SortModes[(i+1)%len(agent.SortModes)]
			return
		}
	}
	m.sort = agent.SortState
}

// ---------------------------------------------------------------------------
// view
// ---------------------------------------------------------------------------

func (m *model) View() string {
	var b strings.Builder

	b.WriteString(headerStyle.Render(m.pad(" sloppy-toppy " + m.version)))
	b.WriteString("\n")
	b.WriteString(summaryStyle.Render(m.pad(m.summary())))
	b.WriteString("\n")

	if spend := m.spendLine(); spend != "" {
		b.WriteString(footerStyle.Render(spend))
		b.WriteString("\n")
	}

	b.WriteString(colHeadStyle.Render(m.pad(m.columnHeader())))
	b.WriteString("\n")

	rows := m.visibleRows()
	if len(m.agents) == 0 {
		b.WriteString(footerStyle.Render("  no live agent sessions found"))
		b.WriteString("\n")
	}
	// Draw the window starting at offset, not always at 0.
	for i := m.offset; i < m.offset+rows && i < len(m.agents); i++ {
		line := m.row(m.agents[i])
		if i == m.cursor {
			line = selectedRow.Render(m.pad(line))
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if hidden := len(m.agents) - (m.offset + rows); hidden > 0 {
		b.WriteString(footerStyle.Render(fmt.Sprintf("  … %d more below", hidden)))
		b.WriteString("\n")
	}

	if m.showDetail {
		if a := m.selected(); a != nil {
			b.WriteString(detailStyle.Render(m.detail(a)))
			b.WriteString("\n")
		}
	}

	for _, err := range m.errs {
		b.WriteString(warnStyle.Render("  ! " + err.Error()))
		b.WriteString("\n")
	}

	b.WriteString(footerStyle.Render(
		"q quit · s sort · a ended · d detail · r refresh · j/k move"))
	return b.String()
}

// visibleRows is how many agent rows fit given the chrome currently drawn.
func (m *model) visibleRows() int {
	if m.height <= 0 {
		return len(m.agents)
	}
	chrome := 6 // header, summary, column header, footer, scroll hint, slack
	if m.showDetail {
		chrome += 8
	}
	if len(m.spends) > 0 {
		chrome++ // the spend strip takes a line when a source is configured
	}
	if len(m.errs) > 0 {
		chrome += len(m.errs)
	}
	return maxInt(1, m.height-chrome)
}

func (m *model) summary() string {
	var live int
	var stuck int
	var tokPerMin, costPerHr, ctxSum float64
	var ctxCount int
	for _, a := range m.agents {
		if a.State == agent.StateDone {
			continue
		}
		live++
		tokPerMin += a.TokRate * 60
		if a.CostSource.Known() {
			costPerHr += a.CostRate
		}
		if a.CtxWindow > 0 {
			ctxSum += a.CtxPct()
			ctxCount++
		}
		if a.State == agent.StateStuck {
			stuck++
		}
	}

	parts := []string{
		fmt.Sprintf("%d agent%s", live, plural(live)),
		agent.FmtTokens(int64(tokPerMin)) + "/min",
		"$" + agent.FmtCostRate(costPerHr) + "/hr",
	}
	if ctxCount > 0 {
		parts = append(parts, fmt.Sprintf("ctx %.0f%% avg", ctxSum/float64(ctxCount)))
	}
	if stuck > 0 {
		parts = append(parts, fmt.Sprintf("%d STUCK", stuck))
	}
	parts = append(parts, "["+strings.ToUpper(string(m.sort))+"]")
	if m.showEnded {
		parts = append(parts, "+ended")
	}
	return strings.Join(parts, " · ")
}

func (m *model) spendLine() string {
	var parts []string
	for _, s := range m.spends {
		if s == nil {
			continue
		}
		if s.Err != nil {
			parts = append(parts, s.Source+": unavailable")
			continue
		}
		p := s.Source + ": "
		if s.HasCredits {
			p += "$" + agent.FmtUSD(s.CreditsRemaining) + " left · "
		}
		p += "$" + agent.FmtCostRate(s.CostRate) + "/hr"
		parts = append(parts, p)
	}
	return strings.Join(parts, "   ")
}

// Column widths. The row is assembled in two pieces so the STATE cell can be
// colourised without lipgloss escape codes being counted as width, which means
// the layout is encoded in more than one place — these constants are that one
// place, and everything else derives from them.
//
// Previously the title budget was a hardcoded 84 while the real fixed width was
// 93, so every row ran 9 columns over the terminal and wrapped. On an 80-column
// terminal even the header wrapped before a single agent was drawn.
const (
	colAgent  = 16
	colModel  = 24
	colTok    = 7
	colCtx    = 8
	colCost   = 8
	colState  = 7
	colTool   = 12
	colUptime = 8

	// Two literal spaces separate cost from state and uptime from title.
	colSeparators = 1 + 2

	// fixedWidth is every column before TITLE, including separators.
	fixedWidth = colAgent + colModel + colTok + colCtx + colCost +
		colState + colTool + colUptime + colSeparators

	// minTitleWidth keeps the title column usable on a very narrow terminal
	// even though the row will then overflow — there is nothing else to give.
	minTitleWidth = 10
)

// Compact layout, used when the full one cannot fit. An 80-column terminal is
// entirely normal, and the full layout needs 93 columns before the title even
// starts — so rather than overflow and wrap (which breaks the whole viewport),
// narrow terminals drop TOOL and UPTIME and tighten AGENT and MODEL.
const (
	compactAgent = 14
	compactModel = 14

	compactFixed = compactAgent + compactModel + colTok + colCtx + colCost +
		colState + colSeparators
)

// layout is the set of column widths in use at the current terminal width.
type layout struct {
	agent, model int
	showTool     bool
	showUptime   bool
	fixed        int
	title        int
}

// layoutFor picks the widest layout that fits, so a row never wraps.
func (m *model) layoutFor() layout {
	// An unknown terminal size (before the first WindowSizeMsg) gets the full
	// layout with a sane fixed title budget.
	if m.width <= 0 {
		return layout{agent: colAgent, model: colModel, showTool: true,
			showUptime: true, fixed: fixedWidth, title: 48}
	}
	if m.width >= fixedWidth+minTitleWidth {
		return layout{agent: colAgent, model: colModel, showTool: true,
			showUptime: true, fixed: fixedWidth, title: m.width - fixedWidth}
	}
	return layout{
		agent: compactAgent, model: compactModel,
		fixed: compactFixed,
		title: maxInt(minTitleWidth, m.width-compactFixed),
	}
}

func (m *model) columnHeader() string {
	l := m.layoutFor()
	head := fmt.Sprintf("%-*s%-*s%*s%*s%*s %-*s",
		l.agent, "AGENT", l.model, "MODEL", colTok, "TOK/s",
		colCtx, "CTX%", colCost, "$/HR", colState, "STATE")
	if l.showTool {
		head += fmt.Sprintf("%-*s", colTool, "TOOL")
	}
	if l.showUptime {
		head += fmt.Sprintf("%*s", colUptime, "UPTIME")
	}
	return head + "  " + "TITLE"
}

func (m *model) row(a *agent.Agent) string {
	tool := a.LastTool
	if tool == "" {
		tool = "—"
	}
	// Fall back only after sanitizing: a title made entirely of control
	// characters is empty once stripped, and a blank row is less useful than
	// the session id.
	title := agent.Sanitize(a.Title)
	if title == "" {
		title = a.SessionID
	}
	l := m.layoutFor()
	state := stateStyles[a.State].Render(fmt.Sprintf("%-*s", colState, string(a.State)))

	// State is styled, so it is formatted separately from the rest of the
	// row: lipgloss escape codes would otherwise be counted as width.
	left := fmt.Sprintf("%-*s%-*s%*s%*s%*s ",
		l.agent, agent.Truncate(a.Label(), l.agent),
		l.model, agent.Truncate(a.ShortModel(), l.model),
		colTok, agent.FmtRate(a.TokRate),
		colCtx, ctxCell(a),
		colCost, costCell(a),
	)

	var right strings.Builder
	if l.showTool {
		fmt.Fprintf(&right, "%-*s", colTool, agent.Truncate(tool, colTool))
	}
	if l.showUptime {
		fmt.Fprintf(&right, "%*s", colUptime, agent.FmtUptime(a.Uptime(m.now)))
	}
	fmt.Fprintf(&right, "  %s", agent.Truncate(title, l.title))

	return left + state + right.String()
}

// ctxCell marks an approximated context figure with a leading ~, so a
// provider that can only give a lower bound never looks like a measurement.
func ctxCell(a *agent.Agent) string {
	if a.CtxWindow <= 0 {
		return "—"
	}
	s := fmt.Sprintf("%.1f%%", a.CtxPct())
	if !a.CtxAccurate {
		s = "~" + s
	}
	return s
}

// costCell distinguishes an unknown cost from a genuinely zero one.
func costCell(a *agent.Agent) string {
	if !a.CostSource.Known() {
		return "?"
	}
	return agent.FmtCostRate(a.CostRate)
}

func (m *model) selected() *agent.Agent {
	if m.cursor < 0 || m.cursor >= len(m.agents) {
		return nil
	}
	return m.agents[m.cursor]
}

func (m *model) detail(a *agent.Agent) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s · %s\n", a.Label(), a.SessionID)
	fmt.Fprintf(&b, "%s\n", a.Title)

	ctxNote := "measured"
	if !a.CtxAccurate {
		ctxNote = "lower bound"
	}
	fmt.Fprintf(&b, "model %s · window %s · ctx %.1f%% (%s)\n",
		a.Model, agent.FmtTokens(a.CtxWindow), a.CtxPct(), ctxNote)

	fmt.Fprintf(&b, "tokens in %s · out %s · reasoning %s · cache-read %s · cache-write %s\n",
		agent.FmtTokens(a.InputTokens), agent.FmtTokens(a.OutputTokens),
		agent.FmtTokens(a.ReasoningTokens), agent.FmtTokens(a.CacheReadTokens),
		agent.FmtTokens(a.CacheWriteTokens))

	switch a.CostSource {
	case agent.CostReported:
		fmt.Fprintf(&b, "cost $%s (reported)", agent.FmtUSD(a.CostUSD))
	case agent.CostEstimated:
		fmt.Fprintf(&b, "cost $%s (estimated from local pricing)", agent.FmtUSD(a.CostUSD))
	default:
		fmt.Fprintf(&b, "cost unknown (no pricing for %s)", a.Model)
	}
	fmt.Fprintf(&b, " · %d api calls · %d tool calls · %s tok/s\n",
		a.APICallCount, a.ToolCallCount, agent.FmtRate(a.TokRate))

	fmt.Fprintf(&b, "state %s", string(a.State))
	if a.Origin != "" {
		fmt.Fprintf(&b, " · origin %s", a.Origin)
	}
	if a.IdleSeconds > 0 {
		fmt.Fprintf(&b, " · idle %.0fs", a.IdleSeconds)
	}
	if a.LastActionDesc != "" {
		fmt.Fprintf(&b, " · %s", a.LastActionDesc)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "uptime %s", agent.FmtUptime(a.Uptime(m.now)))
	if a.OwnerPID > 0 {
		fmt.Fprintf(&b, " · pid %d", a.OwnerPID)
	}
	if a.CWD != "" {
		fmt.Fprintf(&b, " · %s", a.CWD)
	}
	if a.GitBranch != "" {
		fmt.Fprintf(&b, " · %s", a.GitBranch)
	}
	return b.String()
}

// pad extends s to the terminal width so background styles fill the line.
func (m *model) pad(s string) string {
	if m.width <= 0 || lipgloss.Width(s) >= m.width {
		return s
	}
	return s + strings.Repeat(" ", m.width-lipgloss.Width(s))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
