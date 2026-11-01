package auth

import "testing"

// The usage-limit refusal is proposed upstream; until it merges, the fork gate
// (TestFork*) blocks on it.
func TestForkUsageLimitRefusal(t *testing.T) {
	t.Run("pool out of quota", TestPoolOutOfCodexQuotaReportsUsageLimit)
	t.Run("weekly window", TestWeeklyUsageLimitUsesSecondaryWindow)
	t.Run("retryable without evidence", TestCooldownStaysRetryableWithoutUsageLimitEvidence)
	t.Run("body forms", TestCodexUsageLimitWindowReadsEveryBodyForm)
	t.Run("model state and websocket event", TestUsageLimitEvidenceFromModelStateAndWebsocketEvent)
	t.Run("manager and restart", TestManagerReportsUsageLimitOnceEveryCredentialIsRefused)
}
