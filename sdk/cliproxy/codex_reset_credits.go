package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
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
	// codexResetCreditsAfterRedeem is the pause after a reset: ChatGPT's usage shows a
	// reset within seconds, and the next check lifts a cooldown that a refusal sent
	// before the reset brought back.
	codexResetCreditsAfterRedeem = 10 * time.Minute
	// codexResetCreditsAttemptWindow is how long after asking for a reset a free
	// login's quota cooldowns are lifted at every level: they predate the reset.
	codexResetCreditsAttemptWindow = 30 * time.Minute
	// codexResetCreditsLiftGap is how long after lifting a cooldown a login refused
	// again keeps its new one: ChatGPT's usage and its answers disagree, and lifting
	// again would only bring more refusals.
	codexResetCreditsLiftGap = 2 * time.Hour

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

// codexResetCreditState is what the check remembers about one login between sweeps.
type codexResetCreditState struct {
	// nextCheck is the earliest time the login may be considered again.
	nextCheck time.Time
	// attemptedAt is the last time a reset was asked for, answered or not.
	attemptedAt time.Time
	// liftedAt is the last time a cooldown was lifted because ChatGPT reported the
	// login free.
	liftedAt time.Time
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
		states := make(map[string]*codexResetCreditState)
		timer := time.NewTimer(codexResetCreditsStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.checkCodexResetCredits(ctx, states, time.Now())
			timer.Reset(codexResetCreditsTick)
		}
	}()
}

// checkCodexResetCredits considers every blocked Codex OAuth login whose next
// check is due. A login that is not blocked costs no request.
func (s *Service) checkCodexResetCredits(ctx context.Context, states map[string]*codexResetCreditState, now time.Time) {
	if s == nil || s.coreManager == nil || states == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	settings, enabled := codexResetCreditsSettings(cfg)
	if !enabled {
		return
	}
	// clock advances the sweep's start by the time spent so far, so each decision and
	// backoff uses the time it is made at rather than when the sweep began.
	started := time.Now()
	clock := func() time.Time { return now.Add(time.Since(started)) }

	known := make(map[string]struct{})
	for _, auth := range s.coreManager.List() {
		if ctx.Err() != nil {
			return
		}
		if !codexResetCreditsEligible(auth, settings) {
			continue
		}
		known[auth.ID] = struct{}{}
		state := states[auth.ID]
		if state == nil {
			state = &codexResetCreditState{}
			states[auth.ID] = state
		}
		at := clock()
		if at.Before(state.nextCheck) {
			continue
		}
		blockedUntil, blocked := codexLocalBlock(auth, at)
		if !blocked {
			continue
		}
		backoff := s.considerCodexResetCredit(ctx, cfg, settings, state, auth, blockedUntil, clock)
		state.nextCheck = clock().Add(backoff)
	}
	for id := range states {
		if _, ok := known[id]; !ok {
			delete(states, id)
		}
	}
}

// codexResetCreditsEligible reports whether a credential is a Codex OAuth login the
// settings cover. A login without its ChatGPT account ID is left alone: a reset is
// redeemed for one workspace, and without the ID ChatGPT would pick the default one.
func codexResetCreditsEligible(auth *coreauth.Auth, settings codexResetCreditSettings) bool {
	if provider, ok := oauthProbeProvider(auth); !ok || provider != "codex" {
		return false
	}
	return codexAccountID(auth) != "" && settings.covers(auth)
}

func codexAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	return strings.TrimSpace(accountID)
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

// codexCredentialQuotaHeld reports whether the proxy holds the whole credential in a
// quota cooldown, which it does after a usage_limit_reached refusal.
func codexCredentialQuotaHeld(auth *coreauth.Auth, now time.Time) bool {
	return auth.Quota.Exceeded && strings.EqualFold(auth.Quota.Reason, "credential_quota") && auth.Quota.NextRecoverAt.After(now)
}

