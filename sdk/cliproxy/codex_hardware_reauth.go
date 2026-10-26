package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const codexHardwareReauthAcquisitionTimeout = 30 * time.Second

type codexOuterBinder interface {
	BindCodexOuter(context.Context, string, uint64, string, *codexauth.CodexTokenStorage) (*coreauth.Auth, error)
}

type codexHardwareDeviceFlow interface {
	VerificationURL() string
	UserCode() string
	ExpiresIn() time.Duration
	Complete(context.Context) (*coreauth.Auth, error)
}

type codexHardwareDeviceFlowFactory func(context.Context, *config.Config) (codexHardwareDeviceFlow, error)

type codexHardwareReauthFlow struct {
	ready      chan struct{}
	targetID   string
	epoch      uint64
	accountID  string
	expiresAt  time.Time
	hint       executor.CodexHardwareReauthHint
	err        error
	deviceFlow codexHardwareDeviceFlow
	cancel     context.CancelFunc
}

type codexHardwareReauthCoordinator struct {
	binder        codexOuterBinder
	currentConfig func() *config.Config
	factory       codexHardwareDeviceFlowFactory
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	flows         map[string]*codexHardwareReauthFlow
}

func newCodexHardwareReauthCoordinator(binder codexOuterBinder, currentConfig func() *config.Config) *codexHardwareReauthCoordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &codexHardwareReauthCoordinator{
		binder:        binder,
		currentConfig: currentConfig,
		factory: func(ctx context.Context, cfg *config.Config) (codexHardwareDeviceFlow, error) {
			return sdkauth.StartCodexDeviceFlow(ctx, cfg)
		},
		ctx:    ctx,
		cancel: cancel,
		flows:  make(map[string]*codexHardwareReauthFlow),
	}
}

func (c *codexHardwareReauthCoordinator) Close() {
	if c == nil || c.cancel == nil {
		return
	}
	c.cancel()
}

func codexHardwareReauthAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if accountID, _ := auth.Metadata["account_id"].(string); strings.TrimSpace(accountID) != "" {
		return strings.TrimSpace(accountID)
	}
	idToken, _ := auth.Metadata["id_token"].(string)
	claims, errParse := codexauth.ParseJWTToken(strings.TrimSpace(idToken))
	if errParse != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.GetAccountID())
}

func codexHardwareReauthEligible(auth *coreauth.Auth) (string, bool) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || auth.RegistrationEpoch == 0 ||
		!strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") ||
		auth.AuthSourceKind() != coreauth.AuthSourceFile || auth.AuthKind() != coreauth.AuthKindOAuth {
		return "", false
	}
	accountID := codexHardwareReauthAccountID(auth)
	return accountID, accountID != ""
}

func (flow *codexHardwareReauthFlow) clientHint(now time.Time) executor.CodexHardwareReauthHint {
	hint := flow.hint
	if now.IsZero() {
		now = time.Now()
	}
	hint.ExpiresIn = flow.expiresAt.Sub(now)
	return hint
}

