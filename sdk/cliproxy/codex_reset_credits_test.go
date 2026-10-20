package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// resetBackend stands in for ChatGPT's usage and reset-credit endpoints, per access token.
type resetBackend struct {
	mu            sync.Mutex
	usage         map[string]string // token -> usage body while blocked
	usageAfter    map[string]string // token -> usage body after a reset
	credits       map[string]string // token -> credit list body
	consumeCode   []string          // answers to consume, in order; the last one repeats
	consumeStatus int               // HTTP status of consume answers; 0 means 200
	consumeFail   int               // consume requests to fail with 500 first, spending nothing
	loseAnswers   int               // consume requests to carry out, then answer 500
	onUsage       func()            // runs while a usage answer is being prepared
	onList        func()            // runs while a credit list is being answered
	redeemed      map[string]bool
	usageCalls    map[string]int
	listCalls     map[string]int
	consumed      []resetConsume
}

type resetConsume struct {
	token     string
	accountID string
	requestID string
	creditID  string
}

func newResetBackend(t *testing.T) *resetBackend {
	t.Helper()
	backend := &resetBackend{
		usage:      map[string]string{},
		usageAfter: map[string]string{},
		credits:    map[string]string{},
		redeemed:   map[string]bool{},
		usageCalls: map[string]int{},
		listCalls:  map[string]int{},
	}
	server := httptest.NewServer(http.HandlerFunc(backend.serve))
	t.Cleanup(server.Close)
	originalUsage, originalList, originalConsume := codexUsageURL, codexResetCreditsURL, codexResetCreditsConsumeURL
	codexUsageURL = server.URL + "/wham/usage"
	codexResetCreditsURL = server.URL + "/wham/rate-limit-reset-credits"
	codexResetCreditsConsumeURL = server.URL + "/wham/rate-limit-reset-credits/consume"
	t.Cleanup(func() {
		codexUsageURL, codexResetCreditsURL, codexResetCreditsConsumeURL = originalUsage, originalList, originalConsume
	})
	return backend
}

func (b *resetBackend) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/wham/usage":
		b.usageCalls[token]++
		if b.onUsage != nil {
			b.onUsage()
		}
		if body, ok := b.usageAfter[token]; ok && b.redeemed[token] {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(b.usage[token]))
	case "/wham/rate-limit-reset-credits":
		b.listCalls[token]++
		if b.onList != nil {
			b.onList()
		}
		_, _ = w.Write([]byte(b.credits[token]))
	case "/wham/rate-limit-reset-credits/consume":
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		b.consumed = append(b.consumed, resetConsume{
			token:     token,
			accountID: r.Header.Get("Chatgpt-Account-Id"),
			requestID: payload["redeem_request_id"],
			creditID:  payload["credit_id"],
		})
		if b.consumeFail > 0 {
			b.consumeFail--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if b.loseAnswers > 0 {
			b.loseAnswers--
			b.redeemed[token] = true
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		code := "reset"
		if len(b.consumeCode) > 0 {
			code = b.consumeCode[0]
			if len(b.consumeCode) > 1 {
				b.consumeCode = b.consumeCode[1:]
			}
		}
		if code == "reset" || code == "already_redeemed" {
			b.redeemed[token] = true
		}
		if b.consumeStatus != 0 {
			w.WriteHeader(b.consumeStatus)
		}
		_, _ = fmt.Fprintf(w, `{"code": %q, "windows_reset": 1}`, code)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *resetBackend) consumes() []resetConsume {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]resetConsume(nil), b.consumed...)
}

func (b *resetBackend) calls(token string) (usage, list int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usageCalls[token], b.listCalls[token]
}

type usageWindow struct {
	usedPercent float64
	length      time.Duration
	resetAt     time.Time
}

func usageBody(limitReached bool, applicable int, windows ...usageWindow) string {
	window := func(index int) string {
		if index >= len(windows) {
			return "null"
		}
		w := windows[index]
		return fmt.Sprintf(`{"used_percent": %v, "limit_window_seconds": %d, "reset_at": %d}`,
			w.usedPercent, int64(w.length/time.Second), w.resetAt.Unix())
	}
	return fmt.Sprintf(`{"plan_type": "pro", "rate_limit": {"allowed": %t, "limit_reached": %t, "primary_window": %s, "secondary_window": %s},
		"credits": {"has_credits": true, "balance": "100"},
		"rate_limit_reset_credits": {"available_count": %d, "applicable_available_count": %d}}`,
		!limitReached, limitReached, window(0), window(1), applicable, applicable)
}

type resetCreditFixture struct {
	id      string
	status  string
	kind    string
	expires time.Time
}

func creditsBody(credits ...resetCreditFixture) string {
	entries := make([]string, 0, len(credits))
	for _, credit := range credits {
		status, kind := credit.status, credit.kind
		if status == "" {
			status = "available"
		}
		if kind == "" {
			kind = codexResetCreditType
		}
		expires := "null"
		if !credit.expires.IsZero() {
			expires = strconv.Quote(credit.expires.UTC().Format(time.RFC3339Nano))
		}
		entries = append(entries, fmt.Sprintf(`{"id": %q, "reset_type": %q, "status": %q, "granted_at": "2026-09-22T20:47:13Z", "expires_at": %s, "title": "Full reset"}`,
			credit.id, kind, status, expires))
	}
	return fmt.Sprintf(`{"available_count": %d, "credits": [%s]}`, len(credits), strings.Join(entries, ","))
}

