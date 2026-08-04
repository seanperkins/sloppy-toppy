package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRollout builds a synthetic Codex rollout transcript.
func writeRollout(t *testing.T, lines []string, modTime time.Time) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "sessions", "2026", "08", "04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-08-04T10-00-00-test.jsonl")

	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return base
}

// sessionMetaLine reproduces the live payload shape, including the field that
// broke the first implementation: `context_window` is an OBJECT, not a token
// count. Typing it as an integer made the whole record fail to decode, which
// silently discarded cwd, source, and the session id with it.
const sessionMetaLine = `{"timestamp":"2026-08-04T10:00:00.000Z","type":"session_meta","payload":{` +
	`"id":"019f-abc","session_id":"019f-abc","cwd":"/Users/x/sites/demo-project",` +
	`"originator":"acpx","source":"vscode","cli_version":"0.145.0",` +
	`"context_window":{"window_id":"019f-def"},"model_provider":"openai",` +
	`"history_mode":"legacy","git":{"branch":"main","commit_hash":"abc"}}}`

const turnContextLine = `{"timestamp":"2026-08-04T10:00:01.000Z","type":"turn_context",` +
	`"payload":{"model":"gpt-5.6-sol","cwd":"/Users/x/sites/demo-project"}}`

const tokenCountLine = `{"timestamp":"2026-08-04T10:05:00.000Z","type":"event_msg","payload":{` +
	`"type":"token_count","info":{` +
	`"total_token_usage":{"input_tokens":466913,"cached_input_tokens":398336,` +
	`"cache_write_input_tokens":100,"output_tokens":5605,"reasoning_output_tokens":3777,` +
	`"total_tokens":472518},` +
	`"last_token_usage":{"input_tokens":73536,"cached_input_tokens":72448,` +
	`"cache_write_input_tokens":0,"output_tokens":1506,"reasoning_output_tokens":931,` +
	`"total_tokens":75042},` +
	`"model_context_window":258400}}}`

func TestParsesRollout(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 10, 0, 0, time.UTC)
	base := writeRollout(t, []string{sessionMetaLine, turnContextLine, tokenCountLine}, now)

	agents, err := New(base).Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	a := agents[0]

	if a.SessionID != "019f-abc" {
		t.Fatalf("session id = %q, want 019f-abc", a.SessionID)
	}
	// These three all come from session_meta, so they are the canary for the
	// object-vs-integer decode bug.
	if a.CWD != "/Users/x/sites/demo-project" {
		t.Fatalf("cwd = %q — session_meta failed to decode", a.CWD)
	}
	if a.Instance != "demo-project" {
		t.Fatalf("instance = %q, want demo-project", a.Instance)
	}
	if a.Origin != "interactive" {
		t.Fatalf("origin = %q, want interactive (source=vscode)", a.Origin)
	}
	if a.GitBranch != "main" {
		t.Fatalf("branch = %q, want main", a.GitBranch)
	}
	if a.Model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", a.Model)
	}

	// Codex self-reports its window, which beats any table we could ship.
	if a.CtxWindow != 258400 {
		t.Fatalf("ctx window = %d, want the self-reported 258400", a.CtxWindow)
	}
	// Context fill comes from the last call's own usage, so it is a real
	// measurement rather than a running total.
	if !a.CtxAccurate {
		t.Fatal("codex context fill should be marked exact")
	}
	if want := int64(73536 + 0 + 1506); a.CtxUsed != want {
		t.Fatalf("ctx used = %d, want %d", a.CtxUsed, want)
	}

	// Codex reports two overlapping breakdowns; both must be removed so the
	// Agent's fields are disjoint and nothing is billed twice.
	if want := int64(466913 - 398336); a.InputTokens != want {
		t.Fatalf("input tokens = %d, want %d (cached portion removed)", a.InputTokens, want)
	}
	if a.CacheReadTokens != 398336 {
		t.Fatalf("cache read = %d, want 398336", a.CacheReadTokens)
	}
	// This is the assertion that previously pinned the bug in place: it
	// checked reasoning alone while output still carried the same tokens.
	if want := int64(5605 - 3777); a.OutputTokens != want {
		t.Fatalf("output tokens = %d, want %d (reasoning is a component of "+
			"output, not a sibling — counting both double-bills it)",
			a.OutputTokens, want)
	}
	if a.ReasoningTokens != 3777 {
		t.Fatalf("reasoning = %d, want 3777 (kept as a display breakdown)", a.ReasoningTokens)
	}

	// The whole subtraction rests on this identity, so assert it directly.
	if got := a.InputTokens + a.CacheReadTokens + a.OutputTokens + a.ReasoningTokens; got != 472518 {
		t.Fatalf("disjoint fields sum to %d, want total_tokens 472518", got)
	}
}

