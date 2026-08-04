package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/jsonl"
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

// TestContentBlockSplitCountedOnce pins the CRITICAL regression: Claude Code
// writes one JSONL line per content block and repeats an identical usage
// object on each, so summing every line inflated tokens, cost and every
// derived rate by roughly the block count (~2x on real transcripts).
//
// The original fixture could not catch this — it had no message.id at all and
// exactly one line per call.
func TestContentBlockSplitCountedOnce(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)

	// One API call, emitted as three lines sharing (message.id, requestId).
	const blockFmt = `{"type":"assistant","timestamp":"2026-08-04T18:00:0%d.000Z",` +
		`"sessionId":"s1","requestId":"req_AAA","entrypoint":"cli","cwd":"/x/demo",` +
		`"message":{"id":"msg_AAA","model":"claude-opus-5","usage":{"input_tokens":10,` +
		`"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000,"output_tokens":300}}}`

	lines := []string{
		fmt.Sprintf(blockFmt, 0),
		fmt.Sprintf(blockFmt, 1),
		fmt.Sprintf(blockFmt, 2),
	}
	base := writeTranscript(t, "-x-demo", lines, now)

	agents, err := New(base).Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	a := agents[0]

	if a.OutputTokens != 300 {
		t.Fatalf("output = %d, want 300 — three lines of one call were summed",
			a.OutputTokens)
	}
	if a.CacheReadTokens != 20000 {
		t.Fatalf("cache read = %d, want 20000", a.CacheReadTokens)
	}
	if a.APICallCount != 1 {
		t.Fatalf("api calls = %d, want 1 (three content blocks, one call)", a.APICallCount)
	}
}

