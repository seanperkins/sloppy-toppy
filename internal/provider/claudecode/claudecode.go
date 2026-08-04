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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/jsonl"
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

// DefaultAssumeEndedAfter is how long an *interactive* transcript may sit
// untouched before the session is treated as finished.
//
// Claude Code writes no end marker — there is no terminal stop record, and the
// last line of a finished transcript is ordinary metadata — so a completed run
// and a wedged one are indistinguishable from file content alone. The
// inference is therefore applied only where it is safe: a human-driven session
// goes quiet because someone walked away, and hiding it keeps the table
// readable.
//
// Unattended sessions are deliberately excluded. Marking one DONE on nothing
// but silence would retire the exact alert this tool exists to raise; they stay
// live and classify STUCK instead. Bound how far back they are considered with
// --lookback rather than by guessing that quiet means finished.
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

// record is the subset of a transcript line this adapter reads. Claude Code
// writes many record types; everything not named here is ignored.
type record struct {
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	SessionID  string          `json:"sessionId"`
	RequestID  string          `json:"requestId"`
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
	// ID together with the record's requestId identifies one API call.
	// Claude Code emits one line per content block and repeats the same
	// usage object on each, so this pair is what makes usage summable.
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   *usageField     `json:"usage"`
	Content json.RawMessage `json:"content"`
}

// syntheticModel marks records Claude Code generates itself rather than
// receiving from the API. They carry zero usage but would otherwise win the
// "last model seen" race and leave the session unpriced.
const syntheticModel = "<synthetic>"

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

// Poll implements provider.Provider.
func (p *Provider) Poll(ctx context.Context, now time.Time) ([]*agent.Agent, error) {
	root := filepath.Join(jsonl.ExpandHome(p.Base), "projects")
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
		SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}

	var (
		perModel  = map[string]*usageField{} // usage summed per model, for pricing
		seenCalls = map[string]bool{}        // dedupe key -> already counted
		seenTools = map[string]bool{}
		lastCall  *usageField
		calls     int64
		tools     int64
		sawAny    bool
	)

	rd := bufio.NewReaderSize(f, jsonl.ReaderBufBytes)
	for {
		line, truncated, err := jsonl.ReadLine(rd, jsonl.MaxLineBytes)
		if truncated {
			// An oversized line is skipped, not fatal. Ending the scan here
			// (which is what an unchecked bufio.Scanner does) would freeze the
			// session's reported spend at its pre-truncation value forever,
			// with no error anywhere — a monitoring bypass.
			a.Incomplete = true
		}
		if len(line) > 0 {
			if r, ok := decodeRecord(line); ok {
				sawAny = true
				applyRecord(a, r, perModel, seenCalls, seenTools, &lastCall, &calls, &tools)
			}
			// A record that fails to decode is normal on a live session: the
			// agent may be mid-write on the final line.
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A genuine read error means we saw only part of the file.
				a.Incomplete = true
			}
			break
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

	// Sum the per-model buckets into the session totals.
	for _, u := range perModel {
		a.InputTokens += u.InputTokens
		a.OutputTokens += u.OutputTokens
		a.CacheReadTokens += u.CacheReadTokens
		a.CacheWriteTokens += u.CacheCreationTokens
	}
	a.APICallCount = calls
	a.ToolCallCount = tools

	// The project slug flattens path separators to dashes and is therefore
	// not reversible; cwd is recorded in the transcript itself and is exact.
	if a.CWD != "" {
		a.Instance = filepath.Base(a.CWD)
	} else {
		a.Instance = projectLabel(filepath.Dir(path))
	}

	// The last call's prefix is a real measurement of context fill, not an
	// approximation — flag it as such so the UI need not hedge.
	if lastCall != nil {
		a.CtxUsed = lastCall.prefixTokens()
		a.CtxAccurate = true
	}
	if w, ok := pricing.ContextWindow(a.Model); ok {
		a.CtxWindow = w
	}

	// Price each model's own tokens at its own rates. A session that switched
	// models — 10% of live transcripts do — would otherwise be billed entirely
	// at whichever model happened to write last, in either direction.
	a.CostUSD, a.CostSource = pricePerModel(perModel, now)

	// Only infer an end for sessions a human was driving: they stop because
	// someone walked away, and hiding them is what keeps the table readable.
	//
	// For an unattended session there is no such inference to make. Nothing in
	// a transcript distinguishes "finished cleanly" from "wedged" — there is no
	// terminal stop marker — so calling it DONE would silently retire exactly
	// the alert this tool exists to raise. Left live, it stays STUCK and
	// visible; use --lookback to bound how far back sessions are considered.
	assumeEnded := p.AssumeEndedAfter
	if assumeEnded <= 0 {
		assumeEnded = DefaultAssumeEndedAfter
	}
	if a.IsInteractive() && now.Sub(a.LastActivityAt) > assumeEnded {
		a.EndedAt = a.LastActivityAt
		a.EndReason = "inferred: no transcript activity"
	}

	if a.Title == "" {
		a.Title = a.SessionID
	}
	return a
}

