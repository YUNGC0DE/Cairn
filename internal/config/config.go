// Package config resolves cairn's settings.
//
// Cairn ships zero config files. Settings come from git config, so they
// live where a developer already looks and can be set per repo, and from
// environment variables for one-off overrides. Env wins over git config.
package config

import (
	"strconv"
	"strings"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/distill"
	"github.com/YUNGC0DE/git-cairn/internal/gitx"
	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

// Mode is where a record is written.
type Mode string

const (
	// ModeMessage writes into the commit body. Travels with clone for free,
	// works everywhere, and is irreversible.
	ModeMessage Mode = "message"
	// ModeNotes writes into refs/notes/cairn. Keeps history clean and lets a
	// record be rewritten or deleted, at the cost of an explicit refspec.
	ModeNotes Mode = "notes"
)

// Config is the resolved settings for one run.
type Config struct {
	Enabled      bool
	Mode         Mode
	Engine       string
	Model        string
	VerifyModel  string
	Effort       string
	Budget       time.Duration
	PromptBudget int
	DiffBudget   int
	Debug        bool
}

// DefaultDiffBudget caps how much staged diff is sent for distillation.
//
// Measured: at 60 kB a 36-file commit was truncated, and the verification pass
// then — correctly — marked every claim "unverifiable", silently turning a
// verified record into a partial one. 120 kB covers ordinary commits whole; when
// it does not, the truncation is reported rather than left to look like a
// confidence problem.
const DefaultDiffBudget = 120000

// Load resolves configuration for a repository. A nil repo resolves env only.
func Load(repo *gitx.Repo, getenv func(string) string) Config {
	c := Config{
		Enabled:      true,
		Mode:         ModeMessage,
		Budget:       distill.DefaultBudget,
		PromptBudget: transcript.DefaultBudget,
		DiffBudget:   DefaultDiffBudget,
	}
	git := func(string) string { return "" }
	if repo != nil {
		git = repo.ConfigGet
	}

	pick := func(env, key string) string {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v
		}
		return strings.TrimSpace(git(key))
	}

	if v := pick("CAIRN_ENABLED", "cairn.enabled"); v != "" {
		c.Enabled = truthy(v)
	}
	// CAIRN_SKIP is the escape hatch for a single commit: `CAIRN_SKIP=1 git commit`
	// (`--no-verify` would skip every hook, not just ours).
	if truthy(getenv("CAIRN_SKIP")) {
		c.Enabled = false
	}
	if v := pick("CAIRN_MODE", "cairn.mode"); v != "" {
		if m := Mode(strings.ToLower(v)); m == ModeMessage || m == ModeNotes {
			c.Mode = m
		}
	}
	c.Engine = pick("CAIRN_ENGINE", "cairn.engine")
	c.Model = pick("CAIRN_MODEL", "cairn.model")
	c.VerifyModel = pick("CAIRN_VERIFY_MODEL", "cairn.verifyModel")
	// Reasoning effort, for engines that take one. Left empty the engine decides;
	// `claude` uses low, because distillation is extraction, not reasoning.
	c.Effort = pick("CAIRN_EFFORT", "cairn.effort")
	if v := pick("CAIRN_TIMEOUT", "cairn.timeout"); v != "" {
		if d, ok := parseDuration(v); ok {
			c.Budget = d
		}
	}
	if v := pick("CAIRN_PROMPT_BUDGET", "cairn.promptBudget"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.PromptBudget = n
		}
	}
	if v := pick("CAIRN_DIFF_BUDGET", "cairn.diffBudget"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.DiffBudget = n
		}
	}
	c.Debug = truthy(getenv("CAIRN_DEBUG"))
	return c
}

// parseDuration accepts both "12" (seconds, the friendly form) and "12s".
func parseDuration(v string) (time.Duration, bool) {
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
