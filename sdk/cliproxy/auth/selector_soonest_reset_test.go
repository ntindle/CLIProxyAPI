package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaSignals(used5h string, reset5h time.Time, used7d string, reset7d time.Time) map[string]string {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "allowed",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    "1.0",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":          strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10),
	}
	if used5h != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = used5h
	}
	if !reset5h.IsZero() {
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(reset5h.Unix(), 10)
	}
	if used7d != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = used7d
	}
	if !reset7d.IsZero() {
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(reset7d.Unix(), 10)
	}
	return signals
}

func claudeAuthWithQuota(id string, now time.Time, used5h string, reset5h time.Time, used7d string, reset7d time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		Quota: QuotaState{
			ObservedAt: now,
			Signals:    claudeQuotaSignals(used5h, reset5h, used7d, reset7d),
		},
	}
}

func TestForkQuotaWindowsClaudeKeepsCredentialWideWindowsOnly(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(3 * 24 * time.Hour)

	windows := QuotaWindows("claude", QuotaState{
		ObservedAt: now,
		Signals:    claudeQuotaSignals("0.25", reset5h, "0.58", reset7d),
	})

	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want the 5h and 7d windows only", windows)
	}
	short, long := windows[0], windows[1]
	if short.Name != "5h" || short.Duration != 5*time.Hour || short.Used != 0.25 || !short.ResetAt.Equal(reset5h) {
		t.Fatalf("short window = %+v", short)
	}
	if long.Name != "7d" || long.Duration != 7*24*time.Hour || long.Used != 0.58 || !long.ResetAt.Equal(reset7d) {
		t.Fatalf("long window = %+v", long)
	}
}

func TestForkQuotaWindowsClaudeRejectedStatusCountsAsFullyUsed(t *testing.T) {
	windows := QuotaWindows("claude", QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status":      "rejected",
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.97",
	}})
	if len(windows) != 1 || windows[0].Used != 1 {
		t.Fatalf("windows = %+v, want one fully used window", windows)
	}
}

func TestForkQuotaWindowsCodexUsesAbsoluteResetBeforeRelative(t *testing.T) {
	observedAt := time.Unix(1_790_000_000, 0)
	weeklyReset := observedAt.Add(48 * time.Hour)

	windows := QuotaWindows("codex", QuotaState{
		ObservedAt: observedAt,
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":           "17",
			"X-Codex-Primary-Window-Minutes":         "10080",
			"X-Codex-Primary-Reset-At":               strconv.FormatInt(weeklyReset.Unix(), 10),
			"X-Codex-Primary-Reset-After-Seconds":    "1",
			"X-Codex-Secondary-Used-Percent":         "100",
			"X-Codex-Secondary-Window-Minutes":       "300",
			"X-Codex-Secondary-Reset-After-Seconds":  "600",
			"X-Codex-Bengalfox-Primary-Used-Percent": "99",
			"X-Codex-Plan-Type":                      "pro",
		},
	})

	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want primary and secondary", windows)
	}
	short, long := windows[0], windows[1]
	if short.Name != "secondary" || short.Duration != 5*time.Hour || short.Used != 1 || !short.ResetAt.Equal(observedAt.Add(10*time.Minute)) {
		t.Fatalf("short window = %+v", short)
	}
	if long.Name != "primary" || long.Duration != 7*24*time.Hour || long.Used != 0.17 || !long.ResetAt.Equal(weeklyReset) {
		t.Fatalf("long window = %+v", long)
	}
}