func resetCreditsConfig(mode string) *internalconfig.Config {
	cfg := &internalconfig.Config{}
	cfg.Codex.ResetCredits.Mode = mode
	return cfg
}

// registerCodexLogin registers an OAuth Codex login. blockedUntil puts it in a quota
// cooldown, as a refused request does; window headers record its last rate-limit snapshot.
func registerCodexLogin(t *testing.T, manager *coreauth.Manager, id string, blockedUntil time.Time, observedAt time.Time, signals map[string]string) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:         id,
		FileName:   id + ".json",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
		Metadata:   map[string]any{"access_token": "token-" + id, "account_id": "acct-" + id},
	}
	if !blockedUntil.IsZero() {
		auth.Status = coreauth.StatusError
		auth.Unavailable = true
		auth.NextRetryAfter = blockedUntil
		auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: blockedUntil}
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register(%s): %v", id, errRegister)
	}
	if len(signals) > 0 {
		manager.ObserveQuotaSignals(id, "codex", headerOf(signals), observedAt)
	}
}

func weeklyExhaustedSignals(resetAt time.Time) map[string]string {
	return map[string]string{
		"X-Codex-Primary-Used-Percent":   "100",
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(resetAt.Unix(), 10),
		"X-Codex-Limit-Reached":          "true",
		"X-Codex-Credits-Has-Credits":    "true",
	}
}

func TestForkCodexResetCreditsSettings(t *testing.T) {
	if _, enabled := codexResetCreditsSettings(nil); enabled {
		t.Fatal("nil config must disable reset credits")
	}
	if _, enabled := codexResetCreditsSettings(&internalconfig.Config{}); enabled {
		t.Fatal("reset credits must be off by default")
	}
	for _, mode := range []string{"off", "false", "nonsense"} {
		if _, enabled := codexResetCreditsSettings(resetCreditsConfig(mode)); enabled {
			t.Fatalf("mode %q enabled reset credits", mode)
		}
	}
	for mode, wantDryRun := range map[string]bool{"on": false, "true": false, " ON ": false, "dry-run": true, "dryrun": true} {
		settings, enabled := codexResetCreditsSettings(resetCreditsConfig(mode))
		if !enabled || settings.dryRun != wantDryRun {
			t.Fatalf("mode %q: enabled=%t dryRun=%t, want enabled with dryRun=%t", mode, enabled, settings.dryRun, wantDryRun)
		}
		if settings.minWait != defaultCodexResetCreditsMinWait || settings.expiringWithin != defaultCodexResetCreditsExpiringWithin {
			t.Fatalf("mode %q: defaults = %s, %s", mode, settings.minWait, settings.expiringWithin)
		}
	}

	home := resetCreditsConfig("on")
	home.Home.Enabled = true
	if _, enabled := codexResetCreditsSettings(home); enabled {
		t.Fatal("home mode must disable reset credits")
	}

	cfg := resetCreditsConfig("on")
	cfg.Codex.ResetCredits.MinWait = "90m"
	cfg.Codex.ResetCredits.ExpiringWithin = "0"
	cfg.Codex.ResetCredits.AuthFiles = []string{"codex-pro.json", " codex-team-id ", ""}
	settings, _ := codexResetCreditsSettings(cfg)
	if settings.minWait != 90*time.Minute || settings.expiringWithin != 0 {
		t.Fatalf("durations = %s, %s; want 90m and off", settings.minWait, settings.expiringWithin)
	}
	cfg.Codex.ResetCredits.MinWait = "soon"
	if settings, _ := codexResetCreditsSettings(cfg); settings.minWait != defaultCodexResetCreditsMinWait {
		t.Fatalf("unparseable min-wait = %s, want the default", settings.minWait)
	}
	for _, testCase := range []struct {
		auth *coreauth.Auth
		want bool
	}{
		{&coreauth.Auth{ID: "a", FileName: "/data/auths/codex-pro.json"}, true},
		{&coreauth.Auth{ID: "codex-team-id"}, true},
		{&coreauth.Auth{ID: "b", FileName: "codex-other.json"}, false},
	} {
		if got := settings.covers(testCase.auth); got != testCase.want {
			t.Fatalf("covers(%s, %s) = %t, want %t", testCase.auth.ID, testCase.auth.FileName, got, testCase.want)
		}
	}
	if !(codexResetCreditSettings{}).covers(&coreauth.Auth{ID: "any"}) {
		t.Fatal("an empty auth-files list must cover every login")
	}
}

func TestForkCodexResetCreditsYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "oauth:\n  providers:\n    codex:\n      reset-credits:\n        mode: \"on\"\n        min-wait: \"4h\"\n        auth-files: [\"codex-pro.json\"]\n"
	if errWrite := os.WriteFile(path, []byte(yaml), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := internalconfig.LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig: %v", errLoad)
	}
	got := cfg.Codex.ResetCredits
	if got.Mode != "on" || got.MinWait != "4h" || len(got.AuthFiles) != 1 || got.AuthFiles[0] != "codex-pro.json" {
		t.Fatalf("oauth.providers.codex.reset-credits = %+v", got)
	}

	// The image's first-start configuration, filled in as docker/fork/entrypoint.sh does.
	template, errRead := os.ReadFile(filepath.Join("..", "..", "docker", "fork", "config.default.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	dataDir := t.TempDir()
	lines := strings.Split(string(template), "\n")
	for i, line := range lines {
		if strings.Contains(line, "__API_KEYS__") {
			lines[i] = `    - "sk-test"`
		}
	}
	filled := strings.ReplaceAll(strings.Join(lines, "\n"), "__DATA_DIR__", dataDir)
	defaultPath := filepath.Join(dataDir, "config.yaml")
	if errWrite := os.WriteFile(defaultPath, []byte(filled), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	defaults, errDefaults := internalconfig.LoadConfig(defaultPath)
	if errDefaults != nil {
		t.Fatalf("LoadConfig(config.default.yaml): %v", errDefaults)
	}
	settings, enabled := codexResetCreditsSettings(defaults)
	if !enabled || settings.dryRun || settings.minWait != defaultCodexResetCreditsMinWait || settings.expiringWithin != defaultCodexResetCreditsExpiringWithin {
		t.Fatalf("default image config: enabled=%t settings=%+v, want on with the default thresholds", enabled, settings)
	}
}

func TestForkSoonestCodexResetCredit(t *testing.T) {
	now := time.Now()
	credit, ok := soonestCodexResetCredit([]byte(creditsBody(
		resetCreditFixture{id: "never"},
		resetCreditFixture{id: "late", expires: now.Add(17 * 24 * time.Hour)},
		resetCreditFixture{id: "used", status: "redeemed", expires: now.Add(time.Hour)},
		resetCreditFixture{id: "other", kind: "something_else", expires: now.Add(2 * time.Hour)},
		resetCreditFixture{id: "soon", expires: now.Add(9 * 24 * time.Hour)},
	)))
	if !ok || credit.id != "soon" {
		t.Fatalf("soonest credit = %+v, %t; want soon", credit, ok)
	}
	if credit, ok := soonestCodexResetCredit([]byte(creditsBody(resetCreditFixture{id: "never"}))); !ok || credit.id != "never" {
		t.Fatalf("credit without expiry = %+v, %t; want it when it is the only one", credit, ok)
	}
	if _, ok := soonestCodexResetCredit([]byte(`{"available_count": 0, "credits": []}`)); ok {
		t.Fatal("an empty list must have no credit")
	}
	if _, ok := soonestCodexResetCredit([]byte(`not json`)); ok {
		t.Fatal("an unreadable list must have no credit")
	}
	// Expiries given as unix seconds, as a number or a string, count too.
	numeric := fmt.Sprintf(`{"credits": [
		{"id": "rfc", "reset_type": %q, "status": "available", "expires_at": %q},
		{"id": "seconds", "reset_type": %q, "status": "available", "expires_at": %d},
		{"id": "text-seconds", "reset_type": %q, "status": "available", "expires_at": "%d"}]}`,
		codexResetCreditType, now.Add(5*24*time.Hour).UTC().Format(time.RFC3339),
		codexResetCreditType, now.Add(2*24*time.Hour).Unix(),
		codexResetCreditType, now.Add(3*24*time.Hour).Unix())
	if credit, ok := soonestCodexResetCredit([]byte(numeric)); !ok || credit.id != "seconds" {
		t.Fatalf("soonest of mixed expiry formats = %+v, %t; want seconds", credit, ok)
	}
}

// assertNextCheck checks a scheduled check against the sweep time plus a backoff,
// allowing for the time the sweep itself took.
func assertNextCheck(t *testing.T, states map[string]*codexResetCreditState, id string, want time.Time) {
	t.Helper()
	state, ok := states[id]
	if !ok || state.nextCheck.Before(want) || state.nextCheck.Sub(want) > 5*time.Second {
		var got time.Time
		if ok {
			got = state.nextCheck
		}
		t.Fatalf("next check for %s = %s (scheduled %t), want %s", id, got, ok, want)
	}
}

// A login whose plan spends ChatGPT credits past the weekly limit keeps answering,
// so only its rate-limit headers show it is blocked.
func TestForkCodexResetCreditsRedeemsLongBlock(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 2, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.usageAfter["token-codex-pro"] = usageBody(false, 0, usageWindow{0, 7 * 24 * time.Hour, now.Add(7 * 24 * time.Hour)})
	backend.credits["token-codex-pro"] = creditsBody(
		resetCreditFixture{id: "credit-nov", expires: now.Add(17 * 24 * time.Hour)},
		resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)},
	)

	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", time.Time{}, now.Add(-time.Minute), weeklyExhaustedSignals(weeklyReset))
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)

	consumed := backend.consumes()
	if len(consumed) != 1 {
		t.Fatalf("consume requests = %d, want 1", len(consumed))
	}
	wantRequestID := uuid.NewSHA1(codexResetCreditRequestSpace, []byte("codex-pro\x00credit-oct")).String()
	if got := consumed[0]; got.creditID != "credit-oct" || got.requestID != wantRequestID || got.token != "token-codex-pro" || got.accountID != "acct-codex-pro" {
		t.Fatalf("consume = %+v, want the soonest-expiring credit with request id %s on the login's own account", got, wantRequestID)
	}
	auth, _ := manager.GetByID("codex-pro")
	windows := coreauth.QuotaWindows("codex", auth.Quota)
	if len(windows) != 1 || windows[0].Used != 0 {
		t.Fatalf("windows after the reset = %+v, want the re-read usage", windows)
	}
	assertNextCheck(t, states, "codex-pro", now.Add(codexResetCreditsAfterRedeem))

	// Within the pause after a reset nothing is asked, even if the headers still say blocked.
	manager.ObserveQuotaSignals("codex-pro", "codex", headerOf(weeklyExhaustedSignals(weeklyReset)), now.Add(time.Minute))
	service.checkCodexResetCredits(context.Background(), states, now.Add(2*time.Minute))
	if got := len(backend.consumes()); got != 1 {
		t.Fatalf("consume requests after a second check = %d, want 1", got)
	}
}