// codexQuotaCooldownHeld reports whether the proxy holds the credential or any of
// its models in a cooldown caused by quota. With model-level cooling a
// usage_limit_reached refusal cools only the model it was for.
func codexQuotaCooldownHeld(auth *coreauth.Auth, now time.Time) bool {
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
			return true
		}
	}
	return false
}

// codexCooldownFingerprint summarizes the login's quota cooldowns, so a refusal
// recorded while ChatGPT's usage was being read is not taken as one that usage answers.
func codexCooldownFingerprint(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	var parts []string
	if auth.Quota.Exceeded {
		parts = append(parts, fmt.Sprintf("|%s|%d|%d", auth.Quota.Reason, auth.Quota.NextRecoverAt.UnixNano(), auth.Quota.BackoffLevel))
	}
	for model, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded {
			parts = append(parts, fmt.Sprintf("%s|%s|%d|%d", model, state.Quota.Reason, state.Quota.NextRecoverAt.UnixNano(), state.UpdatedAt.UnixNano()))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
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

// parseCodexUsageState reads the usage answer. It reports false unless the answer
// says outright whether the login is allowed and whether its limit is reached: an
// empty or unfamiliar answer proves neither a block nor a recovery.
func parseCodexUsageState(body []byte, now time.Time) (codexUsageState, bool) {
	root := gjson.ParseBytes(body)
	rateLimit := firstGJSON(root, "rate_limit", "rateLimit")
	limitReached := firstGJSON(rateLimit, "limit_reached", "limitReached")
	allowed := firstGJSON(rateLimit, "allowed")
	isBool := func(value gjson.Result) bool { return value.Type == gjson.True || value.Type == gjson.False }
	if !rateLimit.IsObject() || !isBool(limitReached) || !isBool(allowed) {
		return codexUsageState{}, false
	}
	usage := codexUsageState{
		limitReached: limitReached.Bool(),
		notAllowed:   !allowed.Bool(),
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
	return usage, true
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
	id      string
	expires time.Time
}

// soonestCodexResetCredit returns the available Codex reset that expires first.
// A reset without an expiry sorts last.
func soonestCodexResetCredit(body []byte) (codexResetCredit, bool) {
	credits := gjson.GetBytes(body, "credits")
	if !credits.IsArray() {
		return codexResetCredit{}, false
	}
	var available []codexResetCredit
	credits.ForEach(func(_, item gjson.Result) bool {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" || !strings.EqualFold(item.Get("status").String(), "available") ||
			!strings.EqualFold(item.Get("reset_type").String(), codexResetCreditType) {
			return true
		}
		available = append(available, codexResetCredit{id: id, expires: codexResetCreditExpiry(item.Get("expires_at"))})
		return true
	})
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

// codexResetCreditExpiry reads an expiry given as an RFC 3339 timestamp or as unix
// seconds. Anything else counts as no expiry.
func codexResetCreditExpiry(value gjson.Result) time.Time {
	switch value.Type {
	case gjson.Number:
		if seconds := value.Int(); seconds > 0 {
			return time.Unix(seconds, 0)
		}
	case gjson.String:
		raw := strings.TrimSpace(value.String())
		if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
			return parsed
		}
		if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	}
	return time.Time{}
}

// considerCodexResetCredit decides about one blocked login, redeems a reset when
// it is worth it, and returns how long to leave the login alone. clock gives the
// current time for each step.
func (s *Service) considerCodexResetCredit(ctx context.Context, cfg *config.Config, settings codexResetCreditSettings, state *codexResetCreditState, auth *coreauth.Auth, localUntil time.Time, clock func() time.Time) time.Duration {
	label := codexResetCreditLabel(auth)
	fingerprint := codexCooldownFingerprint(auth)
	if latest, ok := s.coreManager.GetByID(auth.ID); ok {
		fingerprint = codexCooldownFingerprint(latest)
	}
	now := clock()
	body, errUsage := s.oauthProbeGET(ctx, cfg, "codex", auth, codexUsageURL)
	if errUsage != nil {
		log.Debugf("codex reset credits: usage for %s: %v", label, errUsage)
		return codexResetCreditsErrorBackoff
	}
	usage, known := parseCodexUsageState(body, now)
	if !known {
		log.Debugf("codex reset credits: usage for %s does not say whether it is limited", label)
		return codexResetCreditsErrorBackoff
	}
	s.coreManager.ObserveQuotaSignals(auth.ID, "codex", codexUsageQuotaHeaders(body), now)
	until, binding, blocked := usage.block(localUntil, now)
	if !blocked {
		s.reconcileCodexCooldown(ctx, state, auth.ID, label, fingerprint, clock())
		return codexResetCreditsWaitBackoff
	}
	if usage.applicableKnown && usage.applicable <= 0 {
		return codexResetCreditsNoCreditBackoff
	}
	if wait := until.Sub(now); wait < codexResetCreditsExpiringMinWait && wait < settings.minWait {
		return codexResetCreditsWaitBackoff
	}
	if window, behind := usage.behindPace(binding, now); behind {
		log.Debugf("codex reset credits: %s is blocked for %s, but a reset would push back its %s window (%.0f%% used, reset %s)",
			label, until.Sub(now).Round(time.Minute), window.Duration, window.Used*100, window.ResetAt.Format(time.RFC3339))
		return codexResetCreditsWaitBackoff
	}

	listing, status, errList := s.codexResetCreditRequest(ctx, cfg, auth, http.MethodGet, codexResetCreditsURL, nil)
	if errList == nil && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
		errList = fmt.Errorf("unexpected status %d", status)
	}
	if errList != nil {
		log.Debugf("codex reset credits: listing for %s: %v", label, errList)
		return codexResetCreditsErrorBackoff
	}
	credit, found := soonestCodexResetCredit(listing)
	if !found {
		return codexResetCreditsNoCreditBackoff
	}

	// The requests above take time: decide on the block that is left now.
	now = clock()
	wait := until.Sub(now)
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
	current, currentCfg, currentSettings, covered := s.codexResetCreditsCurrent(auth.ID)
	if !covered || currentSettings.dryRun {
		return codexResetCreditsWaitBackoff
	}

	state.attemptedAt = clock()
	requestID := uuid.NewSHA1(codexResetCreditRequestSpace, []byte(current.ID+"\x00"+credit.id)).String()
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": requestID, "credit_id": credit.id})
	answer, status, errConsume := s.codexResetCreditRequest(ctx, currentCfg, current, http.MethodPost, codexResetCreditsConsumeURL, payload)
	if errConsume != nil {
		log.Warnf("codex reset credits: redeeming a reset for %s: %v", label, errConsume)
		return codexResetCreditsErrorBackoff
	}
	// A refusal carries its reason as a code as well, so the code decides, not the status.
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(answer, "code").String()))
	switch code {
	case "reset", "already_redeemed":
		// A login spending ChatGPT credits past its limit has no cooldown to clear.
		if latest, ok := s.coreManager.GetByID(current.ID); ok && codexInCooldown(latest, clock()) {
			s.clearCodexCooldown(ctx, current.ID, label)
		}
		if refreshed, errRefresh := s.oauthProbeGET(ctx, currentCfg, "codex", current, codexUsageURL); errRefresh == nil {
			s.coreManager.ObserveQuotaSignals(current.ID, "codex", codexUsageQuotaHeaders(refreshed), clock())
		}
		log.Infof("codex reset credits: redeemed a reset (%s) for %s, which was blocked for %s; %d window(s) reset",
			expiry, label, wait.Round(time.Minute), gjson.GetBytes(answer, "windows_reset").Int())
		return codexResetCreditsAfterRedeem
	case "nothing_to_reset":
		log.Infof("codex reset credits: ChatGPT found nothing to reset for %s", label)
		return codexResetCreditsWaitBackoff
	case "no_credit":
		return codexResetCreditsNoCreditBackoff
	default:
		log.Warnf("codex reset credits: unexpected answer (status %d) redeeming a reset for %s: %q", status, label, code)
		return codexResetCreditsErrorBackoff
	}
}

