package executor

import (
	"strings"
	"testing"
)

// Fable 5.2 and Mythos 5.2 (released 2026-11-09). Claude Code 2.1.321 sends
// both per-turn-control, which Mythos 5.1 still goes without. The cloak gives
// Fable 5.2 the preserved-thinking opt-in and progress display, and neither 5.2
// model the Fable 5.1 reporting-outcomes block or its claude-opus-5 fallback.

func TestClaudeFable52ModelMatching(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-fable-5-2":           true,
		"claude-fable-5-2[1m]":       true,
		"anthropic/claude-fable-5.2": true,
		"CLAUDE-MYTHOS-5-2":          true,
		"claude-fable-5-20":          false,
		"claude-fable-5-1":           false,
		"claude-mythos-5-1":          false,
		"claude-opus-5-5":            false,
	} {
		if got := isClaudeFable52Model(model); got != want {
			t.Errorf("isClaudeFable52Model(%q) = %v, want %v", model, got, want)
		}
		if want && isClaudeFable51Model(model) {
			t.Errorf("isClaudeFable51Model(%q) = true, want the 5.1-only additions kept off 5.2", model)
		}
	}
}

func TestClaudeFable52RequestCapabilities(t *testing.T) {
	for _, tt := range []struct {
		model             string
		perTurnControl    bool
		preservedThinking bool
		progressDisplay   bool
	}{
		{model: "claude-fable-5-2", perTurnControl: true, preservedThinking: true, progressDisplay: true},
		{model: "claude-fable-5-2[1m]", perTurnControl: true, preservedThinking: true, progressDisplay: true},
		{model: "claude-mythos-5-2", perTurnControl: true, preservedThinking: false, progressDisplay: true},
		{model: "claude-fable-5-1", perTurnControl: true, preservedThinking: true, progressDisplay: true},
		{model: "claude-mythos-5-1", perTurnControl: false, preservedThinking: false, progressDisplay: true},
	} {
		t.Run(tt.model, func(t *testing.T) {
			if got := claudeModelHasPerTurnEffort(tt.model); got != tt.perTurnControl {
				t.Errorf("claudeModelHasPerTurnEffort = %v, want %v", got, tt.perTurnControl)
			}
			if got := isClaudePreservedThinkingModel(tt.model); got != tt.preservedThinking {
				t.Errorf("isClaudePreservedThinkingModel = %v, want %v", got, tt.preservedThinking)
			}
			if got := claudeModelUsesProgressDisplay(tt.model); got != tt.progressDisplay {
				t.Errorf("claudeModelUsesProgressDisplay = %v, want %v", got, tt.progressDisplay)
			}
			body := []byte(`{"model":"` + tt.model + `","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`)
			betas := claudeCodeCLIBetas(body, nil, true)
			if got := strings.Contains(betas, "per-turn-control-2026-07-01"); got != tt.perTurnControl {
				t.Errorf("per-turn-control in %q = %v, want %v", betas, got, tt.perTurnControl)
			}
		})
	}
}
