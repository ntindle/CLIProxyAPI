package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	metaauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/meta"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	museCodeProvider = "meta"
	// museCodeRelayMaxBody caps the request and response bodies the relay carries.
	museCodeRelayMaxBody = 16 << 20
)

// museCodeRelayPaths are the Muse Code client endpoints, relative to
// /muse-code, that the relay forwards. The client asks for them at the root of
// the origin its base URL names, and refuses to start without the model
// catalog. Endpoints that mint or revoke credentials, and telemetry, are left
// out on purpose.
var museCodeRelayPaths = []string{"models", "config", "search", "browser_open"}

// registerMuseCodeRoutes serves the Muse Code client endpoints so the Muse CLI
// can use the proxy as its endpoint.
func (s *Server) registerMuseCodeRoutes() {
	museCode := s.engine.Group("/muse-code")
	museCode.Use(AuthMiddleware(s.accessManager), s.accountPoolMiddleware())
	for _, path := range museCodeRelayPaths {
		museCode.GET("/"+path, s.museCodeRelay)
		museCode.POST("/"+path, s.museCodeRelay)
	}
}

// museCodeRelay forwards one Muse Code client request to Meta with a Meta
// credential and returns the answer unchanged, except that the model catalog
// hides the models an OAuth model alias has taken over. These payloads are
// already in Meta's format and must not pass through a protocol translator.
func (s *Server) museCodeRelay(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Meta auth manager unavailable"})
		return
	}
	if !s.handlers.KeyAllowsProvider(c, museCodeProvider) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "Muse Code is not available to this API key",
			"type":    "invalid_request_error",
			"code":    "not_found",
		}})
		return
	}

	var body []byte
	if c.Request.Method != http.MethodGet && c.Request.Body != nil {
		requestBody, errRead := io.ReadAll(io.LimitReader(c.Request.Body, museCodeRelayMaxBody))
		if errRead != nil {
			c.JSON(clienterror.HTTPStatusFromErrorOr(errRead, http.StatusBadRequest), gin.H{"error": "Failed to read Muse Code request"})
			return
		}
		body = requestBody
	}

	ctx := context.WithValue(c.Request.Context(), "gin", c)
	selected, errSelect := s.handlers.AuthManager.SelectAuth(ctx, museCodeProvider, "", coreexecutor.Options{Headers: c.Request.Header.Clone()})
	if errSelect != nil {
		for _, value := range auth.SafeResponseHeaders(errSelect).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
		c.JSON(clienterror.HTTPStatusFromErrorOr(errSelect, http.StatusServiceUnavailable), gin.H{"error": errSelect.Error()})
		return
	}
	if selected == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Meta auth unavailable"})
		return
	}

	upstreamURL, errURL := museCodeUpstreamURL(selected, c.Request.URL.Path, c.Request.URL.RawQuery)
	if errURL != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errURL.Error()})
		return
	}
	headers := museCodeRelayHeaders(c.Request.Header)
	var shadowed map[string]string
	if c.Request.Method == http.MethodGet && strings.TrimSuffix(c.Request.URL.Path, "/") == "/muse-code/models" {
		shadowed = museCodeShadowedModels(s.getConfig(), selected)
	}
	if len(shadowed) > 0 {
		// The catalog sent back can differ from Meta's, so Meta must not answer
		// 304 for a list this client was never given.
		headers.Del("If-None-Match")
	}
	req, errRequest := s.handlers.AuthManager.NewHttpRequest(ctx, selected, c.Request.Method, upstreamURL, body, headers)
	if errRequest != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errRequest, http.StatusBadGateway), gin.H{"error": errRequest.Error()})
		return
	}
	resp, errDo := s.handlers.AuthManager.HttpRequest(ctx, selected, req)
	if errDo != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errDo, http.StatusBadGateway), gin.H{"error": errDo.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("muse code relay: close response body error: %v", errClose)
		}
	}()
	upstreamBody, errRead := io.ReadAll(io.LimitReader(resp.Body, museCodeRelayMaxBody))
	if errRead != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errRead, http.StatusBadGateway), gin.H{"error": "Failed to read Muse Code response"})
		return
	}
	relayedHeaders := []string{"Content-Type", "Cache-Control", "Etag", "Last-Modified"}
	if len(shadowed) > 0 && resp.StatusCode == http.StatusOK {
		if hidden, changed := hideShadowedMuseCodeModels(upstreamBody, shadowed); changed {
			upstreamBody = hidden
			// Meta's validators describe Meta's list, not this one.
			relayedHeaders = relayedHeaders[:2]
		}
	}
	for _, name := range relayedHeaders {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = c.Writer.Write(upstreamBody)
}

