package unpackerr

import (
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// syncthingStall is how long the local byte count of a Syncthing item may stay
// unchanged, while still short of the size the Starr app reports, before we
// stop waiting and extract anyway. This keeps a download whose reported size
// can never be reached (ignored files, skipped torrent files) from waiting forever.
const syncthingStall = 30 * time.Minute

// syncthingIncomplete reports whether a Syncthing-delivered download is still
// missing data. The Starr app marks a download complete as soon as the remote
// client finishes, but Syncthing copies files in its own order and creates a
// file's temp marker only when it starts that file. A multi-part archive can
// therefore sit half-delivered with no .tmp file present, and extracting it
// then fails (missing first volume, or a ratio error from missing volumes).
// Comparing the local size to the size the Starr app reported catches that.
func syncthingIncomplete(item *Extract, now time.Time) (have, want uint64, waiting bool) {
	want = item.Size
	if want == 0 {
		return 0, 0, false // size unknown; fall back to the .tmp check alone.
	}

	have = localDownloadSize(item.Path)
	if have >= want {
		return have, want, false
	}

	if have != item.SyncSeen || item.SyncSeenAt.IsZero() {
		item.SyncSeen = have
		item.SyncSeenAt = now
	}

	return have, want, now.Sub(item.SyncSeenAt) < syncthingStall
}

// localDownloadSize sums the regular files under path, ignoring Syncthing's
// in-progress temp files so partial data does not count as delivered.
func localDownloadSize(path string) uint64 {
	var total uint64

	_ = filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // unreadable entries just do not count.
		}

		if name := entry.Name(); strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".syncthing.") {
			return nil
		}

		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}

		return nil
	})

	return total
}
