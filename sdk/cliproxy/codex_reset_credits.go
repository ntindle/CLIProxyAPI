package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ChatGPT grants accounts banked usage-limit resets ("Redeem reset" under Codex's
// /usage). Redeeming one restarts every Codex rate-limit window of the account at
// once. The proxy holds each Codex login's token, so it can spend them for its
// logins: when a login is blocked long enough, or a reset is about to expire.

const (
	codexResetCreditsStartDelay = 30 * time.Second
	codexResetCreditsTick       = time.Minute

	defaultCodexResetCreditsMinWait        = 6 * time.Hour
	defaultCodexResetCreditsExpiringWithin = 72 * time.Hour
	// codexResetCreditsExpiringMinWait is the shortest block an expiring reset is
	// spent on; a block about to end frees almost nothing.
	codexResetCreditsExpiringMinWait = 30 * time.Minute

	// How long a credential is left alone after a check, by outcome.
	codexResetCreditsErrorBackoff    = 5 * time.Minute
	codexResetCreditsWaitBackoff     = 15 * time.Minute
	codexResetCreditsNoCreditBackoff = time.Hour
	// codexResetCreditsRedeemGap keeps a credential from being redeemed twice for
	// one block while the provider catches up with the first reset.
	codexResetCreditsRedeemGap = 30 * time.Minute

	codexResetCreditType = "codex_rate_limits"
)

// Provider reset-credit endpoints. Variables so tests can point them at a local server.
var (
	codexResetCreditsURL        = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

// codexResetCreditRequestSpace derives each redemption's idempotency key from the
// credential and the credit, so a retry after a lost answer, even across a restart,
// is reported as already redeemed instead of spending a second credit.
var codexResetCreditRequestSpace = uuid.MustParse("6f2c64f4-5d0b-4f55-9a0c-2f1e2a3c7d61")

type codexResetCreditSettings struct {
	dryRun         bool
	minWait        time.Duration
	expiringWithin time.Duration
	only           map[string]struct{}
}

// codexResetCreditsSettings resolves oauth.providers.codex.reset-credits. It is
// disabled unless the mode is "on" or "dry-run".
func codexResetCreditsSettings(cfg *config.Config) (codexResetCreditSettings, bool) {
	if cfg == nil || cfg.Home.Enabled {
		return codexResetCreditSettings{}, false
	}
	section := cfg.Codex.ResetCredits
	settings := codexResetCreditSettings{
		minWait:        codexResetCreditsDuration(section.MinWait, defaultCodexResetCreditsMinWait),
		expiringWithin: codexResetCreditsDuration(section.ExpiringWithin, defaultCodexResetCreditsExpiringWithin),
	}
	switch strings.ToLower(strings.TrimSpace(section.Mode)) {
	case "on", "true", "enabled":
	case "dry-run", "dryrun", "dry_run":
		settings.dryRun = true
	default:
		return codexResetCreditSettings{}, false
	}
	for _, name := range section.AuthFiles {
		if name = strings.TrimSpace(name); name != "" {
			if settings.only == nil {
				settings.only = make(map[string]struct{}, len(section.AuthFiles))
			}
			settings.only[name] = struct{}{}
		}
	}
	return settings, true
}

// codexResetCreditsDuration parses a duration setting. Blank or unparseable
// values use the default; zero and negative values mean zero.
func codexResetCreditsDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "":
		return fallback
	case "0", "off", "false", "none", "disabled", "never":
		return 0
	}
	parsed, errParse := time.ParseDuration(raw)
	if errParse != nil {
		return fallback
	}
	if parsed < 0 {
		return 0
	}
	return parsed
}

func (s codexResetCreditSettings) covers(auth *coreauth.Auth) bool {
	if len(s.only) == 0 {
		return true
	}
	for _, name := range []string{auth.ID, auth.FileName, filepath.Base(auth.FileName)} {
		if _, ok := s.only[strings.TrimSpace(name)]; ok && name != "" {
			return true
		}
	}
	return false
}

