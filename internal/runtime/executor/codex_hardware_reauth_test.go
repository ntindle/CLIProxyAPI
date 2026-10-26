package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const testCodexHardwareMessage = "You’re approved for Daybreak Blue, but this response cannot be shown as this session was not authenticated with one of your authorized hardware keys. Please re-authenticate with your hardware key and try your request again."

type testCodexHardwareReauthStarter struct {
	hint  CodexHardwareReauthHint
	err   error
	calls int
}

func (s *testCodexHardwareReauthStarter) StartCodexHardwareReauth(context.Context, *cliproxyauth.Auth) (CodexHardwareReauthHint, error) {
	s.calls++
	return s.hint, s.err
}

func validTestCodexHardwareHint() CodexHardwareReauthHint {
	return CodexHardwareReauthHint{
		VerificationURL: "https://auth.openai.com/codex/device",
		UserCode:        "ABCD-EFGH",
		ExpiresIn:       15 * time.Minute,
	}
}

func TestCodexHardwareReauthDecoratesExactProviderError(t *testing.T) {
	starter := &testCodexHardwareReauthStarter{hint: validTestCodexHardwareHint()}
	executor := NewCodexExecutor(nil, starter)
	auth := &cliproxyauth.Auth{ID: "selected"}
	payload := []byte(`{"error":{"code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}}`)

	got := executor.decorateCodexHardwareReauth(context.Background(), auth, payload)
	message := gjson.GetBytes(got, "error.message").String()
	if !strings.HasPrefix(message, testCodexHardwareMessage) || !strings.Contains(message, "open https://auth.openai.com/codex/device and enter code ABCD-EFGH") || !strings.Contains(message, "failed request was not replayed") {
		t.Fatalf("unexpected recovery message: %q", message)
	}
	if gotType := gjson.GetBytes(got, "error.recovery.type").String(); gotType != codexHardwareReauthRecoveryType {
		t.Fatalf("recovery type = %q", gotType)
	}
	if gotCode := gjson.GetBytes(got, "error.recovery.user_code").String(); gotCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", gotCode)
	}
	if gotExpiry := gjson.GetBytes(got, "error.recovery.expires_in").Int(); gotExpiry != 900 {
		t.Fatalf("expires_in = %d", gotExpiry)
	}
	if starter.calls != 1 {
		t.Fatalf("starter calls = %d", starter.calls)
	}

	again := executor.decorateCodexHardwareReauth(context.Background(), auth, got)
	if !bytes.Equal(again, got) {
		t.Fatal("already-decorated error changed")
	}
	if starter.calls != 1 {
		t.Fatalf("already-decorated error restarted flow: calls = %d", starter.calls)
	}
}

func TestCodexHardwareReauthRecognizesTerminalShapes(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		recoveryPath string
	}{
		{
			name:         "response failed",
			payload:      `{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"` + testCodexHardwareMessage + `"}}}`,
			recoveryPath: "response.error.recovery",
		},
		{
			name:         "wrapped body",
			payload:      `{"body":{"error":{"code":"cyber-policy","message":"` + testCodexHardwareMessage + `"}}}`,
			recoveryPath: "body.error.recovery",
		},
		{
			name:         "top level",
			payload:      `{"type":"error","code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}`,
			recoveryPath: "recovery",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			starter := &testCodexHardwareReauthStarter{hint: validTestCodexHardwareHint()}
			executor := NewCodexExecutor(nil, starter)
			got := executor.decorateCodexHardwareReauth(context.Background(), &cliproxyauth.Auth{ID: "selected"}, []byte(test.payload))
			if gotType := gjson.GetBytes(got, test.recoveryPath+".type").String(); gotType != codexHardwareReauthRecoveryType {
				t.Fatalf("recovery type = %q", gotType)
			}
			if starter.calls != 1 {
				t.Fatalf("starter calls = %d", starter.calls)
			}
		})
	}
}

func TestCodexHardwareReauthLeavesOtherErrorsUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "ordinary cyber policy", payload: []byte(`{"error":{"code":"cyberPolicy","message":"This request is not allowed by policy."}}`)},
		{name: "hardware wording without code", payload: []byte(`{"error":{"code":"invalid_request_error","message":"` + testCodexHardwareMessage + `"}}`)},
		{name: "invalid json", payload: []byte(`not-json`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			starter := &testCodexHardwareReauthStarter{hint: validTestCodexHardwareHint()}
			executor := NewCodexExecutor(nil, starter)
			got := executor.decorateCodexHardwareReauth(context.Background(), &cliproxyauth.Auth{ID: "selected"}, test.payload)
			if !bytes.Equal(got, test.payload) {
				t.Fatalf("payload changed: %s", got)
			}
			if starter.calls != 0 {
				t.Fatalf("starter calls = %d", starter.calls)
			}
		})
	}
}

func TestCodexHardwareReauthStartFailureLeavesProviderError(t *testing.T) {
	payload := []byte(`{"error":{"code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}}`)
	for _, starter := range []*testCodexHardwareReauthStarter{
		{err: errors.New("unavailable")},
		{hint: CodexHardwareReauthHint{VerificationURL: "https://example.com/device", UserCode: "CODE", ExpiresIn: time.Minute}},
	} {
		executor := NewCodexExecutor(nil, starter)
		got := executor.decorateCodexHardwareReauth(context.Background(), &cliproxyauth.Auth{ID: "selected"}, payload)
		if !bytes.Equal(got, payload) {
			t.Fatalf("provider error changed after failed start: %s", got)
		}
	}
}

func TestCodexHardwareReauthIsReturnedThroughExecutePaths(t *testing.T) {
	tests := []struct {
		name  string
		serve func(http.ResponseWriter)
	}{
		{
			name: "http status",
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}}`))
			},
		},
		{
			name: "terminal event",
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}}}` + "\n\n"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { test.serve(w) }))
			defer server.Close()
			starter := &testCodexHardwareReauthStarter{hint: validTestCodexHardwareHint()}
			executor := NewCodexExecutor(&config.Config{}, starter)
			auth := &cliproxyauth.Auth{ID: "selected", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if err == nil {
				t.Fatal("expected provider error")
			}
			if statusCodeFromTestError(t, err) != http.StatusBadRequest {
				t.Fatalf("unexpected status: %v", err)
			}
			if !strings.Contains(err.Error(), testCodexHardwareMessage) || !strings.Contains(err.Error(), "ABCD-EFGH") || !strings.Contains(err.Error(), "https://auth.openai.com/codex/device") {
				t.Fatalf("recovery instructions missing from returned error: %v", err)
			}
			if starter.calls != 1 {
				t.Fatalf("starter calls = %d", starter.calls)
			}
		})
	}
}

func TestCodexHardwareReauthIsReturnedThroughExecuteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"code":"cyberPolicy","message":"` + testCodexHardwareMessage + `"}}}` + "\n\n"))
	}))
	defer server.Close()
	starter := &testCodexHardwareReauthStarter{hint: validTestCodexHardwareHint()}
	executor := NewCodexExecutor(&config.Config{}, starter)
	auth := &cliproxyauth.Auth{ID: "selected", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err == nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if err == nil {
		t.Fatal("expected terminal stream error")
	}
	if statusCodeFromTestError(t, err) != http.StatusBadRequest {
		t.Fatalf("unexpected status: %v", err)
	}
	if !strings.Contains(err.Error(), testCodexHardwareMessage) || !strings.Contains(err.Error(), "ABCD-EFGH") || !strings.Contains(err.Error(), "https://auth.openai.com/codex/device") {
		t.Fatalf("recovery instructions missing from returned stream error: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("starter calls = %d", starter.calls)
	}
}