func (c *codexHardwareReauthCoordinator) StartCodexHardwareReauth(_ context.Context, auth *coreauth.Auth) (executor.CodexHardwareReauthHint, error) {
	if c == nil || c.binder == nil || c.factory == nil || c.currentConfig == nil {
		return executor.CodexHardwareReauthHint{}, fmt.Errorf("Codex hardware reauthentication is unavailable")
	}
	accountID, eligible := codexHardwareReauthEligible(auth)
	if !eligible {
		return executor.CodexHardwareReauthHint{}, fmt.Errorf("Codex credential is not eligible for hardware reauthentication")
	}
	now := time.Now()
	key := strings.TrimSpace(auth.ID)

	c.mu.Lock()
	if existing := c.flows[key]; existing != nil {
		if existing.epoch == auth.RegistrationEpoch && existing.accountID == accountID && existing.expiresAt.After(now) {
			ready := existing.ready
			c.mu.Unlock()
			select {
			case <-ready:
				if existing.err != nil {
					return executor.CodexHardwareReauthHint{}, existing.err
				}
				return existing.clientHint(time.Now()), nil
			case <-c.ctx.Done():
				return executor.CodexHardwareReauthHint{}, c.ctx.Err()
			}
		}
		if existing.cancel != nil {
			existing.cancel()
		}
		delete(c.flows, key)
	}
	entry := &codexHardwareReauthFlow{
		ready:     make(chan struct{}),
		targetID:  key,
		epoch:     auth.RegistrationEpoch,
		accountID: accountID,
		expiresAt: now.Add(codexHardwareReauthAcquisitionTimeout),
	}
	c.flows[key] = entry
	c.mu.Unlock()

	startCtx, cancelStart := context.WithTimeout(c.ctx, codexHardwareReauthAcquisitionTimeout)
	deviceFlow, errStart := c.factory(startCtx, c.currentConfig())
	cancelStart()
	if errStart == nil && (deviceFlow == nil || strings.TrimSpace(deviceFlow.VerificationURL()) == "" || strings.TrimSpace(deviceFlow.UserCode()) == "" || deviceFlow.ExpiresIn() <= 0) {
		errStart = fmt.Errorf("Codex device authorization returned an invalid response")
	}

	c.mu.Lock()
	if c.flows[key] != entry {
		c.mu.Unlock()
		return executor.CodexHardwareReauthHint{}, fmt.Errorf("Codex hardware reauthentication target changed")
	}
	if errStart != nil {
		entry.err = fmt.Errorf("failed to start Codex hardware reauthentication")
		close(entry.ready)
		delete(c.flows, key)
		c.mu.Unlock()
		return executor.CodexHardwareReauthHint{}, entry.err
	}
	entry.deviceFlow = deviceFlow
	entry.expiresAt = time.Now().Add(deviceFlow.ExpiresIn())
	entry.hint = executor.CodexHardwareReauthHint{
		VerificationURL: deviceFlow.VerificationURL(),
		UserCode:        deviceFlow.UserCode(),
		ExpiresIn:       deviceFlow.ExpiresIn(),
	}
	flowCtx, cancelFlow := context.WithDeadline(c.ctx, entry.expiresAt)
	entry.cancel = cancelFlow
	close(entry.ready)
	hint := entry.clientHint(time.Now())
	c.mu.Unlock()

	go c.complete(entry, flowCtx)
	return hint, nil
}

func (c *codexHardwareReauthCoordinator) complete(entry *codexHardwareReauthFlow, flowCtx context.Context) {
	defer func() {
		if entry.cancel != nil {
			entry.cancel()
		}
		c.mu.Lock()
		if c.flows[entry.targetID] == entry {
			delete(c.flows, entry.targetID)
		}
		c.mu.Unlock()
	}()
	record, errComplete := entry.deviceFlow.Complete(flowCtx)
	if errComplete != nil || record == nil {
		log.Warn("Codex hardware-key reauthentication did not complete")
		return
	}
	tokenStorage, okStorage := record.Storage.(*codexauth.CodexTokenStorage)
	if !okStorage || tokenStorage == nil || strings.TrimSpace(tokenStorage.AccountID) != entry.accountID {
		log.Warn("Codex hardware-key reauthentication returned a different account; credentials were not changed")
		return
	}
	persistCtx, cancelPersist := context.WithTimeout(c.ctx, codexHardwareReauthAcquisitionTimeout)
	defer cancelPersist()
	if _, errBind := c.binder.BindCodexOuter(persistCtx, entry.targetID, entry.epoch, entry.accountID, tokenStorage); errBind != nil {
		log.Warn("Codex hardware-key reauthentication completed but the credential changed before it could be saved")
		return
	}
	log.Info("Codex hardware-key reauthentication completed; retry the original request")
}

func (s *Service) codexHardwareReauthStarter() executor.CodexHardwareReauthStarter {
	if s == nil || s.coreManager == nil {
		return nil
	}
	s.codexHardwareReauthMu.Lock()
	defer s.codexHardwareReauthMu.Unlock()
	if s.codexHardwareReauth == nil {
		s.codexHardwareReauth = newCodexHardwareReauthCoordinator(s.coreManager, s.currentConfig)
	}
	return s.codexHardwareReauth
}