// startCodexResetCredits checks blocked Codex logins every minute while
// oauth.providers.codex.reset-credits is on. The setting is read on every check,
// so it can be turned on or off without a restart.
func (s *Service) startCodexResetCredits(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		nextCheck := make(map[string]time.Time)
		timer := time.NewTimer(codexResetCreditsStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.checkCodexResetCredits(ctx, nextCheck, time.Now())
			timer.Reset(codexResetCreditsTick)
		}
	}()
}

// checkCodexResetCredits considers every blocked Codex OAuth login whose next
// check is due. A login that is not blocked costs no request. nextCheck carries
// the earliest time each credential may be considered again.
func (s *Service) checkCodexResetCredits(ctx context.Context, nextCheck map[string]time.Time, now time.Time) {
	if s == nil || s.coreManager == nil || nextCheck == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	settings, enabled := codexResetCreditsSettings(cfg)
	if !enabled {
		return
	}

	known := make(map[string]struct{})
	for _, auth := range s.coreManager.List() {
		if ctx.Err() != nil {
			return
		}
		if provider, ok := oauthProbeProvider(auth); !ok || provider != "codex" || !settings.covers(auth) {
			continue
		}
		known[auth.ID] = struct{}{}
		if next, pending := nextCheck[auth.ID]; pending && now.Before(next) {
			continue
		}
		blockedUntil, blocked := codexLocalBlock(auth, now)
		if !blocked {
			continue
		}
		nextCheck[auth.ID] = now.Add(s.considerCodexResetCredit(ctx, cfg, settings, auth, blockedUntil, now))
	}
	for id := range nextCheck {
		if _, ok := known[id]; !ok {
			delete(nextCheck, id)
		}
	}
}

// codexLocalBlock reports whether the proxy's own view of the credential says it
// is blocked: a quota cooldown from a refused request, or a fully used window in
// the latest rate-limit headers. A login whose plan allows spending ChatGPT credits
// past its limit keeps answering, so only the headers show it is past the limit.
func codexLocalBlock(auth *coreauth.Auth, now time.Time) (time.Time, bool) {
	var until time.Time
	blocked := false
	consider := func(quota coreauth.QuotaState) {
		if quota.Exceeded && quota.NextRecoverAt.After(now) {
			blocked = true
			if quota.NextRecoverAt.After(until) {
				until = quota.NextRecoverAt
			}
		}
	}
	consider(auth.Quota)
	for _, state := range auth.ModelStates {
		if state != nil {
			consider(state.Quota)
		}
	}
	for _, window := range coreauth.QuotaWindows("codex", auth.Quota) {
		if window.Used >= 1 && window.ResetAt.After(now) {
			blocked = true
			if window.ResetAt.After(until) {
				until = window.ResetAt
			}
		}
	}
	for key, value := range auth.Quota.Signals {
		if strings.EqualFold(strings.TrimSpace(key), "x-codex-limit-reached") && strings.EqualFold(strings.TrimSpace(value), "true") {
			blocked = true
		}
	}
	return until, blocked
}

// codexInCooldown reports whether the proxy holds the credential, or any of its
// models, in a cooldown.
func codexInCooldown(auth *coreauth.Auth, now time.Time) bool {
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return true
	}
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if (state.Unavailable && state.NextRetryAfter.After(now)) || (state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now)) {
			return true
		}
	}
	return false
}

// codexUsageState is what the usage endpoint says about a login right now.
type codexUsageState struct {
	limitReached    bool
	notAllowed      bool
	windows         []coreauth.QuotaWindow
	applicableKnown bool
	applicable      int64
}

func parseCodexUsageState(body []byte, now time.Time) codexUsageState {
	root := gjson.ParseBytes(body)
	rateLimit := firstGJSON(root, "rate_limit", "rateLimit")
	usage := codexUsageState{
		limitReached: firstGJSON(rateLimit, "limit_reached", "limitReached").Bool(),
	}
	if allowed := firstGJSON(rateLimit, "allowed"); allowed.Exists() && allowed.Type != gjson.Null {
		usage.notAllowed = !allowed.Bool()
	}
	signals := make(map[string]string)
	for name, values := range codexUsageQuotaHeaders(body) {
		if len(values) > 0 {
			signals[name] = values[0]
		}
	}
	usage.windows = coreauth.QuotaWindows("codex", coreauth.QuotaState{ObservedAt: now, Signals: signals})
	credits := firstGJSON(root, "rate_limit_reset_credits", "rateLimitResetCredits")
	if count := firstGJSON(credits, "applicable_available_count", "applicableAvailableCount"); count.Type == gjson.Number {
		usage.applicableKnown, usage.applicable = true, count.Int()
	} else if count = firstGJSON(credits, "available_count", "availableCount"); count.Type == gjson.Number {
		usage.applicableKnown, usage.applicable = true, count.Int()
	}
	return usage
}

