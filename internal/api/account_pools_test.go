package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
		{"POST", "/v1/images/generations", "review-key", 403},
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

// forkCodexKeyPolicyClient mirrors the Gateway's client keys: Codex keys routed
// through pools, an unpooled Codex key named by an alias, a pooled Claude key
// whose pools also hold Codex logins, and the unpooled Meta and xAI keys that
// could reach Codex voice and search before key scopes covered them.
func forkCodexKeyPolicyClient() config.ClientConfig {
	codexChain, supermemoryChain := []string{"primary", "shared"}, []string{"shared"}
	return config.ClientConfig{
		AccountPools: &[]config.ClientAccountPool{
			{Name: "primary", AuthIDs: []string{"codex-primary"}},
			{Name: "shared", AuthIDs: []string{"codex-shared"}},
		},
		KeyScopes: &[]config.ClientKeyScope{
			{KeyPrefix: "sk-codex-", Providers: []string{"codex"}, Pools: &codexChain},
			{KeyPrefix: "sk-supermemory-", Providers: []string{"codex"}, Pools: &supermemoryChain},
			{KeyPrefix: "sk-openai-", Providers: []string{"openai"}},
			{KeyPrefix: "sk-claude-", Providers: []string{"claude"}, Pools: &codexChain},
			{KeyPrefix: "sk-muse-", Providers: []string{"meta"}},
			{KeyPrefix: "sk-grok-", Providers: []string{"xai"}},
		},
	}
}

func forkCodexRoutePath(pattern string) string {
	return strings.ReplaceAll(pattern, ":call_id", "call-1")
}

func forkErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return ""
	}
	return payload.Error.Code
}

func TestForkCodexRoutesApplyKeyScopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sdkCfg := &config.SDKConfig{Client: forkCodexKeyPolicyClient()}
	homeManager := auth.NewManager(nil, nil, nil)
	homeManager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	for _, mode := range []struct {
		name    string
		manager *auth.Manager
		want    map[string]int
	}{
		{name: "local", want: map[string]int{
			"sk-codex-test": 204, "sk-supermemory-test": 204, "sk-openai-test": 204, "legacy-key": 204,
			"sk-claude-test": 403, "sk-muse-test": 403, "sk-grok-test": 403,
		}},
		// Key scopes do not apply in Home mode, and pool keys are refused there as before.
		{name: "home", manager: homeManager, want: map[string]int{
			"sk-muse-test": 204, "sk-grok-test": 204, "sk-openai-test": 204, "legacy-key": 204,
			"sk-codex-test": 503, "sk-supermemory-test": 503, "sk-claude-test": 503,
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			s := &Server{handlers: handlers.NewBaseAPIHandlers(sdkCfg, mode.manager)}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("userApiKey", strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
			}, s.accountPoolMiddleware())
			noContent := func(c *gin.Context) { c.Status(http.StatusNoContent) }
			for route := range codexOnlyRoutes {
				method, pattern, _ := strings.Cut(route, " ")
				router.Handle(method, pattern, noContent)
			}
			router.POST("/v1/responses", noContent)
			serve := func(method, path, key string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(method, path, nil)
				request.Header.Set("Authorization", "Bearer "+key)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				return recorder
			}
			for route := range codexOnlyRoutes {
				method, pattern, _ := strings.Cut(route, " ")
				for key, want := range mode.want {
					recorder := serve(method, forkCodexRoutePath(pattern), key)
					if recorder.Code != want {
						t.Errorf("%s with %s: status=%d want=%d body=%s", route, key, recorder.Code, want, recorder.Body.String())
						continue
					}
					if want == http.StatusForbidden {
						if code := forkErrorCode(t, recorder.Body.Bytes()); code != "provider_not_allowed" {
							t.Errorf("%s with %s: code=%q want provider_not_allowed", route, key, code)
						}
					}
				}
			}
			// Model routes keep resolving the scope from the model, after the middleware.
			if recorder := serve(http.MethodPost, "/v1/responses", "sk-muse-test"); recorder.Code != http.StatusNoContent {
				t.Errorf("Meta key on /v1/responses: status=%d", recorder.Code)
			}
		})
	}
}

