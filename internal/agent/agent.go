// Package agent defines the provider-neutral view of a running AI agent
// session, shared by every adapter, the monitor, and both UIs.
package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// State is the lifecycle bucket an agent is shown in.
type State string

const (
	StateActive  State = "ACTIVE"
	StateWaiting State = "WAITING"
	StateStuck   State = "STUCK"
	StateIdle    State = "IDLE"
	StateDone    State = "DONE"
)

// stateRank orders states in the default sort: most interesting first.
var stateRank = map[State]int{
	StateActive:  0,
	StateWaiting: 1,
	StateStuck:   2,
	StateIdle:    3,
	StateDone:    4,
}

// Rank returns the sort rank of a state. Unknown states sort last rather
// than panicking, so a future adapter cannot crash the table.
func (s State) Rank() int {
	if r, ok := stateRank[s]; ok {
		return r
	}
	return len(stateRank)
}

// InteractiveOrigins are session origins that legitimately sit idle for a
// long time: a human-facing chat thread stays open indefinitely, so a long
// idle there means an abandoned conversation, not a wedged worker.
//
// This is matched against Agent.Origin (the session's own source, e.g. the
// Hermes `sessions.source` column) and deliberately NOT against the
// provider or instance name. A profile may be *named* "slack" while running
// cron jobs, and a "default" profile may serve interactive chats.
var InteractiveOrigins = map[string]bool{
	"slack":       true,
	"telegram":    true,
	"discord":     true,
	"whatsapp":    true,
	"signal":      true,
	"teams":       true,
	"matrix":      true,
	"web":         true,
	"chat":        true,
	"tui":         true,
	"interactive": true,
}

// CostSource records how an agent's cost figure was obtained.
type CostSource string

const (
	// CostUnknown means no cost could be determined — the provider does not
	// report one and we have no pricing for the model. The UI must show this
	// as unknown rather than as zero dollars.
	CostUnknown CostSource = ""
	// CostReported means the provider supplied the figure directly.
	CostReported CostSource = "reported"
	// CostEstimated means we derived it from the local pricing table.
	CostEstimated CostSource = "estimated"
)

// Known reports whether a cost figure is meaningful at all.
func (c CostSource) Known() bool { return c != CostUnknown }

// Agent is one agent session as seen by sloppy-toppy.
//
// Token counters are cumulative totals for the session. Adapters are
// responsible for normalising to that shape: Hermes and Codex store
// cumulative counters directly, while Claude Code records per-API-call
// usage that the adapter must sum.
//
// CtxUsed, CtxWindow and CostUSD are supplied by the adapter rather than
// derived here, because providers differ in how well they can answer:
// Codex self-reports its context window, Claude Code exposes the true
// current context fill via the last call's cache-read prefix, and Hermes
// can only offer a lower bound. Baking one provider's approximation into
// this struct is what made the model Hermes-specific before.
type Agent struct {
	// Identity.
	Provider  string // adapter name: "hermes", "claude", "codex"
	Instance  string // profile / project / workspace within that provider
	SessionID string

	// Origin is the session's own source as recorded by the provider —
	// "cli", "cron", "telegram", "slack". Drives STUCK classification.
	// Empty means the provider does not distinguish origins.
	Origin string

	Title string
	Model string

	// Zero time means "unknown"; EndedAt zero means still running.
	StartedAt      time.Time
	LastActivityAt time.Time
	EndedAt        time.Time
	EndReason      string

	// Cumulative token counters.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// Adapter-supplied. CtxUsed is the best estimate of tokens currently
	// occupying the window; CtxAccurate marks whether that is a real
	// measurement or a lower bound, so the UI can flag it honestly.
	CtxUsed     int64
	CtxWindow   int64
	CtxAccurate bool

	// CostUSD is the session cost, meaningful only when CostSource is not
	// CostUnknown. Providers differ: Hermes reports a dollar figure, while
	// Claude Code and Codex record only token counts, so their cost must be
	// derived from a pricing table — and cannot be derived at all for a
	// model we have no rates for. Tracking the provenance keeps a derived
	// or missing cost from being presented as an authoritative zero.
	CostUSD    float64
	CostSource CostSource

	LastTool       string
	LastActionDesc string
	CWD            string
	GitBranch      string
	APICallCount   int64
	ToolCallCount  int64
	OwnerPID       int

	// Incomplete means part of this session's source could not be read, so
	// the token and cost figures are a floor rather than a total. A silently
	// truncated read is worse than no reading at all in a spend monitor: it
	// looks authoritative while under-reporting exactly the runaway usage the
	// tool exists to catch.
	Incomplete bool

	// Computed by the monitor.
	TokRate     float64 // tokens/sec, EMA of poll diffs
	CostRate    float64 // $/hour, EMA of poll diffs
	State       State
	IdleSeconds float64
}