// reconcileCodexCooldown lifts quota cooldowns the proxy holds on a login that
// ChatGPT reports free.
//
// A credential-wide quota cooldown is lifted whenever that happens: a reset was
// redeemed by hand, or ChatGPT reset limits itself. Model-level quota cooldowns are
// lifted only shortly after this check asked for a reset, when they predate it (an
// answer that was lost, or a refusal sent before the reset); at other times one may
// be a single model's own limit, which the usage answer does not cover.
//
// fingerprint is the login's cooldowns from before the usage request; a refusal
// recorded since then is newer than the usage and stays. A login refused again soon
// after a lift keeps its cooldown: ChatGPT's usage and its answers disagree.
func (s *Service) reconcileCodexCooldown(ctx context.Context, state *codexResetCreditState, authID, label, fingerprint string, now time.Time) {
	current, _, settings, covered := s.codexResetCreditsCurrent(authID)
	if !covered {
		return
	}
	recent := !state.attemptedAt.IsZero() && now.Sub(state.attemptedAt) < codexResetCreditsAttemptWindow
	if !codexCredentialQuotaHeld(current, now) && !(recent && codexQuotaCooldownHeld(current, now)) {
		return
	}
	if codexCooldownFingerprint(current) != fingerprint {
		return
	}
	if !recent && !state.liftedAt.IsZero() && now.Sub(state.liftedAt) < codexResetCreditsLiftGap {
		log.Debugf("codex reset credits: %s was refused again after its cooldown was lifted at %s; leaving the new one",
			label, state.liftedAt.Format(time.RFC3339))
		return
	}
	if settings.dryRun {
		log.Infof("codex reset credits: dry run: would clear the cooldown of %s, which ChatGPT no longer limits", label)
		return
	}
	if s.clearCodexCooldown(ctx, authID, label) {
		state.liftedAt = now
		log.Infof("codex reset credits: cleared the cooldown of %s, which ChatGPT no longer limits", label)
	}
}

