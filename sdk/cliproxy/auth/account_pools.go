package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type accountPoolsContextKey struct{}

// AccountPoolPolicy is an immutable request-local authorization snapshot.
type AccountPoolPolicy struct {
	groups    []config.ClientAccountPool
	providers map[string]bool
	namespace string
}

// ResolveAccountPools resolves the longest matching client scope. Unknown pool
// references fail closed; an empty pool is useful while provisioning a new login.
func ResolveAccountPools(cfg config.ClientConfig, clientKey string) (*AccountPoolPolicy, error) {
	if cfg.KeyScopes == nil || clientKey == "" {
		return nil, nil
	}
	var selected *config.ClientKeyScope
	for i := range *cfg.KeyScopes {
		scope := &(*cfg.KeyScopes)[i]
		prefix := strings.TrimSpace(scope.KeyPrefix)
		if prefix == "" || !strings.HasPrefix(clientKey, prefix) {
			continue
		}
		if len(scope.Providers) == 0 && scope.Pools == nil {
			continue
		}
		if selected == nil || len(prefix) > len(strings.TrimSpace(selected.KeyPrefix)) {
			selected = scope
		}
	}
	if selected == nil || selected.Pools == nil {
		return nil, nil
	}
	policy := &AccountPoolPolicy{providers: make(map[string]bool)}
	for _, provider := range selected.Providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		switch provider {
		case "anthropic":
			provider = "claude"
		case "openai", "chatgpt":
			provider = "codex"
		case "muse":
			provider = "meta"
		case "grok":
			provider = "xai"
		}
		if provider != "" {
			policy.providers[provider] = true
		}
	}
	if len(policy.providers) == 0 {
		return nil, fmt.Errorf("account-pool scope must name providers")
	}
	groups := make(map[string]config.ClientAccountPool)
	if cfg.AccountPools != nil {
		for _, group := range *cfg.AccountPools {
			if group.Name == "" || strings.TrimSpace(group.Name) != group.Name {
				return nil, fmt.Errorf("invalid account-pool name")
			}
			if _, exists := groups[group.Name]; exists {
				return nil, fmt.Errorf("duplicate account-pool name")
			}
			if group.AuthKind != "" && group.AuthKind != AuthKindOAuth && group.AuthKind != AuthKindAPIKey {
				return nil, fmt.Errorf("invalid account-pool auth-kind")
			}
			group.AuthIDs = slices.Clone(group.AuthIDs)
			group.AuthFiles = slices.Clone(group.AuthFiles)
			groups[group.Name] = group
		}
	}
	seen := make(map[string]bool)
	for _, name := range *selected.Pools {
		group, ok := groups[name]
		if !ok || seen[name] {
			return nil, fmt.Errorf("unknown or repeated account-pool reference")
		}
		seen[name] = true
		policy.groups = append(policy.groups, group)
	}
	encoded, _ := json.Marshal(struct {
		Groups    []config.ClientAccountPool
		Providers map[string]bool
	}{policy.groups, policy.providers})
	digest := sha256.Sum256(append([]byte(clientKey+"\x00"), encoded...))
	policy.namespace = hex.EncodeToString(digest[:])
	return policy, nil
}

// WithAccountPools attaches a server-resolved policy. Callers cannot supply it
// through request headers or model names. A nil policy clears an older snapshot.
func WithAccountPools(ctx context.Context, policy *AccountPoolPolicy) context.Context {
	return context.WithValue(ctx, accountPoolsContextKey{}, policy)
}

func accountPoolsFromContext(ctx context.Context) *AccountPoolPolicy {
	if ctx == nil {
		return nil
	}
	policy, _ := ctx.Value(accountPoolsContextKey{}).(*AccountPoolPolicy)
	return policy
}

// HasAccountPools reports whether the request has account-level restrictions.
func HasAccountPools(ctx context.Context) bool { return accountPoolsFromContext(ctx) != nil }

// Allows reports membership independently of transient quota/cooldown state.
func (p *AccountPoolPolicy) Allows(a *Auth) bool { return p == nil || p.tier(a) >= 0 }

func (p *AccountPoolPolicy) tier(a *Auth) int {
	if a == nil || !p.providers[strings.ToLower(a.Provider)] {
		return -1
	}
	for i, group := range p.groups {
		if group.AuthKind != "" && a.AuthKind() != group.AuthKind {
			continue
		}
		if slices.Contains(group.AuthIDs, a.ID) || (a.FileName != "" && slices.Contains(group.AuthFiles, filepath.Base(a.FileName))) {
			return i
		}
	}
	return -1
}

// availableAuthsForPoolSelector keeps quota, cooldown and concurrency on the
// original credential. Pool order takes precedence over credential priority;
// affinity is retained within the first currently available pool.
func (m *Manager) availableAuthsForPoolSelector(ctx context.Context, selector Selector, auths []*Auth, provider, model string, now time.Time) ([]*Auth, []*Auth, error) {
	policy := accountPoolsFromContext(ctx)
	if policy == nil {
		return m.availableAuthsForSelector(selector, auths, provider, model, now)
	}
	weightedSelector := selector
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		weightedSelector = affinity.fallback
	}
	if _, weighted := weightedSelector.(*WeightedRoundRobinSelector); weighted {
		auths = positiveWeightAuths(auths)
	}
	available, err := m.availableAuthsForRouteModelAcrossPriorities(auths, provider, model, now)
	if err != nil {
		return nil, nil, err
	}
	best := len(policy.groups)
	var tierAuths []*Auth
	for _, a := range available {
		tier := policy.tier(a)
		if tier < 0 || tier > best {
			continue
		}
		if tier < best {
			best, tierAuths = tier, nil
		}
		tierAuths = append(tierAuths, a)
	}
	return m.availableAuthsForSelector(selector, tierAuths, provider, model, now)
}

func accountPoolAffinityNamespace(provider string, metadata map[string]any) string {
	if namespace, _ := metadata[cliproxyexecutor.AccountPoolScopeMetadataKey].(string); namespace != "" {
		return provider + "::pool:" + namespace
	}
	return provider
}