// museCodeShadowedModels maps each model ID an OAuth model alias on the
// credential's channel has taken over to the upstream model the alias runs
// instead: a request for that ID never reaches the model of that name. The
// credential's own aliases come first, as they do when a request runs.
func museCodeShadowedModels(cfg *config.Config, selected *auth.Auth) map[string]string {
	if selected == nil {
		return nil
	}
	channel := auth.OAuthModelAliasChannel(selected.Provider, selected.AuthKind())
	if channel == "" {
		return nil
	}
	aliases := auth.OAuthModelAliasesFromAttributes(selected.Attributes)
	if cfg != nil {
		for rawChannel, entries := range cfg.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(rawChannel), channel) {
				aliases = append(aliases, entries...)
			}
		}
	}
	shadowed := make(map[string]string, len(aliases))
	for _, entry := range aliases {
		name, alias := strings.TrimSpace(entry.Name), strings.TrimSpace(entry.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		if _, exists := shadowed[strings.ToLower(alias)]; !exists {
			shadowed[strings.ToLower(alias)] = name
		}
	}
	return shadowed
}

// hideShadowedMuseCodeModels marks a catalog entry hidden when an alias has
// taken over its ID and the model the alias runs is in the same catalog, so a
// model picker offers only what a request would reach. Muse still resolves a
// hidden model named explicitly, and the alias then runs it. The body comes
// back unchanged when nothing is hidden or it is not a catalog this reads.
func hideShadowedMuseCodeModels(body []byte, shadowed map[string]string) ([]byte, bool) {
	data := gjson.GetBytes(body, "data")
	if len(shadowed) == 0 || !data.IsArray() {
		return body, false
	}
	entries := data.Array()
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		listed[strings.ToLower(strings.TrimSpace(entry.Get("id").String()))] = true
	}
	out, changed := body, false
	for i, entry := range entries {
		target, ok := shadowed[strings.ToLower(strings.TrimSpace(entry.Get("id").String()))]
		if !ok || !listed[strings.ToLower(target)] {
			continue
		}
		museCode := entry.Get("metadata.muse-code")
		if !museCode.IsObject() || museCode.Get("is_hidden").Bool() {
			continue
		}
		updated, errSet := sjson.SetBytes(out, "data."+strconv.Itoa(i)+".metadata.muse-code.is_hidden", true)
		if errSet != nil {
			return body, false
		}
		out, changed = updated, true
	}
	return out, changed
}

// MuseCodeURL returns the address of a Muse Code endpoint, such as
// /muse-code/models, on the origin the Meta credential talks to.
func MuseCodeURL(selected *auth.Auth, path string) (string, error) {
	return museCodeUpstreamURL(selected, path, "")
}

// museCodeUpstreamURL maps a relayed request onto the origin the credential
// talks to. Muse Code endpoints live at the root of that origin, beside the
// versioned API the credential's base URL names.
func museCodeUpstreamURL(selected *auth.Auth, path, rawQuery string) (string, error) {
	baseURL := ""
	if selected != nil {
		if selected.Attributes != nil {
			baseURL = strings.TrimSpace(selected.Attributes["base_url"])
		}
		for _, key := range []string{"base_url", "api_base_url"} {
			if baseURL != "" || selected.Metadata == nil {
				break
			}
			if value, ok := selected.Metadata[key].(string); ok {
				baseURL = strings.TrimSpace(value)
			}
		}
	}
	if baseURL == "" {
		baseURL = metaauth.DefaultAPIBaseURL
	}
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Meta credential has an invalid base URL")
	}
	upstream := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path, RawQuery: rawQuery}
	return upstream.String(), nil
}

// museCodeRelayHeaders keeps the request headers of the Muse Code client that
// Meta's endpoints read. The client authenticates to the proxy with a proxy
// API key, so its Authorization header is never forwarded; the Meta credential
// is injected when the request is prepared.
func museCodeRelayHeaders(in http.Header) http.Header {
	out := make(http.Header)
	for name, values := range in {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case canonical == "Accept", canonical == "Content-Type", canonical == "Traceparent", canonical == "If-None-Match":
		case strings.HasPrefix(canonical, "X-Tbh-"), strings.HasPrefix(canonical, "X-Meta-Ai-"):
		default:
			continue
		}
		out[canonical] = append([]string(nil), values...)
	}
	return out
}