// Key uniquely identifies a session across polls and providers.
func (a *Agent) Key() string {
	return a.Provider + ":" + a.Instance + ":" + a.SessionID
}

// Label is the short display name, e.g. "hermes:slack".
func (a *Agent) Label() string {
	if a.Instance == "" {
		return a.Provider
	}
	return a.Provider + ":" + a.Instance
}

// BurnTokens are the tokens that cost real money at full rate: input,
// output and reasoning. Cache reads are billed at a steep discount and are
// surfaced separately.
func (a *Agent) BurnTokens() int64 {
	return a.InputTokens + a.OutputTokens + a.ReasoningTokens
}

// CtxPct is how full the context window is, clamped to [0, 100].
func (a *Agent) CtxPct() float64 {
	if a.CtxWindow <= 0 {
		return 0
	}
	pct := 100 * float64(a.CtxUsed) / float64(a.CtxWindow)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// Uptime is wall-clock session age, frozen at EndedAt for finished sessions.
func (a *Agent) Uptime(now time.Time) time.Duration {
	if a.StartedAt.IsZero() {
		return 0
	}
	end := now
	if !a.EndedAt.IsZero() {
		end = a.EndedAt
	}
	if d := end.Sub(a.StartedAt); d > 0 {
		return d
	}
	return 0
}

// IsInteractive reports whether this session's origin is a human-facing
// chat thread, which is exempt from STUCK detection.
func (a *Agent) IsInteractive() bool {
	return InteractiveOrigins[strings.ToLower(a.Origin)]
}

// ShortModel trims a vendor prefix for display: "deepseek/v4-flash" -> "v4-flash".
func (a *Agent) ShortModel() string {
	if a.Model == "" {
		return "—"
	}
	if i := strings.LastIndex(a.Model, "/"); i >= 0 && i+1 < len(a.Model) {
		return a.Model[i+1:]
	}
	return a.Model
}

// ---------------------------------------------------------------------------
// sorting
// ---------------------------------------------------------------------------

// SortMode selects the secondary sort column; state is always primary.
type SortMode string

const (
	SortState  SortMode = "state"
	SortCost   SortMode = "cost"
	SortTokens SortMode = "tokens"
	SortCtx    SortMode = "ctx"
)

// SortModes is the cycle order used by the TUI's sort key.
var SortModes = []SortMode{SortState, SortCost, SortTokens, SortCtx}

// ValidSortMode reports whether s names a known sort mode.
func ValidSortMode(s string) bool {
	for _, m := range SortModes {
		if SortMode(s) == m {
			return true
		}
	}
	return false
}

// Sort orders agents in place: state rank first, then the chosen metric
// descending, with a stable tiebreak on key so rows do not jitter between
// refreshes when two agents tie.
func Sort(agents []*Agent, mode SortMode) {
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		if ra, rb := a.State.Rank(), b.State.Rank(); ra != rb {
			return ra < rb
		}
		switch mode {
		case SortTokens:
			if a.TokRate != b.TokRate {
				return a.TokRate > b.TokRate
			}
		case SortCtx:
			if a.CtxPct() != b.CtxPct() {
				return a.CtxPct() > b.CtxPct()
			}
		default: // SortState and SortCost both fall back to cost rate
			if a.CostRate != b.CostRate {
				return a.CostRate > b.CostRate
			}
		}
		if a.CostRate != b.CostRate {
			return a.CostRate > b.CostRate
		}
		return a.Key() < b.Key()
	})
}

// ---------------------------------------------------------------------------
// formatting helpers, shared by the batch renderer and the TUI
// ---------------------------------------------------------------------------

