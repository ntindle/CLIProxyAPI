package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

const (
	keyScopeClaudeModel = "key-scope-claude-model"
	keyScopeCodexModel  = "key-scope-codex-model"
	keyScopeMetaModel   = "key-scope-meta-model"
	keyScopeSharedModel = "key-scope-shared-model"
)

func keyScopeTestHandler(t *testing.T) *BaseAPIHandler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("key-scope-claude-client", "claude", []*registry.ModelInfo{{ID: keyScopeClaudeModel}, {ID: keyScopeSharedModel}})
	modelRegistry.RegisterClient("key-scope-codex-client", "codex", []*registry.ModelInfo{{ID: keyScopeCodexModel}, {ID: keyScopeSharedModel}})
	modelRegistry.RegisterClient("key-scope-meta-client", "meta", []*registry.ModelInfo{{ID: keyScopeMetaModel}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("key-scope-claude-client")
		modelRegistry.UnregisterClient("key-scope-codex-client")
		modelRegistry.UnregisterClient("key-scope-meta-client")
	})

	cfg := &sdkconfig.SDKConfig{}
	cfg.Client.KeyScopes = &[]config.ClientKeyScope{
		{KeyPrefix: "sk-claude-", Providers: []string{"anthropic"}},
		{KeyPrefix: "sk-codex-", Providers: []string{"codex"}},
		{KeyPrefix: "sk-muse-", Providers: []string{"Muse"}},
		{KeyPrefix: "sk-", Providers: []string{"claude", "codex"}},
		{KeyPrefix: "sk-empty-", Providers: nil},
		{KeyPrefix: "", Providers: []string{"meta"}},
	}
	return NewBaseAPIHandlers(cfg, nil)
}

func keyScopeRequestContext(apiKey string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if apiKey != "" {
		c.Set("userApiKey", apiKey)
	}
	return c, recorder
}

func TestForkKeyScopeMatchesLongestPrefix(t *testing.T) {
	handler := keyScopeTestHandler(t)

	for _, testCase := range []struct {
		name   string
		apiKey string
		want   []string
		scoped bool
	}{
		{name: "claude key", apiKey: "sk-claude-abc", want: []string{"claude"}, scoped: true},
		{name: "codex key", apiKey: "sk-codex-abc", want: []string{"codex"}, scoped: true},
		{name: "muse alias resolves to meta", apiKey: "sk-muse-abc", want: []string{"meta"}, scoped: true},
		{name: "shorter prefix applies when no longer one matches", apiKey: "sk-other", want: []string{"claude", "codex"}, scoped: true},
		{name: "entry without providers is skipped", apiKey: "sk-empty-abc", want: []string{"claude", "codex"}, scoped: true},
		{name: "key matching no entry is unrestricted", apiKey: "admin-key"},
		{name: "request without a key is unrestricted", apiKey: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			c, _ := keyScopeRequestContext(testCase.apiKey)
			allowed, scoped := handler.keyScope(c)
			if scoped != testCase.scoped {
				t.Fatalf("scoped = %t, want %t", scoped, testCase.scoped)
			}
			if len(allowed) != len(testCase.want) {
				t.Fatalf("allowed = %v, want %v", allowed, testCase.want)
			}
			for _, provider := range testCase.want {
				if _, ok := allowed[provider]; !ok {
					t.Fatalf("allowed = %v, want %v", allowed, testCase.want)
				}
			}
		})
	}
}

