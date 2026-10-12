package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector is a fill-first selector that drains credentials in the
// order their quota expires instead of in credential ID order.
//
// Subscription quota is use-it-or-lose-it: whatever is left in a window when it
// resets is gone. Preferring the credential whose longest running window (the
// weekly budget) resets soonest spends the quota that is about to expire first
// and leaves the credentials with the most time left for later.
//
// Ordering, first match wins:
//  1. Credentials with a fully consumed running window go last, the one that
//     recovers first leading.
//  2. The soonest reset of the longest running window, unknown resets last.
//  3. The soonest reset of the shortest running window, unknown resets last.
//  4. Credential ID, so the choice is stable when nothing is known yet.
//
// A window that has already rolled over counts as unknown: the provider only
// starts the next one when the credential is used again, so there is nothing to
// lose by leaving it for later.
type SoonestResetSelector struct{}

// Pick selects the available credential whose quota expires soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	picked := pickSoonestReset(available, now)
	if picked != nil && len(available) > 1 {
		horizon := quotaResetHorizonFor(picked, now)
		selectorLogEntry(ctx).Debugf("soonest-reset: picked auth=%s provider=%s candidates=%d long_reset=%s short_reset=%s drained_until=%s",
			picked.ID, picked.Provider, len(available), formatResetInstant(horizon.long), formatResetInstant(horizon.short), formatResetInstant(horizon.drainedUntil))
	}
	return picked, nil
}

// quotaResetHorizon summarises the running credential-wide quota windows that
// drive soonest-reset ordering. A zero time means unknown.
type quotaResetHorizon struct {
	// long is when the longest running window resets.
	long time.Time
	// short is when the shortest running window resets, when it is a different window.
	short time.Time
	// drainedUntil is when the last fully consumed running window resets.
	drainedUntil time.Time
}

func quotaResetHorizonFor(auth *Auth, now time.Time) quotaResetHorizon {
	var horizon quotaResetHorizon
	if auth == nil {
		return horizon
	}
	windows := QuotaWindows(auth.Provider, auth.Quota)
	if len(windows) == 0 {
		return horizon
	}
	for _, window := range windows {
		if window.Used >= 1 && window.ResetAt.After(now) && window.ResetAt.After(horizon.drainedUntil) {
			horizon.drainedUntil = window.ResetAt
		}
	}
	// QuotaWindows orders windows from shortest to longest. The longest window is
	// chosen by length, not among the running ones: once it rolls over, a still
	// running session window must not stand in for it.
	if longest := windows[len(windows)-1]; longest.ResetAt.After(now) {
		horizon.long = longest.ResetAt
	}
	if len(windows) > 1 {
		if shortest := windows[0]; shortest.ResetAt.After(now) {
			horizon.short = shortest.ResetAt
		}
	}
	return horizon
}

func pickSoonestReset(available []*Auth, now time.Time) *Auth {
	var picked *Auth
	var pickedHorizon quotaResetHorizon
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		horizon := quotaResetHorizonFor(candidate, now)
		if picked == nil || soonestResetLess(horizon, candidate.ID, pickedHorizon, picked.ID) {
			picked = candidate
			pickedHorizon = horizon
		}
	}
	return picked
}

func soonestResetLess(a quotaResetHorizon, aID string, b quotaResetHorizon, bID string) bool {
	aDrained, bDrained := !a.drainedUntil.IsZero(), !b.drainedUntil.IsZero()
	if aDrained != bDrained {
		return !aDrained
	}
	if aDrained && !a.drainedUntil.Equal(b.drainedUntil) {
		return a.drainedUntil.Before(b.drainedUntil)
	}
	if less, decided := earlierKnownInstant(a.long, b.long); decided {
		return less
	}
	if less, decided := earlierKnownInstant(a.short, b.short); decided {
		return less
	}
	return aID < bID
}

// earlierKnownInstant orders known instants before unknown ones and earlier
// before later. decided is false when the two do not differ.
func earlierKnownInstant(a, b time.Time) (less bool, decided bool) {
	switch {
	case a.IsZero() && b.IsZero():
		return false, false
	case a.IsZero():
		return false, true
	case b.IsZero():
		return true, true
	case a.Equal(b):
		return false, false
	default:
		return a.Before(b), true
	}
}

func formatResetInstant(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339)
}
