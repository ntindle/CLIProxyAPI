package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func soonestResetConfig() *internalconfig.Config {
	return &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first", FillFirstOrder: "soonest-reset"},
	}
}

func TestForkSoonestResetRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(soonestResetConfig())
	if state.strategy != "fill-first" || !state.fillFirstSoonestReset {
		t.Fatalf("state = %+v, want fill-first in soonest-reset order", state)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
	}

	plain := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "fill-first"},
	})
	if _, ok := newRoutingSelector(plain).(*coreauth.FillFirstSelector); !ok {
		t.Fatalf("selector type without an order = %T, want *auth.FillFirstSelector", newRoutingSelector(plain))
	}

	roundRobin := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "round-robin", FillFirstOrder: "soonest-reset"},
	})
	if roundRobin.fillFirstSoonestReset {
		t.Fatal("fill-first-order must be ignored by other strategies")
	}

	withAffinity := soonestResetConfig()
	withAffinity.Routing.SessionAffinity = true
	selector := newRoutingSelector(normalizedRoutingRuntimeState(withAffinity))
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type with session affinity = %T, want *auth.SessionAffinitySelector", selector)
	}
	affinity.Stop()
}

func TestForkQuotaRefreshInterval(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		mutate      func(*internalconfig.Config)
		wantEnabled bool
		want        time.Duration
	}{
		{name: "default", mutate: func(*internalconfig.Config) {}, wantEnabled: true, want: defaultQuotaRefreshInterval},
		{name: "explicit", mutate: func(cfg *internalconfig.Config) { cfg.Routing.QuotaRefreshInterval = "45m" }, wantEnabled: true, want: 45 * time.Minute},
		{name: "clamped to minimum", mutate: func(cfg *internalconfig.Config) { cfg.Routing.QuotaRefreshInterval = "5s" }, wantEnabled: true, want: minQuotaRefreshInterval},
		{name: "unparseable falls back", mutate: func(cfg *internalconfig.Config) { cfg.Routing.QuotaRefreshInterval = "soon" }, wantEnabled: true, want: defaultQuotaRefreshInterval},
		{name: "zero disables", mutate: func(cfg *internalconfig.Config) { cfg.Routing.QuotaRefreshInterval = "0" }},
		{name: "off disables", mutate: func(cfg *internalconfig.Config) { cfg.Routing.QuotaRefreshInterval = "off" }},
		{name: "other order disables", mutate: func(cfg *internalconfig.Config) { cfg.Routing.FillFirstOrder = "id" }},
		{name: "other strategy disables", mutate: func(cfg *internalconfig.Config) { cfg.Routing.Strategy = "round-robin" }},
		{name: "home mode disables", mutate: func(cfg *internalconfig.Config) { cfg.Home.Enabled = true }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := soonestResetConfig()
			testCase.mutate(cfg)
			got, enabled := quotaRefreshInterval(cfg)
			if enabled != testCase.wantEnabled || got != testCase.want {
				t.Fatalf("quotaRefreshInterval() = %s, %t; want %s, %t", got, enabled, testCase.want, testCase.wantEnabled)
			}
		})
	}
	if _, enabled := quotaRefreshInterval(nil); enabled {
		t.Fatal("nil config must disable the refresher")
	}
}