// block returns when the login's exhausted windows reset and the length of the
// longest of them. A login refused for another reason reports no exhausted window;
// it falls back to the proxy's own cooldown.
func (u codexUsageState) block(fallback, now time.Time) (time.Time, time.Duration, bool) {
	var until time.Time
	var binding time.Duration
	exhausted := false
	for _, window := range u.windows {
		if window.Used >= 1 && window.ResetAt.After(now) {
			exhausted = true
			if window.ResetAt.After(until) {
				until = window.ResetAt
			}
			if window.Duration > binding {
				binding = window.Duration
			}
		}
	}
	if exhausted {
		return until, binding, true
	}
	if (u.limitReached || u.notAllowed) && fallback.After(now) {
		return fallback, 0, true
	}
	return time.Time{}, 0, false
}

// behindPace returns a window longer than the blocked one whose share used is less
// than the share of it that has passed. A reset restarts that window too, moving its
// reset a full window out, which would cost it more allowance than the reset frees.
func (u codexUsageState) behindPace(binding time.Duration, now time.Time) (coreauth.QuotaWindow, bool) {
	for _, window := range u.windows {
		if window.Duration <= binding || window.Duration <= 0 || window.Used < 0 || window.Used >= 1 {
			continue
		}
		remaining := window.ResetAt.Sub(now)
		if window.ResetAt.IsZero() || remaining <= 0 {
			continue
		}
		elapsed := 1 - float64(remaining)/float64(window.Duration)
		if window.Used < elapsed {
			return window, true
		}
	}
	return coreauth.QuotaWindow{}, false
}

type codexResetCredit struct {
	ID        string `json:"id"`
	ResetType string `json:"reset_type"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	expires   time.Time
}

// soonestCodexResetCredit returns the available Codex reset that expires first.
// A reset without an expiry sorts last.
func soonestCodexResetCredit(body []byte) (codexResetCredit, bool) {
	var payload struct {
		Credits []codexResetCredit `json:"credits"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return codexResetCredit{}, false
	}
	available := make([]codexResetCredit, 0, len(payload.Credits))
	for _, credit := range payload.Credits {
		if strings.TrimSpace(credit.ID) == "" || !strings.EqualFold(credit.Status, "available") ||
			!strings.EqualFold(credit.ResetType, codexResetCreditType) {
			continue
		}
		if parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(credit.ExpiresAt)); errParse == nil {
			credit.expires = parsed
		}
		available = append(available, credit)
	}
	if len(available) == 0 {
		return codexResetCredit{}, false
	}
	sort.SliceStable(available, func(i, j int) bool {
		a, b := available[i].expires, available[j].expires
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return a.Before(b)
	})
	return available[0], true
}

