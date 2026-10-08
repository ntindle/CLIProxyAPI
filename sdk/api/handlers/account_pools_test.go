package handlers

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestForkAccountPoolsModelListAndPolicyRefresh(t *testing.T) {
	h := keyScopeTestHandler(t)
	h.AuthManager = coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"key-scope-claude-client", "pool-personal"} {
		if _, err := h.AuthManager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	registry.GetGlobalRegistry().RegisterClient("pool-personal", "claude", []*registry.ModelInfo{{ID: "pool-personal-only"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("pool-personal") })
	chain := []string{"reviewer"}
	h.Cfg.Client.AccountPools = &[]config.ClientAccountPool{{Name: "reviewer", AuthIDs: []string{"key-scope-claude-client"}}}
	h.Cfg.Client.KeyScopes = &[]config.ClientKeyScope{{KeyPrefix: "review-", Providers: []string{"claude"}, Pools: &chain}}
	c, _ := keyScopeRequestContext("review-key")
	body := []byte(`{"data":[{"id":"` + keyScopeClaudeModel + `"},{"id":"pool-personal-only"}]}`)
	filtered := h.scopeModelList(c, body)
	if gjson.GetBytes(filtered, "data.#").Int() != 1 || gjson.GetBytes(filtered, "data.0.id").String() != keyScopeClaudeModel {
		t.Fatalf("model list leaked: %s", filtered)
	}
	ctx := context.WithValue(context.Background(), "gin", c)
	ctx, errMsg := h.accountPoolExecutionContext(ctx)
	if errMsg != nil || !coreauth.HasAccountPools(ctx) {
		t.Fatalf("policy missing: %v", errMsg)
	}
	// A socket keeps its Gin context, but each turn must resolve the current config.
	missing := []string{"removed"}
	(*h.Cfg.Client.KeyScopes)[0].Pools = &missing
	if _, errMsg = h.accountPoolExecutionContext(ctx); errMsg == nil {
		t.Fatal("old policy survived revocation")
	}
	(*h.Cfg.Client.KeyScopes)[0].Pools = nil
	ctx, errMsg = h.accountPoolExecutionContext(ctx)
	if errMsg != nil || coreauth.HasAccountPools(ctx) {
		t.Fatal("old policy survived scope removal")
	}
}
