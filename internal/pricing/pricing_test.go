package pricing

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testNow = time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)

func TestLookupKnownModels(t *testing.T) {
	tests := []struct {
		model     string
		wantIn    float64
		wantOut   float64
		wantFound bool
	}{
		{model: "claude-opus-5", wantIn: 5, wantOut: 25, wantFound: true},
		{model: "claude-fable-5", wantIn: 10, wantOut: 50, wantFound: true},
		{model: "claude-haiku-4-5", wantIn: 1, wantOut: 5, wantFound: true},
		// Provider-decorated forms must resolve to the same entry.
		{model: "anthropic/claude-opus-5", wantIn: 5, wantOut: 25, wantFound: true},
		{model: "anthropic.claude-opus-5", wantIn: 5, wantOut: 25, wantFound: true},
		{model: "claude-opus-5[1m]", wantIn: 5, wantOut: 25, wantFound: true},
		// Fast mode is a real premium tier, not a suffix to strip.
		{model: "claude-opus-5-fast", wantIn: 10, wantOut: 50, wantFound: true},
		// An unknown model must report as unknown rather than defaulting.
		{model: "gpt-5.6-sol", wantFound: false},
		{model: "", wantFound: false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			r, ok := Lookup(tc.model, testNow)
			if ok != tc.wantFound {
				t.Fatalf("found = %v, want %v", ok, tc.wantFound)
			}
			if !ok {
				return
			}
			if r.InputPerMTok != tc.wantIn || r.OutputPerMTok != tc.wantOut {
				t.Fatalf("rates = %v/%v, want %v/%v",
					r.InputPerMTok, r.OutputPerMTok, tc.wantIn, tc.wantOut)
			}
		})
	}
}

// TestIntroPricingExpires checks that promotional pricing applies while live
// and reverts afterwards, rather than being frozen in whenever the binary
// happened to be built.
func TestIntroPricingExpires(t *testing.T) {
	during := time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, time.September, 15, 0, 0, 0, 0, time.UTC)

	if r, _ := Lookup("claude-sonnet-5", during); r.InputPerMTok != 2 {
		t.Fatalf("intro input rate = %v, want 2", r.InputPerMTok)
	}
	if r, _ := Lookup("claude-sonnet-5", after); r.InputPerMTok != 3 {
		t.Fatalf("post-intro input rate = %v, want 3", r.InputPerMTok)
	}
}

func TestCost(t *testing.T) {
	r := std(5, 25) // $5 in / $25 out per MTok
	got := Cost(Usage{
		Input:      1_000_000,
		Output:     1_000_000,
		CacheRead:  1_000_000, // 0.1x input = $0.50
		CacheWrite: 1_000_000, // 1.25x input = $6.25
	}, r)
	want := 5.0 + 25.0 + 0.5 + 6.25
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

// TestReasoningDefaultsToOutputRate documents that a provider billing
// reasoning at the output rate needs no explicit entry.
func TestReasoningDefaultsToOutputRate(t *testing.T) {
	r := std(5, 25)
	got := Cost(Usage{Reasoning: 1_000_000}, r)
	if diff := got - 25.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("reasoning cost = %v, want 25 (the output rate)", got)
	}
}

func TestEstimateUnknownModelReportsUnknown(t *testing.T) {
	cost, known := Estimate("some/unlisted-model", Usage{Input: 1_000_000}, testNow)
	if known {
		t.Fatal("unknown model reported as priced")
	}
	if cost != 0 {
		t.Fatalf("cost = %v, want 0 alongside known=false", cost)
	}
}

// TestOverridesPriceUnknownModels covers the escape hatch that lets a user
// price OpenRouter or self-hosted models we ship no rates for.
func TestOverridesPriceUnknownModels(t *testing.T) {
	t.Cleanup(func() { overrides = nil })

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	// Deliberately omit the cache multipliers to check they are defaulted.
	body := `{"gpt-5.6-sol": {"input_per_mtok": 2.5, "output_per_mtok": 10}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadOverrides(path); err != nil {
		t.Fatal(err)
	}

	r, ok := Lookup("gpt-5.6-sol", testNow)
	if !ok {
		t.Fatal("override not applied")
	}
	if r.InputPerMTok != 2.5 || r.OutputPerMTok != 10 {
		t.Fatalf("override rates = %v/%v, want 2.5/10", r.InputPerMTok, r.OutputPerMTok)
	}
	if r.CacheReadMult != cacheReadMult {
		t.Fatalf("omitted cache multiplier = %v, want default %v", r.CacheReadMult, cacheReadMult)
	}
}

func TestMissingOverrideFileIsNotAnError(t *testing.T) {
	if err := LoadOverrides(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("missing override file returned %v, want nil", err)
	}
}

func TestContextWindow(t *testing.T) {
	if w, ok := ContextWindow("claude-opus-5"); !ok || w != 1_000_000 {
		t.Fatalf("opus-5 window = %v (found %v), want 1000000", w, ok)
	}
	if w, ok := ContextWindow("claude-haiku-4-5"); !ok || w != 200_000 {
		t.Fatalf("haiku window = %v (found %v), want 200000", w, ok)
	}
	if _, ok := ContextWindow("gpt-5.6-sol"); ok {
		t.Fatal("unknown model reported a context window")
	}
}
