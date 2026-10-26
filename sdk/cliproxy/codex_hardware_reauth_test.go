package cliproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type testCodexHardwareFlow struct {
	release       chan struct{}
	completeReady chan struct{}
	record        *coreauth.Auth
	err           error
	once          sync.Once
}

func (f *testCodexHardwareFlow) VerificationURL() string {
	return "https://auth.openai.com/codex/device"
}
func (f *testCodexHardwareFlow) UserCode() string         { return "ABCD-EFGH" }
func (f *testCodexHardwareFlow) ExpiresIn() time.Duration { return 15 * time.Minute }
func (f *testCodexHardwareFlow) Complete(ctx context.Context) (*coreauth.Auth, error) {
	f.once.Do(func() { close(f.completeReady) })
	select {
	case <-f.release:
		return f.record, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type testCodexOuterBinder struct {
	mu        sync.Mutex
	calls     int
	id        string
	epoch     uint64
	account   string
	tokens    *codexauth.CodexTokenStorage
	called    chan struct{}
	calledOne sync.Once
	err       error
}

func (b *testCodexOuterBinder) BindCodexOuter(_ context.Context, id string, epoch uint64, account string, tokens *codexauth.CodexTokenStorage) (*coreauth.Auth, error) {
	b.mu.Lock()
	b.calls++
	b.id = id
	b.epoch = epoch
	b.account = account
	b.tokens = tokens
	b.mu.Unlock()
	b.calledOne.Do(func() { close(b.called) })
	if b.err != nil {
		return nil, b.err
	}
	return &coreauth.Auth{ID: id}, nil
}

func testCodexHardwareAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:                "credential.json",
		Provider:          "codex",
		FileName:          "credential.json",
		RegistrationEpoch: 9,
		Metadata: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"account_id":    "account-1",
		},
	}
}

func newTestCodexHardwareCoordinator(binder codexOuterBinder, factory codexHardwareDeviceFlowFactory) *codexHardwareReauthCoordinator {
	coordinator := newCodexHardwareReauthCoordinator(binder, func() *config.Config { return &config.Config{} })
	coordinator.factory = factory
	return coordinator
}

func waitForCodexHardwareFlowRemoval(t *testing.T, coordinator *codexHardwareReauthCoordinator, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		coordinator.mu.Lock()
		_, exists := coordinator.flows[id]
		coordinator.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("hardware reauthentication flow did not finish")
}

func TestCodexHardwareReauthCoordinatorDeduplicatesAndBindsSelectedCredential(t *testing.T) {
	record := &coreauth.Auth{Storage: &codexauth.CodexTokenStorage{
		IDToken: "new-id", AccessToken: "new-access", RefreshToken: "new-refresh", AccountID: "account-1",
	}}
	flow := &testCodexHardwareFlow{release: make(chan struct{}), completeReady: make(chan struct{}), record: record}
	binder := &testCodexOuterBinder{called: make(chan struct{})}
	factoryCalls := 0
	coordinator := newTestCodexHardwareCoordinator(binder, func(context.Context, *config.Config) (codexHardwareDeviceFlow, error) {
		factoryCalls++
		return flow, nil
	})
	defer coordinator.Close()
	auth := testCodexHardwareAuth()

	first, err := coordinator.StartCodexHardwareReauth(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	<-flow.completeReady
	second, err := coordinator.StartCodexHardwareReauth(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d", factoryCalls)
	}
	if first.VerificationURL != second.VerificationURL || first.UserCode != second.UserCode || first.UserCode != "ABCD-EFGH" {
		t.Fatalf("deduplicated hints differ: first=%+v second=%+v", first, second)
	}
	if first.ExpiresIn <= 0 || second.ExpiresIn <= 0 {
		t.Fatal("device hint did not include a positive expiry")
	}

	close(flow.release)
	select {
	case <-binder.called:
	case <-time.After(time.Second):
		t.Fatal("completed device flow was not bound")
	}
	binder.mu.Lock()
	defer binder.mu.Unlock()
	if binder.calls != 1 || binder.id != auth.ID || binder.epoch != auth.RegistrationEpoch || binder.account != "account-1" {
		t.Fatalf("unexpected bind target: calls=%d id=%q epoch=%d account=%q", binder.calls, binder.id, binder.epoch, binder.account)
	}
	if binder.tokens == nil || binder.tokens.AccessToken != "new-access" {
		t.Fatal("completed OAuth tokens were not passed to the binder")
	}
}

func TestCodexHardwareReauthCoordinatorRejectsDifferentCompletedAccount(t *testing.T) {
	flow := &testCodexHardwareFlow{
		release:       make(chan struct{}),
		completeReady: make(chan struct{}),
		record: &coreauth.Auth{Storage: &codexauth.CodexTokenStorage{
			IDToken: "other-id", AccessToken: "other-access", RefreshToken: "other-refresh", AccountID: "account-2",
		}},
	}
	binder := &testCodexOuterBinder{called: make(chan struct{})}
	coordinator := newTestCodexHardwareCoordinator(binder, func(context.Context, *config.Config) (codexHardwareDeviceFlow, error) {
		return flow, nil
	})
	defer coordinator.Close()
	auth := testCodexHardwareAuth()
	if _, err := coordinator.StartCodexHardwareReauth(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	<-flow.completeReady
	close(flow.release)
	waitForCodexHardwareFlowRemoval(t, coordinator, auth.ID)
	binder.mu.Lock()
	defer binder.mu.Unlock()
	if binder.calls != 0 {
		t.Fatalf("different account was bound: calls = %d", binder.calls)
	}
}

func TestCodexHardwareReauthCoordinatorRequiresFileBackedOAuthIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*coreauth.Auth)
	}{
		{name: "missing account", mutate: func(a *coreauth.Auth) { delete(a.Metadata, "account_id") }},
		{name: "missing epoch", mutate: func(a *coreauth.Auth) { a.RegistrationEpoch = 0 }},
		{name: "different provider", mutate: func(a *coreauth.Auth) { a.Provider = "openai" }},
		{name: "runtime source", mutate: func(a *coreauth.Auth) { a.Attributes = map[string]string{"runtime_only": "true"} }},
		{name: "api key", mutate: func(a *coreauth.Auth) {
			a.Attributes = map[string]string{"auth_kind": "apikey"}
			delete(a.Metadata, "access_token")
			delete(a.Metadata, "refresh_token")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factoryCalls := 0
			binder := &testCodexOuterBinder{called: make(chan struct{}), err: errors.New("must not bind")}
			coordinator := newTestCodexHardwareCoordinator(binder, func(context.Context, *config.Config) (codexHardwareDeviceFlow, error) {
				factoryCalls++
				return nil, errors.New("must not start")
			})
			defer coordinator.Close()
			auth := testCodexHardwareAuth()
			test.mutate(auth)
			if _, err := coordinator.StartCodexHardwareReauth(context.Background(), auth); err == nil {
				t.Fatal("ineligible credential unexpectedly started reauthentication")
			}
			if factoryCalls != 0 {
				t.Fatalf("factory calls = %d", factoryCalls)
			}
		})
	}
}