// TestRecordsWithoutIDsStillCounted checks the dedupe does not silently drop
// older records that carry no identity to dedupe on — under-reporting would be
// the same class of bug in the other direction.
func TestRecordsWithoutIDsStillCounted(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	base := writeTranscript(t, "-x-demo", []string{assistantOne, assistantTwo}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	if agents[0].OutputTokens != 750 {
		t.Fatalf("output = %d, want 750 (300+450, neither record has an id)",
			agents[0].OutputTokens)
	}
	if agents[0].APICallCount != 2 {
		t.Fatalf("api calls = %d, want 2", agents[0].APICallCount)
	}
}

// TestMixedModelSessionPricedPerModel pins that a session which switched
// models is billed per model, not entirely at whichever model wrote last.
func TestMixedModelSessionPricedPerModel(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)

	// 1M output tokens on opus-5 ($25/MTok), then 1M on haiku-4-5 ($5/MTok).
	mk := func(id, model string, sec int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-08-04T18:00:%02d.000Z",`+
			`"sessionId":"s1","requestId":"req_%s","entrypoint":"cli","cwd":"/x/demo",`+
			`"message":{"id":"msg_%s","model":"%s","usage":{"input_tokens":0,`+
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,`+
			`"output_tokens":1000000}}}`, sec, id, id, model)
	}
	base := writeTranscript(t, "-x-demo",
		[]string{mk("A", "claude-opus-5", 0), mk("B", "claude-haiku-4-5", 1)}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	a := agents[0]

	// Priced per model: 25 + 5. Priced at the last model seen (haiku): 10.
	// Priced at the first: 50. Only the per-model sum gives 30.
	const want = 30.0
	if diff := a.CostUSD - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("cost = %v, want %v (opus 1M out @$25 + haiku 1M out @$5)", a.CostUSD, want)
	}
	if a.CostSource != agent.CostEstimated {
		t.Fatalf("cost source = %q, want estimated", a.CostSource)
	}
}

// TestUnpricedModelInMixSessionReportsUnknown checks that a session containing
// a model we have no rates for reports unknown rather than a total that is
// silently missing a component.
func TestUnpricedModelInMixSessionReportsUnknown(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	unpriced := `{"type":"assistant","timestamp":"2026-08-04T18:00:01.000Z",` +
		`"sessionId":"s1","requestId":"req_B","entrypoint":"cli","cwd":"/x/demo",` +
		`"message":{"id":"msg_B","model":"some-unlisted-model","usage":{"output_tokens":5000}}}`
	base := writeTranscript(t, "-x-demo", []string{assistantOne, unpriced}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	if agents[0].CostSource.Known() {
		t.Fatalf("cost source = %q; a session containing an unpriced model must "+
			"not report a total that omits it", agents[0].CostSource)
	}
}

// TestSyntheticModelDoesNotWinDisplayModel covers records Claude Code
// generates itself: they carry zero usage but previously overwrote the model,
// leaving a live session with blank cost and blank context.
func TestSyntheticModelDoesNotWinDisplayModel(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	synth := `{"type":"assistant","timestamp":"2026-08-04T18:09:00.000Z",` +
		`"sessionId":"s1","requestId":"req_S","entrypoint":"cli",` +
		`"message":{"id":"msg_S","model":"<synthetic>","usage":{"input_tokens":0,` +
		`"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	base := writeTranscript(t, "-x-demo", []string{assistantOne, synth}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	a := agents[0]
	if a.Model != "claude-opus-5" {
		t.Fatalf("model = %q, want claude-opus-5 (a synthetic record won the race)", a.Model)
	}
	if a.CtxWindow == 0 {
		t.Fatal("context window is 0 — the synthetic model defeated the lookup")
	}
	if !a.CostSource.Known() {
		t.Fatal("cost unknown — the synthetic model defeated pricing")
	}
}

// TestOversizedLineSkippedNotFatal pins the monitoring bypass: an unchecked
// bufio.Scanner stopped the whole scan on a >16MB line, and the partial
// session was reported as authoritative with no error anywhere.
func TestOversizedLineSkippedNotFatal(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)

	huge := `{"type":"user","timestamp":"2026-08-04T18:00:30.000Z","sessionId":"s1",` +
		`"toolUseResult":"` + strings.Repeat("A", jsonl.MaxLineBytes+1024) + `"}`
	base := writeTranscript(t, "-x-demo", []string{assistantOne, huge, assistantTwo}, now)

	agents, err := New(base).Poll(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	a := agents[0]

	// The record *after* the oversized line must still be counted.
	if a.OutputTokens != 750 {
		t.Fatalf("output = %d, want 750 — parsing stopped at the oversized line "+
			"instead of skipping it", a.OutputTokens)
	}
	if !a.Incomplete {
		t.Fatal("session not flagged incomplete after discarding an oversized line")
	}
}

// TestUnattendedSessionNotInferredEnded pins that the end-inference cannot
// retire the alert the tool exists to raise. There is no completion marker in a
// transcript, so an unattended session going quiet must stay live (and so
// classify STUCK) rather than being called DONE.
func TestUnattendedSessionNotInferredEnded(t *testing.T) {
	lastRecord := time.Date(2026, time.August, 4, 18, 5, 0, 0, time.UTC)
	sdk := `{"type":"assistant","timestamp":"2026-08-04T18:05:00.000Z","sessionId":"s2",` +
		`"entrypoint":"sdk-cli","message":{"model":"claude-opus-5",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}}`
	base := writeTranscript(t, "-x-demo", []string{sdk}, lastRecord)

	agents, _ := New(base).Poll(context.Background(), lastRecord.Add(6*time.Hour))
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if !agents[0].EndedAt.IsZero() {
		t.Fatal("an unattended quiet session was inferred as ended; a wedged " +
			"agent would be silently hidden instead of reported STUCK")
	}
}

// TestInstanceFromCWDNotSlug pins the label fix: the project slug flattens
// path separators to dashes and is not reversible, so a hyphenated project
// collapsed to its final word (sloppy-toppy displayed as "toppy").
func TestInstanceFromCWDNotSlug(t *testing.T) {
	now := time.Date(2026, time.August, 4, 18, 10, 0, 0, time.UTC)
	rec := `{"type":"assistant","timestamp":"2026-08-04T18:00:00.000Z","sessionId":"s1",` +
		`"entrypoint":"cli","cwd":"/Users/x/sites/sloppy-toppy",` +
		`"message":{"model":"claude-opus-5","usage":{"output_tokens":1}}}`
	base := writeTranscript(t, "-Users-x-sites-sloppy-toppy", []string{rec}, now)

	agents, _ := New(base).Poll(context.Background(), now)
	if got := agents[0].Instance; got != "sloppy-toppy" {
		t.Fatalf("instance = %q, want sloppy-toppy (the slug's last dash segment is %q)",
			got, "toppy")
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