// FmtUptime renders a duration in top's compact style: 45s, 12m30, 3h05, 2d04h.
func FmtUptime(d time.Duration) string {
	s := int64(d.Seconds())
	if s <= 0 {
		return "—"
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02d", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02d", s/3600, (s%3600)/60)
	default:
		return fmt.Sprintf("%dd%02dh", s/86400, (s%86400)/3600)
	}
}

// FmtTokens renders a token count as 512, 1.4k or 2.3M.
func FmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FmtUSD renders a dollar amount, keeping sub-cent costs legible.
//
// Negatives are rendered, not clamped: an overdrawn OpenRouter balance is a
// real value, and showing "-12.00" as "0.00" would hide exactly the state a
// spend monitor exists to surface.
func FmtUSD(x float64) string {
	if x < 0 {
		return "-" + FmtUSD(-x)
	}
	switch {
	case x == 0:
		return "0.00"
	case x < 0.01:
		return fmt.Sprintf("%.5f", x)
	case x < 100:
		return fmt.Sprintf("%.2f", x)
	default:
		return fmt.Sprintf("%.0f", x)
	}
}

// FmtRate renders a tokens/sec rate, flooring noise to a flat zero.
func FmtRate(tokPerSec float64) string {
	if tokPerSec < 0.05 {
		return "0.0"
	}
	return fmt.Sprintf("%.1f", tokPerSec)
}

// FmtCostRate renders a $/hour rate.
func FmtCostRate(usdPerHour float64) string {
	return fmt.Sprintf("%.2f", usdPerHour)
}

// Sanitize strips terminal control sequences from a provider-supplied string.
//
// Every displayed field here originates in a file written by another program:
// a session title is a user prompt, an action description is a runtime's own
// text. Rendering those to a terminal verbatim lets whoever influenced them
// drive the terminal — CSI can erase and repaint a row (showing STUCK as
// ACTIVE, or a $40/hr burn as $0.02), and OSC 52 writes the system clipboard.
//
// This runs at the render boundary rather than in each adapter, so a new
// adapter cannot forget it. JSON output does not need it: encoding/json
// escapes C0 control bytes already.
func Sanitize(s string) string {
	if s == "" {
		return s
	}
	// Fast path: the overwhelming majority of strings are already clean.
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// Drop an entire escape sequence, not just the ESC byte — leaving the
		// parameter bytes behind would spray "[2K" into the table.
		if r == 0x1b {
			i = skipEscapeSequence(runes, i)
			continue
		}
		if isControl(r) {
			// Whitespace controls become a space so words do not run together;
			// everything else is dropped outright.
			if r == '\t' || r == '\n' || r == '\r' {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// isControl reports whether r is a C0 or C1 control character, or DEL.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// skipEscapeSequence returns the index of the last rune of the escape
// sequence starting at i, so the caller's loop increment lands past it.
func skipEscapeSequence(r []rune, i int) int {
	// i points at ESC. Nothing follows it: consume just the ESC.
	if i+1 >= len(r) {
		return i
	}
	switch r[i+1] {
	case '[': // CSI: parameters/intermediates, then a final byte in @..~
		j := i + 2
		for j < len(r) && r[j] >= 0x20 && r[j] <= 0x3f {
			j++
		}
		for j < len(r) && r[j] >= 0x20 && r[j] <= 0x2f {
			j++
		}
		if j < len(r) {
			j++ // the final byte
		}
		return j - 1
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC: run to ST or BEL
		j := i + 2
		for j < len(r) {
			if r[j] == 0x07 { // BEL terminator
				return j
			}
			if r[j] == 0x1b && j+1 < len(r) && r[j+1] == '\\' { // ESC \ (ST)
				return j + 1
			}
			j++
		}
		return len(r) - 1 // unterminated: swallow the rest
	default:
		// Two-byte escape (e.g. ESC c full reset).
		return i + 1
	}
}

// Truncate shortens s to at most n display characters, appending an ellipsis
// when it had to cut. Operates on runes so multi-byte titles are not split.
//
// It also sanitizes: truncation is the last thing every renderer does to a
// provider string, which makes it the one chokepoint neither UI can bypass.
// Sanitizing before measuring also keeps the width calculation honest — an
// escape sequence occupies runes but no columns.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = Sanitize(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