// clearCodexCooldown lifts the proxy's cooldowns on a login. Like the management
// reset-quota action it clears every cooldown of the login, so a model that failed
// for another reason is tried once more before its own cooldown returns.
func (s *Service) clearCodexCooldown(ctx context.Context, authID, label string) bool {
	if _, _, errReset := s.coreManager.ResetQuota(ctx, authID); errReset != nil {
		log.Warnf("codex reset credits: clearing the cooldown of %s: %v", label, errReset)
		return false
	}
	return true
}

// codexResetCreditsCurrent re-reads the setting and the credential just before a
// reset is spent or a cooldown lifted, and returns them while the setting still
// covers that login. The setting or the credential may have changed while the
// requests before it ran.
func (s *Service) codexResetCreditsCurrent(authID string) (*coreauth.Auth, *config.Config, codexResetCreditSettings, bool) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	settings, enabled := codexResetCreditsSettings(cfg)
	if !enabled {
		return nil, nil, codexResetCreditSettings{}, false
	}
	current, ok := s.coreManager.GetByID(authID)
	if !ok || current == nil || !codexResetCreditsEligible(current, settings) {
		return nil, nil, codexResetCreditSettings{}, false
	}
	return current, cfg, settings, true
}

// codexResetCreditRequest sends one reset-credit request with the login's own
// token and headers, through its proxy settings, and returns the answer whatever
// its status.
func (s *Service) codexResetCreditRequest(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, method, endpoint string, body []byte) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, oauthProbeTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(requestCtx, method, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return nil, 0, fmt.Errorf("create request: %w", errRequest)
	}
	req.Header = oauthProbeHeaders(cfg, "codex", auth)
	client := helps.NewProxyAwareHTTPClient(requestCtx, cfg, auth, 0)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("codex reset credits: close response body: %v", errClose)
		}
	}()
	answer, errRead := io.ReadAll(io.LimitReader(resp.Body, oauthProbeMaxBody))
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", errRead)
	}
	return answer, resp.StatusCode, nil
}

func codexResetCreditLabel(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(filepath.Base(auth.FileName)); name != "" && name != "." {
		return name
	}
	return auth.ID
}
