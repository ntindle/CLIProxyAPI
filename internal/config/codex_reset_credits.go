package config

// CodexResetCreditsConfig controls when the proxy spends a Codex login's banked
// usage-limit resets, the credits Codex offers under /usage as "Redeem reset".
// A reset restarts every rate-limit window of the login, so the next scheduled
// reset moves to a full window from the redemption.
type CodexResetCreditsConfig struct {
	// Mode is "off" (the default), "dry-run" (log what would be redeemed) or "on".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// MinWait is the shortest block a reset is spent on. Default "6h".
	MinWait string `yaml:"min-wait,omitempty" json:"min-wait,omitempty"`
	// ExpiringWithin lets a reset that expires this soon be spent on a shorter
	// block, of at least 30 minutes. Default "72h"; "0" turns this off.
	ExpiringWithin string `yaml:"expiring-within,omitempty" json:"expiring-within,omitempty"`
	// AuthFiles limits redemption to these auth file names or credential IDs.
	// Empty means every Codex OAuth login.
	AuthFiles []string `yaml:"auth-files,omitempty" json:"auth-files,omitempty"`
}