// TestBurnExcludesDoubleCountedReasoning guards the number actually shown on
// screen: BurnTokens sums input+output+reasoning, so if reasoning were left
// inside output it would be counted twice in TOK/s and in cost.
func TestBurnExcludesDoubleCountedReasoning(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 10, 0, 0, time.UTC)
	base := writeRollout(t, []string{sessionMetaLine, turnContextLine, tokenCountLine}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	a := agents[0]

	// input(68577) + output(1828) + reasoning(3777) = 74182.
	// With the bug it was 68577 + 5605 + 3777 = 77959, +5.1%.
	const want = int64(68577 + 1828 + 3777)
	if got := a.BurnTokens(); got != want {
		t.Fatalf("burn = %d, want %d — reasoning is being counted twice", got, want)
	}
}

// TestComponentsAgreeGuardsTheInvariant checks the cross-check that would have
// caught the double-count: TotalTokens was decoded and never read.
func TestComponentsAgreeGuardsTheInvariant(t *testing.T) {
	ok := &codexUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}
	if !ok.componentsAgree() {
		t.Fatal("input+output == total was reported as disagreeing")
	}
	// If a future payload made reasoning additive, total would no longer match.
	bad := &codexUsage{InputTokens: 100, OutputTokens: 20, ReasoningOutput: 5, TotalTokens: 125}
	if bad.componentsAgree() {
		t.Fatal("a payload where reasoning is additive was accepted as consistent")
	}
	// Absent total: nothing to check against, so do not cry wolf.
	if !(&codexUsage{InputTokens: 1}).componentsAgree() {
		t.Fatal("a payload with no total_tokens was reported as inconsistent")
	}
}

// TestUnattendedRolloutNotInferredEnded mirrors the Claude Code case: a quiet
// unattended session must stay live so it can classify STUCK.
func TestUnattendedRolloutNotInferredEnded(t *testing.T) {
	written := time.Date(2026, time.August, 4, 10, 5, 0, 0, time.UTC)
	// originator acpx with no interactive source -> unattended.
	meta := `{"timestamp":"2026-08-04T10:00:00.000Z","type":"session_meta","payload":{` +
		`"id":"019f-zzz","session_id":"019f-zzz","cwd":"/Users/x/sites/demo-project",` +
		`"originator":"acpx","cli_version":"0.145.0","git":{"branch":"main"}}}`
	base := writeRollout(t, []string{meta, turnContextLine, tokenCountLine}, written)

	agents, _ := New(base).Poll(context.Background(), written.Add(6*time.Hour))
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if agents[0].IsInteractive() {
		t.Fatalf("origin %q counted as interactive", agents[0].Origin)
	}
	if !agents[0].EndedAt.IsZero() {
		t.Fatal("an unattended quiet rollout was inferred as ended, hiding a " +
			"potentially wedged agent")
	}
}

// TestUnknownModelCostIsUnknown pins that a model we ship no rates for is
// reported as unknown rather than as free.
func TestUnknownModelCostIsUnknown(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 10, 0, 0, time.UTC)
	base := writeRollout(t, []string{sessionMetaLine, turnContextLine, tokenCountLine}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	if agents[0].CostSource.Known() {
		t.Fatalf("gpt-5.6-sol reported a known cost source (%q); we ship no rates for it",
			agents[0].CostSource)
	}
	if agents[0].CostUSD != 0 {
		t.Fatalf("cost = %v alongside an unknown source", agents[0].CostUSD)
	}
}

// TestQuietSessionInferredEnded covers the absence of an end marker: without
// this inference every finished run accumulates as STUCK forever.
func TestQuietSessionInferredEnded(t *testing.T) {
	written := time.Date(2026, time.August, 4, 10, 5, 0, 0, time.UTC)
	base := writeRollout(t, []string{sessionMetaLine, turnContextLine, tokenCountLine}, written)

	// Poll three hours later, well past the one-hour quiet threshold.
	now := written.Add(3 * time.Hour)
	agents, _ := New(base).Poll(context.Background(), now)
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if agents[0].EndedAt.IsZero() {
		t.Fatal("a long-quiet rollout was not inferred as ended")
	}
	if agents[0].EndReason == "" {
		t.Fatal("inferred end carries no reason, so it looks like an observed end")
	}

	// A freshly-written rollout must stay live.
	agents, _ = New(base).Poll(context.Background(), written.Add(time.Minute))
	if !agents[0].EndedAt.IsZero() {
		t.Fatal("an active rollout was inferred as ended")
	}
}

func TestLookbackSkipsOldFiles(t *testing.T) {
	old := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	base := writeRollout(t, []string{sessionMetaLine, tokenCountLine}, old)

	now := old.Add(48 * time.Hour)
	p := New(base)
	p.Lookback = 12 * time.Hour
	agents, err := p.Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents past the lookback window, want 0", len(agents))
	}
}

func TestMissingBaseIsNotAnError(t *testing.T) {
	agents, err := New(filepath.Join(t.TempDir(), "absent")).
		Poll(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("missing codex install returned %v, want nil", err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents from a missing install", len(agents))
	}
}
