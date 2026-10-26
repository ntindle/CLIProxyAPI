package executor

import "github.com/router-for-me/CLIProxyAPI/v8/internal/config"

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg            *config.Config
	hardwareReauth CodexHardwareReauthStarter
}

func NewCodexExecutor(cfg *config.Config, hardwareReauth ...CodexHardwareReauthStarter) *CodexExecutor {
	executor := &CodexExecutor{cfg: cfg}
	if len(hardwareReauth) > 0 {
		executor.hardwareReauth = hardwareReauth[0]
	}
	return executor
}

func (e *CodexExecutor) Identifier() string { return "codex" }

func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ModelLevelCooling
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *CodexExecutor) SupportsApplyPatch() bool { return e != nil }
