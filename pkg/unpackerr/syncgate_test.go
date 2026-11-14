package unpackerr

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncthingIncomplete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, size int) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("a.rar", 100)
	write("a.r00", 100)
	write(".syncthing.a.r01.tmp", 50) // partial data must not count.

	now := time.Now()
	item := &Extract{Path: dir, Size: 300}

	if _, _, waiting := syncthingIncomplete(item, now); !waiting {
		t.Fatal("expected to wait while 200 of 300 bytes are present")
	}

	// Unchanged for longer than the stall window: stop waiting.
	if _, _, waiting := syncthingIncomplete(item, now.Add(syncthingStall+time.Minute)); waiting {
		t.Fatal("expected a stalled sync to proceed")
	}

	write("a.r01", 100)

	if have, want, waiting := syncthingIncomplete(item, now); waiting || have != 300 || want != 300 {
		t.Fatalf("complete download: have %d want %d waiting %v", have, want, waiting)
	}

	// Unknown size never blocks.
	if _, _, waiting := syncthingIncomplete(&Extract{Path: dir}, now); waiting {
		t.Fatal("unknown size must not wait")
	}
}
