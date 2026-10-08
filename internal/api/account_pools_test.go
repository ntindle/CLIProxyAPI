package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestForkAccountPoolsRouteGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chain := []string{"reviewer"}
	cfg := &config.SDKConfig{Client: config.ClientConfig{
		AccountPools: &[]config.ClientAccountPool{{Name: "reviewer"}},
		KeyScopes:    &[]config.ClientKeyScope{{KeyPrefix: "review-", Providers: []string{"claude"}, Pools: &chain}},
	}}
	s := &Server{handlers: handlers.NewBaseAPIHandlers(cfg, nil)}
	for _, tc := range []struct {
		method, path, key string
		want              int
	}{
		{"POST", "/v1/messages", "review-key", 204},
		{"POST", "/v1/responses", "review-key", 204},
		{"GET", "/v1/responses", "review-key", 204},
		{"GET", "/v1/models", "review-key", 204},
		{"GET", "/v1/realtime", "review-key", 403},
		{"POST", "/v1/videos", "review-key", 403},
		{"POST", "/v1beta/interactions", "review-key", 403},
		{"POST", "/muse-code/api/query", "review-key", 403},
		{"POST", "/new-endpoint", "review-key", 403},
		{"POST", "/new-endpoint", "legacy-key", 204},
	} {
		t.Run(tc.method+tc.path+tc.key, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("userApiKey", tc.key) }, s.accountPoolMiddleware())
			router.Handle(tc.method, tc.path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
}

func TestForkAccountPoolsDirectSearchUsesAuthenticatedPolicy(t *testing.T) {
	server := newTestServer(t)
	chain := []string{"reviewer"}
	server.handlers.Cfg.Client.AccountPools = &[]config.ClientAccountPool{{Name: "reviewer", AuthIDs: []string{"reviewer"}}}
	server.handlers.Cfg.Client.KeyScopes = &[]config.ClientKeyScope{{KeyPrefix: "test-", Providers: []string{"codex"}, Pools: &chain}}
	executor := &codexSearchCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	for _, id := range []string{"reviewer", "personal"} {
		if _, err := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
			ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "test-token"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/alpha/search", strings.NewReader(`{"query":"test"}`))
		r.Header.Set("Authorization", "Bearer test-key")
		r.Header.Set("X-Account-Pool", "personal")
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 200 || len(executor.authIDs) != 1 || executor.authIDs[0] != "reviewer" {
		t.Fatalf("direct search escaped pool: status=%d ids=%v", w.Code, executor.authIDs)
	}
	(*server.handlers.Cfg.Client.AccountPools)[0].AuthIDs = nil
	if w := request(); w.Code == 200 || len(executor.authIDs) != 1 {
		t.Fatalf("empty reviewer pool used another account: status=%d ids=%v", w.Code, executor.authIDs)
	}
}
