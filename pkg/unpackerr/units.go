package unpackerr

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/Unpackerr/unpackerr/pkg/extract"
	"golift.io/xtractr"
)

// A season pack usually holds one folder per episode, each with its own archive
// set, and a sync tool may deliver those folders hours apart. With qBittorrent's
// file list Unpackerr knows when one folder is complete, so it extracts each
// top-level folder (a "unit") on its own as soon as all of that folder's files
// are in place, and the Starr app can import those episodes early. Only when:
//   - qBittorrent's file list is available (the per-file check, not the size check);
//   - no archive sits directly in the download folder;
//   - at least two top-level folders hold archives; and
//   - some folder is ready while another is not (otherwise one extraction is simpler).
//
// Once split, the item stays WAITING between rounds and is EXTRACTED only after the
// last unit. One unit is extracted per round, so a failure belongs to that unit: it is
// retried after retry_delay up to max_retries while other units carry on.

// archiveName matches file names that are (part of) an archive.
var archiveName = regexp.MustCompile(`(?i)\.(rar|r\d\d|s\d\d|zip|z\d\d|7z|\d\d\d|tar|gz|tgz|bz2|tbz2|xz|iso)$`)

var (
	errUnitsFailed    = errors.New("folders failed to extract")
	errRemnantBlocked = errors.New("blocked by interrupted-extraction remnants")
)

// unitCheck is torrentCheck per top-level folder.
type unitCheck struct {
	files, missing int
	archive        bool
}

// addUnitFile counts rel (root/unit/... or root/file) and returns its unit, or nil
// for a file directly in the download folder.
func (c *torrentCheck) addUnitFile(rel string) *unitCheck {
	parts := strings.SplitN(rel, string(filepath.Separator), 3) //nolint:mnd // root, unit, rest.
	if len(parts) < 3 {                                         //nolint:mnd
		if archiveName.MatchString(rel) {
			c.topArchive = true
		}

		return nil
	}

	if c.units == nil {
		c.units = make(map[string]*unitCheck)
	}

	unit := c.units[parts[1]]
	if unit == nil {
		unit = &unitCheck{}
		c.units[parts[1]] = unit
	}

	unit.files++
	unit.archive = unit.archive || archiveName.MatchString(parts[2])

	return unit
}