// applyRecord folds one decoded transcript record into the agent under
// construction.
func applyRecord(
	a *agent.Agent, r record,
	perModel map[string]*usageField,
	seenCalls, seenTools map[string]bool,
	lastCall **usageField, calls, tools *int64,
) {
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
		a.Title = jsonl.FirstLine(r.LastPrompt)
	}
	if r.Entrypoint != "" {
		a.Origin = mapOrigin(r.Entrypoint, r.Origin)
	}

	// Tool lines repeat per content block too, so count distinct tool uses.
	if r.ToolUseID != "" {
		if !seenTools[r.ToolUseID] {
			seenTools[r.ToolUseID] = true
			*tools++
		}
	} else if len(r.ToolResult) > 0 {
		*tools++
	}

	if r.Message == nil {
		return
	}
	// A synthetic record is Claude Code's own bookkeeping; letting it win the
	// "current model" race leaves the session unpriced and its window unknown.
	if r.Message.Model != "" && r.Message.Model != syntheticModel {
		a.Model = r.Message.Model
	}

	u := r.Message.Usage
	if u == nil {
		return
	}
	// One API call is written as several lines, one per content block, each
	// repeating an identical usage object. Counting them all inflates tokens,
	// cost and every derived rate by roughly the number of blocks.
	key := r.Message.ID + "\x00" + r.RequestID
	if r.Message.ID == "" && r.RequestID == "" {
		// No identity to dedupe on: count it, since dropping it would
		// under-report. Records like this predate the id fields.
		key = ""
	} else {
		if seenCalls[key] {
			return
		}
		seenCalls[key] = true
	}

	model := r.Message.Model
	if model == "" {
		model = a.Model
	}
	bucket := perModel[model]
	if bucket == nil {
		bucket = &usageField{}
		perModel[model] = bucket
	}
	bucket.InputTokens += u.InputTokens
	bucket.OutputTokens += u.OutputTokens
	bucket.CacheReadTokens += u.CacheReadTokens
	bucket.CacheCreationTokens += u.CacheCreationTokens

	copied := *u
	*lastCall = &copied
	*calls++
}

// pricePerModel totals a session's cost from per-model usage.
//
// If any model that actually consumed tokens has no rates, the total cannot be
// stated, so the whole session reports unknown rather than a figure that is
// silently missing a component.
func pricePerModel(perModel map[string]*usageField, now time.Time) (float64, agent.CostSource) {
	var total float64
	priced := false
	for model, u := range perModel {
		usage := pricing.Usage{
			Input:      u.InputTokens,
			Output:     u.OutputTokens,
			CacheRead:  u.CacheReadTokens,
			CacheWrite: u.CacheCreationTokens,
		}
		if usage == (pricing.Usage{}) {
			continue // a model that burned nothing needs no rates
		}
		cost, known := pricing.Estimate(model, usage, now)
		if !known {
			return 0, agent.CostUnknown
		}
		total += cost
		priced = true
	}
	if !priced {
		return 0, agent.CostUnknown
	}
	return total, agent.CostEstimated
}

// decodeRecord parses one transcript line, reporting whether it was usable.
func decodeRecord(line []byte) (record, bool) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return r, false
	}
	return r, true
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
