package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript builds a synthetic Claude Code project transcript.
func writeTranscript(t *testing.T, project string, lines []string, modTime time.Time) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "3ececa67-b93c-48e8-8a90-21baa47036a7.jsonl")

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

// Assistant records carry per-call usage; there are no cumulative counters,
// so the adapter must sum them.
const assistantOne = `{"type":"assistant","timestamp":"2026-08-04T18:00:00.000Z",` +
	`"sessionId":"3ececa67","cwd":"/Users/x/sites/demo","gitBranch":"main","entrypoint":"cli",` +
	`"message":{"model":"claude-opus-5","usage":{"input_tokens":10,` +
	`"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000,"output_tokens":300}}}`

const assistantTwo = `{"type":"assistant","timestamp":"2026-08-04T18:05:00.000Z",` +
	`"sessionId":"3ececa67","entrypoint":"cli",` +
	`"message":{"model":"claude-opus-5","usage":{"input_tokens":2,` +
	`"cache_creation_input_tokens":2454,"cache_read_input_tokens":81558,"output_tokens":450}}}`

const titleLine = `{"type":"ai-title","sessionId":"3ececa67","aiTitle":"Code review for project"}`

func TestParsesTranscript(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	base := writeTranscript(t, "-Users-x-sites-demo",
		[]string{assistantOne, titleLine, assistantTwo}, now)

	agents, err := New(base).Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	a := agents[0]

	if a.Title != "Code review for project" {
		t.Fatalf("title = %q, want the ai-title value", a.Title)
	}
	if a.Instance != "demo" {
		t.Fatalf("instance = %q, want demo (from the project slug)", a.Instance)
	}
	if a.Model != "claude-opus-5" {
		t.Fatalf("model = %q", a.Model)
	}

	// Per-call usage must be summed into cumulative totals.
	if a.OutputTokens != 750 {
		t.Fatalf("output = %d, want 750 (300 + 450 summed)", a.OutputTokens)
	}
	if a.CacheReadTokens != 101558 {
		t.Fatalf("cache read = %d, want 101558", a.CacheReadTokens)
	}
	if a.APICallCount != 2 {
		t.Fatalf("api calls = %d, want 2", a.APICallCount)
	}

	// Context comes from the LAST call's prefix, not the running total —
	// that is what makes it a real measurement.
	if !a.CtxAccurate {
		t.Fatal("claude code context fill should be marked exact")
	}
	if want := int64(2 + 81558 + 2454 + 450); a.CtxUsed != want {
		t.Fatalf("ctx used = %d, want %d (last call's prefix)", a.CtxUsed, want)
	}
	if a.CtxWindow != 1_000_000 {
		t.Fatalf("ctx window = %d, want 1000000 for opus-5", a.CtxWindow)
	}

	// Claude Code records no cost, so it must be derived and labelled.
	if a.CostSource != "estimated" {
		t.Fatalf("cost source = %q, want estimated", a.CostSource)
	}
	if a.CostUSD <= 0 {
		t.Fatalf("derived cost = %v, want > 0", a.CostUSD)
	}
}

// TestOriginDistinguishesInteractiveFromSDK covers the signal STUCK
// detection depends on: a human at a terminal versus an unattended run.
func TestOriginDistinguishesInteractiveFromSDK(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)

	base := writeTranscript(t, "-Users-x-sites-demo", []string{assistantOne}, now)
	agents, _ := New(base).Poll(context.Background(), now)
	if agents[0].Origin != "interactive" || !agents[0].IsInteractive() {
		t.Fatalf("entrypoint=cli gave origin %q, want interactive", agents[0].Origin)
	}

	sdkLine := `{"type":"assistant","timestamp":"2026-08-04T18:00:00.000Z",` +
		`"sessionId":"s2","entrypoint":"sdk-cli","message":{"model":"claude-opus-5",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}}`
	base = writeTranscript(t, "-Users-x-sites-demo", []string{sdkLine}, now)
	agents, _ = New(base).Poll(context.Background(), now)
	if agents[0].Origin != "sdk" {
		t.Fatalf("entrypoint=sdk-cli gave origin %q, want sdk", agents[0].Origin)
	}
	if agents[0].IsInteractive() {
		t.Fatal("an sdk run must be eligible for STUCK, so it cannot be interactive")
	}
}

// TestQuietSessionInferredEnded pins the behaviour that stops finished runs
// from piling up as STUCK forever. On a live install this collapsed 25 false
// STUCK rows to zero.
func TestQuietSessionInferredEnded(t *testing.T) {
	// The fixture's last record is stamped 18:05, so the file mtime must
	// agree with it for the quiet interval to mean anything.
	lastRecord := time.Date(2026, time.August, 4, 18, 5, 0, 0, time.UTC)
	base := writeTranscript(t, "-Users-x-sites-demo",
		[]string{assistantOne, assistantTwo}, lastRecord)

	agents, _ := New(base).Poll(context.Background(), lastRecord.Add(6*time.Hour))
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if agents[0].EndedAt.IsZero() {
		t.Fatal("a long-quiet transcript was not inferred as ended")
	}
	if agents[0].EndReason == "" {
		t.Fatal("inferred end carries no reason, so it looks like an observed end")
	}

	// A transcript written moments ago must stay live.
	agents, _ = New(base).Poll(context.Background(), lastRecord.Add(time.Minute))
	if !agents[0].EndedAt.IsZero() {
		t.Fatal("an active transcript was inferred as ended")
	}
}

func TestLookbackSkipsOldTranscripts(t *testing.T) {
	old := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	base := writeTranscript(t, "-Users-x-sites-demo", []string{assistantOne}, old)

	p := New(base)
	p.Lookback = 12 * time.Hour
	agents, err := p.Poll(context.Background(), old.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents past the lookback window, want 0", len(agents))
	}
}

// TestTruncatedTrailingLineIsTolerated covers reading a transcript that the
// running agent is appending to right now.
func TestTruncatedTrailingLineIsTolerated(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	base := writeTranscript(t, "-Users-x-sites-demo",
		[]string{assistantOne, `{"type":"assistant","message":{"usa`}, now)

	agents, err := New(base).Poll(context.Background(), now)
	if err != nil {
		t.Fatalf("partial trailing line returned %v, want nil", err)
	}
	if len(agents) != 1 || agents[0].OutputTokens != 300 {
		t.Fatalf("complete records were not preserved alongside a partial line")
	}
}

func TestMissingBaseIsNotAnError(t *testing.T) {
	agents, err := New(filepath.Join(t.TempDir(), "absent")).
		Poll(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("missing claude install returned %v, want nil", err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents from a missing install", len(agents))
	}
}