// A login refused with usage_limit_reached sits in a cooldown; once a reset is redeemed
// the cooldown is cleared so requests reach it again.
func TestForkCodexResetCreditsClearsCooldownWithExpiringCredit(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	sessionReset := now.Add(2 * time.Hour)
	// The weekly window is 46% used with 30% of it passed: ahead of pace.
	weeklyReset := now.Add(time.Duration(0.7 * float64(7*24*time.Hour)))
	backend.usage["token-codex-team"] = usageBody(true, 3,
		usageWindow{100, 5 * time.Hour, sessionReset}, usageWindow{46, 7 * 24 * time.Hour, weeklyReset})
	backend.usageAfter["token-codex-team"] = usageBody(false, 0,
		usageWindow{0, 5 * time.Hour, now.Add(5 * time.Hour)}, usageWindow{0, 7 * 24 * time.Hour, now.Add(7 * 24 * time.Hour)})
	backend.credits["token-codex-team"] = creditsBody(
		resetCreditFixture{id: "credit-expiring", expires: now.Add(2 * 24 * time.Hour)},
		resetCreditFixture{id: "credit-later", expires: now.Add(20 * 24 * time.Hour)},
	)

	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-team", sessionReset, time.Time{}, nil)
	before, _ := manager.GetByID("codex-team")
	if !codexInCooldown(before, now) {
		t.Fatal("precondition: the login must start in a quota cooldown")
	}
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	consumed := backend.consumes()
	if len(consumed) != 1 || consumed[0].creditID != "credit-expiring" {
		t.Fatalf("consume = %+v, want one redemption of the expiring credit", consumed)
	}
	after, _ := manager.GetByID("codex-team")
	if codexInCooldown(after, time.Now()) || after.Quota.Exceeded || after.Unavailable {
		t.Fatalf("cooldown after the reset: exceeded=%t unavailable=%t next=%s", after.Quota.Exceeded, after.Unavailable, after.Quota.NextRecoverAt)
	}
}