func TestForkCodexOnlyRoutesMatchRegisteredRoutes(t *testing.T) {
	server := newTestServer(t)
	registered := make(map[string]bool)
	for _, route := range server.engine.Routes() {
		key := route.Method + " " + route.Path
		registered[key] = true
		usesCodex := strings.HasPrefix(route.Path, "/v1/realtime") || strings.HasPrefix(route.Path, "/v1/live") || strings.HasSuffix(route.Path, "/alpha/search")
		if usesCodex && !codexOnlyRoutes[key] {
			t.Errorf("%s always uses a Codex credential but is missing from codexOnlyRoutes; check its credential selection for pools, then add it", key)
		}
	}
	for key := range codexOnlyRoutes {
		if !registered[key] {
			t.Errorf("codexOnlyRoutes lists %s, which the server does not register", key)
		}
	}
}

// TestForkVoiceAndSearchKeyPolicyOnServer runs the real route table, so it also
// fails if a route loses accountPoolMiddleware. It never completes a WebSocket
// upgrade: those handlers dial the Codex upstream directly.
func TestForkVoiceAndSearchKeyPolicyOnServer(t *testing.T) {
	keys := []string{"sk-codex-test", "sk-supermemory-test", "sk-openai-test", "sk-claude-test", "sk-muse-test", "sk-grok-test", "legacy-key"}
	server := newTestServerWithConfig(t, &config.Config{SDKConfig: config.SDKConfig{APIKeys: keys}})
	executor := &codexSearchCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	// The login outside every pool has the highest priority, so unpooled keys pick it.
	for _, credential := range []struct{ id, priority string }{
		{"codex-primary", "0"}, {"codex-shared", "100"}, {"codex-outside", "1000"},
	} {
		if _, errRegister := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
			ID:         credential.id,
			Provider:   "codex",
			Status:     auth.StatusActive,
			Attributes: map[string]string{"priority": credential.priority},
			Metadata:   map[string]any{"access_token": credential.id + "-token"},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	serve := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer)
		if strings.HasPrefix(body, "v=0") {
			request.Header.Set("Content-Type", "application/sdp")
		} else if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, request)
		return recorder
	}
	selectedBy := func(method, path, bearer, body string) (*httptest.ResponseRecorder, []string) {
		executor.authIDs = nil
		recorder := serve(method, path, bearer, body)
		return recorder, slices.Clone(executor.authIDs)
	}
	callRoutes := []struct{ path, body string }{
		{"/v1/live", "v=0\r\n"},
		{"/v1/realtime", "v=0\r\n"},
		{"/v1/realtime/calls", "v=0\r\n"},
		{"/v1/alpha/search", `{"query":"test"}`},
		{"/backend-api/codex/alpha/search", `{"query":"test"}`},
	}

	t.Run("keys without Codex are refused on every voice and search route", func(t *testing.T) {
		server.handlers.Cfg.Client = forkCodexKeyPolicyClient()
		for route := range codexOnlyRoutes {
			method, pattern, _ := strings.Cut(route, " ")
			for _, key := range []string{"sk-claude-test", "sk-muse-test", "sk-grok-test"} {
				recorder := serve(method, forkCodexRoutePath(pattern), key, "")
				if recorder.Code != http.StatusForbidden || forkErrorCode(t, recorder.Body.Bytes()) != "provider_not_allowed" {
					t.Errorf("%s with %s: status=%d body=%s", route, key, recorder.Code, recorder.Body.String())
				}
			}
		}
		if executor.httpCalls != 0 {
			t.Fatalf("refused keys reached the upstream %d times with %v", executor.httpCalls, executor.authIDs)
		}
	})

	t.Run("Codex keys choose inside their pools and unscoped keys are unchanged", func(t *testing.T) {
		server.handlers.Cfg.Client = forkCodexKeyPolicyClient()
		for key, want := range map[string]string{
			"sk-codex-test":       "codex-primary",
			"sk-supermemory-test": "codex-shared",
			"sk-openai-test":      "codex-outside",
			"legacy-key":          "codex-outside",
		} {
			for _, route := range callRoutes {
				recorder, ids := selectedBy(http.MethodPost, route.path, key, route.body)
				if recorder.Code != http.StatusOK || !slices.Equal(ids, []string{want}) {
					t.Errorf("%s with %s: status=%d used=%v want=%s body=%s", route.path, key, recorder.Code, ids, want, recorder.Body.String())
				}
			}
		}
	})

	t.Run("pool keys reach the other voice routes", func(t *testing.T) {
		server.handlers.Cfg.Client = forkCodexKeyPolicyClient()
		callsBefore := executor.httpCalls
		for _, tc := range []struct {
			method, path string
			want         int
		}{
			{http.MethodGet, "/v1/realtime?model=gpt-realtime", http.StatusUpgradeRequired},
			{http.MethodGet, "/v1/realtime?call_id=call-1", http.StatusUpgradeRequired},
			{http.MethodGet, "/v1/realtime/calls/call-1", http.StatusUpgradeRequired},
			{http.MethodGet, "/v1/live/call-1", http.StatusUpgradeRequired},
			{http.MethodPost, "/v1/realtime/calls/call-1/hangup", http.StatusNotFound},
			{http.MethodPost, "/v1/realtime/sessions", http.StatusOK},
			{http.MethodPost, "/v1/realtime/client_secrets", http.StatusOK},
			// These answer 501 without choosing a credential, which is why pool keys
			// may call them. If upstream starts serving one, check its credential
			// selection for pools before keeping it in codexOnlyRoutes.
			{http.MethodPost, "/v1/realtime/transcription_sessions", http.StatusNotImplemented},
			{http.MethodGet, "/v1/realtime/translations", http.StatusNotImplemented},
			{http.MethodPost, "/v1/realtime/translations", http.StatusNotImplemented},
			{http.MethodPost, "/v1/realtime/translations/client_secrets", http.StatusNotImplemented},
			{http.MethodPost, "/v1/realtime/calls/call-1/accept", http.StatusNotImplemented},
			{http.MethodPost, "/v1/realtime/calls/call-1/reject", http.StatusNotImplemented},
			{http.MethodPost, "/v1/realtime/calls/call-1/refer", http.StatusNotImplemented},
		} {
			if recorder := serve(tc.method, tc.path, "sk-codex-test", ""); recorder.Code != tc.want {
				t.Errorf("%s %s: status=%d want=%d body=%s", tc.method, tc.path, recorder.Code, tc.want, recorder.Body.String())
			}
		}
		if executor.httpCalls != callsBefore {
			t.Fatalf("routes without a call reached the upstream with %v", executor.authIDs)
		}
	})

	t.Run("client secrets carry the minting key's pools and scope", func(t *testing.T) {
		server.handlers.Cfg.Client = forkCodexKeyPolicyClient()
		mint := serve(http.MethodPost, "/v1/realtime/client_secrets", "sk-codex-test", `{"session":{"type":"realtime","model":"gpt-realtime"}}`)
		var minted struct {
			Value string `json:"value"`
		}
		if errUnmarshal := json.Unmarshal(mint.Body.Bytes(), &minted); mint.Code != http.StatusOK || errUnmarshal != nil || minted.Value == "" {
			t.Fatalf("mint: status=%d body=%s", mint.Code, mint.Body.String())
		}
		useSecret := func() (*httptest.ResponseRecorder, []string) {
			return selectedBy(http.MethodPost, "/v1/realtime/calls", minted.Value, "v=0\r\n")
		}
		if recorder, ids := useSecret(); recorder.Code != http.StatusOK || !slices.Equal(ids, []string{"codex-primary"}) {
			t.Fatalf("secret call: status=%d used=%v body=%s", recorder.Code, ids, recorder.Body.String())
		}
		// Each use resolves the minting key's current pools.
		pools := *server.handlers.Cfg.Client.AccountPools
		pools[0].AuthIDs = nil
		if recorder, ids := useSecret(); recorder.Code != http.StatusOK || !slices.Equal(ids, []string{"codex-shared"}) {
			t.Fatalf("secret call after the primary pool emptied: status=%d used=%v", recorder.Code, ids)
		}
		pools[1].AuthIDs = nil
		if recorder, ids := useSecret(); recorder.Code < http.StatusBadRequest || len(ids) != 0 {
			t.Fatalf("secret call with empty pools: status=%d used=%v", recorder.Code, ids)
		}
		(*server.handlers.Cfg.Client.KeyScopes)[0].Providers = []string{"meta"}
		if recorder, ids := useSecret(); recorder.Code != http.StatusForbidden || len(ids) != 0 || forkErrorCode(t, recorder.Body.Bytes()) != "provider_not_allowed" {
			t.Fatalf("secret call after the key lost Codex: status=%d used=%v body=%s", recorder.Code, ids, recorder.Body.String())
		}
		// A client secret is not an API key: it cannot mint another secret.
		if recorder := serve(http.MethodPost, "/v1/realtime/client_secrets", minted.Value, `{}`); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("secret minting a secret: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
}
