// Package codex reads agent sessions from Codex rollout transcripts.
//
// Format verified on a live install (2026-08-04). Codex writes one JSONL
// rollout per session under ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
//
// Codex is the most generous of the three providers: its `token_count`
// events carry cumulative totals, the last call's usage, the model's own
// context window, and current rate-limit consumption. Nothing has to be
// inferred except cost, which it does not record.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/pricing"
)

// DefaultBase is Codex's home directory.
const DefaultBase = "~/.codex"

// DefaultLookback bounds how recently a rollout must have been written to
// count as a current session, and keeps polling proportional to live work
// rather than to accumulated history.
const DefaultLookback = 12 * time.Hour

// DefaultAssumeEndedAfter is how long a rollout may sit untouched before the
// session is treated as finished. Like Claude Code, Codex writes no end
// marker, so without this a completed run stays visible as STUCK forever.
const DefaultAssumeEndedAfter = time.Hour

// Provider polls Codex rollouts.
type Provider struct {
	Base             string
	Lookback         time.Duration
	AssumeEndedAfter time.Duration
}

// New builds a Codex provider rooted at base.
func New(base string) *Provider {
	if base == "" {
		base = DefaultBase
	}
	return &Provider{
		Base:             base,
		Lookback:         DefaultLookback,
		AssumeEndedAfter: DefaultAssumeEndedAfter,
	}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "codex" }

type rolloutRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMeta is the session_meta payload.
//
// Only fields whose type is confirmed on a live install are typed here.
// Everything else is left out deliberately: Go's decoder fails the *whole*
// struct on a single type mismatch, so speculatively typing a field means
// one surprise shape silently discards cwd, originator, and the session id
// along with it. `context_window` is exactly that trap — it looks like a
// token count but is an object ({"window_id": ...}); the real window comes
// from the token_count event instead.
type sessionMeta struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"session_id"`
	CWD        string   `json:"cwd"`
	Originator string   `json:"originator"`
	Source     string   `json:"source"`
	CLIVersion string   `json:"cli_version"`
	Timestamp  string   `json:"timestamp"`
	Git        *gitMeta `json:"git"`
}

type gitMeta struct {
	Branch string `json:"branch"`
}

// tokenCount is the payload of an event_msg/token_count event.
type tokenCount struct {
	Type string `json:"type"`
	Info *struct {
		TotalTokenUsage *codexUsage `json:"total_token_usage"`
		LastTokenUsage  *codexUsage `json:"last_token_usage"`
		ModelCtxWindow  int64       `json:"model_context_window"`
	} `json:"info"`
	RateLimits *struct {
		Primary *struct {
			UsedPercent float64 `json:"used_percent"`
			ResetsAt    int64   `json:"resets_at"`
		} `json:"primary"`
	} `json:"rate_limits"`
}

type codexUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	CachedInput      int64 `json:"cached_input_tokens"`
	CacheWriteInput  int64 `json:"cache_write_input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningOutput  int64 `json:"reasoning_output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// turnContext carries the model in use for the turn.
type turnContext struct {
	Model  string `json:"model"`
	CWD    string `json:"cwd"`
	Effort string `json:"effort"`
}

// userMessage is used only to derive a human-readable title.
type eventPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// Poll implements provider.Provider.
func (p *Provider) Poll(ctx context.Context, now time.Time) ([]*agent.Agent, error) {
	root := filepath.Join(expandHome(p.Base), "sessions")
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = DefaultLookback
	}
	cutoff := now.Add(-lookback)

	var candidates []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable dirs, keep walking
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if !strings.HasPrefix(filepath.Base(path), "rollout-") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		candidates = append(candidates, path)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	var agents []*agent.Agent
	for _, path := range candidates {
		select {
		case <-ctx.Done():
			return agents, ctx.Err()
		default:
		}
		if a := p.parse(path, now); a != nil {
			agents = append(agents, a)
		}
	}
	return agents, nil
}

