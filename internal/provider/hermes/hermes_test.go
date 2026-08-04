package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// writeFixture builds a synthetic Hermes install: a state.db matching the
// live schema and a gateway.pid pointing at the running test process, so the
// liveness check passes on any OS.
//
// This replaces the previous golden tests, which pinned one developer's live
// session id and asserted a "slack" profile was running — they hard-failed
// (not skipped) on any other machine.
func writeFixture(t *testing.T, profile string, mutate func(*sql.DB)) string {
	t.Helper()

	base := t.TempDir()
	dir := base
	if profile != "default" {
		dir = filepath.Join(base, "profiles", profile)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pid, err := json.Marshal(pidFile{
		PID:       os.Getpid(),
		Kind:      "hermes-gateway",
		StartTime: 1,
		Home:      dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway.pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Column set and types mirror the live schema.
	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT, source TEXT, model TEXT, started_at REAL, ended_at REAL,
			end_reason TEXT, input_tokens INTEGER, output_tokens INTEGER,
			cache_read_tokens INTEGER, cache_write_tokens INTEGER,
			reasoning_tokens INTEGER, estimated_cost_usd REAL,
			actual_cost_usd REAL, last_activity_at REAL,
			last_activity_description TEXT, title TEXT, cwd TEXT,
			git_branch TEXT, api_call_count INTEGER, tool_call_count INTEGER
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY, session_id TEXT, tool_name TEXT
		);`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`
		INSERT INTO sessions VALUES
		('sess-cli','cli','test/model-1',100.0,NULL,NULL,100,50,40,5,10,
		 0.001,NULL,995.0,'','CLI Fixture','/tmp','main',2,1),
		('sess-chat','telegram','test/model-1',100.0,NULL,NULL,10,5,0,0,0,
		 0.0005,NULL,500.0,'','Chat Fixture','/tmp','main',1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (session_id, tool_name) VALUES ('sess-cli','terminal')`); err != nil {
		t.Fatal(err)
	}

	if mutate != nil {
		mutate(db)
	}
	return base
}

func TestReadsSessionsAndOrigins(t *testing.T) {
	base := writeFixture(t, "default", nil)
	p := New(base)

	agents, err := p.Poll(context.Background(), time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}

	byID := map[string]int{}
	for i, a := range agents {
		byID[a.SessionID] = i
	}

	cli := agents[byID["sess-cli"]]
	// The regression this whole refactor turned on: the session's own
	// `source` column must land on Origin, not the profile name.
	if cli.Origin != "cli" {
		t.Fatalf("origin = %q, want %q (the sessions.source column)", cli.Origin, "cli")
	}
	if cli.Instance != "default" {
		t.Fatalf("instance = %q, want %q", cli.Instance, "default")
	}
	if cli.IsInteractive() {
		t.Fatal("a cli session must not count as interactive")
	}
	if cli.LastTool != "terminal" {
		t.Fatalf("last tool = %q, want terminal", cli.LastTool)
	}
	if cli.CostSource.Known() != true {
		t.Fatal("hermes reports cost directly; CostSource should be known")
	}

	chat := agents[byID["sess-chat"]]
	if chat.Origin != "telegram" || !chat.IsInteractive() {
		t.Fatalf("chat origin = %q, interactive = %v; want telegram/true",
			chat.Origin, chat.IsInteractive())
	}
}

// TestContextIsMarkedApproximate documents that Hermes can only give a lower
// bound, so the UI must not present it as measured.
func TestContextIsMarkedApproximate(t *testing.T) {
	base := writeFixture(t, "default", nil)
	agents, err := New(base).Poll(context.Background(), time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.CtxAccurate {
			t.Fatal("hermes context fill claimed to be exact")
		}
	}
	// Cache reads are excluded from context on purpose: they accumulate per
	// call and would push the figure past 100%.
	for _, a := range agents {
		if a.SessionID == "sess-cli" && a.CtxUsed != 160 {
			t.Fatalf("ctx used = %d, want 160 (in+out+reasoning, no cache reads)", a.CtxUsed)
		}
	}
}

func TestPrefersActualCostOverEstimate(t *testing.T) {
	base := writeFixture(t, "default", func(db *sql.DB) {
		if _, err := db.Exec(
			`UPDATE sessions SET actual_cost_usd = 0.042 WHERE id = 'sess-cli'`); err != nil {
			t.Fatal(err)
		}
	})
	agents, err := New(base).Poll(context.Background(), time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.SessionID == "sess-cli" && a.CostUSD != 0.042 {
			t.Fatalf("cost = %v, want the settled actual_cost_usd 0.042", a.CostUSD)
		}
	}
}

func TestDeadGatewayIsSkipped(t *testing.T) {
	base := writeFixture(t, "default", nil)
	// PID 0 is never a live process.
	pid, _ := json.Marshal(pidFile{PID: 0, Kind: "hermes-gateway"})
	if err := os.WriteFile(filepath.Join(base, "gateway.pid"), pid, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := New(base).Discover(); len(got) != 0 {
		t.Fatalf("discovered %d installs behind a dead gateway, want 0", len(got))
	}
}

// TestProcessAliveIsPortable pins the fix for the /proc check, which
// reported every process as dead on macOS and the BSDs.
func TestProcessAliveIsPortable(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("the running test process was reported dead")
	}
	if processAlive(0) {
		t.Fatal("pid 0 reported alive")
	}
}

func TestDiscoversProfiles(t *testing.T) {
	base := writeFixture(t, "worker", nil)
	installs := New(base).Discover()
	if len(installs) != 1 {
		t.Fatalf("discovered %d installs, want 1", len(installs))
	}
	if installs[0].Profile != "worker" {
		t.Fatalf("profile = %q, want worker", installs[0].Profile)
	}
}

// TestDSNEscapesAwkwardPaths covers a base directory containing characters
// that are significant in a URI.
func TestDSNEscapesAwkwardPaths(t *testing.T) {
	dsn := readOnlyDSN("/tmp/weird?dir/state.db")
	if want := "mode=ro"; !contains(dsn, want) {
		t.Fatalf("dsn %q missing %q", dsn, want)
	}
	// The '?' in the path must not be read as the query separator.
	if contains(dsn, "/weird?dir/") {
		t.Fatalf("dsn %q left an unescaped '?' in the path", dsn)
	}
}

func TestContextCacheParsing(t *testing.T) {
	dir := t.TempDir()
	body := "" +
		"# cached at: 1234\n" + // a comment must not become a model entry
		"models:\n" +
		"  test/model-1@https://api.example/v1: 4096\n" +
		"other/model: 8192\n" +
		"broken: not-a-number\n"
	if err := os.WriteFile(filepath.Join(dir, "context_length_cache.yaml"),
		[]byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cache := loadContextCache(dir)
	if _, bad := cache["# cached at"]; bad {
		t.Fatal("a comment line was parsed as a model entry")
	}
	if _, bad := cache["broken"]; bad {
		t.Fatal("a non-numeric value was accepted")
	}
	// Keys contain colons, so the split must be on the last one.
	if got := modelCtxWindow("test/model-1", cache, 128_000); got != 4096 {
		t.Fatalf("window = %d, want 4096 (matched via the @base_url suffix)", got)
	}
	if got := modelCtxWindow("unlisted/model", cache, 128_000); got != 128_000 {
		t.Fatalf("window = %d, want the 128000 fallback", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
