package unpackerr

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golift.io/xtractr"
)

// packFiles is a qBittorrent list for Pack/E01..E03, each with two volumes and a
// Sample folder, plus a top-level nfo.
func packFiles() []qbitFile {
	files := []qbitFile{{Name: "Pack/pack.nfo", Size: 5, Priority: 1}}

	for ep := 1; ep <= 3; ep++ {
		dir := fmt.Sprintf("Pack/E%02d/", ep)
		files = append(files,
			qbitFile{Name: dir + "e.rar", Size: 100, Priority: 1},
			qbitFile{Name: dir + "e.r00", Size: 100, Priority: 1},
			qbitFile{Name: dir + "Sample/sample.mkv", Size: 9, Priority: 1},
		)
	}

	return files
}

// deliverEpisode writes one episode folder's files at their listed sizes.
func deliverEpisode(t *testing.T, download string, ep int) {
	t.Helper()

	dir := filepath.Join(download, fmt.Sprintf("E%02d", ep))
	writeSized(t, filepath.Join(dir, "e.rar"), 100)
	writeSized(t, filepath.Join(dir, "e.r00"), 100)
	writeSized(t, filepath.Join(dir, "Sample", "sample.mkv"), 9)
}

func packTestItem(t *testing.T) (*Unpackerr, *Extract, *fakeQbit) {
	t.Helper()

	fake := newFakeQbit(t, "/")
	fake.add(packFiles()...)

	unpack, item := qbitTestItem(t, fake.URL+"/")

	item.Path = filepath.Join(filepath.Dir(item.Path), "Pack")
	writeSized(t, filepath.Join(item.Path, "pack.nfo"), 5)

	unpack.KeepHistory = 10
	unpack.Map = map[string]*Extract{"Pack": item}

	return unpack, item, fake
}

// finishRound plays the xtractr callback for the round in progress.
func finishRound(unpack *Unpackerr, item *Extract, err error, now time.Time) {
	unit := item.Units.Round
	resp := &xtractr.Response{
		Done:     true,
		X:        &xtractr.Xtract{Name: "Pack", Path: filepath.Join(item.Path, unit)},
		Started:  now,
		NewFiles: []string{filepath.Join(item.Path, unit, "episode.mkv")},
		Error:    err,
	}

	unpack.handleXtractrCallback(resp)
}

func TestTorrentCheckSplittable(t *testing.T) {
	t.Parallel()

	download := filepath.Join(t.TempDir(), "Pack")

	check, _ := checkTorrentFiles(download, packFiles())
	if !check.splittable() || !slices.Equal(check.archiveUnits(), []string{"E01", "E02", "E03"}) {
		t.Fatalf("pack: splittable=%v units=%v", check.splittable(), check.archiveUnits())
	}

	top := append(packFiles(), qbitFile{Name: "Pack/extra.rar", Size: 1, Priority: 1})
	if check, _ := checkTorrentFiles(download, top); check.splittable() {
		t.Fatal("an archive in the top folder keeps the download whole")
	}

	one := []qbitFile{{Name: "Pack/E01/e.rar", Size: 1, Priority: 1}, {Name: "Pack/Sample/s.mkv", Size: 1, Priority: 1}}
	if check, _ := checkTorrentFiles(download, one); check.splittable() {
		t.Fatal("one archive folder is not a pack")
	}
}