// archiveUnits are the top-level folders that hold an archive, sorted.
func (c *torrentCheck) archiveUnits() []string {
	out := make([]string, 0, len(c.units))

	for name, unit := range c.units {
		if unit.archive {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out
}

// splittable reports whether the download's layout allows extracting it folder by folder.
func (c *torrentCheck) splittable() bool {
	return !c.topArchive && len(c.archiveUnits()) >= 2 // one folder is not a pack.
}

// readyUnits are the archive units that are complete on disk, with no Syncthing temp
// file under them, and that still need extracting (not done, dead, or in retry backoff).
func (u *Unpackerr) readyUnits(item *Extract, check *torrentCheck, now time.Time) []string {
	var ready []string

	for _, name := range check.archiveUnits() {
		unit := check.units[name]
		if unit.missing > 0 || item.Units.Has(name) || !u.unitRetryDue(item.Units, name, now) {
			continue
		}

		if findSyncthingTemp(filepath.Join(item.Path, name)) == "" {
			ready = append(ready, name)
		}
	}

	// Units that never failed go first, so a unit that keeps failing cannot starve the rest.
	slices.SortStableFunc(ready, func(a, b string) int {
		return item.Units.LastFail(a).Compare(item.Units.LastFail(b))
	})

	return ready
}

func (u *Unpackerr) unitRetryDue(units *extract.Units, name string, now time.Time) bool {
	if units == nil || units.FailedAt == nil {
		return true
	}

	failed, ok := units.FailedAt[name]

	return !ok || now.Sub(failed) >= u.RetryDelay.Duration
}

// unitCheck decides for a split (or splittable) download. split is false when the
// download is handled as one, and the caller carries on with the whole-download check.
func (u *Unpackerr) unitCheck(name string, item *Extract, now time.Time, check *torrentCheck) (qbitState, bool) {
	if item.Units == nil {
		if (check.missing == 0 && check.temp == "") || !check.splittable() {
			return qbitUnused, false
		}

		if len(u.readyUnits(item, check, now)) == 0 {
			return qbitUnused, false // nothing to start yet; log as a whole download.
		}

		item.Units = &extract.Units{All: check.archiveUnits()}
		u.Printf("[%s] Extracting %s one folder at a time as each folder's files arrive (%d folders)",
			item.Label(), name, len(item.Units.All))
	}

	units := item.Units
	for _, unit := range check.archiveUnits() {
		if !slices.Contains(units.All, unit) {
			units.All = append(units.All, unit)
		}
	}

	if ready := u.readyUnits(item, check, now); len(ready) > 0 {
		units.Round = ready[0]
		u.Printf("[%s] Folder complete, extracting %s (%d of %d folders done): %s",
			item.Label(), units.Round, len(units.Done), len(units.All), name)

		return qbitUnit, true
	}

	if item.Note != noteWaitingFiles || now.Sub(item.QbitLogAt) >= qbitLogEvery {
		item.QbitLogAt = now
		u.Printf("[%s] Completed item still arriving: %s, %d of %d folders extracted, %d of %d files in place (%sB of %sB)",
			item.Label(), name, len(units.Done), len(units.All), check.files-check.missing, check.files,
			bytefmt.ByteSize(check.have), bytefmt.ByteSize(check.want))
	}

	u.setItemNote(name, item, noteWaitingFiles)

	return qbitWaiting, true
}

// roundTarget is what this extraction round covers: the whole download, one unit
// folder, or (without a file list) the whole download minus finished units.
func roundTarget(item *Extract) (string, func(xtractr.ArchiveCandidate) bool) {
	units := item.Units
	if units == nil {
		return item.Path, nil
	}

	if units.Round != "" {
		return filepath.Join(item.Path, units.Round), nil
	}

	return item.Path, func(c xtractr.ArchiveCandidate) bool {
		rel, err := filepath.Rel(item.Path, c.Path)
		if err != nil {
			return true
		}

		return !units.Has(strings.SplitN(rel, string(filepath.Separator), 2)[0]) //nolint:mnd
	}
}

// roundSnapshot is keepDirSnapshot per unit: a retry of a unit reuses that unit's
// listing, and another unit gets its own.
func roundSnapshot(item *Extract, paths []string) (map[string]os.FileInfo, error) {
	if item.Units == nil {
		return keepDirSnapshot(item.PreFiles, paths...)
	}

	if item.Units.Snaps == nil {
		item.Units.Snaps = make(map[string]map[string]os.FileInfo)
	}

	snap, err := keepDirSnapshot(item.Units.Snaps[item.Units.Round], paths...)
	if err == nil {
		item.Units.Snaps[item.Units.Round] = snap
	}

	return snap, err
}

// unitRoundDone records a finished round. It returns true while units remain, after
// putting the item back to WAITING; false when this was the last unit, after setting
// resp.NewFiles (and resp.Error if a unit failed for good) for the final status.
// Caller holds the history lock.
func (u *Unpackerr) unitRoundDone(name string, item *Extract, resp *xtractr.Response, now time.Time) bool {
	units := item.Units

	if units.Round != "" {
		units.Done = append(units.Done, units.Round)
	} else {
		for _, unit := range units.All {
			if !units.Has(unit) {
				units.Done = append(units.Done, unit)
			}
		}
	}

	units.Files = append(units.Files, resp.NewFiles...)
	delete(units.Snaps, units.Round)
	units.Round = ""
	item.PreFiles = nil

	u.Printf("[%s] Extraction Finished: %s => %d of %d folders extracted, files extracted: %d, wrote: %sB",
		item.Label(), name, len(units.Done), len(units.All), len(resp.NewFiles), bytefmt.ByteSize(resp.Size))

	return u.unitsContinue(name, item, resp, now)
}

// unitRoundFailed records a failed single-unit round. The unit is retried after
// retry_delay, unless it hit a limit or ran out of retries. Returns like unitRoundDone.
func (u *Unpackerr) unitRoundFailed(
	name string, item *Extract, resp *xtractr.Response, final bool, now time.Time,
) bool {
	units := item.Units
	unit := units.Round

	if units.Fails == nil {
		units.Fails = make(map[string]uint)
		units.FailedAt = make(map[string]time.Time)
	}

	units.Fails[unit]++
	units.FailedAt[unit] = now
	units.Round = ""

	if final || units.Fails[unit] > u.maxRetries() {
		units.Dead = append(units.Dead, unit)
		u.Errorf("[%s] Extraction Failed for good: %s folder %s (%d attempts): %v",
			item.Label(), name, unit, units.Fails[unit], resp.Error)
	} else {
		u.Retries++
		u.Errorf("[%s] Extraction Failed: %s folder %s, retrying in %v (%d/%d): %v",
			item.Label(), name, unit, u.RetryDelay.Duration, units.Fails[unit], u.maxRetries(), resp.Error)
	}

	return u.unitsContinue(name, item, resp, now)
}

func (u *Unpackerr) unitsContinue(name string, item *Extract, resp *xtractr.Response, now time.Time) bool {
	units := item.Units
	cleanRoundTemp(item)

	if len(units.Done)+len(units.Dead) < len(units.All) {
		item.Status = WAITING
		item.Updated = now.Add(-u.StartDelay.Duration) // the next unit need not wait for start_delay.
		item.Resp = resp
		item.Note = ""
		u.maybeRecordHistory(name, item)
		u.notifyQueueLocked()

		return true
	}

	resp.NewFiles = slices.Clone(units.Files)
	if len(units.Dead) > 0 {
		resp.Error = fmt.Errorf("%w: %d of %d: %s", errUnitsFailed, len(units.Dead), len(units.All),
			strings.Join(units.Dead, ", "))
	}

	return false
}

// unitOutputResp is item.Resp with every file written by finished units, for the
// clean-up after import when a split item leaves the Starr queue before its last unit.
func unitOutputResp(item *Extract) *xtractr.Response {
	resp := &xtractr.Response{}

	if item.Resp != nil {
		copied := *item.Resp
		resp = &copied
	}

	resp.NewFiles = slices.Clone(item.Units.Files)
	resp.Error = nil

	return resp
}

// hasUnitOutput is true for a split item with at least one unit extracted.
func hasUnitOutput(item *Extract) bool {
	return item.Units != nil && len(item.Units.Done) > 0
}

// cloneUnits deep-copies units for the history file (Snaps are not saved).
func cloneUnits(units *extract.Units) *extract.Units {
	if units == nil {
		return nil
	}

	out := &extract.Units{
		All:   slices.Clone(units.All),
		Done:  slices.Clone(units.Done),
		Dead:  slices.Clone(units.Dead),
		Files: slices.Clone(units.Files),
		Round: units.Round,
	}

	if units.Fails != nil {
		out.Fails = make(map[string]uint, len(units.Fails))
		out.FailedAt = make(map[string]time.Time, len(units.FailedAt))

		maps.Copy(out.Fails, units.Fails)
		maps.Copy(out.FailedAt, units.FailedAt)
	}

	return out
}

// roundFiles returns the path, archive filter and archives for this extraction round.
// For a split item whose round finds no archive, the round counts as done with nothing
// written, and files is empty so the caller stops.
func (u *Unpackerr) roundFiles(
	name string, item *Extract, files xtractr.ArchiveList, now time.Time,
) (string, func(xtractr.ArchiveCandidate) bool, xtractr.ArchiveList) {
	path, accept := roundTarget(item)
	if item.Units == nil {
		return path, accept, files
	}

	files = xtractr.FindCompressedFiles(xtractr.Filter{Path: path, Accept: accept})
	if len(files) > 0 {
		return path, accept, files
	}

	u.lockHistory()
	defer u.unlockHistory()

	resp := &xtractr.Response{Started: now, X: &xtractr.Xtract{Name: name}}
	if !u.unitRoundDone(name, item, resp, now) {
		u.updateQueueStatus(&newStatus{Name: name, Status: EXTRACTED, Resp: resp}, now, true)
	}

	return path, accept, nil
}

// unitCallback handles a finished round of a split item. It returns true when it set
// the item's status itself, false when the normal success or failure path should run
// (a round over every remaining unit, or the last unit finishing cleanly).
// Caller holds the history lock.
func (u *Unpackerr) unitCallback(item *Extract, resp *xtractr.Response, remnants bool, now time.Time) bool {
	name := resp.X.Name
	failed := remnants || resp.Error != nil

	switch {
	case item.Units.Round == "" && failed:
		return false // a whole-download round: the normal retry applies.
	case failed:
		final := xtractr.IsLimitError(resp.Error) || (remnants && remnantAction(u.RemnantAction) == "off")
		if resp.Error == nil {
			resp.Error = errRemnantBlocked
		}

		if u.unitRoundFailed(name, item, resp, final, now) {
			return true
		}
	case u.unitRoundDone(name, item, resp, now):
		return true
	case resp.Error == nil:
		return false // the last unit: EXTRACTED with every unit's files.
	}

	item.NoRetry = true // every unit is finished; the failed ones already used their retries.
	u.Errorf("[%s] Extraction Failed: %s: %v", item.Label(), name, resp.Error)
	u.updateQueueStatus(&newStatus{Name: name, Status: EXTRACTFAILED, Resp: resp}, now, true)

	return true
}

// roundExtractTo keeps a unit round's temporary output outside the download folder.
// By default xtractr writes next to the extracted path, which for a unit is inside the
// download, where the Starr app could import a half-written file. Next to the download
// (as a whole-download round does) it is out of the Starr app's sight until moved in.
func roundExtractTo(item *Extract, path string) string {
	if path == item.Path {
		return ""
	}

	return strings.TrimRight(item.Path, `/\`) + suffix
}

// cleanRoundTemp removes the folder roundExtractTo used, once it is empty again.
func cleanRoundTemp(item *Extract) {
	_ = os.Remove(strings.TrimRight(item.Path, `/\`) + suffix)
}
