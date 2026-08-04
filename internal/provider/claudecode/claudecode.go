// Package claudecode reads agent sessions from Claude Code's transcript
// files.
//
// Format verified on a live install (2026-08-04). Claude Code stores one
// JSONL transcript per session under ~/.claude/projects/<slug>/<uuid>.jsonl.
// Unlike Hermes and Codex it keeps no cumulative counters and no cost: every
// assistant record carries a per-API-call `usage` object, so this adapter
// sums them and prices the result locally.
//
// The upside of that format is context accuracy. A call's
// input + cache_read + cache_creation + output is the true size of the
// prefix that call sent, so the most recent record gives real context fill
// rather than the lower bound Hermes can offer.
package claudecode

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

// DefaultBase is Claude Code's home directory.
const DefaultBase = "~/.claude"

// DefaultLookback bounds how far back a transcript may have been touched and
// still count as a session worth showing.
//
// This is a hard performance requirement, not a display preference: a live
// install here held 2541 transcripts totalling 903 MB, of which 7 had been
// touched in the last hour. Parsing all of them costs ~2.2s per poll, which
// would be paid every refresh. Filtering on modification time first reduces
// a full scan to a handful of stat calls.
const DefaultLookback = 12 * time.Hour

// DefaultAssumeEndedAfter is how long a transcript may sit untouched before
// the session is treated as finished.
//
// Claude Code writes no end marker, so a completed run and a wedged one look
// identical on disk — the only difference is how long ago the last record
// was written. Without this inference every finished session accumulates as
// STUCK forever, and the genuinely wedged agent you want to catch is buried
// under hours of history. An hour is well past any single API call, so a
// transcript quiet that long has stopped rather than stalled.
const DefaultAssumeEndedAfter = time.Hour

// Provider polls Claude Code transcripts.
type Provider struct {
	Base     string
	Lookback time.Duration
	// AssumeEndedAfter marks quiet sessions as finished; see the constant.
	AssumeEndedAfter time.Duration
}

// New builds a Claude Code provider rooted at base.
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
func (p *Provider) Name() string { return "claude" }

// record is the subset of a transcript line this adapter reads. Claude Code
// writes many record types; everything not named here is ignored.
type record struct {
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	SessionID  string          `json:"sessionId"`
	CWD        string          `json:"cwd"`
	GitBranch  string          `json:"gitBranch"`
	Entrypoint string          `json:"entrypoint"`
	AITitle    string          `json:"aiTitle"`
	LastPrompt string          `json:"lastPrompt"`
	Origin     *originField    `json:"origin"`
	Message    *messageField   `json:"message"`
	ToolUseID  string          `json:"toolUseID"`
	ToolResult json.RawMessage `json:"toolUseResult"`
}

type originField struct {
	Kind string `json:"kind"`
}

type messageField struct {
	Model   string          `json:"model"`
	Usage   *usageField     `json:"usage"`
	Content json.RawMessage `json:"content"`
}

// usageField mirrors the Anthropic usage object. Cache creation is reported
// both as a flat total and as a per-TTL breakdown; the flat field is used.
type usageField struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// prefixTokens is the total context this call sent — the real measure of how
// full the window was at that moment.
func (u *usageField) prefixTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
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
	root := filepath.Join(expandHome(p.Base), "projects")
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = DefaultLookback
	}
	cutoff := now.Add(-lookback)

	var candidates []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable project directory should not abort the walk.
			return nil //nolint:nilerr // partial results beat no results
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
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

