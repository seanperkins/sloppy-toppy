// Package cli renders snapshots for non-interactive use: a plain table for
// humans and JSON lines for scripts.
//
// Nothing here imports the TUI, so the --once and --json paths never
// initialise a terminal UI and stay fast enough for cron.
package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/provider"
)

// costCell renders a cost rate, distinguishing "no cost known" from "$0.00".
// A model we have no pricing for must not read as free.
func costCell(a *agent.Agent) string {
	if !a.CostSource.Known() {
		return "?"
	}
	return agent.FmtCostRate(a.CostRate)
}

// ctxCell renders context fill, marking a lower-bound estimate with a
// leading ~ so an approximation is never mistaken for a measurement.
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

// RenderTable formats agents as an aligned plain-text table.
func RenderTable(agents []*agent.Agent, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s%-26s%6s%8s%8s %-7s%-14s%8s  %s\n",
		"AGENT", "MODEL", "TOK/s", "CTX%", "$/HR", "STATE", "TOOL", "UPTIME", "TITLE")
	for _, a := range agents {
		title := a.Title
		if title == "" {
			title = a.SessionID
		}
		tool := a.LastTool
		if tool == "" {
			tool = "—"
		}
		fmt.Fprintf(&b, "%-18s%-26s%6s%8s%8s %-7s%-14s%8s  %s\n",
			agent.Truncate(a.Label(), 18),
			agent.Truncate(a.ShortModel(), 26),
			agent.FmtRate(a.TokRate),
			ctxCell(a),
			costCell(a),
			string(a.State),
			agent.Truncate(tool, 14),
			agent.FmtUptime(a.Uptime(now)),
			agent.Truncate(title, 48),
		)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderSpend formats remote spend snapshots as trailing summary lines.
func RenderSpend(snaps []*provider.Snapshot) string {
	var lines []string
	for _, s := range snaps {
		if s == nil {
			continue
		}
		if s.Err != nil {
			lines = append(lines, fmt.Sprintf("%s: unavailable (%v)", s.Source, s.Err))
			continue
		}
		parts := []string{s.Source + ":"}
		if s.HasCredits {
			parts = append(parts, "$"+agent.FmtUSD(s.CreditsRemaining)+" left")
		}
		// A rate needs two fetches; one --once run cannot have one, and
		// printing $0.00 there is indistinguishable from spending nothing.
		if s.RateKnown {
			parts = append(parts, "$"+agent.FmtCostRate(s.CostRate)+"/hr")
		} else {
			parts = append(parts, "$?/hr (needs a second poll)")
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

// AgentJSON is the stable JSON-lines shape emitted by --json.
type AgentJSON struct {
	Agent      string  `json:"agent"`
	Provider   string  `json:"provider"`
	Instance   string  `json:"instance"`
	SessionID  string  `json:"session_id"`
	Origin     string  `json:"origin"`
	Title      string  `json:"title"`
	Model      string  `json:"model"`
	State      string  `json:"state"`
	IdleSecs   float64 `json:"idle_seconds"`
	TokPerSec  float64 `json:"tokens_per_sec"`
	CostPerHr  float64 `json:"cost_per_hour"`
	CtxPct     float64 `json:"ctx_pct"`
	CtxWindow  int64   `json:"ctx_window"`
	CtxExact   bool    `json:"ctx_exact"`
	Tokens     int64   `json:"total_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	CostSource string  `json:"cost_source"`
	UptimeSecs float64 `json:"uptime_seconds"`
	EndReason  string  `json:"end_reason,omitempty"`
	Incomplete bool    `json:"incomplete,omitempty"`
	LastTool   string  `json:"last_tool"`
	LastAction string  `json:"last_action"`
	CWD        string  `json:"cwd,omitempty"`
	GitBranch  string  `json:"git_branch,omitempty"`
	OwnerPID   int     `json:"owner_pid,omitempty"`
}

// ToJSON converts an agent to its serialisable form.
//
// cost_source is emitted alongside cost so a consumer can tell a reported
// figure from an estimate from a total unknown — a zero cost_usd with
// cost_source "" means "we don't know", not "free".
func ToJSON(a *agent.Agent, now time.Time) AgentJSON {
	// Provider-derived strings are sanitized here too, not just in the table.
	// encoding/json escapes control bytes on the wire, so a JSON *parser* is
	// safe either way — but the decoded value would still carry a live escape
	// sequence, and `jq -r .title` prints it straight to a terminal.
	return AgentJSON{
		Agent:      a.Label(),
		Provider:   a.Provider,
		Instance:   a.Instance,
		SessionID:  a.SessionID,
		Origin:     a.Origin,
		Title:      agent.Sanitize(a.Title),
		Model:      a.Model,
		State:      string(a.State),
		IdleSecs:   round(a.IdleSeconds, 1),
		TokPerSec:  round(a.TokRate, 2),
		CostPerHr:  round(a.CostRate, 4),
		CtxPct:     round(a.CtxPct(), 2),
		CtxWindow:  a.CtxWindow,
		CtxExact:   a.CtxAccurate,
		Tokens:     a.BurnTokens(),
		CostUSD:    round(a.CostUSD, 6),
		CostSource: string(a.CostSource),
		UptimeSecs: round(a.Uptime(now).Seconds(), 1),
		EndReason:  a.EndReason,
		Incomplete: a.Incomplete,
		LastTool:   agent.Sanitize(a.LastTool),
		LastAction: agent.Sanitize(a.LastActionDesc),
		CWD:        agent.Sanitize(a.CWD),
		GitBranch:  agent.Sanitize(a.GitBranch),
		OwnerPID:   a.OwnerPID,
	}
}

func round(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}
