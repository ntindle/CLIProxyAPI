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
	mu          sync.Mutex
	usage       map[string]string // token -> usage body while blocked
	usageAfter  map[string]string // token -> usage body after a reset
	credits     map[string]string // token -> credit list body
	consumeCode []string          // answers to consume, in order; the last one repeats
	consumeFail int               // consume requests to fail with 500 first
	redeemed    map[string]bool
	usageCalls  map[string]int
	listCalls   map[string]int
	consumed    []resetConsume
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
		if body, ok := b.usageAfter[token]; ok && b.redeemed[token] {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(b.usage[token]))
	case "/wham/rate-limit-reset-credits":
		b.listCalls[token]++
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
	if !ok || credit.ID != "soon" {
		t.Fatalf("soonest credit = %+v, %t; want soon", credit, ok)
	}
	if credit, ok := soonestCodexResetCredit([]byte(creditsBody(resetCreditFixture{id: "never"}))); !ok || credit.ID != "never" {
		t.Fatalf("credit without expiry = %+v, %t; want it when it is the only one", credit, ok)
	}
	if _, ok := soonestCodexResetCredit([]byte(`{"available_count": 0, "credits": []}`)); ok {
		t.Fatal("an empty list must have no credit")
	}
	if _, ok := soonestCodexResetCredit([]byte(`not json`)); ok {
		t.Fatal("an unreadable list must have no credit")
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
	nextCheck := make(map[string]time.Time)

	service.checkCodexResetCredits(context.Background(), nextCheck, now)

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
	if next := nextCheck["codex-pro"]; !next.Equal(now.Add(codexResetCreditsRedeemGap)) {
		t.Fatalf("next check = %s, want %s", next, now.Add(codexResetCreditsRedeemGap))
	}

	// Within the redeem gap nothing is asked, even if the headers still say blocked.
	manager.ObserveQuotaSignals("codex-pro", "codex", headerOf(weeklyExhaustedSignals(weeklyReset)), now.Add(time.Minute))
	service.checkCodexResetCredits(context.Background(), nextCheck, now.Add(2*time.Minute))
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

	service.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)

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

	service.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)

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

	service.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)

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

	service.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)

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
	off.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)
	for _, id := range []string{"codex-free", "codex-listed", "codex-unlisted"} {
		if usage, list := backend.calls("token-" + id); usage != 0 || list != 0 {
			t.Fatalf("with the feature off, %s got %d usage and %d list request(s)", id, usage, list)
		}
	}

	cfg := resetCreditsConfig("on")
	cfg.Codex.ResetCredits.AuthFiles = []string{"codex-free.json", "codex-listed.json"}
	service := &Service{cfg: cfg, coreManager: manager}
	service.checkCodexResetCredits(context.Background(), make(map[string]time.Time), now)

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
	nextCheck := make(map[string]time.Time)

	service.checkCodexResetCredits(context.Background(), nextCheck, now)
	if next := nextCheck["codex-pro"]; !next.Equal(now.Add(codexResetCreditsErrorBackoff)) {
		t.Fatalf("next check after a failed redemption = %s, want the error backoff", next)
	}
	service.checkCodexResetCredits(context.Background(), nextCheck, now.Add(codexResetCreditsErrorBackoff))

	consumed := backend.consumes()
	if len(consumed) != 2 || consumed[0].requestID == "" || consumed[0].requestID != consumed[1].requestID {
		t.Fatalf("consume requests = %+v, want two with one request id", consumed)
	}
	retried := now.Add(codexResetCreditsErrorBackoff)
	if next := nextCheck["codex-pro"]; !next.Equal(retried.Add(codexResetCreditsRedeemGap)) {
		t.Fatalf("next check = %s, want the redeem gap: already_redeemed counts as a redemption", next)
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
	nextCheck := make(map[string]time.Time)

	service.checkCodexResetCredits(context.Background(), nextCheck, now)

	auth, _ := manager.GetByID("codex-pro")
	if !codexInCooldown(auth, now) {
		t.Fatal("nothing_to_reset must leave the cooldown in place")
	}
	if next := nextCheck["codex-pro"]; !next.Equal(now.Add(codexResetCreditsWaitBackoff)) {
		t.Fatalf("next check = %s, want the wait backoff: nothing_to_reset is not a redemption", next)
	}
}

// The usage endpoint has the last word: a login the proxy still holds in a cooldown
// that ChatGPT no longer limits, or that no reset applies to, is not redeemed.
func TestForkCodexResetCreditsTrustsUsageEndpoint(t *testing.T) {
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
	nextCheck := make(map[string]time.Time)

	service.checkCodexResetCredits(context.Background(), nextCheck, now)

	for _, id := range []string{"codex-free", "codex-none"} {
		if usage, list := backend.calls("token-" + id); usage != 1 || list != 0 {
			t.Fatalf("%s: %d usage and %d list request(s), want 1 and 0", id, usage, list)
		}
	}
	if consumed := backend.consumes(); len(consumed) != 0 {
		t.Fatalf("consume = %+v, want none", consumed)
	}
	if next := nextCheck["codex-free"]; !next.Equal(now.Add(codexResetCreditsWaitBackoff)) {
		t.Fatalf("next check for a login ChatGPT no longer limits = %s, want the wait backoff", next)
	}
	if next := nextCheck["codex-none"]; !next.Equal(now.Add(codexResetCreditsNoCreditBackoff)) {
		t.Fatalf("next check without an applicable reset = %s, want the no-credit backoff", next)
	}
}

func headerOf(signals map[string]string) http.Header {
	headers := make(http.Header)
	for name, value := range signals {
		headers.Set(name, value)
	}
	return headers
}
