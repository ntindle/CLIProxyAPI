package cliproxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	defaultQuotaRefreshInterval = 30 * time.Minute
	minQuotaRefreshInterval     = time.Minute
	// quotaRefreshTick is how often idle credentials are checked for a stale snapshot.
	quotaRefreshTick = time.Minute
	// quotaRefreshStartDelay lets the first auth scan and token refresh finish.
	quotaRefreshStartDelay = 10 * time.Second
	// quotaRefreshRetryBackoff is the wait before a failed refresh is retried.
	quotaRefreshRetryBackoff = 5 * time.Minute
)

// Provider usage endpoints. Variables so tests can point them at a local server.
var (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

// fillFirstSoonestReset reports whether routing.fill-first-order selects the
// soonest-reset order.
func fillFirstSoonestReset(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Routing.FillFirstOrder)) {
	case "soonest-reset", "soonestreset", "soonest_reset":
		return true
	default:
		return false
	}
}

// quotaRefreshInterval resolves routing.quota-refresh-interval. The refresher
// only runs when credentials are ordered by their quota reset time, because
// nothing else reads the snapshot of a credential that is not in use.
func quotaRefreshInterval(cfg *config.Config) (time.Duration, bool) {
	if cfg == nil || cfg.Home.Enabled || !normalizedRoutingRuntimeState(cfg).fillFirstSoonestReset {
		return 0, false
	}
	raw := strings.ToLower(strings.TrimSpace(cfg.Routing.QuotaRefreshInterval))
	switch raw {
	case "":
		return defaultQuotaRefreshInterval, true
	case "0", "0s", "off", "false", "none", "disabled", "never":
		return 0, false
	}
	interval, errParse := time.ParseDuration(raw)
	if errParse != nil || interval <= 0 {
		return defaultQuotaRefreshInterval, true
	}
	if interval < minQuotaRefreshInterval {
		interval = minQuotaRefreshInterval
	}
	return interval, true
}

// startQuotaRefresher keeps the quota windows of idle Claude and Codex OAuth
// credentials current. A credential in use reports its windows on every
// response; one that is not would otherwise have unknown reset times, and
// soonest-reset ordering could never prefer it.
func (s *Service) startQuotaRefresher(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		nextAttempt := make(map[string]time.Time)
		timer := time.NewTimer(quotaRefreshStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.refreshIdleQuotaSignals(ctx, nextAttempt, time.Now())
			timer.Reset(quotaRefreshTick)
		}
	}()
}

// refreshIdleQuotaSignals polls the usage endpoint of every eligible credential
// whose snapshot is older than the configured interval. nextAttempt carries the
// earliest time each credential may be polled again.
func (s *Service) refreshIdleQuotaSignals(ctx context.Context, nextAttempt map[string]time.Time, now time.Time) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	interval, enabled := quotaRefreshInterval(cfg)
	if !enabled {
		return
	}

	auths := s.coreManager.List()
	perProvider := make(map[string]int, 2)
	known := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if provider, ok := oauthProbeProvider(auth); ok {
			perProvider[provider]++
			known[auth.ID] = struct{}{}
		}
	}
	for id := range nextAttempt {
		if _, ok := known[id]; !ok {
			delete(nextAttempt, id)
		}
	}

	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		provider, ok := oauthProbeProvider(auth)
		// A provider with a single credential has nothing to order.
		if !ok || perProvider[provider] < 2 {
			continue
		}
		if !auth.Quota.ObservedAt.IsZero() && now.Sub(auth.Quota.ObservedAt) < interval {
			continue
		}
		if next, pending := nextAttempt[auth.ID]; pending && now.Before(next) {
			continue
		}
		headers, errFetch := s.fetchQuotaSignals(ctx, cfg, provider, auth)
		if errFetch != nil {
			nextAttempt[auth.ID] = now.Add(quotaRefreshRetryBackoff)
			log.Debugf("quota refresh: %s credential %s: %v", provider, auth.ID, errFetch)
			continue
		}
		nextAttempt[auth.ID] = now.Add(interval)
		if s.coreManager.ObserveQuotaSignals(auth.ID, provider, headers, time.Now()) {
			log.Debugf("quota refresh: %s credential %s updated from the usage endpoint", provider, auth.ID)
		}
	}
}