func TestForkCodexResetCreditsKeepsCreditsForLongBlocks(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(time.Duration(0.7 * float64(7*24*time.Hour)))
	for id, block := range map[string]time.Duration{"codex-short": 20 * time.Minute, "codex-session": 2 * time.Hour} {
		token := "token-" + id
		backend.usage[token] = usageBody(true, 1,
			usageWindow{100, 5 * time.Hour, now.Add(block)}, usageWindow{46, 7 * 24 * time.Hour, weeklyReset})
		backend.credits[token] = creditsBody(resetCreditFixture{id: "credit-" + id, expires: now.Add(20 * 24 * time.Hour)})
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-short", now.Add(20*time.Minute), time.Time{}, nil)
	registerCodexLogin(t, manager, "codex-session", now.Add(2*time.Hour), time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if consumed := backend.consumes(); len(consumed) != 0 {
		t.Fatalf("consume = %+v, want none: neither block reaches min-wait and no credit is expiring", consumed)
	}
	if _, list := backend.calls("token-codex-short"); list != 0 {
		t.Fatalf("a 20-minute block listed credits %d time(s); it cannot qualify", list)
	}
	if _, list := backend.calls("token-codex-session"); list != 1 {
		t.Fatalf("a 2-hour block listed credits %d time(s), want 1 to look for an expiring one", list)
	}
}

func TestForkCodexResetCreditsKeepsWeeklyPace(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	// The weekly window is 10% used with 70% of it passed: a reset would move its reset
	// a full week out and cost more than the 2 hours it frees.
	weeklyReset := now.Add(time.Duration(0.3 * float64(7*24*time.Hour)))
	backend.usage["token-codex-team"] = usageBody(true, 1,
		usageWindow{100, 5 * time.Hour, now.Add(2 * time.Hour)}, usageWindow{10, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-team"] = creditsBody(resetCreditFixture{id: "credit-expiring", expires: now.Add(24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-team", now.Add(2*time.Hour), time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if consumed := backend.consumes(); len(consumed) != 0 {
		t.Fatalf("consume = %+v, want none while the weekly window is behind pace", consumed)
	}
	if _, list := backend.calls("token-codex-team"); list != 0 {
		t.Fatalf("credits were listed %d time(s) for a block that cannot qualify", list)
	}
}

func TestForkCodexResetCreditsDryRun(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
	service := &Service{cfg: resetCreditsConfig("dry-run"), coreManager: manager}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if consumed := backend.consumes(); len(consumed) != 0 {
		t.Fatalf("dry run redeemed: %+v", consumed)
	}
	if _, list := backend.calls("token-codex-pro"); list != 1 {
		t.Fatalf("dry run listed credits %d time(s), want 1", list)
	}
}

func TestForkCodexResetCreditsLeavesOtherLoginsAlone(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	for _, id := range []string{"codex-free", "codex-listed", "codex-unlisted"} {
		backend.usage["token-"+id] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
		backend.credits["token-"+id] = creditsBody(resetCreditFixture{id: "credit-" + id, expires: now.Add(9 * 24 * time.Hour)})
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-free", time.Time{}, now, map[string]string{
		"X-Codex-Primary-Used-Percent": "40", "X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At": strconv.FormatInt(weeklyReset.Unix(), 10),
	})
	registerCodexLogin(t, manager, "codex-listed", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
	registerCodexLogin(t, manager, "codex-unlisted", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))

	off := &Service{cfg: resetCreditsConfig(""), coreManager: manager}
	off.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)
	for _, id := range []string{"codex-free", "codex-listed", "codex-unlisted"} {
		if usage, list := backend.calls("token-" + id); usage != 0 || list != 0 {
			t.Fatalf("with the feature off, %s got %d usage and %d list request(s)", id, usage, list)
		}
	}

	cfg := resetCreditsConfig("on")
	cfg.Codex.ResetCredits.AuthFiles = []string{"codex-free.json", "codex-listed.json"}
	service := &Service{cfg: cfg, coreManager: manager}
	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if usage, _ := backend.calls("token-codex-free"); usage != 0 {
		t.Fatalf("a login that is not blocked got %d usage request(s)", usage)
	}
	if usage, _ := backend.calls("token-codex-unlisted"); usage != 0 {
		t.Fatalf("a login outside auth-files got %d usage request(s)", usage)
	}
	if consumed := backend.consumes(); len(consumed) != 1 || consumed[0].creditID != "credit-codex-listed" {
		t.Fatalf("consume = %+v, want only the listed blocked login", consumed)
	}
}

// A redemption whose answer is lost is retried with the same request id, so the
// provider can recognise it instead of spending a second credit.
func TestForkCodexResetCreditsRetriesWithSameRequestID(t *testing.T) {
	backend := newResetBackend(t)
	backend.consumeFail = 1
	backend.consumeCode = []string{"already_redeemed"}
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)
	assertNextCheck(t, states, "codex-pro", now.Add(codexResetCreditsErrorBackoff))
	retried := now.Add(codexResetCreditsErrorBackoff + 10*time.Second)
	service.checkCodexResetCredits(context.Background(), states, retried)

	consumed := backend.consumes()
	if len(consumed) != 2 || consumed[0].requestID == "" || consumed[0].requestID != consumed[1].requestID {
		t.Fatalf("consume requests = %+v, want two with one request id", consumed)
	}
	// already_redeemed counts as a redemption.
	assertNextCheck(t, states, "codex-pro", retried.Add(codexResetCreditsAfterRedeem))
}

// A redemption ChatGPT carried out but whose answer was lost leaves the proxy's
// cooldown behind; the next check finds the login free and lifts it.
func TestForkCodexResetCreditsLiftsCooldownAfterLostAnswer(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	backend := newResetBackend(t)
	backend.loseAnswers = 1
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-team"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.usageAfter["token-codex-team"] = usageBody(false, 0, usageWindow{0, 7 * 24 * time.Hour, now.Add(7 * 24 * time.Hour)})
	backend.credits["token-codex-team"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-team", weeklyReset, time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)
	held, _ := manager.GetByID("codex-team")
	if !codexInCooldown(held, now) {
		t.Fatal("precondition: after a lost answer the cooldown is still held")
	}
	service.checkCodexResetCredits(context.Background(), states, now.Add(codexResetCreditsErrorBackoff+10*time.Second))

	if consumed := backend.consumes(); len(consumed) != 1 {
		t.Fatalf("consume requests = %d, want 1: the login was already free", len(consumed))
	}
	after, _ := manager.GetByID("codex-team")
	if codexInCooldown(after, time.Now()) {
		t.Fatalf("cooldown still held after ChatGPT reported the login free: next recover %s", after.Quota.NextRecoverAt)
	}

	// In dry run the cooldown is only reported.
	dryManager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, dryManager, "codex-team", weeklyReset, time.Time{}, nil)
	dry := &Service{cfg: resetCreditsConfig("dry-run"), coreManager: dryManager}
	dry.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)
	if kept, _ := dryManager.GetByID("codex-team"); !codexInCooldown(kept, now) {
		t.Fatal("dry run lifted a cooldown")
	}
}

