package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// museRelayCaptureExecutor stands in for the Meta executor and records the
// request the relay sends upstream.
type museRelayCaptureExecutor struct {
	request  *http.Request
	body     []byte
	calls    int
	status   int
	response string
}

func (e *museRelayCaptureExecutor) Identifier() string { return "meta" }

func (e *museRelayCaptureExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *museRelayCaptureExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *museRelayCaptureExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}

func (e *museRelayCaptureExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *museRelayCaptureExecutor) PrepareRequest(req *http.Request, a *auth.Auth) error {
	req.Header.Set("Authorization", "Bearer "+a.Attributes["api_key"])
	return nil
}

func (e *museRelayCaptureExecutor) HttpRequest(_ context.Context, _ *auth.Auth, req *http.Request) (*http.Response, error) {
	e.calls++
	e.request = req.Clone(req.Context())
	if req.Body != nil {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		e.body = body
	}
	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	response := e.response
	if response == "" {
		response = `{"rows":[{"model_id":"muse-test"}]}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "Etag": []string{`"catalog-1"`}, "Set-Cookie": []string{"session=upstream"}},
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

func museRelayTestServer(t *testing.T, attributes map[string]string) (*Server, *museRelayCaptureExecutor) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"sk-muse-test", "sk-claude-test", "plain-key"},
		},
	}
	cfg.Client.KeyScopes = &[]proxyconfig.ClientKeyScope{
		{KeyPrefix: "sk-muse-", Providers: []string{"muse"}},
		{KeyPrefix: "sk-claude-", Providers: []string{"claude"}},
	}
	cfg.AuthDir = authDir
	cfg.LoggingToFile = false
	cfg.UsageStatisticsEnabled = false

	server := NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))
	executor := &museRelayCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	credential := &auth.Auth{ID: "meta-auth", Provider: "meta", Status: auth.StatusActive, Attributes: attributes}
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("register Meta auth: %v", errRegister)
	}
	return server, executor
}

func TestForkMuseCodeRelayForwardsCatalogRequest(t *testing.T) {
	server, executor := museRelayTestServer(t, map[string]string{"api_key": "meta-key", "base_url": "https://meta.example.com/v1"})

	req := httptest.NewRequest(http.MethodGet, "/muse-code/models?profile=tbh", nil)
	req.Header.Set("Authorization", "Bearer sk-muse-test")
	req.Header.Set("X-Tbh-Session-Id", "session-1")
	req.Header.Set("Cookie", "client=cookie")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if executor.request == nil {
		t.Fatal("the Meta executor did not receive a request")
	}
	if got, want := executor.request.URL.String(), "https://meta.example.com/muse-code/models?profile=tbh"; got != want {
		t.Fatalf("upstream URL = %q, want %q", got, want)
	}
	if got := executor.request.Method; got != http.MethodGet {
		t.Fatalf("upstream method = %q, want GET", got)
	}
	if got := executor.request.Header.Get("Authorization"); got != "Bearer meta-key" {
		t.Fatalf("upstream Authorization = %q, want the Meta credential", got)
	}
	if got := executor.request.Header.Get("X-Tbh-Session-Id"); got != "session-1" {
		t.Fatalf("X-Tbh-Session-Id = %q, want it forwarded", got)
	}
	if got := executor.request.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie = %q, want it dropped", got)
	}
	if got := recorder.Body.String(); got != `{"rows":[{"model_id":"muse-test"}]}` {
		t.Fatalf("body = %s, want the upstream body unchanged", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Etag"); got != `"catalog-1"` {
		t.Fatalf("Etag = %q, want it relayed", got)
	}
	if got := recorder.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie = %q, want it dropped", got)
	}
}

func TestForkMuseCodeRelayPostsToTheDefaultOrigin(t *testing.T) {
	server, executor := museRelayTestServer(t, map[string]string{"api_key": "meta-key"})
	executor.status = http.StatusTooManyRequests

	req := httptest.NewRequest(http.MethodPost, "/muse-code/search", strings.NewReader(`{"query":"muse"}`))
	req.Header.Set("Authorization", "Bearer plain-key")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream status 429", recorder.Code)
	}
	if got, want := executor.request.URL.String(), "https://api.meta.ai/muse-code/search"; got != want {
		t.Fatalf("upstream URL = %q, want %q", got, want)
	}
	if got := executor.request.Method; got != http.MethodPost {
		t.Fatalf("upstream method = %q, want POST", got)
	}
	if got := string(executor.body); got != `{"query":"muse"}` {
		t.Fatalf("upstream body = %q", got)
	}
	if got := executor.request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("upstream Content-Type = %q", got)
	}
}

func TestForkMuseCodeRelayHonorsKeyScopeAndAllowlist(t *testing.T) {
	server, executor := museRelayTestServer(t, map[string]string{"api_key": "meta-key"})

	for _, testCase := range []struct {
		name   string
		method string
		path   string
		key    string
		status int
	}{
		{name: "key scoped to another provider", method: http.MethodGet, path: "/muse-code/models", key: "sk-claude-test", status: http.StatusNotFound},
		{name: "unknown key", method: http.MethodGet, path: "/muse-code/models", key: "wrong-key", status: http.StatusUnauthorized},
		{name: "credential minting is not relayed", method: http.MethodPost, path: "/muse-code/key", key: "sk-muse-test", status: http.StatusNotFound},
		{name: "telemetry is not relayed", method: http.MethodPost, path: "/muse-code/telemetry/logs", key: "sk-muse-test", status: http.StatusNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			req := httptest.NewRequest(testCase.method, testCase.path, nil)
			req.Header.Set("Authorization", "Bearer "+testCase.key)
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, req)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
		})
	}
	if executor.calls != 0 {
		t.Fatalf("the Meta executor received %d request(s), want none", executor.calls)
	}
}

func TestForkMuseCodeUpstreamURL(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		selected *auth.Auth
		want     string
		wantErr  bool
	}{
		{name: "no base URL uses Meta", selected: &auth.Auth{}, want: "https://api.meta.ai/muse-code/models?a=1"},
		{name: "attribute base URL", selected: &auth.Auth{Attributes: map[string]string{"base_url": "http://127.0.0.1:9000/custom/v1/"}}, want: "http://127.0.0.1:9000/muse-code/models?a=1"},
		{name: "metadata base URL", selected: &auth.Auth{Metadata: map[string]any{"api_base_url": "https://meta.example.com/v1"}}, want: "https://meta.example.com/muse-code/models?a=1"},
		{name: "base URL without a host", selected: &auth.Auth{Attributes: map[string]string{"base_url": "not a url"}}, wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, errURL := museCodeUpstreamURL(testCase.selected, "/muse-code/models", "a=1")
			if (errURL != nil) != testCase.wantErr {
				t.Fatalf("error = %v, wantErr %t", errURL, testCase.wantErr)
			}
			if got != testCase.want {
				t.Fatalf("URL = %q, want %q", got, testCase.want)
			}
		})
	}
}

// museRelayCatalog is a Muse Code model catalog in the shape Meta serves it.
const museRelayCatalog = `{"object":"list","data":[` +
	`{"id":"muse-wonder-1.0","metadata":{"muse-code":{"name":"Muse Wonder"}}},` +
	`{"id":"muse-wonder-1.0-contributor","metadata":{"muse-code":{"name":"Muse Wonder"}}},` +
	`{"id":"muse-spark-1.2","metadata":{"muse-code":{"name":"Muse Spark 1.2"}}},` +
	`{"id":"muse-image-1.0","metadata":{"muse-code":{"name":"Muse Image","is_hidden":true}}}]}`

func museRelayCatalogRequest(server *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/muse-code/models?profile=tbh", nil)
	req.Header.Set("Authorization", "Bearer sk-muse-test")
	req.Header.Set("If-None-Match", `"catalog-0"`)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	return recorder
}

func museRelayHidden(t *testing.T, body []byte, id string) bool {
	t.Helper()
	entry := gjson.GetBytes(body, `data.#(id=="`+id+`")`)
	if !entry.Exists() {
		t.Fatalf("the catalog lost %s: %s", id, body)
	}
	return entry.Get("metadata.muse-code.is_hidden").Bool()
}

func TestForkMuseCodeRelayHidesModelsAnAliasTakesOver(t *testing.T) {
	server, executor := museRelayTestServer(t, map[string]string{"api_key": "meta-key", "auth_kind": "oauth"})
	server.cfg.OAuthModelAlias = map[string][]proxyconfig.OAuthModelAlias{"meta": {
		{Name: "muse-wonder-1.0-contributor", Alias: "muse-wonder-1.0", Fork: true},
		// The model this alias runs is not in the catalog, so its ID stays offered.
		{Name: "muse-spark-1.2-contributor", Alias: "muse-spark-1.2", Fork: true},
	}}
	executor.response = museRelayCatalog

	recorder := museRelayCatalogRequest(server)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := executor.request.Header.Get("If-None-Match"); got != "" {
		t.Fatalf("upstream If-None-Match = %q, want it dropped while an alias can change the catalog", got)
	}
	body := recorder.Body.Bytes()
	for id, want := range map[string]bool{"muse-wonder-1.0": true, "muse-wonder-1.0-contributor": false, "muse-spark-1.2": false, "muse-image-1.0": true} {
		if got := museRelayHidden(t, body, id); got != want {
			t.Errorf("%s is_hidden = %t, want %t", id, got, want)
		}
	}
	if got := gjson.GetBytes(body, `data.#(id=="muse-wonder-1.0").metadata.muse-code.name`).String(); got != "Muse Wonder" {
		t.Fatalf("hidden entry name = %q, want the rest of the entry unchanged", got)
	}
	if got := recorder.Header().Get("Etag"); got != "" {
		t.Fatalf("Etag = %q, want Meta's validator dropped for a changed catalog", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestForkMuseCodeRelayLeavesTheCatalogAloneWithoutAnAlias(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		attributes map[string]string
		aliases    map[string][]proxyconfig.OAuthModelAlias
	}{
		{name: "no alias", attributes: map[string]string{"api_key": "meta-key", "auth_kind": "oauth"}},
		{
			name:       "alias for another channel",
			attributes: map[string]string{"api_key": "meta-key", "auth_kind": "oauth"},
			aliases:    map[string][]proxyconfig.OAuthModelAlias{"codex": {{Name: "muse-wonder-1.0-contributor", Alias: "muse-wonder-1.0"}}},
		},
		// A request on a Meta API key gets no OAuth model alias, so its catalog keeps every model.
		{
			name:       "Meta API key",
			attributes: map[string]string{"api_key": "meta-key"},
			aliases:    map[string][]proxyconfig.OAuthModelAlias{"meta": {{Name: "muse-wonder-1.0-contributor", Alias: "muse-wonder-1.0"}}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, executor := museRelayTestServer(t, testCase.attributes)
			server.cfg.OAuthModelAlias = testCase.aliases
			executor.response = museRelayCatalog

			recorder := museRelayCatalogRequest(server)
			if got := recorder.Body.String(); got != museRelayCatalog {
				t.Fatalf("body = %s, want Meta's catalog unchanged", got)
			}
			if got := executor.request.Header.Get("If-None-Match"); got != `"catalog-0"` {
				t.Fatalf("upstream If-None-Match = %q, want it forwarded", got)
			}
			if got := recorder.Header().Get("Etag"); got != `"catalog-1"` {
				t.Fatalf("Etag = %q, want it relayed", got)
			}
		})
	}
}

func TestForkMuseCodeShadowedModelsPrefersTheCredentialsOwnAlias(t *testing.T) {
	selected := &auth.Auth{Provider: "meta", Attributes: map[string]string{"auth_kind": "oauth"}}
	auth.SetOAuthModelAliasesAttribute(selected, []proxyconfig.OAuthModelAlias{{Name: "muse-own-contributor", Alias: "muse-wonder-1.0"}})
	cfg := &proxyconfig.Config{OAuthModelAlias: map[string][]proxyconfig.OAuthModelAlias{"Meta": {
		{Name: "muse-wonder-1.0-contributor", Alias: "muse-wonder-1.0"},
		{Name: "muse-spark-1.3-contributor", Alias: "MUSE-SPARK-1.3"},
		{Name: "muse-spark-1.2", Alias: "muse-spark-1.2"},
	}}}

	got := museCodeShadowedModels(cfg, selected)
	want := map[string]string{"muse-wonder-1.0": "muse-own-contributor", "muse-spark-1.3": "muse-spark-1.3-contributor"}
	if len(got) != len(want) {
		t.Fatalf("shadowed = %v, want %v", got, want)
	}
	for alias, name := range want {
		if got[alias] != name {
			t.Fatalf("shadowed[%s] = %q, want %q (all: %v)", alias, got[alias], name, got)
		}
	}
}

func TestForkHideShadowedMuseCodeModelsKeepsBodiesItDoesNotRead(t *testing.T) {
	shadowed := map[string]string{"muse-test": "muse-test-contributor"}
	for _, body := range []string{
		`{"rows":[{"model_id":"muse-test"}]}`,
		`not json`,
		// Entries without Muse Code metadata are not Muse Code catalog rows.
		`{"data":[{"id":"muse-test"},{"id":"muse-test-contributor"}]}`,
	} {
		got, changed := hideShadowedMuseCodeModels([]byte(body), shadowed)
		if changed || string(got) != body {
			t.Fatalf("hideShadowedMuseCodeModels(%s) = %s, %t; want it unchanged", body, got, changed)
		}
	}
}
