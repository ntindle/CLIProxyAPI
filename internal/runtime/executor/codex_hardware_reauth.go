package executor

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexHardwareReauthRecoveryType = "hardware_key_reauthentication"

// CodexHardwareReauthHint contains only the short-lived, client-safe portion
// of a Gateway-owned device authorization. It never identifies the account or
// exposes refresh/access tokens.
type CodexHardwareReauthHint struct {
	VerificationURL string
	UserCode        string
	ExpiresIn       time.Duration
}

// CodexHardwareReauthStarter starts a credential-bound recovery flow after
// OpenAI explicitly requires a fresh hardware-key-authenticated session.
type CodexHardwareReauthStarter interface {
	StartCodexHardwareReauth(context.Context, *cliproxyauth.Auth) (CodexHardwareReauthHint, error)
}

type codexHardwareErrorPath struct {
	object   string
	message  string
	recovery string
}

var codexHardwareErrorPaths = []codexHardwareErrorPath{
	{object: "error", message: "error.message", recovery: "error.recovery"},
	{object: "body.error", message: "body.error.message", recovery: "body.error.recovery"},
	{object: "response.error", message: "response.error.message", recovery: "response.error.recovery"},
	{message: "message", recovery: "recovery"},
}

func normalizedCodexErrorCode(code string) string {
	replacer := strings.NewReplacer("_", "", "-", "", " ", "")
	return replacer.Replace(strings.ToLower(strings.TrimSpace(code)))
}

func codexHardwareKeyReauthPaths(payload []byte) []codexHardwareErrorPath {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil
	}
	matches := make([]codexHardwareErrorPath, 0, 2)
	for _, candidate := range codexHardwareErrorPaths {
		codePath := "code"
		if candidate.object != "" {
			object := gjson.GetBytes(payload, candidate.object)
			if !object.Exists() || !object.IsObject() {
				continue
			}
			codePath = candidate.object + ".code"
		}
		if normalizedCodexErrorCode(gjson.GetBytes(payload, codePath).String()) != "cyberpolicy" {
			continue
		}
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, candidate.message).String()))
		if !strings.Contains(message, "session was not authenticated") || !strings.Contains(message, "hardware key") {
			continue
		}
		matches = append(matches, candidate)
	}
	return matches
}

func validCodexHardwareReauthHint(hint CodexHardwareReauthHint) bool {
	verificationURL := strings.TrimSpace(hint.VerificationURL)
	parsed, errParse := url.Parse(verificationURL)
	if errParse != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "auth.openai.com") {
		return false
	}
	code := strings.TrimSpace(hint.UserCode)
	if code == "" || len(code) > 128 || strings.IndexFunc(code, unicode.IsControl) >= 0 {
		return false
	}
	return hint.ExpiresIn > 0
}

// decorateCodexHardwareReauth starts at most one coordinator-owned flow and
// adds its device instructions to every matching error object in the payload.
// Ordinary cyber-policy refusals remain byte-for-byte unchanged.
func (e *CodexExecutor) decorateCodexHardwareReauth(ctx context.Context, auth *cliproxyauth.Auth, payload []byte) []byte {
	paths := codexHardwareKeyReauthPaths(payload)
	if len(paths) == 0 || e == nil || e.hardwareReauth == nil || auth == nil {
		return payload
	}
	for _, path := range paths {
		if strings.TrimSpace(gjson.GetBytes(payload, path.recovery+".type").String()) == codexHardwareReauthRecoveryType {
			return payload
		}
	}
	hint, errStart := e.hardwareReauth.StartCodexHardwareReauth(ctx, auth)
	if errStart != nil || !validCodexHardwareReauthHint(hint) {
		log.Warn("codex hardware-key reauthentication could not be started")
		return payload
	}
	verificationURL := strings.TrimSpace(hint.VerificationURL)
	userCode := strings.TrimSpace(hint.UserCode)
	expiresIn := int64(hint.ExpiresIn / time.Second)
	if expiresIn < 1 {
		expiresIn = 1
	}
	decorated := payload
	for _, path := range paths {
		message := strings.TrimSpace(gjson.GetBytes(decorated, path.message).String())
		message += fmt.Sprintf(" Gateway reauthentication: open %s and enter code %s. After it succeeds, retry this request; the failed request was not replayed.", verificationURL, userCode)
		decorated, _ = sjson.SetBytes(decorated, path.message, message)
		decorated, _ = sjson.SetBytes(decorated, path.recovery+".type", codexHardwareReauthRecoveryType)
		decorated, _ = sjson.SetBytes(decorated, path.recovery+".verification_url", verificationURL)
		decorated, _ = sjson.SetBytes(decorated, path.recovery+".user_code", userCode)
		decorated, _ = sjson.SetBytes(decorated, path.recovery+".expires_in", expiresIn)
	}
	return decorated
}

func (e *CodexExecutor) newCodexStatusErr(ctx context.Context, auth *cliproxyauth.Auth, statusCode int, body []byte) statusErr {
	body = e.decorateCodexHardwareReauth(ctx, auth, body)
	return newCodexStatusErrWithCooling(statusCode, body, e.modelLevelCooling())
}