// Turning the feature off, or a login leaving it, while a check is under way stops
// the redemption before anything is spent.
func TestForkCodexResetCreditsRechecksBeforeSpending(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	for name, change := range map[string]func(*testing.T, *Service, *coreauth.Manager){
		"mode off": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			service.cfgMu.Lock()
			service.cfg = resetCreditsConfig("off")
			service.cfgMu.Unlock()
		},
		"dry run": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			service.cfgMu.Lock()
			service.cfg = resetCreditsConfig("dry-run")
			service.cfgMu.Unlock()
		},
		"login left auth-files": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			cfg := resetCreditsConfig("on")
			cfg.Codex.ResetCredits.AuthFiles = []string{"codex-other.json"}
			service.cfgMu.Lock()
			service.cfg = cfg
			service.cfgMu.Unlock()
		},
		"login disabled": func(t *testing.T, _ *Service, manager *coreauth.Manager) {
			auth, _ := manager.GetByID("codex-pro")
			auth.Disabled = true
			if _, errUpdate := manager.Update(coreauth.WithSkipPersist(context.Background()), auth); errUpdate != nil {
				t.Errorf("disable the login: %v", errUpdate)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newResetBackend(t)
			backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
			backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
			manager := coreauth.NewManager(nil, nil, nil)
			registerCodexLogin(t, manager, "codex-pro", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
			service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
			backend.onList = func() { change(t, service, manager) }

			service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

			if _, list := backend.calls("token-codex-pro"); list != 1 {
				t.Fatalf("credit list requests = %d, want 1", list)
			}
			if consumed := backend.consumes(); len(consumed) != 0 {
				t.Fatalf("consume = %+v, want none after the change", consumed)
			}
		})
	}
}

// ChatGPT may give a refusal or a repeat as a status other than 200 that still
// names its code; the code decides.
func TestForkCodexResetCreditsReadsCodeOfRefusals(t *testing.T) {
	backend := newResetBackend(t)
	backend.consumeStatus = http.StatusConflict
	backend.consumeCode = []string{"already_redeemed"}
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)

	assertNextCheck(t, states, "codex-pro", now.Add(codexResetCreditsAfterRedeem))
}

func TestForkCodexResetCreditsNeedsAccountID(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "codex-pro",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
		Metadata:   map[string]any{"access_token": "token-codex-pro"},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.ObserveQuotaSignals("codex-pro", "codex", headerOf(weeklyExhaustedSignals(weeklyReset)), now)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if usage, list := backend.calls("token-codex-pro"); usage != 0 || list != 0 {
		t.Fatalf("a login without an account ID got %d usage and %d list request(s)", usage, list)
	}
}

func TestForkCodexResetCreditsNothingToReset(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	backend := newResetBackend(t)
	backend.consumeCode = []string{"nothing_to_reset"}
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", weeklyReset, time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)

	auth, _ := manager.GetByID("codex-pro")
	if !codexInCooldown(auth, now) {
		t.Fatal("nothing_to_reset must leave the cooldown in place")
	}
	// nothing_to_reset is not a redemption.
	assertNextCheck(t, states, "codex-pro", now.Add(codexResetCreditsWaitBackoff))
}

// The usage endpoint has the last word: a login the proxy still holds in a cooldown
// that ChatGPT no longer limits has the cooldown lifted and nothing redeemed, and a
// login no reset applies to is left alone.
func TestForkCodexResetCreditsFollowsUsageEndpoint(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	backend.usage["token-codex-free"] = usageBody(false, 1, usageWindow{60, 7 * 24 * time.Hour, now.Add(4 * 24 * time.Hour)})
	backend.usage["token-codex-none"] = usageBody(true, 0, usageWindow{100, 7 * 24 * time.Hour, now.Add(4 * 24 * time.Hour)})
	for _, id := range []string{"codex-free", "codex-none"} {
		backend.credits["token-"+id] = creditsBody(resetCreditFixture{id: "credit-" + id, expires: now.Add(9 * 24 * time.Hour)})
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-free", now.Add(4*24*time.Hour), time.Time{}, nil)
	registerCodexLogin(t, manager, "codex-none", now.Add(4*24*time.Hour), time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)

	for _, id := range []string{"codex-free", "codex-none"} {
		if usage, list := backend.calls("token-" + id); usage != 1 || list != 0 {
			t.Fatalf("%s: %d usage and %d list request(s), want 1 and 0", id, usage, list)
		}
	}
	if consumed := backend.consumes(); len(consumed) != 0 {
		t.Fatalf("consume = %+v, want none", consumed)
	}
	assertNextCheck(t, states, "codex-free", now.Add(codexResetCreditsWaitBackoff))
	assertNextCheck(t, states, "codex-none", now.Add(codexResetCreditsNoCreditBackoff))
	if free, _ := manager.GetByID("codex-free"); codexInCooldown(free, time.Now()) {
		t.Fatal("the cooldown of a login ChatGPT no longer limits must be lifted")
	}
	if none, _ := manager.GetByID("codex-none"); !codexInCooldown(none, time.Now()) {
		t.Fatal("the cooldown of a login ChatGPT still limits must stay")
	}
}

// registerCodexModelCooldown registers a login that model-level cooling has blocked:
// only the model the refusal was for is in a quota cooldown.
func registerCodexModelCooldown(t *testing.T, manager *coreauth.Manager, id string, until time.Time) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:         id,
		FileName:   id + ".json",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
		Metadata:   map[string]any{"access_token": "token-" + id, "account_id": "acct-" + id},
		ModelStates: map[string]*coreauth.ModelState{"gpt-6-astra": {
			Status:         coreauth.StatusError,
			Unavailable:    true,
			NextRetryAfter: until,
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until},
		}},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register(%s): %v", id, errRegister)
	}
	registered, _ := manager.GetByID(id)
	if !codexQuotaCooldownHeld(registered, time.Now()) || codexCredentialQuotaHeld(registered, time.Now()) {
		t.Fatal("precondition: only a model-level quota cooldown")
	}
}

