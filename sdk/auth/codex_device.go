package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexLoginModeMetadataKey             = "codex_login_mode"
	codexLoginModeDevice                  = "device"
	codexDeviceUserCodeURL                = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	codexDeviceTokenURL                   = "https://auth.openai.com/api/accounts/deviceauth/token"
	codexDeviceVerificationURL            = "https://auth.openai.com/codex/device"
	codexDeviceTokenExchangeRedirectURI   = "https://auth.openai.com/deviceauth/callback"
	codexDeviceTimeout                    = 15 * time.Minute
	codexDeviceDefaultPollIntervalSeconds = 5
)

// CodexDeviceFlow is a single-use, remote-friendly Codex device authorization.
// The device auth identifier remains gateway-local; callers only expose the
// verification URL and short-lived user code to the person authenticating.
type CodexDeviceFlow struct {
	mu           sync.Mutex
	consumed     bool
	httpClient   *http.Client
	authService  *codex.CodexAuth
	deviceAuthID string
	userCode     string
	pollInterval time.Duration
}

// VerificationURL returns the first-party OpenAI page used to complete the flow.
func (f *CodexDeviceFlow) VerificationURL() string {
	if f == nil {
		return ""
	}
	return codexDeviceVerificationURL
}

// UserCode returns the short-lived code entered on the verification page.
func (f *CodexDeviceFlow) UserCode() string {
	if f == nil {
		return ""
	}
	return f.userCode
}

// ExpiresIn reports the maximum time OpenAI allows the device flow to remain pending.
func (f *CodexDeviceFlow) ExpiresIn() time.Duration {
	if f == nil {
		return 0
	}
	return codexDeviceTimeout
}

type codexDeviceUserCodeRequest struct {
	ClientID string `json:"client_id"`
}

type codexDeviceUserCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	UserCodeAlt  string          `json:"usercode"`
	Interval     json.RawMessage `json:"interval"`
}

type codexDeviceTokenRequest struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

type codexDeviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

func shouldUseCodexDeviceFlow(opts *LoginOptions) bool {
	if opts == nil || opts.Metadata == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(opts.Metadata[codexLoginModeMetadataKey]), codexLoginModeDevice)
}

func (a *CodexAuthenticator) loginWithDeviceFlow(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	flow, err := StartCodexDeviceFlow(ctx, cfg)
	if err != nil {
		return nil, err
	}

	fmt.Println("Starting Codex device authentication...")
	fmt.Printf("Codex device URL: %s\n", flow.VerificationURL())
	fmt.Printf("Codex device code: %s\n", flow.UserCode())

	if !opts.NoBrowser {
		if !browser.IsAvailable() {
			log.Warn("No browser available; please open the device URL manually")
		} else if errOpen := browser.OpenURL(flow.VerificationURL()); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		}
	}

	return flow.Complete(ctx)
}

// StartCodexDeviceFlow requests a device code without opening a browser or
// starting a loopback callback listener. This makes it safe for a remote
// Gateway to hand the verification step to a client machine.
func StartCodexDeviceFlow(ctx context.Context, cfg *config.Config) (*CodexDeviceFlow, error) {
	if cfg == nil {
		return nil, fmt.Errorf("codex device flow requires configuration")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	httpClient := util.SetProxy(&cfg.SDKConfig, &http.Client{})
	userCodeResp, err := requestCodexDeviceUserCode(ctx, httpClient)
	if err != nil {
		return nil, err
	}
	userCode := strings.TrimSpace(userCodeResp.UserCode)
	if userCode == "" {
		userCode = strings.TrimSpace(userCodeResp.UserCodeAlt)
	}
	deviceAuthID := strings.TrimSpace(userCodeResp.DeviceAuthID)
	if userCode == "" || deviceAuthID == "" {
		return nil, fmt.Errorf("codex device flow did not return required fields")
	}
	return &CodexDeviceFlow{
		httpClient:   httpClient,
		authService:  codex.NewCodexAuth(cfg),
		deviceAuthID: deviceAuthID,
		userCode:     userCode,
		pollInterval: parseCodexDevicePollInterval(userCodeResp.Interval),
	}, nil
}

// Complete waits for a previously started device flow, exchanges the result,
// and returns an unpersisted Codex auth record. A flow may be completed once.
func (f *CodexDeviceFlow) Complete(ctx context.Context) (*coreauth.Auth, error) {
	if f == nil || f.httpClient == nil || f.authService == nil {
		return nil, fmt.Errorf("codex device flow is unavailable")
	}
	f.mu.Lock()
	if f.consumed {
		f.mu.Unlock()
		return nil, fmt.Errorf("codex device flow was already completed")
	}
	f.consumed = true
	f.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	pollCtx, cancel := context.WithTimeout(ctx, codexDeviceTimeout)
	defer cancel()
	tokenResp, err := pollCodexDeviceToken(pollCtx, f.httpClient, f.deviceAuthID, f.userCode, f.pollInterval)
	if err != nil {
		return nil, err
	}
	authCode := strings.TrimSpace(tokenResp.AuthorizationCode)
	codeVerifier := strings.TrimSpace(tokenResp.CodeVerifier)
	codeChallenge := strings.TrimSpace(tokenResp.CodeChallenge)
	if authCode == "" || codeVerifier == "" || codeChallenge == "" {
		return nil, fmt.Errorf("codex device flow token response missing required fields")
	}
	authBundle, err := f.authService.ExchangeCodeForTokensWithRedirect(
		pollCtx,
		authCode,
		codexDeviceTokenExchangeRedirectURI,
		&codex.PKCECodes{CodeVerifier: codeVerifier, CodeChallenge: codeChallenge},
	)
	if err != nil {
		return nil, codex.NewAuthenticationError(codex.ErrCodeExchangeFailed, err)
	}
	return NewCodexAuthenticator().buildAuthRecord(f.authService, authBundle)
}

func requestCodexDeviceUserCode(ctx context.Context, client *http.Client) (*codexDeviceUserCodeResponse, error) {
	body, err := json.Marshal(codexDeviceUserCodeRequest{ClientID: codex.ClientID})
	if err != nil {
		return nil, fmt.Errorf("failed to encode codex device request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexDeviceUserCodeURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create codex device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to request codex device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read codex device code response: %w", err)
	}

	if !codexDeviceIsSuccessStatus(resp.StatusCode) {
		trimmed := strings.TrimSpace(string(respBody))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("codex device endpoint is unavailable (status %d)", resp.StatusCode)
		}
		if trimmed == "" {
			trimmed = "empty response body"
		}
		return nil, fmt.Errorf("codex device code request failed with status %d: %s", resp.StatusCode, trimmed)
	}

	var parsed codexDeviceUserCodeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode codex device code response: %w", err)
	}

	return &parsed, nil
}