func (p *Provider) parse(path string, now time.Time) *agent.Agent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil
	}

	a := &agent.Agent{Provider: "codex"}

	var (
		lastTotals *codexUsage
		lastCall   *codexUsage
		sawAny     bool
	)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r rolloutRecord
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		sawAny = true

		if ts := parseTime(r.Timestamp); !ts.IsZero() {
			if a.StartedAt.IsZero() || ts.Before(a.StartedAt) {
				a.StartedAt = ts
			}
			if ts.After(a.LastActivityAt) {
				a.LastActivityAt = ts
			}
		}

		switch r.Type {
		case "session_meta":
			var m sessionMeta
			if json.Unmarshal(r.Payload, &m) != nil {
				continue
			}
			if m.SessionID != "" {
				a.SessionID = m.SessionID
			} else if m.ID != "" {
				a.SessionID = m.ID
			}
			a.CWD = m.CWD
			if m.Git != nil {
				a.GitBranch = m.Git.Branch
			}
			// `source` says where the session came from ("vscode", "cli"),
			// which is the interactivity signal; `originator` names the
			// driving program ("acpx") and is the better fallback when
			// source is absent.
			if m.Source != "" {
				a.Origin = mapOrigin(m.Source)
			} else if m.Originator != "" {
				a.Origin = mapOrigin(m.Originator)
			}
			if a.Instance == "" && m.CWD != "" {
				a.Instance = filepath.Base(m.CWD)
			}

		case "turn_context":
			var tc turnContext
			if json.Unmarshal(r.Payload, &tc) == nil && tc.Model != "" {
				a.Model = tc.Model
			}

		case "event_msg":
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(r.Payload, &probe) != nil {
				continue
			}
			switch probe.Type {
			case "token_count":
				var tc tokenCount
				if json.Unmarshal(r.Payload, &tc) != nil || tc.Info == nil {
					continue
				}
				if tc.Info.TotalTokenUsage != nil {
					lastTotals = tc.Info.TotalTokenUsage
				}
				if tc.Info.LastTokenUsage != nil {
					lastCall = tc.Info.LastTokenUsage
				}
				if tc.Info.ModelCtxWindow > 0 {
					// Codex self-reports the window, which beats any table
					// we could ship.
					a.CtxWindow = tc.Info.ModelCtxWindow
				}
			case "user_message":
				var ev eventPayload
				if json.Unmarshal(r.Payload, &ev) == nil && a.Title == "" && ev.Message != "" {
					a.Title = firstLine(ev.Message)
				}
			}
		}
	}
	if !sawAny {
		return nil
	}

	if a.SessionID == "" {
		a.SessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if a.Instance == "" && a.CWD != "" {
		a.Instance = filepath.Base(a.CWD)
	}
	if a.LastActivityAt.IsZero() {
		a.LastActivityAt = info.ModTime()
	}

	if lastTotals != nil {
		// Codex's cumulative `input_tokens` already includes the cached
		// portion, so subtract it out to keep the fields non-overlapping.
		a.InputTokens = lastTotals.InputTokens - lastTotals.CachedInput
		if a.InputTokens < 0 {
			a.InputTokens = 0
		}
		a.CacheReadTokens = lastTotals.CachedInput
		a.CacheWriteTokens = lastTotals.CacheWriteInput
		a.OutputTokens = lastTotals.OutputTokens
		a.ReasoningTokens = lastTotals.ReasoningOutput
	}

	// The last call's own usage is the live prefix size — a real measurement
	// of context fill rather than a running total.
	if lastCall != nil {
		a.CtxUsed = lastCall.InputTokens + lastCall.CacheWriteInput + lastCall.OutputTokens
		a.CtxAccurate = true
	}

	cost, known := pricing.Estimate(a.Model, pricing.Usage{
		Input:      a.InputTokens,
		Output:     a.OutputTokens,
		CacheRead:  a.CacheReadTokens,
		CacheWrite: a.CacheWriteTokens,
		Reasoning:  a.ReasoningTokens,
	}, now)
	if known {
		a.CostUSD, a.CostSource = cost, agent.CostEstimated
	} else {
		// Codex commonly runs non-Anthropic models we ship no rates for.
		// Reporting that plainly beats implying the session was free.
		a.CostSource = agent.CostUnknown
	}

	assumeEnded := p.AssumeEndedAfter
	if assumeEnded <= 0 {
		assumeEnded = DefaultAssumeEndedAfter
	}
	if now.Sub(a.LastActivityAt) > assumeEnded {
		a.EndedAt = a.LastActivityAt
		a.EndReason = "inferred: no rollout activity"
	}

	if a.Title == "" {
		a.Title = a.SessionID
	}
	return a
}

// mapOrigin translates Codex's session source onto the interactive versus
// autonomous distinction that STUCK detection depends on.
func mapOrigin(src string) string {
	switch strings.ToLower(src) {
	case "vscode", "cli", "tui", "terminal", "codex_cli_rs", "codex-cli":
		// A human is driving, so a long idle is someone stepping away.
		return "interactive"
	default:
		// Anything driving Codex programmatically (acpx, CI, a harness) has
		// nobody watching, so a long idle there is worth flagging.
		return src
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
