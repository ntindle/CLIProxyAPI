package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// homeProvider is the routing provider used in Home mode, where credentials
// are selected by the Home control plane and key scopes do not apply.
const homeProvider = "home"

// keyScopeProviderAliases maps the names people use for a provider to the
// provider key credentials are registered under.
var keyScopeProviderAliases = map[string]string{
	"anthropic": "claude",
	"openai":    "codex",
	"chatgpt":   "codex",
	"muse":      "meta",
	"grok":      "xai",
}

func normalizeKeyScopeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if canonical, ok := keyScopeProviderAliases[provider]; ok {
		return canonical
	}
	return provider
}

// keyScope returns the providers the client API key of the request may use.
// scoped is false when no client.key-scopes entry matches the key, in which
// case the key is unrestricted. When several entries match, the longest
// key-prefix wins.
func (h *BaseAPIHandler) keyScope(c *gin.Context) (allowed map[string]struct{}, scoped bool) {
	if h == nil || h.Cfg == nil || c == nil || h.Cfg.Client.KeyScopes == nil {
		return nil, false
	}
	apiKey := strings.TrimSpace(c.GetString("userApiKey"))
	if apiKey == "" {
		return nil, false
	}
	longest := -1
	for _, scope := range *h.Cfg.Client.KeyScopes {
		prefix := strings.TrimSpace(scope.KeyPrefix)
		if prefix == "" || len(prefix) <= longest || !strings.HasPrefix(apiKey, prefix) {
			continue
		}
		providers := make(map[string]struct{}, len(scope.Providers))
		for _, provider := range scope.Providers {
			if normalized := normalizeKeyScopeProvider(provider); normalized != "" {
				providers[normalized] = struct{}{}
			}
		}
		// An entry that names no provider is a configuration mistake, not a lockout.
		if len(providers) == 0 {
			continue
		}
		allowed, longest = providers, len(prefix)
	}
	return allowed, longest >= 0
}

// KeyAllowsProvider reports whether the request's client API key may use the
// provider. Routes that serve a single provider without resolving a model call
// it; an unrestricted key may use every provider.
func (h *BaseAPIHandler) KeyAllowsProvider(c *gin.Context, provider string) bool {
	allowed, scoped := h.keyScope(c)
	if !scoped {
		return true
	}
	_, ok := allowed[normalizeKeyScopeProvider(provider)]
	return ok
}

// scopeProviders narrows the providers resolved for a request to the ones the
// request's client API key may use. It wraps providersForExecution, so every
// execution path applies the same rule. A model that only other providers
// serve is reported as not found, the answer an unknown model gets.
func (h *BaseAPIHandler) scopeProviders(ctx context.Context, modelName string) func([]string, string, *interfaces.ErrorMessage) ([]string, string, *interfaces.ErrorMessage) {
	return func(providers []string, normalizedModel string, errMsg *interfaces.ErrorMessage) ([]string, string, *interfaces.ErrorMessage) {
		if errMsg != nil || len(providers) == 0 || ctx == nil {
			return providers, normalizedModel, errMsg
		}
		ginCtx, _ := ctx.Value("gin").(*gin.Context)
		allowed, scoped := h.keyScope(ginCtx)
		if !scoped {
			return providers, normalizedModel, errMsg
		}
		kept := make([]string, 0, len(providers))
		for _, provider := range providers {
			normalized := strings.ToLower(strings.TrimSpace(provider))
			if normalized == homeProvider {
				return providers, normalizedModel, errMsg
			}
			if _, ok := allowed[normalized]; ok {
				kept = append(kept, provider)
			}
		}
		if len(kept) > 0 {
			return kept, normalizedModel, nil
		}
		body := `{"error":{"message":"","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
		body, errSet := sjson.Set(body, "error.message", "model "+modelName+" is not available to this API key")
		if errSet != nil {
			body = `{"error":{"message":"model is not available to this API key","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
		}
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(body)}
	}
}

// scopeModelList removes from a model-list response the models the request's
// client API key may not use. It understands every list shape the proxy
// serves: an array under "data" (OpenAI, Anthropic, Grok) or under "models"
// (Codex, Gemini), with the model named by id, slug, model or name.
func (h *BaseAPIHandler) scopeModelList(c *gin.Context, body []byte) []byte {
	allowed, scoped := h.keyScope(c)
	if !scoped || !gjson.ValidBytes(body) {
		return body
	}
	modelRegistry := registry.GetGlobalRegistry()
	for _, listKey := range []string{"data", "models"} {
		list := gjson.GetBytes(body, listKey)
		if !list.IsArray() {
			continue
		}
		kept := make([]string, 0, len(list.Array()))
		firstID, lastID := "", ""
		list.ForEach(func(_, entry gjson.Result) bool {
			id := modelListEntryID(entry)
			if id == "" {
				return true
			}
			if !h.accountPoolModelAllowed(c, id) {
				return true
			}
			for _, provider := range modelRegistry.GetModelProviders(id) {
				if _, ok := allowed[strings.ToLower(provider)]; ok {
					kept = append(kept, entry.Raw)
					if firstID == "" {
						firstID = id
					}
					lastID = id
					break
				}
			}
			return true
		})
		scopedBody, errSet := sjson.SetRawBytes(body, listKey, []byte("["+strings.Join(kept, ",")+"]"))
		if errSet != nil {
			return body
		}
		body = scopedBody
		// The Anthropic list names its first and last entries.
		for field, value := range map[string]string{"first_id": firstID, "last_id": lastID} {
			if gjson.GetBytes(body, field).Exists() {
				if updated, errField := sjson.SetBytes(body, field, value); errField == nil {
					body = updated
				}
			}
		}
	}
	return body
}

func modelListEntryID(entry gjson.Result) string {
	for _, field := range []string{"id", "slug", "model", "name"} {
		if value := strings.TrimSpace(entry.Get(field).String()); value != "" {
			return strings.TrimPrefix(value, "models/")
		}
	}
	return ""
}
