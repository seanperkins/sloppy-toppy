// Package openrouter reports OpenRouter account spend.
//
// OpenRouter is deliberately a Spend source rather than a Provider: it is a
// remote billing API with no per-session heartbeat, no tool calls and no
// context window, so it has nothing to say in an agent table. It surfaces as
// a summary strip instead, on its own slow poll interval — a billing
// endpoint must not be hit at the table's refresh rate.
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/provider"
)

// creditsURL reports the account's granted and consumed credit.
const creditsURL = "https://openrouter.ai/api/v1/credits"

// DefaultInterval is how often the credits endpoint may be polled. Balance
// moves slowly and the endpoint is rate-limited, so this is far longer than
// the agent table's refresh.
const DefaultInterval = 60 * time.Second

// Spend fetches OpenRouter account balance and usage.
type Spend struct {
	// APIKey authenticates the request. When empty the source stays silent
	// rather than reporting a failure.
	APIKey string

	// Interval overrides DefaultInterval.
	PollInterval time.Duration

	Client *http.Client
}

// New builds a Spend source, reading the API key from the environment.
//
// OPENROUTER_API_KEY is the conventional name. The key is never logged or
// included in any error message.
func New() *Spend {
	return &Spend{
		APIKey:       os.Getenv("OPENROUTER_API_KEY"),
		PollInterval: DefaultInterval,
		Client:       &http.Client{Timeout: 10 * time.Second},
	}
}

// Name implements provider.Spend.
func (s *Spend) Name() string { return "openrouter" }

// Interval implements provider.Spend.
func (s *Spend) Interval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return DefaultInterval
}

// creditsResponse is OpenRouter's credits payload.
type creditsResponse struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

// Fetch implements provider.Spend.
//
// A nil snapshot with a nil error means "not configured" — the UI shows
// nothing at all rather than a permanent error line.
func (s *Spend) Fetch(ctx context.Context) (*provider.Snapshot, error) {
	if s.APIKey == "" {
		return nil, nil
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, creditsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The status alone is reported: response bodies from an auth
		// endpoint can echo credentials.
		return nil, fmt.Errorf("openrouter returned %s", resp.Status)
	}

	var body creditsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding openrouter response: %w", err)
	}

	return &provider.Snapshot{
		Source:           s.Name(),
		CreditsRemaining: body.Data.TotalCredits - body.Data.TotalUsage,
		HasCredits:       true,
		SpentTotal:       body.Data.TotalUsage,
	}, nil
}
