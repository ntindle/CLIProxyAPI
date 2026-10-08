package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// accountPoolMiddleware excludes protocols that bypass model account selection.
// Existing unpooled keys retain their behavior. New endpoints fail closed until
// their credential-selection path has been checked for pool enforcement.
func (s *Server) accountPoolMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, errMsg := s.handlers.AccountPoolContext(c.Request.Context(), c)
		if errMsg != nil {
			c.AbortWithStatusJSON(errMsg.StatusCode, gin.H{"error": gin.H{"message": errMsg.Error.Error(), "code": "account_pool_unavailable"}})
			return
		}
		if coreauth.HasAccountPools(ctx) && !accountPoolRouteSupported(c.Request.Method, c.FullPath()) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "This endpoint does not support account-pool keys", "code": "account_pool_endpoint_unsupported"}})
			return
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func accountPoolRouteSupported(method, path string) bool {
	if method == http.MethodGet {
		return path == "/v1/models" || path == "/v1/responses" || path == "/backend-api/codex/responses"
	}
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/completions",
		"/v1/responses", "/v1/responses/compact", "/v1/alpha/search",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact", "/backend-api/codex/alpha/search":
		return true
	}
	return false
}
