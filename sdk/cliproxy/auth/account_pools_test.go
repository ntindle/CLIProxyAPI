package auth

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func poolTestConfig() config.ClientConfig {
	reviewer, personal := []string{"reviewer", "shared"}, []string{"personal", "shared"}
	return config.ClientConfig{
		AccountPools: &[]config.ClientAccountPool{
			{Name: "reviewer", AuthIDs: []string{"reviewer"}},
			{Name: "shared", AuthIDs: []string{"shared"}},
			{Name: "personal", AuthIDs: []string{"personal"}},
		},
		KeyScopes: &[]config.ClientKeyScope{
			{KeyPrefix: "review-", Providers: []string{"claude"}, Pools: &reviewer},
			{KeyPrefix: "personal-", Providers: []string{"claude"}, Pools: &personal},
		},
	}
}

func poolTestContext(t *testing.T, cfg config.ClientConfig, key string) context.Context {
	t.Helper()
	policy, err := ResolveAccountPools(cfg, key)
	if err != nil {
		t.Fatal(err)
	}
	return WithAccountPools(context.Background(), policy)
}

func poolTestManager(t *testing.T, selector Selector) (*Manager, *authFallbackExecutor, string) {
	t.Helper()
	m := NewManager(nil, selector, nil)
	m.SetRetryConfig(0, 0, 0)
	executor := &authFallbackExecutor{id: "claude", executeErrors: make(map[string]error)}
	m.RegisterExecutor(executor)
	model := "pool-" + t.Name()
	for _, id := range []string{"reviewer", "personal", "shared"} {
		priority := "0"
		if id == "personal" {
			priority = "1000"
		}
		if id == "shared" {
			priority = "100"
		}
		a := &Auth{ID: id, Provider: "claude", Attributes: map[string]string{"priority": priority}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, executor, model
}

func TestForkAccountPoolsMembershipAndFailClosed(t *testing.T) {
	cfg := poolTestConfig()
	p, err := ResolveAccountPools(cfg, "review-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, provider string
		want         bool
	}{
		{"reviewer", "claude", true}, {"shared", "claude", true}, {"personal", "claude", false}, {"reviewer", "codex", false},
	} {
		if got := p.Allows(&Auth{ID: tc.id, Provider: tc.provider}); got != tc.want {
			t.Fatalf("membership %v = %t", tc, got)
		}
	}
	unknown := []string{"missing"}
	(*cfg.KeyScopes)[0].Pools = &unknown
	if _, err = ResolveAccountPools(cfg, "review-key"); err == nil {
		t.Fatal("unknown pool must fail")
	}
	empty := []string{}
	(*cfg.KeyScopes)[0].Pools = &empty
	p, err = ResolveAccountPools(cfg, "review-key")
	if err != nil || p == nil || p.Allows(&Auth{ID: "reviewer", Provider: "claude"}) {
		t.Fatal("empty chain must deny")
	}
	(*cfg.KeyScopes)[0].Pools = nil
	if p, err = ResolveAccountPools(cfg, "review-key"); err != nil || p != nil {
		t.Fatal("omitted pools must preserve routing")
	}
}

func TestForkAccountPoolsDedicatedThenSharedAndNoPersonalFallback(t *testing.T) {
	m, e, model := poolTestManager(t, &FillFirstSelector{})
	ctx := poolTestContext(t, poolTestConfig(), "review-key")
	req := cliproxyexecutor.Request{Model: model}
	resp, err := m.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "reviewer" {
		t.Fatalf("first choice: %s %v", resp.Payload, err)
	}
	e.executeCalls = nil
	e.executeErrors["reviewer"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exceeded"}
	resp, err = m.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "shared" || !slices.Equal(e.executeCalls, []string{"reviewer", "shared"}) {
		t.Fatalf("fallback: %s %v calls=%v", resp.Payload, err, e.executeCalls)
	}
	e.executeCalls = nil
	e.executeErrors["shared"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exceeded"}
	_, err = m.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err == nil || slices.Contains(e.executeCalls, "personal") {
		t.Fatalf("escaped pool: err=%v calls=%v", err, e.executeCalls)
	}
	personal := poolTestContext(t, poolTestConfig(), "personal-key")
	resp, err = m.Execute(personal, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "personal" {
		t.Fatalf("personal route: %s %v", resp.Payload, err)
	}
}

func TestForkAccountPoolsPinnedAuthAndModelAvailability(t *testing.T) {
	m, _, model := poolTestManager(t, &RoundRobinSelector{})
	ctx := poolTestContext(t, poolTestConfig(), "review-key")
	_, err := m.SelectAuth(ctx, "claude", model, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "personal"}})
	if err == nil {
		t.Fatal("pinned personal credential escaped pool")
	}
	registry.GetGlobalRegistry().UnregisterClient("reviewer")
	a, err := m.SelectAuth(ctx, "claude", model, cliproxyexecutor.Options{})
	if err != nil || a.ID != "shared" {
		t.Fatalf("model fallback: %v %v", a, err)
	}
}

func TestForkAccountPoolsAffinityIsolationAndRevocation(t *testing.T) {
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	t.Cleanup(selector.Stop)
	m, _, model := poolTestManager(t, selector)
	cfg := poolTestConfig()
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"identical-session"}}}
	for _, tc := range []struct{ key, want string }{{"review-key", "reviewer"}, {"personal-key", "personal"}} {
		a, err := m.SelectAuth(poolTestContext(t, cfg, tc.key), "claude", model, opts)
		if err != nil || a.ID != tc.want {
			t.Fatalf("affinity %s: %v %v", tc.key, a, err)
		}
	}
	(*cfg.AccountPools)[0].AuthIDs = nil
	a, err := m.SelectAuth(poolTestContext(t, cfg, "review-key"), "claude", model, opts)
	if err != nil || a.ID != "shared" {
		t.Fatalf("revocation: %v %v", a, err)
	}
}

