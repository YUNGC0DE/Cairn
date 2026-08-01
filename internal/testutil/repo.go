// Package testutil builds throwaway git repositories for tests.
//
// It is a non-test package so tests in any package can import it; nothing in the
// shipped binary depends on it.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/YUNGC0DE/Cairn/internal/gitx"
)

// Repo is a temporary git repository.
type Repo struct {
	*gitx.Repo
	T *testing.T
}

// NewRepo initialises an empty repository with deterministic identity and
// settings, so tests do not depend on the developer's git config.
func NewRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	// macOS temp dirs are symlinked through /private; resolve so paths compare.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	run(t, dir, "init", "--initial-branch=main")
	run(t, dir, "config", "user.name", "Cairn Test")
	run(t, dir, "config", "user.email", "test@cairn.invalid")
	run(t, dir, "config", "commit.gpgsign", "false")
	run(t, dir, "config", "core.hooksPath", filepath.Join(dir, ".git", "hooks"))

	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatalf("open temp repo: %v", err)
	}
	return &Repo{Repo: repo, T: t}
}

// Write creates or overwrites a file, creating parent directories.
func (r *Repo) Write(path, content string) {
	r.T.Helper()
	full := filepath.Join(r.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.T.Fatal(err)
	}
}

// Add stages paths.
func (r *Repo) Add(paths ...string) {
	r.T.Helper()
	run(r.T, r.Root, append([]string{"add"}, paths...)...)
}

// Commit commits the index with the given message, bypassing hooks.
func (r *Repo) Commit(message string) string {
	r.T.Helper()
	run(r.T, r.Root, "commit", "--no-verify", "-m", message)
	return r.HeadSHA()
}

// Git runs an arbitrary git command and returns stdout.
func (r *Repo) Git(args ...string) string {
	r.T.Helper()
	return run(r.T, r.Root, args...)
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
