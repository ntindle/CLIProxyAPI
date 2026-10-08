package auth

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	claudeQuotaSignalPrefix = "anthropic-ratelimit-unified-"
	codexQuotaSignalPrefix  = "x-codex-"
)

// QuotaWindow is one credential-wide upstream rate-limit window parsed from the
// passive quota signals observed for a credential.
type QuotaWindow struct {
	// Name is the provider label for the window, for example "5h", "7d" or "primary".
	Name string
	// Duration is the window length. Zero means the provider did not report one.
	Duration time.Duration
	// Used is the consumed fraction, where 1 means fully consumed. Negative means unknown.
	Used float64
	// ResetAt is when the window resets. Zero means unknown.
	ResetAt time.Time
}

// QuotaWindows parses the credential-wide rate-limit windows carried by the
// latest observed quota signals. Windows scoped to a single model or feature
// (for example the Claude "7d_oi" window or the Codex additional limits) are
// omitted because they do not describe the credential as a whole.
//
// The result is ordered from the shortest to the longest window. It is nil when
// nothing has been observed or the provider reports no window data.
func QuotaWindows(provider string, quota QuotaState) []QuotaWindow {
	if len(quota.Signals) == 0 {
		return nil
	}
	var windows []QuotaWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows = claudeQuotaWindows(quota.Signals)
	case "codex":
		windows = codexQuotaWindows(quota.Signals, quota.ObservedAt)
	case "meta":
		windows = metaQuotaWindows(quota.Signals)
	default:
		return nil
	}
	if len(windows) > 1 {
		sort.SliceStable(windows, func(i, j int) bool {
			if windows[i].Duration != windows[j].Duration {
				return windows[i].Duration < windows[j].Duration
			}
			return windows[i].Name < windows[j].Name
		})
	}
	return windows
}

func claudeQuotaWindows(signals map[string]string) []QuotaWindow {
	byName := make(map[string]*QuotaWindow)
	for key, raw := range signals {
		rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(key)), claudeQuotaSignalPrefix)
		if !ok {
			continue
		}
		split := strings.LastIndex(rest, "-")
		if split <= 0 {
			continue
		}
		name, field := rest[:split], rest[split+1:]
		duration, okDuration := quotaWindowLabelDuration(name)
		if !okDuration {
			// Not a window label: "status", "reset", "overage-status" and similar
			// credential-level fields share the prefix.
			continue
		}
		window := byName[name]
		if window == nil {
			window = &QuotaWindow{Name: name, Duration: duration, Used: -1}
			byName[name] = window
		}
		value := strings.TrimSpace(raw)
		switch field {
		case "reset":
			if resetAt, okReset := parseQuotaResetInstant(value); okReset {
				window.ResetAt = resetAt
			}
		case "utilization":
			if used, okUsed := parseQuotaFraction(value); okUsed && used > window.Used {
				window.Used = used
			}
		case "status":
			if strings.EqualFold(value, "rejected") && window.Used < 1 {
				window.Used = 1
			}
		}
	}
	windows := make([]QuotaWindow, 0, len(byName))
	for _, window := range byName {
		windows = append(windows, *window)
	}
	return windows
}

func codexQuotaWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	lower := make(map[string]string, len(signals))
	for key, raw := range signals {
		lower[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(raw)
	}
	windows := make([]QuotaWindow, 0, 2)
	for _, name := range []string{"primary", "secondary"} {
		prefix := codexQuotaSignalPrefix + name + "-"
		window := QuotaWindow{Name: name, Used: -1}
		found := false
		if percent, ok := parseQuotaFraction(lower[prefix+"used-percent"]); ok {
			window.Used = percent / 100
			found = true
		}
		if minutes, errMinutes := strconv.ParseFloat(lower[prefix+"window-minutes"], 64); errMinutes == nil && minutes > 0 && !math.IsInf(minutes, 0) {
			window.Duration = time.Duration(minutes * float64(time.Minute))
		}
		if resetAt, ok := parseQuotaResetInstant(lower[prefix+"reset-at"]); ok {
			window.ResetAt = resetAt
			found = true
		} else if seconds, errSeconds := strconv.ParseFloat(lower[prefix+"reset-after-seconds"], 64); errSeconds == nil && seconds >= 0 && !math.IsInf(seconds, 0) && !observedAt.IsZero() {
			window.ResetAt = observedAt.Add(time.Duration(seconds * float64(time.Second)))
			found = true
		}
		if found {
			windows = append(windows, window)
		}
	}
	return windows
}

// metaQuotaWindows reads the Meta subscription windows recorded from the
// response.subscription_usage stream event. Meta does not report the weekly
// window's length, so it is the rolling week the event names it after.
func metaQuotaWindows(signals map[string]string) []QuotaWindow {
	lower := make(map[string]string, len(signals))
	for key, raw := range signals {
		lower[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(raw)
	}
	windows := make([]QuotaWindow, 0, 2)
	for _, name := range []string{"window", "weekly"} {
		prefix := "x-meta-" + name + "-"
		window := QuotaWindow{Name: name, Used: -1}
		if name == "weekly" {
			window.Duration = 7 * 24 * time.Hour
		} else if minutes, errMinutes := strconv.ParseFloat(lower["x-meta-window-minutes"], 64); errMinutes == nil && minutes > 0 && !math.IsInf(minutes, 0) {
			window.Duration = time.Duration(minutes * float64(time.Minute))
		}
		found := false
		if percent, ok := parseQuotaFraction(lower[prefix+"used-percent"]); ok {
			window.Used = percent / 100
			found = true
		}
		if resetAt, ok := parseQuotaResetInstant(lower[prefix+"reset-at"]); ok {
			window.ResetAt = resetAt
			found = true
		}
		if found {
			windows = append(windows, window)
		}
	}
	return windows
}

// quotaWindowLabelDuration reads the duration from a credential-wide window
// label such as "5h" or "7d". Scoped labels such as "7d_oi" are rejected.
func quotaWindowLabelDuration(label string) (time.Duration, bool) {
	digits := 0
	for digits < len(label) && label[digits] >= '0' && label[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits != len(label)-1 {
		return 0, false
	}
	count, errCount := strconv.Atoi(label[:digits])
	if errCount != nil || count <= 0 {
		return 0, false
	}
	var unit time.Duration
	switch label[digits] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(count) * unit, true
}

// parseQuotaResetInstant accepts unix seconds or an RFC 3339 timestamp.
func parseQuotaResetInstant(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, errSeconds := strconv.ParseFloat(raw, 64); errSeconds == nil {
		if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return time.Time{}, false
		}
		whole := math.Floor(seconds)
		return time.Unix(int64(whole), int64((seconds-whole)*float64(time.Second))), true
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func parseQuotaFraction(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// ObserveQuotaSignals records a quota snapshot obtained outside a proxied
// request, such as a provider usage endpoint poll. Headers use the same names
// as the upstream response headers observed on proxied requests. The snapshot
// is ignored when the credential already holds a newer observation.
func (m *Manager) ObserveQuotaSignals(authID, provider string, headers http.Header, observedAt time.Time) bool {
	if m == nil || strings.TrimSpace(authID) == "" || len(headers) == 0 {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		return false
	}
	if auth.Quota.ObservedAt.After(observedAt) {
		return false
	}
	return auth.Quota.ObserveResponseHeadersForProvider(provider, headers, observedAt)
}