func TestForkAccountPoolsSharedCooldownAndRecovery(t *testing.T) {
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	t.Cleanup(selector.Stop)
	m, _, model := poolTestManager(t, selector)
	cfg := poolTestConfig()
	for _, id := range []string{"reviewer", "personal"} {
		a, _ := m.GetByID(id)
		a.Unavailable = true
		a.NextRetryAfter = time.Now().Add(time.Hour)
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"review-key", "personal-key"} {
		a, err := m.SelectAuth(poolTestContext(t, cfg, key), "claude", model, cliproxyexecutor.Options{})
		if err != nil || a.ID != "shared" {
			t.Fatalf("shared %s: %v %v", key, a, err)
		}
	}
	a, _ := m.GetByID("shared")
	a.Unavailable = true
	a.NextRetryAfter = time.Now().Add(time.Hour)
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"review-key", "personal-key"} {
		if _, err := m.SelectAuth(poolTestContext(t, cfg, key), "claude", model, cliproxyexecutor.Options{}); err == nil {
			t.Fatal("shared cooldown was not shared")
		}
	}
}

func TestForkAccountPoolsStreamAndCount(t *testing.T) {
	m, e, model := poolTestManager(t, &FillFirstSelector{})
	ctx := poolTestContext(t, poolTestConfig(), "review-key")
	req := cliproxyexecutor.Request{Model: model}
	resp, err := m.ExecuteCount(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "reviewer" {
		t.Fatalf("count: %s %v", resp.Payload, err)
	}
	e.streamFirstErrors = map[string]error{"reviewer": &Error{HTTPStatus: 429, Message: "quota exceeded"}}
	stream, err := m.ExecuteStream(ctx, []string{"claude"}, req, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		payload += string(chunk.Payload)
	}
	if payload != "shared" || slices.Contains(e.streamCalls, "personal") {
		t.Fatalf("stream: %s calls=%v", payload, e.streamCalls)
	}
}

func TestForkAccountPoolsZeroWeightFallsBack(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		var selector Selector = &WeightedRoundRobinSelector{}
		if affinity {
			s := NewSessionAffinitySelector(selector)
			t.Cleanup(s.Stop)
			selector = s
		}
		m, _, model := poolTestManager(t, selector)
		a, _ := m.GetByID("reviewer")
		a.Attributes["weight"] = "0"
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		selected, err := m.SelectAuth(poolTestContext(t, poolTestConfig(), "review-key"), "claude", model, cliproxyexecutor.Options{})
		if err != nil || selected.ID != "shared" {
			t.Fatalf("zero weight fallback: %v %v", selected, err)
		}
	}
}

func TestForkAccountPoolsHomeFailsClosed(t *testing.T) {
	m := NewManager(nil, nil, nil)
	if _, err := m.pickHomeDispatchSelection(poolTestContext(t, poolTestConfig(), "review-key"), "model", cliproxyexecutor.Options{}); err == nil {
		t.Fatal("Home must reject a pool policy before dispatch")
	}
}

