package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// AccountPoolContext refreshes the policy for each request, including every turn
// on an existing WebSocket. It intentionally uses the authenticated principal.
func (h *BaseAPIHandler) AccountPoolContext(ctx context.Context, c *gin.Context) (context.Context, *interfaces.ErrorMessage) {
	if h == nil || h.Cfg == nil || c == nil {
		return ctx, nil
	}
	policy, err := coreauth.ResolveAccountPools(h.Cfg.Client, c.GetString("userApiKey"))
	if err != nil {
		return ctx, poolError(err)
	}
	if policy != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return ctx, poolError(errors.New("client account pools are unavailable in Home mode"))
	}
	return coreauth.WithAccountPools(ctx, policy), nil
}

func (h *BaseAPIHandler) accountPoolExecutionContext(ctx context.Context) (context.Context, *interfaces.ErrorMessage) {
	if ctx == nil {
		return context.Background(), nil
	}
	c, _ := ctx.Value("gin").(*gin.Context)
	return h.AccountPoolContext(ctx, c)
}

func poolError(err error) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: err}
}

func (h *BaseAPIHandler) accountPoolModelAllowed(c *gin.Context, model string) bool {
	policy, err := coreauth.ResolveAccountPools(h.Cfg.Client, c.GetString("userApiKey"))
	if err != nil {
		return false
	}
	if policy == nil {
		return true
	}
	if h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		return false
	}
	for _, a := range h.AuthManager.List() {
		if !a.Disabled && policy.Allows(a) && registry.GetGlobalRegistry().ClientSupportsModel(a.ID, model) {
			return true
		}
	}
	return false
}
