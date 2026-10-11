package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// accountPoolMiddleware applies the parts of the client key policy that model
// resolution cannot. It attaches the key's account pools to the request and
// refuses pool keys on endpoints whose credential-selection path has not been
// checked for pool enforcement, so new endpoints fail closed. On the endpoints
// that always use a Codex credential, where no model is resolved, it also
// applies the key's provider scope. Unpooled and unscoped keys retain their
// behavior.
func (s *Server) accountPoolMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, errMsg := s.handlers.AccountPoolContext(c.Request.Context(), c)
		if errMsg != nil {
			c.AbortWithStatusJSON(errMsg.StatusCode, gin.H{"error": gin.H{"message": errMsg.Error.Error(), "code": "account_pool_unavailable"}})
			return
		}
		method, path := c.Request.Method, c.FullPath()
		if coreauth.HasAccountPools(ctx) && !accountPoolRouteSupported(method, path) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "This endpoint does not support account-pool keys", "code": "account_pool_endpoint_unsupported"}})
			return
		}
		if codexOnlyRoute(method, path) && !s.keyAllowsCodex(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{
				"message": "Codex is not available to this API key",
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "provider_not_allowed",
			}})
			return
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func accountPoolRouteSupported(method, path string) bool {
	if codexOnlyRoute(method, path) {
		return true
	}
	if method == http.MethodGet {
		return path == "/v1/models" || path == "/v1/responses" || path == "/backend-api/codex/responses"
	}
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/completions",
		"/v1/responses", "/v1/responses/compact",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact":
		return true
	}
	return false
}

// codexOnlyRoutes always use a Codex credential: Codex web search and voice.
// Search selects through the pool-aware scheduler. The voice handlers select a
// Codex OAuth login the same way, and a call's sideband and hangup are pinned
// to the login that created the call and to its creator, so a pool can only
// narrow them. The Realtime capabilities the Codex upstream lacks
// (transcription and translation sessions, SIP control) answer 501 without
// selecting a credential.
var codexOnlyRoutes = map[string]bool{
	"POST /v1/alpha/search":                         true,
	"POST /backend-api/codex/alpha/search":          true,
	"POST /v1/live":                                 true,
	"GET /v1/live/:call_id":                         true,
	"GET /v1/realtime":                              true,
	"POST /v1/realtime":                             true,
	"POST /v1/realtime/calls":                       true,
	"GET /v1/realtime/calls/:call_id":               true,
	"POST /v1/realtime/calls/:call_id/hangup":       true,
	"POST /v1/realtime/calls/:call_id/accept":       true,
	"POST /v1/realtime/calls/:call_id/reject":       true,
	"POST /v1/realtime/calls/:call_id/refer":        true,
	"POST /v1/realtime/client_secrets":              true,
	"POST /v1/realtime/sessions":                    true,
	"POST /v1/realtime/transcription_sessions":      true,
	"GET /v1/realtime/translations":                 true,
	"POST /v1/realtime/translations":                true,
	"POST /v1/realtime/translations/client_secrets": true,
}

func codexOnlyRoute(method, path string) bool { return codexOnlyRoutes[method+" "+path] }

// keyAllowsCodex applies the key's provider scope to a Codex-only route. A
// Realtime client secret is checked against the key that minted it. Key scopes
// do not apply in Home mode, where Home selects the credential.
func (s *Server) keyAllowsCodex(c *gin.Context) bool {
	if s.handlers == nil || s.handlers.AuthManager.HomeEnabled() {
		return true
	}
	return s.handlers.KeyAllowsProvider(c, "codex")
}