// With model-level cooling a lost answer leaves only a model cooled; right after the
// attempt that cooldown predates the reset and is lifted. Without a recent attempt a
// model-level cooldown may be that model's own limit, which usage does not show.
func TestForkCodexResetCreditsModelCooldowns(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	backend := newResetBackend(t)
	backend.loseAnswers = 1
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	for _, id := range []string{"codex-lost", "codex-model-limit"} {
		backend.usage["token-"+id] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
		backend.usageAfter["token-"+id] = usageBody(false, 0, usageWindow{0, 7 * 24 * time.Hour, now.Add(7 * 24 * time.Hour)})
		backend.credits["token-"+id] = creditsBody(resetCreditFixture{id: "credit-" + id, expires: now.Add(9 * 24 * time.Hour)})
	}
	backend.usage["token-codex-model-limit"] = usageBody(false, 1, usageWindow{40, 7 * 24 * time.Hour, weeklyReset})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexModelCooldown(t, manager, "codex-lost", weeklyReset)
	registerCodexModelCooldown(t, manager, "codex-model-limit", weeklyReset)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)

	service.checkCodexResetCredits(context.Background(), states, now)
	service.checkCodexResetCredits(context.Background(), states, now.Add(codexResetCreditsWaitBackoff+10*time.Second))

	if consumed := backend.consumes(); len(consumed) != 1 || consumed[0].creditID != "credit-codex-lost" {
		t.Fatalf("consume = %+v, want one, for the login whose answer was lost", consumed)
	}
	if lost, _ := manager.GetByID("codex-lost"); codexQuotaCooldownHeld(lost, time.Now()) {
		t.Fatal("the model cooldown left by a lost answer must be lifted")
	}
	if limited, _ := manager.GetByID("codex-model-limit"); !codexQuotaCooldownHeld(limited, time.Now()) {
		t.Fatal("a model cooldown without a recent reset attempt must stay")
	}
}

