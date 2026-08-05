package openrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc lets a test intercept the request without changing the
// production code to accept a base URL. The real URL, headers and status
// handling all stay on the path under test.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubClient returns a client that answers every request with the given status
// and body, and records the request it saw.
func stubClient(status int, body string, seen **http.Request) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if seen != nil {
				*seen = r
			}
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	}
}

func TestUnconfiguredSourceStaysSilent(t *testing.T) {
	// A nil snapshot with a nil error is the contract for "not configured".
	// Returning an error instead would park a permanent failure line in the
	// UI for every user who does not use OpenRouter at all.
	s := &Spend{APIKey: ""}

	snap, err := s.Fetch(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil for an unconfigured source", err)
	}
	if snap != nil {
		t.Errorf("snapshot = %+v, want nil", snap)
	}
}

func TestFetchComputesRemainingCredits(t *testing.T) {
	s := &Spend{
		APIKey: "sk-test",
		Client: stubClient(http.StatusOK, `{"data":{"total_credits":50.0,"total_usage":12.25}}`, nil),
	}

	snap, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if got, want := snap.CreditsRemaining, 37.75; got != want {
		t.Errorf("CreditsRemaining = %v, want %v", got, want)
	}
	if got, want := snap.SpentTotal, 12.25; got != want {
		t.Errorf("SpentTotal = %v, want %v", got, want)
	}
	if !snap.HasCredits {
		t.Error("HasCredits = false, want true")
	}
	if snap.Source != "openrouter" {
		t.Errorf("Source = %q, want %q", snap.Source, "openrouter")
	}
	// A rate needs two fetches to diff. The monitor sets it; the source
	// must not claim one, or a single --once run renders "$0.00/hr" for an
	// account that is actively burning.
	if snap.RateKnown {
		t.Error("RateKnown = true from a single fetch, want false")
	}
}

func TestOverdrawnBalanceStaysNegative(t *testing.T) {
	// An account past its credit is exactly when the number matters most.
	// Clamping it to zero would render an overdraft as a healthy balance.
	s := &Spend{
		APIKey: "sk-test",
		Client: stubClient(http.StatusOK, `{"data":{"total_credits":10.0,"total_usage":13.5}}`, nil),
	}

	snap, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.CreditsRemaining >= 0 {
		t.Errorf("CreditsRemaining = %v, want negative", snap.CreditsRemaining)
	}
}

func TestFetchSendsBearerAuth(t *testing.T) {
	var seen *http.Request
	s := &Spend{
		APIKey: "sk-secret-value",
		Client: stubClient(http.StatusOK, `{"data":{}}`, &seen),
	}

	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if seen == nil {
		t.Fatal("no request was issued")
	}
	if got, want := seen.Header.Get("Authorization"), "Bearer sk-secret-value"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if seen.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", seen.Method)
	}
	if !strings.HasPrefix(seen.URL.String(), "https://") {
		t.Errorf("URL = %q, want https", seen.URL)
	}
	// The key must never ride in the query string, where it would land in
	// proxy and server access logs.
	if strings.Contains(seen.URL.String(), "sk-secret-value") {
		t.Error("API key leaked into the request URL")
	}
}

func TestErrorFromBadStatusOmitsTheKeyAndBody(t *testing.T) {
	// Auth endpoints echo credentials in error bodies. The error text is
	// shown in the UI and may be pasted into a bug report, so it must carry
	// the status and nothing else.
	const key = "sk-secret-value"
	body := `{"error":"invalid key sk-secret-value for account"}`
	s := &Spend{
		APIKey: key,
		Client: stubClient(http.StatusUnauthorized, body, nil),
	}

	_, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected an error for a 401")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error text leaked the API key: %q", err)
	}
	if strings.Contains(err.Error(), "invalid key") {
		t.Errorf("error text echoed the response body: %q", err)
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	// A truncated or HTML response (a captive portal, a proxy error page)
	// must fail loudly rather than decode to a zero balance.
	s := &Spend{
		APIKey: "sk-test",
		Client: stubClient(http.StatusOK, `<html>gateway timeout</html>`, nil),
	}

	snap, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if snap != nil {
		t.Errorf("snapshot = %+v, want nil alongside the error", snap)
	}
}

func TestTransportErrorIsWrapped(t *testing.T) {
	s := &Spend{
		APIKey: "sk-test",
		Client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: no route to host")
			}),
		},
	}

	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("expected a transport error")
	} else if !strings.Contains(err.Error(), "openrouter") {
		t.Errorf("error %q does not name the source", err)
	}
}

func TestContextIsThreadedIntoTheRequest(t *testing.T) {
	// Quitting the UI must abort an in-flight billing call rather than hold
	// the process open for the client timeout. Cancellation only reaches the
	// transport if the request carries the caller's context, so this asserts
	// the request's own context is the cancelled one. Swapping
	// NewRequestWithContext for NewRequest fails here.
	//
	// The stub does the ctx check a real http.Transport would; http.Client
	// itself does not inspect the context on behalf of a RoundTripper.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &Spend{
		APIKey: "sk-test",
		Client: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("request did not carry the cancelled context")
			}),
		},
	}

	_, err := s.Fetch(ctx)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestIntervalDefaultsAndOverride(t *testing.T) {
	// The default must stay well above the agent table's refresh: this is a
	// rate-limited billing endpoint, not a heartbeat.
	if DefaultInterval < 30*time.Second {
		t.Errorf("DefaultInterval = %v, too aggressive for a billing API", DefaultInterval)
	}

	s := &Spend{}
	if got := s.Interval(); got != DefaultInterval {
		t.Errorf("zero PollInterval gave %v, want the default %v", got, DefaultInterval)
	}

	s.PollInterval = 5 * time.Minute
	if got := s.Interval(); got != 5*time.Minute {
		t.Errorf("Interval() = %v, want the override 5m", got)
	}

	s.PollInterval = -1
	if got := s.Interval(); got != DefaultInterval {
		t.Errorf("negative PollInterval gave %v, want the default", got)
	}
}

func TestNewReadsKeyFromEnvironment(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-from-env")

	s := New()
	if s.APIKey != "sk-from-env" {
		t.Errorf("APIKey = %q, want it read from OPENROUTER_API_KEY", s.APIKey)
	}
	if s.Client == nil || s.Client.Timeout == 0 {
		t.Error("New must set a client timeout; an unbounded billing call can wedge the poll")
	}
}

func TestNewWithoutEnvIsSilent(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")

	snap, err := New().Fetch(context.Background())
	if snap != nil || err != nil {
		t.Errorf("got (%+v, %v), want (nil, nil) when the env var is unset", snap, err)
	}
}

func TestNameIsStable(t *testing.T) {
	// The name keys the monitor's per-source rate state. Changing it silently
	// resets the $/hr baseline.
	if got := (&Spend{}).Name(); got != "openrouter" {
		t.Errorf("Name() = %q, want %q", got, "openrouter")
	}
}