// considerCodexResetCredit decides about one blocked login, redeems a reset when
// it is worth it, and returns how long to leave the login alone.
func (s *Service) considerCodexResetCredit(ctx context.Context, cfg *config.Config, settings codexResetCreditSettings, auth *coreauth.Auth, localUntil time.Time, now time.Time) time.Duration {
	label := codexResetCreditLabel(auth)
	body, errUsage := s.oauthProbeGET(ctx, cfg, "codex", auth, codexUsageURL)
	if errUsage != nil {
		log.Debugf("codex reset credits: usage for %s: %v", label, errUsage)
		return codexResetCreditsErrorBackoff
	}
	s.coreManager.ObserveQuotaSignals(auth.ID, "codex", codexUsageQuotaHeaders(body), now)
	usage := parseCodexUsageState(body, now)
	until, binding, blocked := usage.block(localUntil, now)
	if !blocked {
		return codexResetCreditsWaitBackoff
	}
	if usage.applicableKnown && usage.applicable <= 0 {
		return codexResetCreditsNoCreditBackoff
	}
	wait := until.Sub(now)
	if wait < codexResetCreditsExpiringMinWait && wait < settings.minWait {
		return codexResetCreditsWaitBackoff
	}
	if window, behind := usage.behindPace(binding, now); behind {
		log.Debugf("codex reset credits: %s is blocked for %s, but a reset would push back its %s window (%.0f%% used, reset %s)",
			label, wait.Round(time.Minute), window.Duration, window.Used*100, window.ResetAt.Format(time.RFC3339))
		return codexResetCreditsWaitBackoff
	}

	listing, errList := s.codexResetCreditRequest(ctx, cfg, auth, http.MethodGet, codexResetCreditsURL, nil)
	if errList != nil {
		log.Debugf("codex reset credits: listing for %s: %v", label, errList)
		return codexResetCreditsErrorBackoff
	}
	credit, found := soonestCodexResetCredit(listing)
	if !found {
		return codexResetCreditsNoCreditBackoff
	}
	expiring := settings.expiringWithin > 0 && !credit.expires.IsZero() && credit.expires.Sub(now) <= settings.expiringWithin
	if wait < settings.minWait && !(expiring && wait >= codexResetCreditsExpiringMinWait) {
		return codexResetCreditsWaitBackoff
	}

	expiry := "no expiry"
	if !credit.expires.IsZero() {
		expiry = "expiring " + credit.expires.Format(time.RFC3339)
	}
	if settings.dryRun {
		log.Infof("codex reset credits: dry run: would redeem a reset (%s) for %s, blocked for %s", expiry, label, wait.Round(time.Minute))
		return codexResetCreditsWaitBackoff
	}

	requestID := uuid.NewSHA1(codexResetCreditRequestSpace, []byte(auth.ID+"\x00"+credit.ID)).String()
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": requestID, "credit_id": credit.ID})
	answer, errConsume := s.codexResetCreditRequest(ctx, cfg, auth, http.MethodPost, codexResetCreditsConsumeURL, payload)
	if errConsume != nil {
		log.Warnf("codex reset credits: redeeming a reset for %s: %v", label, errConsume)
		return codexResetCreditsErrorBackoff
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(answer, "code").String()))
	switch code {
	case "reset", "already_redeemed":
		// A login spending ChatGPT credits past its limit has no cooldown to clear.
		if current, ok := s.coreManager.GetByID(auth.ID); ok && codexInCooldown(current, time.Now()) {
			if _, _, errReset := s.coreManager.ResetQuota(ctx, auth.ID); errReset != nil {
				log.Warnf("codex reset credits: clearing the cooldown of %s: %v", label, errReset)
			}
		}
		if refreshed, errRefresh := s.oauthProbeGET(ctx, cfg, "codex", auth, codexUsageURL); errRefresh == nil {
			s.coreManager.ObserveQuotaSignals(auth.ID, "codex", codexUsageQuotaHeaders(refreshed), time.Now())
		}
		log.Infof("codex reset credits: redeemed a reset (%s) for %s, which was blocked for %s; %d window(s) reset",
			expiry, label, wait.Round(time.Minute), gjson.GetBytes(answer, "windows_reset").Int())
		return codexResetCreditsRedeemGap
	case "nothing_to_reset":
		log.Infof("codex reset credits: ChatGPT found nothing to reset for %s", label)
		return codexResetCreditsWaitBackoff
	case "no_credit":
		return codexResetCreditsNoCreditBackoff
	default:
		log.Warnf("codex reset credits: unexpected answer redeeming a reset for %s: %q", label, code)
		return codexResetCreditsErrorBackoff
	}
}

// codexResetCreditRequest sends one reset-credit request with the login's own
// token and headers, through its proxy settings.
func (s *Service) codexResetCreditRequest(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, method, endpoint string, body []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, oauthProbeTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(requestCtx, method, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	req.Header = oauthProbeHeaders(cfg, "codex", auth)
	client := helps.NewProxyAwareHTTPClient(requestCtx, cfg, auth, 0)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	return readProbeBody(resp)
}

func codexResetCreditLabel(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(filepath.Base(auth.FileName)); name != "" && name != "." {
		return name
	}
	return auth.ID
}
