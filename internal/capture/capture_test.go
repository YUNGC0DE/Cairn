package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/YUNGC0DE/git-cairn/internal/transcript"
)

func TestParsersAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Parsers() {
		if seen[p.Name()] {
			t.Errorf("two parsers answer to %q — offsets and trailers would collide", p.Name())
		}
		seen[p.Name()] = true
	}
	if len(seen) < 2 {
		t.Errorf("want claude-code and cursor registered, got %v", seen)
	}
}

// The pointer is a hash of the transcript and never its contents: a commit that
// carried the session text would put every secret an agent was shown into git
// history, permanently and on every clone.
func TestTranscriptPointerHashesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"role":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := TranscriptPointer(transcript.Ref{Agent: "cursor", ID: "s", Path: path})
	if len(got) != len("sha256:")+64 {
		t.Fatalf("pointer = %q, want a sha256", got)
	}
	if missing := TranscriptPointer(transcript.Ref{Path: filepath.Join(dir, "gone.jsonl")}); missing != "" {
		t.Errorf("pointer for a missing transcript = %q, want empty", missing)
	}
}
