package handlers

import "testing"

// The usage-limit refusal is proposed upstream; until it merges, the fork gate
// (TestFork*) blocks on it.
func TestForkUsageLimitHeaders(t *testing.T) {
	t.Run("usage limit", TestWriteErrorResponseUsageLimitHeaders)
	t.Run("retryable cooldown", TestWriteErrorResponseModelCooldownKeepsRetryAfter)
	t.Run("other headers kept", TestWriteSafeErrorHeadersLeavesOtherHeadersAlone)
}