func TestForkAccountPoolsFileMembershipAndBillingKind(t *testing.T) {
	cfg := poolTestConfig()
	(*cfg.AccountPools)[0] = config.ClientAccountPool{Name: "reviewer", AuthFiles: []string{"reviewer.json"}, AuthKind: AuthKindOAuth}
	p, err := ResolveAccountPools(cfg, "review-key")
	if err != nil {
		t.Fatal(err)
	}
	a := &Auth{ID: "stable-id", Provider: "claude", FileName: "/auths/reviewer.json", Metadata: map[string]any{"access_token": "test-oauth-token"}}
	if !p.Allows(a) {
		t.Fatal("OAuth file membership missing")
	}
	a.Attributes = map[string]string{AttributeAPIKey: "test-api-key"}
	if p.Allows(a) {
		t.Fatal("API credential crossed OAuth-only pool")
	}
	(*cfg.AccountPools)[0].AuthFiles[0] = "changed.json"
	a.Attributes = nil
	if !p.Allows(a) {
		t.Fatal("policy snapshot changed under caller")
	}
}

// A pool must evaluate the request's credential track before choosing its tier.
// A valid outer Codex token remains usable when the nested SIWC token expired.
func TestForkAccountPoolsPreserveRequestCredentialTrack(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "reviewer", Provider: "codex", Status: StatusActive, Metadata: map[string]any{
		"type": "codex", "access_token": "synthetic-outer", "expired": now.Add(time.Hour).Format(time.RFC3339), "use_siwc": true,
		"siwc": map[string]any{"access_token": "synthetic-inner", "expired": now.Add(-time.Hour).Format(time.RFC3339)},
	}}
	cfg := poolTestConfig()
	(*cfg.KeyScopes)[0].Providers = []string{"codex"}
	pooled := poolTestContext(t, cfg, "review-key")
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		headers http.Header
		want    bool
	}{
		{"ordinary", pooled, nil, false},
		{"header", pooled, http.Header{"User-Agent": []string{"codex_cli_rs/0.166.0"}}, true},
		{"context", context.WithValue(pooled, codexClientRequestContextKey{}, true), nil, true},
		{"unpooled-header", context.Background(), http.Header{"User-Agent": []string{"codex_cli_rs/0.166.0"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			got, _, err := m.availableAuthsForPoolSelector(tc.ctx, &FillFirstSelector{}, []*Auth{a}, "codex", "gpt-5.4", now, false, tc.headers)
			if tc.want {
				if err != nil || len(got) != 1 || got[0].ID != a.ID {
					t.Fatalf("valid outer track was not selected: %v", err)
				}
			} else if err == nil {
				t.Fatal("ordinary request accepted expired SIWC")
			}
		})
	}
}

// The upstream never-rebind path must still honor pool order and refuse a
// cross-credential retry even when the bound credential is unavailable or tried.
func TestForkAccountPoolsNeverRebindPreservesBoundCredential(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback: &FillFirstSelector{}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnNever,
			})
			t.Cleanup(selector.Stop)
			m, _, model := poolTestManager(t, selector)
			ctx := poolTestContext(t, poolTestConfig(), "review-key")
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"pool-never"}}}
			pick := func(tried map[string]struct{}) (*Auth, error) {
				if mixed {
					a, _, _, err := m.pickNextMixed(ctx, []string{"claude"}, model, opts, tried)
					return a, err
				}
				a, _, err := m.pickNext(ctx, "claude", model, opts, tried)
				return a, err
			}
			a, err := pick(nil)
			if err != nil || a == nil || a.ID != "reviewer" {
				t.Fatalf("initial pool binding = %v, %v", a, err)
			}
			m.MarkResult(ctx, Result{AuthID: "reviewer", Provider: "claude", Model: model, Success: true, Options: opts})
			m.MarkResult(ctx, Result{AuthID: "reviewer", Provider: "claude", Model: model, Success: false,
				Error: &Error{HTTPStatus: http.StatusBadGateway, Message: "502 overloaded"}, Options: opts})
			for _, tried := range []map[string]struct{}{nil, {"reviewer": {}}, {"reviewer": {}, "shared": {}, "personal": {}}} {
				a, err = pick(tried)
				if err == nil || a != nil {
					t.Fatalf("unavailable binding escaped: auth=%v err=%v", a, err)
				}
			}
		})
	}
}
