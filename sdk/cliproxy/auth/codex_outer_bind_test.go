package auth

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

func outerBindingTokens(account, access string) *codex.CodexTokenStorage {
	return &codex.CodexTokenStorage{
		IDToken:      access + "-id",
		AccessToken:  access,
		RefreshToken: access + "-refresh",
		AccountID:    account,
		LastRefresh:  "2026-10-26T23:00:00Z",
		Email:        "user@example.com",
		Expire:       "2026-10-27T00:00:00Z",
		PlanType:     "pro",
	}
}

func TestBindCodexOuterPreservesUnrelatedCredentialData(t *testing.T) {
	m, base, path, hook := setupSIWCBinding(t, "auth.json")
	disk := readBindingDisk(t, path)
	disk["notes"] = "latest notes"
	disk["use_siwc"] = true
	disk["unknown"] = json.RawMessage(`9007199254740993`)
	if err := codex.WriteJSONFileAtomic(path, disk); err != nil {
		t.Fatal(err)
	}

	got, err := m.BindCodexOuter(context.Background(), base.ID, base.RegistrationEpoch, "account", outerBindingTokens("account", "fresh-access"))
	if err != nil {
		t.Fatal(err)
	}
	onDisk := readBindingDisk(t, path)
	if onDisk["access_token"] != "fresh-access" || onDisk["refresh_token"] != "fresh-access-refresh" || onDisk["id_token"] != "fresh-access-id" {
		t.Fatal("outer OAuth partition was not replaced")
	}
	if onDisk["notes"] != "latest notes" || onDisk["use_siwc"] != true || onDisk["siwc"].(map[string]any)["access_token"] != "old-siwc" {
		t.Fatal("binding overwrote unrelated or SIWC fields")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("binding rounded an unrelated JSON number")
	}
	if got.Metadata["access_token"] != "fresh-access" || got.Metadata["notes"] != "latest notes" || got.Storage != nil {
		t.Fatal("published credential did not match persisted data")
	}
	if got.RegistrationEpoch != base.RegistrationEpoch || got.ProxyURL != base.ProxyURL || got.Prefix != base.Prefix || got.Success != base.Success {
		t.Fatal("binding lost runtime credential state")
	}
	if got.CredentialVersion <= base.CredentialVersion || got.Generation <= base.Generation {
		t.Fatal("binding did not advance credential versions")
	}
	if hook.updates != 1 {
		t.Fatalf("updates = %d", hook.updates)
	}
}

func TestBindCodexOuterRejectsChangedIdentityWithoutPublishing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, m *Manager, base *Auth, path string, tokens *codex.CodexTokenStorage) uint64
	}{
		{
			name: "completed flow account",
			mutate: func(_ *testing.T, _ *Manager, base *Auth, _ string, tokens *codex.CodexTokenStorage) uint64 {
				tokens.AccountID = "other"
				return base.RegistrationEpoch
			},
		},
		{
			name: "backing file account",
			mutate: func(t *testing.T, _ *Manager, base *Auth, path string, _ *codex.CodexTokenStorage) uint64 {
				disk := readBindingDisk(t, path)
				disk["account_id"] = "other"
				if err := codex.WriteJSONFileAtomic(path, disk); err != nil {
					t.Fatal(err)
				}
				return base.RegistrationEpoch
			},
		},
		{
			name: "registration epoch",
			mutate: func(_ *testing.T, _ *Manager, base *Auth, _ string, _ *codex.CodexTokenStorage) uint64 {
				return base.RegistrationEpoch + 1
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m, base, path, hook := setupSIWCBinding(t, "auth.json")
			tokens := outerBindingTokens("account", "fresh-access")
			epoch := test.mutate(t, m, base, path, tokens)
			before, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			_, err := m.BindCodexOuter(context.Background(), base.ID, epoch, "account", tokens)
			if err == nil {
				t.Fatal("binding unexpectedly succeeded")
			}
			after, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed binding changed the backing file")
			}
			got, _ := m.GetByID(base.ID)
			if !reflect.DeepEqual(got, base) || hook.updates != 0 {
				t.Fatal("failed binding published credential state")
			}
		})
	}
}
