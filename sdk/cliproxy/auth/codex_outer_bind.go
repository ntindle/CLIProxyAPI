package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// BindCodexOuter atomically replaces the first-party OAuth partition of an
// existing file-backed Codex credential. The registration and OpenAI account
// captured before authorization must still match when the tokens are committed.
func (m *Manager) BindCodexOuter(ctx context.Context, id string, expectedEpoch uint64, expectedAccount string, tokenStorage *codex.CodexTokenStorage) (*Auth, error) {
	expectedAccount = strings.TrimSpace(expectedAccount)
	if m == nil || strings.TrimSpace(id) == "" || expectedEpoch == 0 || expectedAccount == "" || tokenStorage == nil ||
		strings.TrimSpace(tokenStorage.AccessToken) == "" || strings.TrimSpace(tokenStorage.RefreshToken) == "" ||
		strings.TrimSpace(tokenStorage.IDToken) == "" || strings.TrimSpace(tokenStorage.AccountID) != expectedAccount {
		return nil, fmt.Errorf("bind Codex OAuth: invalid target or token data")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := m.lockAuthMutationContext(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.RLock()
	base := m.auths[id].Clone()
	m.mu.RUnlock()
	if base == nil || base.RegistrationEpoch != expectedEpoch || !strings.EqualFold(strings.TrimSpace(base.Provider), "codex") || !codexBindingHasOuter(base.Metadata) || codexMetadataAccountID(base.Metadata) != expectedAccount {
		return nil, fmt.Errorf("bind Codex OAuth: target is missing, changed, or not a Codex OAuth credential")
	}
	path, errPath := m.codexBindingPath(base)
	if errPath != nil {
		return nil, errPath
	}
	replacement := tokenStorage.OuterCredentialMap()
	var committed *Auth
	cooldownChanged := false
	err = codex.WithFileLock(path, func() error {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		checkedPath, checkErr := m.codexBindingPath(base)
		if checkErr != nil || checkedPath != path {
			return fmt.Errorf("bind Codex OAuth: backing file changed")
		}
		info, errStat := os.Lstat(path)
		if errStat != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("bind Codex OAuth: backing file is not a regular file")
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return fmt.Errorf("bind Codex OAuth: read backing file: %w", errRead)
		}
		var disk map[string]any
		if json.Unmarshal(data, &disk) != nil || !codexBindingHasOuter(disk) {
			return fmt.Errorf("bind Codex OAuth: invalid Codex OAuth file")
		}
		kind, _ := disk["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(kind), "codex") {
			return fmt.Errorf("bind Codex OAuth: backing file type changed")
		}
		if codexMetadataAccountID(disk) != expectedAccount {
			return fmt.Errorf("bind Codex OAuth: backing account changed")
		}

		// Preserve unrelated disk values losslessly and replace only the outer
		// OAuth partition produced by the completed device authorization.
		var raw map[string]json.RawMessage
		if errJSON := json.Unmarshal(data, &raw); errJSON != nil {
			return errJSON
		}
		out := make(map[string]any, len(raw))
		for key, value := range raw {
			out[key] = value
		}
		for key, value := range replacement {
			encoded, errJSON := json.Marshal(value)
			if errJSON != nil {
				return errJSON
			}
			out[key] = json.RawMessage(encoded)
			disk[key] = value
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		latestInfo, errStat := os.Lstat(path)
		if errStat != nil || !os.SameFile(info, latestInfo) {
			return fmt.Errorf("bind Codex OAuth: backing file changed during binding")
		}
		latestData, errLatest := os.ReadFile(path)
		if errLatest != nil || !bytes.Equal(data, latestData) {
			return fmt.Errorf("bind Codex OAuth: backing file changed during binding")
		}
		if errWrite := codex.WriteJSONFileAtomic(path, out); errWrite != nil {
			return fmt.Errorf("bind Codex OAuth: persist binding: %w", errWrite)
		}

		m.mu.Lock()
		current := m.auths[id]
		if current == nil || current.RegistrationEpoch != expectedEpoch || codexMetadataAccountID(current.Metadata) != expectedAccount {
			m.mu.Unlock()
			return fmt.Errorf("bind Codex OAuth: registration changed before publication")
		}
		updated := current.Clone()
		updated.Metadata = disk
		updated.Storage = nil
		updated.CodexRefreshPatch = nil
		if disabled, okDisabled := disk["disabled"].(bool); okDisabled {
			updated.Disabled = disabled
		}
		if updated.Disabled {
			updated.Status = StatusDisabled
		} else if updated.Status == StatusDisabled {
			updated.Status = StatusActive
		}
		now := time.Now()
		updated.LastRefreshedAt = now
		if !updated.Disabled {
			applyPreservedRejectedTokens(current, updated, false)
			if updated.RejectedAccessToken == "" || authAccessToken(updated) != updated.RejectedAccessToken {
				updated.RefreshFailures = 0
				updated.NextRefreshAfter = time.Time{}
				if hasUnauthorizedAuthFailure(current) ||
					(updated.LastError != nil && (isUnauthorizedError(updated.LastError) || isInvalidGrantResultError(updated.LastError))) ||
					(updated.Unavailable && !updated.NextRetryAfter.After(now) && !updated.Quota.NextRecoverAt.After(now)) {
					updated.Unavailable = false
					updated.LastError = nil
					updated.StatusMessage = ""
					updated.Status = StatusActive
					updated.NextRetryAfter = time.Time{}
					cooldownChanged = true
				}
				clearUnauthorizedModelStates(updated, now)
			}
			if len(updated.ModelStates) > 0 {
				cooldownChanged = true
			}
			if current.Quota.Exceeded && (current.Quota.NextRecoverAt.IsZero() || current.Quota.NextRecoverAt.After(now)) {
				updated.Quota = current.Quota.Clone()
				updated.Unavailable = current.Unavailable
				updated.NextRetryAfter = current.NextRetryAfter
				updated.Status = current.Status
			}
		}
		updated.Generation++
		updated.CredentialVersion = max(current.CredentialVersion, 1) + 1
		updated.UpdatedAt = now
		updated.ClearFileSnapshot()
		m.auths[id] = updated
		m.notifyAuthChangeLocked(id)
		committed = updated.Clone()
		m.mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	release()
	m.finishAuthUpdate(ctx, committed, cooldownChanged)
	return committed, nil
}

func codexMetadataAccountID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if accountID, _ := metadata["account_id"].(string); strings.TrimSpace(accountID) != "" {
		return strings.TrimSpace(accountID)
	}
	idToken, _ := metadata["id_token"].(string)
	claims, errParse := codex.ParseJWTToken(strings.TrimSpace(idToken))
	if errParse != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.GetAccountID())
}