func TestForkClaudeUsageQuotaHeaders(t *testing.T) {
	headers := claudeUsageQuotaHeaders([]byte(`{
		"five_hour": {"utilization": 23.0, "resets_at": "2026-10-03T14:00:00.500000+00:00"},
		"seven_day": {"utilization": 58, "resets_at": "2026-10-09T13:00:00Z"},
		"seven_day_opus": null,
		"iguana_necktie": {"utilization": 100, "resets_at": "2026-10-09T13:00:00Z"},
		"extra_usage": {"is_enabled": false}
	}`))

	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.58",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC).Unix(), 10),
	}
	if len(headers) != len(want) {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestForkClaudeUsageQuotaHeadersWindowNotStarted(t *testing.T) {
	headers := claudeUsageQuotaHeaders([]byte(`{"five_hour": {"utilization": 0, "resets_at": null}, "seven_day": null}`))
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "0" {
		t.Fatalf("5h utilization = %q, want 0", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Reset"); got != "" {
		t.Fatalf("5h reset = %q, want none for a window that has not started", got)
	}
	if len(headers) != 1 {
		t.Fatalf("headers = %v, want only the 5h utilization", headers)
	}
}

func TestForkCodexUsageQuotaHeaders(t *testing.T) {
	headers := codexUsageQuotaHeaders([]byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 17, "limit_window_seconds": 604800, "reset_after_seconds": 172800, "reset_at": 1790172800},
			"secondary_window": null
		},
		"credits": {"balance": "0"}
	}`))

	want := map[string]string{
		"X-Codex-Primary-Used-Percent":        "17",
		"X-Codex-Primary-Window-Minutes":      "10080",
		"X-Codex-Primary-Reset-At":            "1790172800",
		"X-Codex-Primary-Reset-After-Seconds": "172800",
		"X-Codex-Plan-Type":                   "pro",
	}
	if len(headers) != len(want) {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}

	if empty := codexUsageQuotaHeaders([]byte(`{"plan_type": "pro"}`)); len(empty) != 0 {
		t.Fatalf("payload without windows produced headers: %v", empty)
	}
}

type usageEndpoint struct {
	mu       sync.Mutex
	requests map[string]int
	fail     map[string]bool
}

func (e *usageEndpoint) handler(body func(token string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		e.mu.Lock()
		e.requests[token]++
		fail := e.fail[token]
		e.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body(token)))
	}
}

func (e *usageEndpoint) count(token string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests["Bearer "+token]
}

func TestForkRefreshIdleQuotaSignalsPollsStaleCredentials(t *testing.T) {
	now := time.Now()
	soonReset := now.Add(24 * time.Hour).Truncate(time.Second)
	laterReset := now.Add(6 * 24 * time.Hour).Truncate(time.Second)

	endpoint := &usageEndpoint{requests: map[string]int{}, fail: map[string]bool{"Bearer token-failing": true}}
	var claudeHeaders http.Header
	var headersMu sync.Mutex
	claudeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersMu.Lock()
		claudeHeaders = r.Header.Clone()
		headersMu.Unlock()
		endpoint.handler(func(token string) string {
			reset := laterReset
			if token == "Bearer token-soon" {
				reset = soonReset
			}
			return `{"five_hour": null, "seven_day": {"utilization": 40, "resets_at": "` + reset.UTC().Format(time.RFC3339) + `"}}`
		})(w, r)
	}))
	defer claudeServer.Close()
	codexServer := httptest.NewServer(endpoint.handler(func(string) string {
		return `{"rate_limit": {"primary_window": {"used_percent": 5, "limit_window_seconds": 604800, "reset_at": ` + strconv.FormatInt(soonReset.Unix(), 10) + `}}}`
	}))
	defer codexServer.Close()

	originalClaudeURL, originalCodexURL := claudeUsageURL, codexUsageURL
	claudeUsageURL, codexUsageURL = claudeServer.URL, codexServer.URL
	t.Cleanup(func() { claudeUsageURL, codexUsageURL = originalClaudeURL, originalCodexURL })

	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	register := func(auth *coreauth.Auth) {
		t.Helper()
		auth.Status = coreauth.StatusActive
		if auth.Attributes == nil {
			auth.Attributes = map[string]string{}
		}
		if _, set := auth.Attributes[coreauth.AttributeAuthKind]; !set {
			auth.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
		}
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	register(&coreauth.Auth{ID: "claude-later", Provider: "claude", Metadata: map[string]any{"access_token": "token-later"}})
	register(&coreauth.Auth{ID: "claude-soon", Provider: "claude", Metadata: map[string]any{"access_token": "token-soon"}})
	register(&coreauth.Auth{ID: "claude-failing", Provider: "claude", Metadata: map[string]any{"access_token": "token-failing"}})
	register(&coreauth.Auth{ID: "claude-disabled", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "token-disabled"}})
	register(&coreauth.Auth{ID: "claude-api-key", Provider: "claude", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey, coreauth.AttributeAPIKey: "sk-test"}})
	// A provider with a single credential has nothing to order, so it is not polled.
	register(&coreauth.Auth{ID: "codex-only", Provider: "codex", Metadata: map[string]any{"access_token": "token-codex", "account_id": "acct-1"}})

	service := &Service{cfg: soonestResetConfig(), coreManager: manager}
	nextAttempt := make(map[string]time.Time)

	service.refreshIdleQuotaSignals(ctx, nextAttempt, now)

	for token, want := range map[string]int{"token-later": 1, "token-soon": 1, "token-failing": 1, "token-disabled": 0, "token-codex": 0} {
		if got := endpoint.count(token); got != want {
			t.Fatalf("requests for %s = %d, want %d", token, got, want)
		}
	}
	headersMu.Lock()
	gotBeta, gotUserAgent := claudeHeaders.Get("Anthropic-Beta"), claudeHeaders.Get("User-Agent")
	headersMu.Unlock()
	if gotBeta != claudeProbeOAuthBeta || gotUserAgent != claudeProbeUserAgent {
		t.Fatalf("claude probe headers: beta=%q user-agent=%q", gotBeta, gotUserAgent)
	}

	soon, _ := manager.GetByID("claude-soon")
	windows := coreauth.QuotaWindows("claude", soon.Quota)
	if len(windows) != 1 || !windows[0].ResetAt.Equal(soonReset) || windows[0].Used != 0.4 {
		t.Fatalf("claude-soon windows = %+v, want one 7d window resetting at %s", windows, soonReset)
	}
	failing, _ := manager.GetByID("claude-failing")
	if len(failing.Quota.Signals) != 0 {
		t.Fatalf("failed poll recorded signals: %v", failing.Quota.Signals)
	}

	// Fresh snapshots are not polled again; the failed credential waits for its backoff.
	service.refreshIdleQuotaSignals(ctx, nextAttempt, now.Add(time.Minute))
	for token, want := range map[string]int{"token-later": 1, "token-soon": 1, "token-failing": 1} {
		if got := endpoint.count(token); got != want {
			t.Fatalf("requests for %s after a fresh snapshot = %d, want %d", token, got, want)
		}
	}

	service.refreshIdleQuotaSignals(ctx, nextAttempt, now.Add(quotaRefreshRetryBackoff+time.Second))
	if got := endpoint.count("token-failing"); got != 2 {
		t.Fatalf("requests for the failed credential after its backoff = %d, want 2", got)
	}
	if got := endpoint.count("token-soon"); got != 1 {
		t.Fatalf("requests for a fresh credential after the failure backoff = %d, want 1", got)
	}

	// Once the snapshot is older than the interval the credential is polled again.
	service.refreshIdleQuotaSignals(ctx, nextAttempt, time.Now().Add(defaultQuotaRefreshInterval+time.Minute))
	if got := endpoint.count("token-soon"); got != 2 {
		t.Fatalf("requests for a stale credential = %d, want 2", got)
	}

	// A second Codex credential makes the provider worth ordering.
	register(&coreauth.Auth{ID: "codex-second", Provider: "codex", Metadata: map[string]any{"access_token": "token-codex-2"}})
	service.refreshIdleQuotaSignals(ctx, nextAttempt, time.Now())
	if endpoint.count("token-codex") != 1 || endpoint.count("token-codex-2") != 1 {
		t.Fatalf("codex requests = %d and %d, want 1 and 1", endpoint.count("token-codex"), endpoint.count("token-codex-2"))
	}
	codex, _ := manager.GetByID("codex-only")
	if got := codex.Quota.Signals["X-Codex-Primary-Window-Minutes"]; got != "10080" {
		t.Fatalf("codex window minutes = %q, want 10080", got)
	}
}

func TestForkRefreshIdleQuotaSignalsDisabledWithoutSoonestReset(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	originalClaudeURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = originalClaudeURL })

	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"claude-a", "claude-b"} {
		auth := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
			Metadata:   map[string]any{"access_token": "token-" + id},
		}
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
	}
	service := &Service{
		cfg:         &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "fill-first"}},
		coreManager: manager,
	}

	service.refreshIdleQuotaSignals(ctx, map[string]time.Time{}, time.Now())

	if requests != 0 {
		t.Fatalf("usage endpoint requests = %d, want 0 when soonest-reset ordering is off", requests)
	}
}
