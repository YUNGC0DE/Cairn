package capture

import (
	"testing"

	"github.com/YUNGC0DE/Cairn/internal/transcript"
	"github.com/YUNGC0DE/Cairn/internal/transcript/cursoride"
)

// TestSharedStoreParsersComputeTheirOwnPointer guards the wiring rather than the
// hashing: Cursor's IDE keeps every conversation in one multi-gigabyte file, and
// the generic pointer would read all of it inside a commit hook.
func TestSharedStoreParsersComputeTheirOwnPointer(t *testing.T) {
	p := ParserByName(cursoride.Name)
	if p == nil {
		t.Fatal("cursor-ide is not registered")
	}
	if _, ok := p.(transcript.Pointerer); !ok {
		t.Fatal("cursor-ide must compute its own transcript pointer")
	}
	// A ref naming a store that is not there must come back empty, not fall
	// through to hashing whatever the path points at.
	got := TranscriptPointer(transcript.Ref{
		Agent: cursoride.Name, ID: "26369f10-2678-4c3c-a14f-b032df1df70e",
		Path: t.TempDir() + "/state.vscdb",
	})
	if got != "" {
		t.Errorf("pointer for a missing store = %q, want empty", got)
	}
}

func TestParsersAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Parsers() {
		if seen[p.Name()] {
			t.Errorf("two parsers answer to %q — offsets and trailers would collide", p.Name())
		}
		seen[p.Name()] = true
	}
	if len(seen) < 3 {
		t.Errorf("want claude-code, cursor-cli and cursor-ide registered, got %v", seen)
	}
}