// The whole lifecycle: split when one folder is complete, a round per folder, a
// failed folder retried later without blocking the others, a restart between
// rounds, and EXTRACTED only after the last folder with every folder's files.
func TestSplitPackLifecycle(t *testing.T) { //nolint:funlen // one story, start to end.
	t.Parallel()

	unpack, item, _ := packTestItem(t)
	unpack.RetryDelay.Duration = 5 * time.Minute
	now := time.Now()

	// Nothing complete: waits as a whole download.
	unpack.extractCompletedDownload("Pack", now, item)

	if item.Units != nil || item.Status != WAITING {
		t.Fatalf("nothing complete: units %+v status %s", item.Units, item.Status)
	}

	// E02 complete: split, extract E02 alone, into a temp folder outside the download.
	deliverEpisode(t, item.Path, 2)
	unpack.extractCompletedDownload("Pack", now, item)

	if item.Units == nil || item.Units.Round != "E02" || item.Status != QUEUED {
		t.Fatalf("E02 complete: units %+v status %s", item.Units, item.Status)
	}

	if got := roundExtractTo(item, filepath.Join(item.Path, "E02")); filepath.Dir(got) != filepath.Dir(item.Path) {
		t.Fatalf("unit temp folder %s must sit next to the download, not in it", got)
	}

	finishRound(unpack, item, nil, now)

	if item.Status != WAITING || !slices.Equal(item.Units.Done, []string{"E02"}) || len(item.Units.Files) != 1 {
		t.Fatalf("after E02: status %s units %+v", item.Status, item.Units)
	}

	// Restart between rounds: the history row brings back the finished folder.
	restarted, restored := restoreFrom(t, unpack, item)
	if restored.Units == nil || !slices.Equal(restored.Units.Done, []string{"E02"}) || restored.Status != WAITING {
		t.Fatalf("restored: %+v", restored)
	}

	unpack, item = restarted, restored
	now = now.Add(time.Minute)

	// E01 and E03 complete: E01 goes first and fails; E03 goes next, E01 waits retry_delay.
	deliverEpisode(t, item.Path, 1)
	deliverEpisode(t, item.Path, 3)
	unpack.extractCompletedDownload("Pack", now, item)

	if item.Units.Round != "E01" {
		t.Fatalf("round %q, want E01", item.Units.Round)
	}

	finishRound(unpack, item, errQbitStatus, now)

	if item.Status != WAITING || item.Units.Fails["E01"] != 1 || item.NoRetry {
		t.Fatalf("E01 failed: status %s units %+v", item.Status, item.Units)
	}

	unpack.extractCompletedDownload("Pack", now, item)

	if item.Units.Round != "E03" {
		t.Fatalf("round %q, want E03 while E01 waits to retry", item.Units.Round)
	}

	finishRound(unpack, item, nil, now)

	unpack.extractCompletedDownload("Pack", now.Add(time.Minute), item)

	if item.Status != WAITING || item.Units.Round != "" {
		t.Fatalf("E01 inside retry_delay: status %s round %q", item.Status, item.Units.Round)
	}

	unpack.extractCompletedDownload("Pack", now.Add(unpack.RetryDelay.Duration), item)

	if item.Units.Round != "E01" {
		t.Fatalf("round %q, want the E01 retry", item.Units.Round)
	}

	finishRound(unpack, item, nil, now)

	if item.Status != EXTRACTED || item.Resp == nil || len(item.Resp.NewFiles) != 3 {
		t.Fatalf("all done: status %s resp %+v", item.Status, item.Resp)
	}
}

// restoreFrom builds a fresh Unpackerr from unpack's history rows, like a restart.
func restoreFrom(t *testing.T, unpack *Unpackerr, item *Extract) (*Unpackerr, *Extract) {
	t.Helper()

	next, fresh := qbitTestItem(t, unpack.Sonarr["0"].QbitURL)
	next.KeepHistory = 10
	next.Map = map[string]*Extract{}
	next.records = slices.Clone(unpack.records)
	next.restoreQueueFromHistory()

	restored := next.Map["Pack"]
	if restored == nil {
		t.Fatal("split item was not restored")
	}

	restored.IDs = fresh.IDs
	restored.Updated = time.Now().Add(-time.Hour) // past start_delay.

	if restored.Path != item.Path {
		t.Fatalf("restored path %s", restored.Path)
	}

	return next, restored
}