func TestForkQuotaWindowsMetaReadsSubscriptionWindows(t *testing.T) {
	windowReset := time.Unix(1_791_465_557, 0)
	weeklyReset := time.Unix(1_791_763_200, 0)
	windows := QuotaWindows("meta", QuotaState{
		ObservedAt: time.Unix(1_791_447_762, 0),
		Signals: map[string]string{
			"X-Meta-Tier":                "tier-1",
			"X-Meta-Window-Used-Percent": "104",
			"X-Meta-Window-Minutes":      "300",
			"X-Meta-Window-Reset-At":     strconv.FormatInt(windowReset.Unix(), 10),
			"X-Meta-Weekly-Used-Percent": "5",
			"X-Meta-Weekly-Reset-At":     strconv.FormatInt(weeklyReset.Unix(), 10),
		},
	})
	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want window and weekly", windows)
	}
	short, long := windows[0], windows[1]
	if short.Name != "window" || short.Duration != 5*time.Hour || short.Used != 1.04 || !short.ResetAt.Equal(windowReset) {
		t.Fatalf("short window = %+v", short)
	}
	if long.Name != "weekly" || long.Duration != 7*24*time.Hour || long.Used != 0.05 || !long.ResetAt.Equal(weeklyReset) {
		t.Fatalf("long window = %+v", long)
	}
}

func TestForkQuotaWindowsUnknownProviderOrEmptySignals(t *testing.T) {
	if got := QuotaWindows("claude", QuotaState{}); got != nil {
		t.Fatalf("empty signals = %+v, want nil", got)
	}
	if got := QuotaWindows("gemini", QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "1"}}); got != nil {
		t.Fatalf("unsupported provider = %+v, want nil", got)
	}
}

func TestForkPickSoonestResetOrdering(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	hours := func(n int) time.Time { return now.Add(time.Duration(n) * time.Hour) }

	for _, testCase := range []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{
			name: "soonest weekly reset wins over credential id",
			auths: []*Auth{
				claudeAuthWithQuota("a-later", now, "0.10", hours(4), "0.10", hours(120)),
				claudeAuthWithQuota("b-sooner", now, "0.90", hours(1), "0.79", hours(24)),
				claudeAuthWithQuota("c-latest", now, "0.00", hours(5), "0.00", hours(160)),
			},
			want: "b-sooner",
		},
		{
			name: "unknown reset goes after a known one",
			auths: []*Auth{
				{ID: "a-unknown", Provider: "claude", Status: StatusActive},
				claudeAuthWithQuota("b-known", now, "0.10", hours(4), "0.10", hours(160)),
			},
			want: "b-known",
		},
		{
			name: "rolled over weekly window counts as unknown even with a running session window",
			auths: []*Auth{
				claudeAuthWithQuota("a-rolled-over", now, "0.50", hours(1), "0.95", hours(-1)),
				claudeAuthWithQuota("b-running", now, "0.10", hours(4), "0.10", hours(100)),
			},
			want: "b-running",
		},
		{
			name: "fully used window goes last",
			auths: []*Auth{
				claudeAuthWithQuota("a-drained", now, "0.20", hours(2), "1.0", hours(10)),
				claudeAuthWithQuota("b-usable", now, "0.20", hours(2), "0.40", hours(100)),
			},
			want: "b-usable",
		},
		{
			name: "among drained credentials the first to recover leads",
			auths: []*Auth{
				claudeAuthWithQuota("a-drained-later", now, "1.0", hours(4), "0.40", hours(20)),
				claudeAuthWithQuota("b-drained-sooner", now, "1.0", hours(1), "0.40", hours(90)),
			},
			want: "b-drained-sooner",
		},
		{
			name: "session window breaks a weekly tie",
			auths: []*Auth{
				claudeAuthWithQuota("a-session-later", now, "0.10", hours(4), "0.10", hours(50)),
				claudeAuthWithQuota("b-session-sooner", now, "0.10", hours(2), "0.10", hours(50)),
			},
			want: "b-session-sooner",
		},
		{
			name: "nothing known falls back to credential id",
			auths: []*Auth{
				{ID: "b", Provider: "claude", Status: StatusActive},
				{ID: "a", Provider: "claude", Status: StatusActive},
			},
			want: "a",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := pickSoonestReset(testCase.auths, now)
			if got == nil || got.ID != testCase.want {
				t.Fatalf("pickSoonestReset() = %v, want %q", got, testCase.want)
			}
		})
	}
}