func TestForkKeyAllowsProvider(t *testing.T) {
	handler := keyScopeTestHandler(t)

	for _, testCase := range []struct {
		name     string
		apiKey   string
		provider string
		want     bool
	}{
		{name: "provider in scope", apiKey: "sk-muse-abc", provider: "meta", want: true},
		{name: "provider named by its alias", apiKey: "sk-muse-abc", provider: "Muse", want: true},
		{name: "provider out of scope", apiKey: "sk-claude-abc", provider: "meta", want: false},
		{name: "unrestricted key", apiKey: "admin-key", provider: "meta", want: true},
		{name: "request without a key", apiKey: "", provider: "meta", want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			c, _ := keyScopeRequestContext(testCase.apiKey)
			if got := handler.KeyAllowsProvider(c, testCase.provider); got != testCase.want {
				t.Fatalf("KeyAllowsProvider = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestForkScopeProvidersNarrowsAndRejects(t *testing.T) {
	handler := keyScopeTestHandler(t)
	contextFor := func(apiKey string) context.Context {
		c, _ := keyScopeRequestContext(apiKey)
		return context.WithValue(context.Background(), "gin", c)
	}

	providers, model, errMsg := handler.scopeProviders(contextFor("sk-claude-abc"), keyScopeSharedModel)([]string{"codex", "claude"}, keyScopeSharedModel, nil)
	if errMsg != nil || model != keyScopeSharedModel || len(providers) != 1 || providers[0] != "claude" {
		t.Fatalf("shared model for a Claude key = %v, %q, %v; want only claude", providers, model, errMsg)
	}

	providers, _, errMsg = handler.scopeProviders(contextFor("sk-claude-abc"), keyScopeCodexModel)([]string{"codex"}, keyScopeCodexModel, nil)
	if errMsg == nil || providers != nil {
		t.Fatalf("Codex model for a Claude key = %v, %v; want a rejection", providers, errMsg)
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("rejection status = %d, want 400", errMsg.StatusCode)
	}
	body := errMsg.Error.Error()
	if gjson.Get(body, "error.code").String() != "model_not_found" || !strings.Contains(gjson.Get(body, "error.message").String(), keyScopeCodexModel) {
		t.Fatalf("rejection body = %s", body)
	}
	rejection := errMsg
	if _, _, got := handler.scopeProviders(contextFor("sk-claude-abc"), "missing")(nil, "", rejection); got != rejection {
		t.Fatal("an existing resolution error must pass through unchanged")
	}

	providers, _, errMsg = handler.scopeProviders(contextFor("admin-key"), keyScopeCodexModel)([]string{"codex"}, keyScopeCodexModel, nil)
	if errMsg != nil || len(providers) != 1 {
		t.Fatalf("unscoped key = %v, %v; want the providers unchanged", providers, errMsg)
	}

	providers, _, errMsg = handler.scopeProviders(contextFor("sk-claude-abc"), "any")([]string{"home"}, "any", nil)
	if errMsg != nil || len(providers) != 1 || providers[0] != "home" {
		t.Fatalf("Home routing = %v, %v; want it left alone", providers, errMsg)
	}

	providers, _, errMsg = handler.scopeProviders(context.Background(), keyScopeCodexModel)([]string{"codex"}, keyScopeCodexModel, nil)
	if errMsg != nil || len(providers) != 1 {
		t.Fatalf("context without a request = %v, %v; want the providers unchanged", providers, errMsg)
	}
}

func TestForkScopeModelListFiltersEveryListShape(t *testing.T) {
	handler := keyScopeTestHandler(t)

	ids := func(body []byte, listKey, field string) []string {
		var out []string
		gjson.GetBytes(body, listKey).ForEach(func(_, entry gjson.Result) bool {
			out = append(out, entry.Get(field).String())
			return true
		})
		return out
	}
	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	openAIList := []byte(`{"object":"list","data":[{"id":"` + keyScopeClaudeModel + `"},{"id":"` + keyScopeCodexModel + `"},{"id":"` + keyScopeSharedModel + `"},{"id":"` + keyScopeMetaModel + `"},{"id":"claude-fable-5-dd-disguised"},{"object":"model"}]}`)
	anthropicList := []byte(`{"data":[{"id":"` + keyScopeCodexModel + `"},{"id":"` + keyScopeClaudeModel + `"},{"id":"` + keyScopeSharedModel + `"}],"first_id":"` + keyScopeCodexModel + `","last_id":"` + keyScopeSharedModel + `","has_more":false}`)
	codexCatalog := []byte(`{"models":[{"slug":"` + keyScopeCodexModel + `","visibility":"list"},{"slug":"` + keyScopeClaudeModel + `"}]}`)
	geminiList := []byte(`{"models":[{"name":"models/` + keyScopeMetaModel + `","displayName":"Meta"},{"name":"models/` + keyScopeCodexModel + `"}]}`)

	claude, _ := keyScopeRequestContext("sk-claude-abc")
	if got := ids(handler.scopeModelList(claude, openAIList), "data", "id"); !equal(got, []string{keyScopeClaudeModel, keyScopeSharedModel}) {
		t.Fatalf("OpenAI list for a Claude key = %v", got)
	}
	scopedAnthropic := handler.scopeModelList(claude, anthropicList)
	if got := ids(scopedAnthropic, "data", "id"); !equal(got, []string{keyScopeClaudeModel, keyScopeSharedModel}) {
		t.Fatalf("Anthropic list for a Claude key = %v", got)
	}
	if first, last := gjson.GetBytes(scopedAnthropic, "first_id").String(), gjson.GetBytes(scopedAnthropic, "last_id").String(); first != keyScopeClaudeModel || last != keyScopeSharedModel {
		t.Fatalf("Anthropic first_id/last_id = %q/%q", first, last)
	}
	if !gjson.GetBytes(scopedAnthropic, "has_more").Exists() {
		t.Fatal("unrelated list fields were dropped")
	}

	codex, _ := keyScopeRequestContext("sk-codex-abc")
	if got := ids(handler.scopeModelList(codex, codexCatalog), "models", "slug"); !equal(got, []string{keyScopeCodexModel}) {
		t.Fatalf("Codex catalog for a Codex key = %v", got)
	}

	muse, _ := keyScopeRequestContext("sk-muse-abc")
	if got := ids(handler.scopeModelList(muse, geminiList), "models", "name"); !equal(got, []string{"models/" + keyScopeMetaModel}) {
		t.Fatalf("Gemini-shaped list for a Muse key = %v", got)
	}
	if got := ids(handler.scopeModelList(muse, codexCatalog), "models", "slug"); len(got) != 0 {
		t.Fatalf("Codex catalog for a Muse key = %v, want it empty", got)
	}
	if !json.Valid(handler.scopeModelList(muse, codexCatalog)) {
		t.Fatal("an emptied list is not valid JSON")
	}

	admin, _ := keyScopeRequestContext("admin-key")
	if got := handler.scopeModelList(admin, openAIList); string(got) != string(openAIList) {
		t.Fatal("an unscoped key must get the list unchanged")
	}
	if got := handler.scopeModelList(claude, []byte("not json")); string(got) != "not json" {
		t.Fatal("a body that is not JSON must pass through unchanged")
	}
}

func TestForkWriteModelListResponseAppliesKeyScope(t *testing.T) {
	handler := keyScopeTestHandler(t)
	c, recorder := keyScopeRequestContext("sk-codex-abc")

	handler.WriteModelListResponse(c, "openai", gin.H{"object": "list", "data": []gin.H{{"id": keyScopeClaudeModel}, {"id": keyScopeCodexModel}}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	data := gjson.GetBytes(recorder.Body.Bytes(), "data").Array()
	if len(data) != 1 || data[0].Get("id").String() != keyScopeCodexModel {
		t.Fatalf("written list = %s, want only the Codex model", recorder.Body.String())
	}
}