// An answer that does not say whether the login is limited proves nothing: the
// cooldown stays and nothing is redeemed.
func TestForkCodexResetCreditsIgnoresUnknownUsage(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	for name, body := range map[string]string{
		"empty":           `{}`,
		"null rate limit": `{"rate_limit": null, "rate_limit_reset_credits": {"available_count": 1}}`,
		"missing flags":   `{"rate_limit": {"primary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_at": 1}}}`,
		"not json":        `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			backend := newResetBackend(t)
			now := time.Now().Truncate(time.Second)
			backend.usage["token-codex-team"] = body
			backend.credits["token-codex-team"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(24 * time.Hour)})
			manager := coreauth.NewManager(nil, nil, nil)
			registerCodexLogin(t, manager, "codex-team", now.Add(4*24*time.Hour), time.Time{}, nil)
			service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
			states := make(map[string]*codexResetCreditState)

			service.checkCodexResetCredits(context.Background(), states, now)

			if team, _ := manager.GetByID("codex-team"); !codexCredentialQuotaHeld(team, time.Now()) {
				t.Fatal("an unknown usage answer lifted the cooldown")
			}
			if _, list := backend.calls("token-codex-team"); list != 0 || len(backend.consumes()) != 0 {
				t.Fatalf("an unknown usage answer led to %d list and %d consume request(s)", list, len(backend.consumes()))
			}
			assertNextCheck(t, states, "codex-team", now.Add(codexResetCreditsErrorBackoff))
		})
	}
}

// A refusal recorded while the usage was being read is newer than that usage, and a
// setting changed meanwhile applies: in both cases the cooldown stays.
func TestForkCodexResetCreditsRechecksBeforeLifting(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	now := time.Now().Truncate(time.Second)
	for name, change := range map[string]func(*testing.T, *Service, *coreauth.Manager){
		"refusal during the read": func(t *testing.T, _ *Service, manager *coreauth.Manager) {
			retry := 3 * time.Hour
			manager.MarkResult(context.Background(), coreauth.Result{
				AuthID: "codex-team", Provider: "codex", Model: "gpt-6-astra",
				Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "quota exceeded"},
				RetryAfter: &retry, CredentialScope: true,
			})
		},
		"mode off": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			service.cfgMu.Lock()
			service.cfg = resetCreditsConfig("off")
			service.cfgMu.Unlock()
		},
		"dry run": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			service.cfgMu.Lock()
			service.cfg = resetCreditsConfig("dry-run")
			service.cfgMu.Unlock()
		},
		"login left auth-files": func(_ *testing.T, service *Service, _ *coreauth.Manager) {
			cfg := resetCreditsConfig("on")
			cfg.Codex.ResetCredits.AuthFiles = []string{"codex-other.json"}
			service.cfgMu.Lock()
			service.cfg = cfg
			service.cfgMu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			backend := newResetBackend(t)
			backend.usage["token-codex-team"] = usageBody(false, 0, usageWindow{40, 7 * 24 * time.Hour, now.Add(4 * 24 * time.Hour)})
			manager := coreauth.NewManager(nil, nil, nil)
			registerCodexLogin(t, manager, "codex-team", now.Add(4*24*time.Hour), time.Time{}, nil)
			service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
			backend.onUsage = func() { change(t, service, manager) }

			service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

			if team, _ := manager.GetByID("codex-team"); !codexQuotaCooldownHeld(team, time.Now()) {
				t.Fatal("the cooldown was lifted despite the change during the usage read")
			}
		})
	}
}

// A login ChatGPT reports free but that is refused again soon after its cooldown was
// lifted keeps the new cooldown, until the lift gap has passed.
func TestForkCodexResetCreditsDoesNotFlap(t *testing.T) {
	coreauth.SetQuotaCooldownDisabled(false)
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	backend.usage["token-codex-team"] = usageBody(false, 0, usageWindow{40, 7 * 24 * time.Hour, now.Add(4 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-team", now.Add(4*24*time.Hour), time.Time{}, nil)
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	states := make(map[string]*codexResetCreditState)
	refuse := func() {
		retry := 3 * time.Hour
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID: "codex-team", Provider: "codex", Model: "gpt-6-astra",
			Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "quota exceeded"},
			RetryAfter: &retry, CredentialScope: true,
		})
		if team, _ := manager.GetByID("codex-team"); !codexCredentialQuotaHeld(team, time.Now()) {
			t.Fatal("precondition: a usage_limit_reached refusal holds the credential")
		}
	}

	service.checkCodexResetCredits(context.Background(), states, now)
	if team, _ := manager.GetByID("codex-team"); codexQuotaCooldownHeld(team, time.Now()) {
		t.Fatal("the first lift did not happen")
	}

	refuse()
	again := now.Add(codexResetCreditsWaitBackoff + 10*time.Second)
	service.checkCodexResetCredits(context.Background(), states, again)
	if team, _ := manager.GetByID("codex-team"); !codexQuotaCooldownHeld(team, time.Now()) {
		t.Fatal("a login refused again right after a lift must keep its cooldown")
	}

	later := now.Add(codexResetCreditsLiftGap + time.Minute)
	service.checkCodexResetCredits(context.Background(), states, later)
	if team, _ := manager.GetByID("codex-team"); codexQuotaCooldownHeld(team, time.Now()) {
		t.Fatal("after the lift gap the cooldown of a free login must be lifted again")
	}
}

// The consume request goes out with the credential as it is when the reset is spent,
// so a token refreshed during the check is the one used.
func TestForkCodexResetCreditsUsesFreshCredential(t *testing.T) {
	backend := newResetBackend(t)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(4 * 24 * time.Hour)
	backend.usage["token-codex-pro"] = usageBody(true, 1, usageWindow{100, 7 * 24 * time.Hour, weeklyReset})
	backend.credits["token-codex-pro"] = creditsBody(resetCreditFixture{id: "credit-oct", expires: now.Add(9 * 24 * time.Hour)})
	manager := coreauth.NewManager(nil, nil, nil)
	registerCodexLogin(t, manager, "codex-pro", time.Time{}, now, weeklyExhaustedSignals(weeklyReset))
	service := &Service{cfg: resetCreditsConfig("on"), coreManager: manager}
	backend.onList = func() {
		auth, _ := manager.GetByID("codex-pro")
		auth.Metadata["access_token"] = "token-refreshed"
		if _, errUpdate := manager.Update(coreauth.WithSkipPersist(context.Background()), auth); errUpdate != nil {
			t.Errorf("refresh the token: %v", errUpdate)
		}
	}

	service.checkCodexResetCredits(context.Background(), make(map[string]*codexResetCreditState), now)

	if consumed := backend.consumes(); len(consumed) != 1 || consumed[0].token != "token-refreshed" {
		t.Fatalf("consume = %+v, want one with the refreshed token", consumed)
	}
}

func headerOf(signals map[string]string) http.Header {
	headers := make(http.Header)
	for name, value := range signals {
		headers.Set(name, value)
	}
	return headers
}
