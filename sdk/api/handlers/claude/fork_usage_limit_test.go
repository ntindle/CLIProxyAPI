package claude

import "testing"

// The usage-limit refusal is proposed upstream; until it merges, the fork gate
// (TestFork*) blocks on it.
func TestForkUsageLimitClaudeHeaders(t *testing.T) {
	t.Run("retry headers", TestClaudeErrorResponseCarriesRetryHeaders)
}