// fetchQuotaSignals reads the provider usage endpoint for one credential and
// returns its windows as the response headers a proxied request would carry.
func (s *Service) fetchQuotaSignals(ctx context.Context, cfg *config.Config, provider string, auth *coreauth.Auth) (http.Header, error) {
	switch provider {
	case "claude":
		body, errGet := s.oauthProbeGET(ctx, cfg, provider, auth, claudeUsageURL)
		if errGet != nil {
			return nil, errGet
		}
		return claudeUsageQuotaHeaders(body), nil
	case "codex":
		body, errGet := s.oauthProbeGET(ctx, cfg, provider, auth, codexUsageURL)
		if errGet != nil {
			return nil, errGet
		}
		return codexUsageQuotaHeaders(body), nil
	default:
		return nil, nil
	}
}

// claudeUsageQuotaHeaders converts the Claude OAuth usage payload into the
// anthropic-ratelimit-unified-* headers that Messages responses carry. The
// usage endpoint reports utilization as a percentage and resets as RFC 3339;
// the headers use a fraction and unix seconds.
func claudeUsageQuotaHeaders(body []byte) http.Header {
	headers := make(http.Header)
	root := gjson.ParseBytes(body)
	for _, window := range []struct{ key, label string }{
		{"five_hour", "5h"},
		{"seven_day", "7d"},
	} {
		node := root.Get(window.key)
		if !node.IsObject() {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + window.label + "-"
		if utilization := node.Get("utilization"); utilization.Type == gjson.Number && utilization.Float() >= 0 {
			headers.Set(prefix+"Utilization", strconv.FormatFloat(utilization.Float()/100, 'f', -1, 64))
		}
		if resetsAt := strings.TrimSpace(node.Get("resets_at").String()); resetsAt != "" {
			if parsed, errParse := time.Parse(time.RFC3339Nano, resetsAt); errParse == nil {
				headers.Set(prefix+"Reset", strconv.FormatInt(parsed.Unix(), 10))
			}
		}
	}
	return headers
}

// codexUsageQuotaHeaders converts the Codex usage payload into the x-codex-*
// headers that Responses requests carry.
func codexUsageQuotaHeaders(body []byte) http.Header {
	headers := make(http.Header)
	root := gjson.ParseBytes(body)
	rateLimit := root.Get("rate_limit")
	if !rateLimit.IsObject() {
		rateLimit = root.Get("rateLimit")
	}
	for _, window := range []struct {
		label string
		keys  []string
	}{
		{"Primary", []string{"primary_window", "primaryWindow"}},
		{"Secondary", []string{"secondary_window", "secondaryWindow"}},
	} {
		node := firstGJSON(rateLimit, window.keys...)
		if !node.IsObject() {
			continue
		}
		prefix := "X-Codex-" + window.label + "-"
		if used := firstGJSON(node, "used_percent", "usedPercent"); used.Type == gjson.Number && used.Float() >= 0 {
			headers.Set(prefix+"Used-Percent", strconv.FormatFloat(used.Float(), 'f', -1, 64))
		}
		if seconds := firstGJSON(node, "limit_window_seconds", "limitWindowSeconds"); seconds.Type == gjson.Number && seconds.Float() > 0 {
			headers.Set(prefix+"Window-Minutes", strconv.FormatInt(int64(seconds.Float()/60), 10))
		}
		if resetAt := firstGJSON(node, "reset_at", "resetAt"); resetAt.Type == gjson.Number && resetAt.Int() > 0 {
			headers.Set(prefix+"Reset-At", strconv.FormatInt(resetAt.Int(), 10))
		}
		if resetAfter := firstGJSON(node, "reset_after_seconds", "resetAfterSeconds"); resetAfter.Type == gjson.Number && resetAfter.Int() >= 0 {
			headers.Set(prefix+"Reset-After-Seconds", strconv.FormatInt(resetAfter.Int(), 10))
		}
	}
	if len(headers) == 0 {
		return headers
	}
	if planType := strings.TrimSpace(firstGJSON(root, "plan_type", "planType").String()); planType != "" && len(planType) <= 64 {
		headers.Set("X-Codex-Plan-Type", planType)
	}
	return headers
}

func firstGJSON(node gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if value := node.Get(key); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}
