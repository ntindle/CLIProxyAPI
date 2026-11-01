package openai

import "testing"

// The usage-limit refusal is proposed upstream; until it merges, the fork gate
// (TestFork*) blocks on it.
func TestForkUsageLimitWebsocket(t *testing.T) {
	t.Run("exposed", TestShouldExposeResponsesUsageLimit)
	t.Run("payload headers", TestResponsesWebsocketUsageLimitPayloadCarriesCodexHeaders)
	t.Run("error event", TestResponsesWebsocketSendsUsageLimitWhenEveryCredentialIsOut)
	t.Run("retryable stays silent", TestResponsesWebsocketKeepsRetryableCooldownSilent)
}