func pollCodexDeviceToken(ctx context.Context, client *http.Client, deviceAuthID, userCode string, interval time.Duration) (*codexDeviceTokenResponse, error) {
	deadline := time.Now().Add(codexDeviceTimeout)

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("codex device authentication timed out after 15 minutes")
		}

		body, err := json.Marshal(codexDeviceTokenRequest{
			DeviceAuthID: deviceAuthID,
			UserCode:     userCode,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to encode codex device poll request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexDeviceTokenURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create codex device poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to poll codex device token: %w", err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("failed to read codex device poll response: %w", readErr)
		}

		switch {
		case codexDeviceIsSuccessStatus(resp.StatusCode):
			var parsed codexDeviceTokenResponse
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				return nil, fmt.Errorf("failed to decode codex device token response: %w", err)
			}
			return &parsed, nil
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
				continue
			}
		default:
			trimmed := strings.TrimSpace(string(respBody))
			if trimmed == "" {
				trimmed = "empty response body"
			}
			return nil, fmt.Errorf("codex device token polling failed with status %d: %s", resp.StatusCode, trimmed)
		}
	}
}

func parseCodexDevicePollInterval(raw json.RawMessage) time.Duration {
	defaultInterval := time.Duration(codexDeviceDefaultPollIntervalSeconds) * time.Second
	if len(raw) == 0 {
		return defaultInterval
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if seconds, convErr := strconv.Atoi(strings.TrimSpace(asString)); convErr == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil && asInt > 0 {
		return time.Duration(asInt) * time.Second
	}

	return defaultInterval
}

func codexDeviceIsSuccessStatus(code int) bool {
	return code >= 200 && code < 300
}

func (a *CodexAuthenticator) buildAuthRecord(authSvc *codex.CodexAuth, authBundle *codex.CodexAuthBundle) (*coreauth.Auth, error) {
	tokenStorage := authSvc.CreateTokenStorage(authBundle)

	if tokenStorage == nil || tokenStorage.Email == "" {
		return nil, fmt.Errorf("codex token storage missing account information")
	}

	planType := ""
	if tokenStorage.PlanType != "" {
		planType = tokenStorage.PlanType
	}
	hashAccountID := ""
	if tokenStorage.IDToken != "" {
		if claims, errParse := codex.ParseJWTToken(tokenStorage.IDToken); errParse == nil && claims != nil {
			if pt := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); pt != "" {
				planType = pt
			}
			accountID := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
			if accountID != "" {
				digest := sha256.Sum256([]byte(accountID))
				hashAccountID = hex.EncodeToString(digest[:])[:8]
			}
		}
	}
	if planType == "" {
		planType = codex.DefaultPlanType
	}
	tokenStorage.PlanType = planType

	fileName := codex.CredentialFileName(tokenStorage.Email, planType, hashAccountID, true)
	metadata := map[string]any{
		"email":      tokenStorage.Email,
		"account_id": tokenStorage.AccountID,
		"plan_type":  planType,
	}

	fmt.Println("Codex authentication successful")
	if authBundle.APIKey != "" {
		fmt.Println("Codex API key obtained and stored")
	}

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: metadata,
		Attributes: map[string]string{
			"plan_type": planType,
		},
	}, nil
}