func TestForkSoonestResetSelectorSkipsSchedulerFastPath(t *testing.T) {
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	if !isBuiltInSelector(manager.Selector()) {
		t.Fatal("soonest-reset selector must be treated as a built-in selector")
	}
	if manager.useSchedulerFastPath() {
		t.Fatal("soonest-reset selector must not use the ID-ordered scheduler fast path")
	}
	manager.SetSelector(&FillFirstSelector{})
	if !manager.useSchedulerFastPath() {
		t.Fatal("fill-first selector must keep the scheduler fast path")
	}
}

func TestForkManagerSoonestResetFollowsObservedQuota(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		selector func() Selector
	}{
		{name: "direct", selector: func() Selector { return &SoonestResetSelector{} }},
		{name: "behind session affinity", selector: func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}, TTL: time.Hour})
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			provider := "claude"
			model := "soonest-reset-model-" + testCase.name
			laterID := "soonest-reset-" + testCase.name + "-a-later"
			soonerID := "soonest-reset-" + testCase.name + "-b-sooner"

			selector := testCase.selector()
			if stoppable, ok := selector.(StoppableSelector); ok {
				defer stoppable.Stop()
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, id := range []string{laterID, soonerID} {
				auth := &Auth{ID: id, Provider: provider, Status: StatusActive}
				if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
					t.Fatalf("Register(%s): %v", id, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}

			session := 0
			pick := func() string {
				t.Helper()
				session++
				opts := cliproxyexecutor.Options{Metadata: map[string]any{
					cliproxyexecutor.DerivedSessionIDMetadataKey: "session-" + strconv.Itoa(session),
				}}
				auth, _, _, errPick := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
				if errPick != nil {
					t.Fatalf("pickNextMixed: %v", errPick)
				}
				if auth == nil {
					t.Fatal("pickNextMixed returned nil auth")
				}
				return auth.ID
			}

			if got := pick(); got != laterID {
				t.Fatalf("pick before any quota is known = %q, want lowest id %q", got, laterID)
			}

			now := time.Now()
			observe := func(id string, weeklyReset time.Time) {
				t.Helper()
				headers := make(http.Header)
				headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
				headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(weeklyReset.Unix(), 10))
				if !manager.ObserveQuotaSignals(id, provider, headers, now) {
					t.Fatalf("ObserveQuotaSignals(%s) = false, want true", id)
				}
			}
			observe(laterID, now.Add(6*24*time.Hour))
			observe(soonerID, now.Add(24*time.Hour))

			if got := pick(); got != soonerID {
				t.Fatalf("pick after quota observation = %q, want soonest reset %q", got, soonerID)
			}
		})
	}
}

func TestForkManagerObserveQuotaSignalsKeepsNewerSnapshot(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	authID := "observe-quota-signals-auth"
	if _, errRegister := manager.Register(WithSkipPersist(ctx), &Auth{ID: authID, Provider: "codex", Status: StatusActive}); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}

	newer := time.Unix(1_790_000_600, 0)
	older := newer.Add(-10 * time.Minute)
	headersFor := func(percent string) http.Header {
		headers := make(http.Header)
		headers.Set("X-Codex-Primary-Used-Percent", percent)
		return headers
	}

	if !manager.ObserveQuotaSignals(authID, "codex", headersFor("40"), newer) {
		t.Fatal("first observation was not recorded")
	}
	if manager.ObserveQuotaSignals(authID, "codex", headersFor("10"), older) {
		t.Fatal("an older snapshot replaced a newer one")
	}
	if manager.ObserveQuotaSignals("missing-auth", "codex", headersFor("10"), newer) {
		t.Fatal("observation for an unknown credential was recorded")
	}
	if manager.ObserveQuotaSignals(authID, "codex", http.Header{"X-Unrelated": []string{"1"}}, newer.Add(time.Minute)) {
		t.Fatal("headers without quota signals were recorded")
	}

	current, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth disappeared")
	}
	if got := current.Quota.Signals["X-Codex-Primary-Used-Percent"]; got != "40" || !current.Quota.ObservedAt.Equal(newer) {
		t.Fatalf("stored snapshot = %q at %s, want 40 at %s", got, current.Quota.ObservedAt, newer)
	}
}
