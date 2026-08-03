package config

import (
	"testing"
	"time"

	"github.com/YUNGC0DE/git-cairn/internal/testutil"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c := Load(nil, env(nil))
	if !c.Enabled {
		t.Error("cairn must be on by default")
	}
	if c.Mode != ModeMessage {
		t.Errorf("mode = %s, want message (the default)", c.Mode)
	}
	// Deliberately above the 12 s the original spec proposed: both distillation passes need ~30 s on
	// real hardware, and 12 s would silently skip verification.
	if c.Budget != 60*time.Second {
		t.Errorf("budget = %s, want 60s", c.Budget)
	}
}

func TestGitConfigIsRead(t *testing.T) {
	repo := testutil.NewRepo(t)
	repo.Git("config", "cairn.mode", "notes")
	repo.Git("config", "cairn.timeout", "30")
	repo.Git("config", "cairn.engine", "cursor-agent")

	c := Load(repo.Repo, env(nil))
	if c.Mode != ModeNotes {
		t.Errorf("mode = %s", c.Mode)
	}
	if c.Budget != 30*time.Second {
		t.Errorf("budget = %s", c.Budget)
	}
	if c.Engine != "cursor-agent" {
		t.Errorf("engine = %q", c.Engine)
	}
}

func TestEnvironmentBeatsGitConfig(t *testing.T) {
	repo := testutil.NewRepo(t)
	repo.Git("config", "cairn.mode", "notes")
	c := Load(repo.Repo, env(map[string]string{"CAIRN_MODE": "message"}))
	if c.Mode != ModeMessage {
		t.Errorf("mode = %s, want the environment override to win", c.Mode)
	}
}

func TestSkipOverridesEnabled(t *testing.T) {
	repo := testutil.NewRepo(t)
	repo.Git("config", "cairn.enabled", "true")
	c := Load(repo.Repo, env(map[string]string{"CAIRN_SKIP": "1"}))
	if c.Enabled {
		t.Error("CAIRN_SKIP must win over an enabling config")
	}
}

func TestTimeoutAcceptsBareSecondsAndDurations(t *testing.T) {
	cases := map[string]time.Duration{
		"20":    20 * time.Second,
		"20s":   20 * time.Second,
		"1m30s": 90 * time.Second,
	}
	for in, want := range cases {
		c := Load(nil, env(map[string]string{"CAIRN_TIMEOUT": in}))
		if c.Budget != want {
			t.Errorf("CAIRN_TIMEOUT=%q gave %s, want %s", in, c.Budget, want)
		}
	}
	// Nonsense must fall back to the default rather than disable the budget: a
	// commit with no time limit is the one failure mode we cannot allow.
	for _, bad := range []string{"", "soon", "0", "-5"} {
		c := Load(nil, env(map[string]string{"CAIRN_TIMEOUT": bad}))
		if c.Budget != 60*time.Second {
			t.Errorf("CAIRN_TIMEOUT=%q gave %s, want the default", bad, c.Budget)
		}
	}
}

func TestUnknownModeIsIgnored(t *testing.T) {
	c := Load(nil, env(map[string]string{"CAIRN_MODE": "carrier-pigeon"}))
	if c.Mode != ModeMessage {
		t.Errorf("mode = %s, want the default for an unknown value", c.Mode)
	}
}