// A folder that hits a limit is not retried; the others still finish, and the item
// ends EXTRACTFAILED (no retry) because a folder is missing.
func TestSplitPackDeadUnit(t *testing.T) {
	t.Parallel()

	unpack, item, _ := packTestItem(t)
	now := time.Now()

	deliverEpisode(t, item.Path, 1)
	unpack.extractCompletedDownload("Pack", now, item)
	finishRound(unpack, item, fmt.Errorf("wrapped: %w", xtractr.ErrMaxRatio), now)

	if !slices.Equal(item.Units.Dead, []string{"E01"}) || item.Status != WAITING {
		t.Fatalf("limit error: status %s units %+v", item.Status, item.Units)
	}

	for _, ep := range []int{2, 3} {
		deliverEpisode(t, item.Path, ep)
		unpack.extractCompletedDownload("Pack", now, item)
		finishRound(unpack, item, nil, now)
	}

	if item.Status != EXTRACTFAILED || !item.NoRetry || !strings.Contains(item.Resp.Error.Error(), "E01") {
		t.Fatalf("status %s noretry %v resp %+v", item.Status, item.NoRetry, item.Resp)
	}
}

// Everything complete at the first look: one extraction, as before.
func TestPackCompleteExtractsWhole(t *testing.T) {
	t.Parallel()

	unpack, item, _ := packTestItem(t)

	for ep := 1; ep <= 3; ep++ {
		deliverEpisode(t, item.Path, ep)
	}

	unpack.extractCompletedDownload("Pack", time.Now(), item)

	if item.Units != nil || item.Status != QUEUED {
		t.Fatalf("complete pack: units %+v status %s", item.Units, item.Status)
	}
}

// A split item that leaves the Starr queue before its last folder is cleaned up
// like an imported item, with every finished folder's files.
func TestSplitPackLeavesQueue(t *testing.T) {
	t.Parallel()

	unpack, item, _ := packTestItem(t)
	now := time.Now()

	deliverEpisode(t, item.Path, 1)
	unpack.extractCompletedDownload("Pack", now, item)
	finishRound(unpack, item, nil, now)

	unpack.Sonarr["0"].polled = true // polled, and the title is not in the queue.
	unpack.checkQueueChanges(now)

	if item.Status != IMPORTED || item.Resp == nil || len(item.Resp.NewFiles) != 1 {
		t.Fatalf("status %s resp %+v", item.Status, item.Resp)
	}
}

// Without a file list the size check applies, and a split item extracts every
// remaining folder at once, skipping the finished ones; their output is not counted
// as delivered data.
func TestSplitPackFallbackSkipsDoneUnits(t *testing.T) {
	t.Parallel()

	unpack, item, fake := packTestItem(t)
	now := time.Now()

	deliverEpisode(t, item.Path, 1)
	unpack.extractCompletedDownload("Pack", now, item)
	finishRound(unpack, item, nil, now)
	writeSized(t, item.Units.Files[0], 1000) // extracted output, bigger than the archives.

	fake.Close()

	item.Size = 5 + 3*209
	unpack.extractCompletedDownload("Pack", now, item)

	if item.Status != WAITING || item.Note != noteWaitingSyncthing {
		t.Fatalf("output must not count toward the size: status %s note %q", item.Status, item.Note)
	}

	deliverEpisode(t, item.Path, 2)
	deliverEpisode(t, item.Path, 3)
	unpack.extractCompletedDownload("Pack", now, item)

	path, accept := roundTarget(item)
	if item.Status != QUEUED || path != item.Path || accept == nil {
		t.Fatalf("fallback round: status %s path %s", item.Status, path)
	}

	if accept(xtractr.ArchiveCandidate{Path: filepath.Join(item.Path, "E01", "e.rar")}) ||
		!accept(xtractr.ArchiveCandidate{Path: filepath.Join(item.Path, "E02", "e.rar")}) {
		t.Fatal("the remaining round must skip E01 and include E02")
	}
}

// A unit in retry does not go before a unit that never failed.
func TestSplitPackFailedUnitDoesNotStarve(t *testing.T) {
	t.Parallel()

	unpack, item, _ := packTestItem(t)
	unpack.RetryDelay.Duration = 0
	now := time.Now()

	deliverEpisode(t, item.Path, 1)
	unpack.extractCompletedDownload("Pack", now, item)
	finishRound(unpack, item, errQbitStatus, now)

	deliverEpisode(t, item.Path, 2)
	unpack.extractCompletedDownload("Pack", now, item)

	if item.Units.Round != "E02" {
		t.Fatalf("round %q, want E02 before the E01 retry", item.Units.Round)
	}
}