// parse reads one transcript into an Agent, returning nil if it contains no
// usable session data.
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

	a := &agent.Agent{
		Provider:  "claude",
		Instance:  projectLabel(filepath.Dir(path)),
		SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}

	var (
		total    usageField
		lastCall *usageField
		calls    int64
		tools    int64
		sawAny   bool
	)

	sc := bufio.NewScanner(f)
	// Transcript lines carry whole tool results and can be very large; the
	// default 64 KB scanner limit would silently truncate them mid-session.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			continue // a partially-written trailing line is normal on a live session
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
		if r.CWD != "" {
			a.CWD = r.CWD
		}
		if r.GitBranch != "" && r.GitBranch != "HEAD" {
			a.GitBranch = r.GitBranch
		}
		if r.AITitle != "" {
			a.Title = r.AITitle
		} else if a.Title == "" && r.LastPrompt != "" {
			a.Title = firstLine(r.LastPrompt)
		}
		if r.Entrypoint != "" {
			a.Origin = mapOrigin(r.Entrypoint, r.Origin)
		}
		if r.ToolUseID != "" || len(r.ToolResult) > 0 {
			tools++
		}

		if r.Message != nil {
			if r.Message.Model != "" {
				a.Model = r.Message.Model
			}
			if u := r.Message.Usage; u != nil {
				total.InputTokens += u.InputTokens
				total.OutputTokens += u.OutputTokens
				total.CacheReadTokens += u.CacheReadTokens
				total.CacheCreationTokens += u.CacheCreationTokens
				copied := *u
				lastCall = &copied
				calls++
			}
		}
	}
	if !sawAny {
		return nil
	}

	// Transcripts carry no explicit end marker, so file modification time is
	// the best available "last touched" signal when records lack timestamps.
	if a.LastActivityAt.IsZero() {
		a.LastActivityAt = info.ModTime()
	}

	a.InputTokens = total.InputTokens
	a.OutputTokens = total.OutputTokens
	a.CacheReadTokens = total.CacheReadTokens
	a.CacheWriteTokens = total.CacheCreationTokens
	a.APICallCount = calls
	a.ToolCallCount = tools

	// The last call's prefix is a real measurement of context fill, not an
	// approximation — flag it as such so the UI need not hedge.
	if lastCall != nil {
		a.CtxUsed = lastCall.prefixTokens()
		a.CtxAccurate = true
	}
	if w, ok := pricing.ContextWindow(a.Model); ok {
		a.CtxWindow = w
	}

	// No cost is recorded anywhere, so derive it — and say so.
	cost, known := pricing.Estimate(a.Model, pricing.Usage{
		Input:      total.InputTokens,
		Output:     total.OutputTokens,
		CacheRead:  total.CacheReadTokens,
		CacheWrite: total.CacheCreationTokens,
	}, now)
	if known {
		a.CostUSD, a.CostSource = cost, agent.CostEstimated
	} else {
		a.CostSource = agent.CostUnknown
	}

	// Infer an end rather than reporting a long-finished run as wedged.
	// EndReason records that this is inferred, not observed, so the
	// distinction survives into --json.
	assumeEnded := p.AssumeEndedAfter
	if assumeEnded <= 0 {
		assumeEnded = DefaultAssumeEndedAfter
	}
	if now.Sub(a.LastActivityAt) > assumeEnded {
		a.EndedAt = a.LastActivityAt
		a.EndReason = "inferred: no transcript activity"
	}

	if a.Title == "" {
		a.Title = a.SessionID
	}
	return a
}

// mapOrigin translates Claude Code's entrypoint and origin fields onto the
// interactive/autonomous distinction that STUCK detection depends on.
//
// "cli" means a human is driving the session from a terminal, so a long idle
// is someone stepping away rather than a wedged agent. "sdk-cli" is a
// programmatic run with nobody watching, which is exactly the case worth
// flagging.
func mapOrigin(entrypoint string, origin *originField) string {
	if origin != nil && origin.Kind == "human" {
		return "interactive"
	}
	switch entrypoint {
	case "cli":
		return "interactive"
	case "sdk-cli", "sdk":
		return "sdk"
	default:
		return entrypoint
	}
}

// projectLabel turns Claude Code's path-slug directory name into something
// readable: "-Users-sean-sites-sloppy-toppy" becomes "sloppy-toppy".
func projectLabel(dir string) string {
	base := filepath.Base(dir)
	if i := strings.LastIndex(base, "-"); i >= 0 && i+1 < len(base) {
		// The slug flattens path separators to dashes, so the trailing
		// segment is usually the project directory name.
		trimmed := strings.TrimLeft(base, "-")
		parts := strings.Split(trimmed, "-")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return base
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
