package extract

import (
	"os"
	"slices"
	"strings"
	"time"

	"golift.io/starr"
	"golift.io/xtractr"
)

// Extract holds data for files being extracted.
type Extract struct {
	Syncthing bool
	// SplitFlac splits FLAC and APE images referenced by a CUE sheet.
	SplitFlac bool
	// APEFormat is ape, wav, or flac. Empty means ape.
	APEFormat xtractr.AudioFormat
	// APECompression is the Monkey's Audio level used when APEFormat is ape.
	// A default of 0 is set to 2000.
	APECompression int
	Retries        uint
	// HookFail is how many webhook or command-hook deliveries failed for this extract.
	HookFail uint
	// HookMessages maps webhook/cmdhook instance slug to Discord message id or Telegram message_id.
	HookMessages map[string]string
	Path         string // Local path (resolved for extraction on this host).
	OutputPath   string // Original path from Starr app (may be UNC/remote — used for ManualImport).
	App          starr.App
	// Name is an optional Starr instance label for logs, hooks, and the dashboard.
	// Empty uses App (Sonarr, Radarr, Folder, …). App stays the dialect for logic.
	Name        string
	URL         string
	Updated     time.Time
	DeleteDelay time.Duration
	DeleteOrig  bool
	Status      Status
	// Due is the next status-timer deadline. The main loop stamps it so HTTP
	// queue snapshots do not read live config or the folder tracker.
	Due     time.Time
	DueKind string
	IDs     map[string]any
	Resp    *xtractr.Response
	XProg   *Progress
	// Note is a short waiting-state caption for the live queue (e.g. no archives yet).
	Note string
	// Event is how an item arrived: fsnotify, polling, later manual. Starr omits it for now.
	Event string
	// PreFiles maps cleaned full paths present in each archive dest before
	// extraction to their Lstat info (nil when the stat failed). Dest folders
	// come from FindCompressedFiles so nested archive dirs are included.
	// Snapshot once per queue item; retries must not fold in leftovers that
	// failed to clear.
	PreFiles map[string]os.FileInfo
	// NoRetry is set for limit errors, remnant_action=off, or exhausted retries.
	// EXTRACTFAILED must not re-enter the retry loop or be promoted to DELETED
	// (that bounces a still-completed Starr item back to WAITING).
	NoRetry bool
	// MaxBytes is the resolved byte cap for this Starr item (0 = unlimited).
	MaxBytes uint64
	// Size is the download size the Starr app reports for this item (0 = unknown).
	// Every queue poll refreshes it. With Syncthing enabled, extraction waits
	// until this many bytes are on disk.
	Size uint64
	// SyncSeen is the finished-file byte count last measured for a Syncthing item,
	// SyncActive the newest Syncthing temp file write seen, and SyncSeenAt when
	// either last changed. Used to tell a slow sync from a stalled one.
	SyncSeen   uint64
	SyncActive time.Time
	SyncSeenAt time.Time
	// QbitLogAt is when the qBittorrent file check last logged for this item.
	QbitLogAt time.Time
	// Units is set when a multi-folder download (a season pack) is extracted one
	// folder at a time as each folder's files arrive. Nil means one extraction.
	Units *Units
}

// Units tracks a download that is extracted one top-level folder (unit) at a time.
// It is saved in the history file so a restart neither repeats nor forgets a unit.
type Units struct {
	// All is every top-level folder of the download that holds an archive.
	All []string `json:"all"`
	// Done are the units extracted so far, Dead the units that failed for good.
	Done []string `json:"done,omitempty"`
	Dead []string `json:"dead,omitempty"`
	// Files are the files written by finished unit rounds, for clean-up after import.
	Files []string `json:"files,omitempty"`
	// Fails counts failed rounds per unit; FailedAt is when the last one failed.
	Fails    map[string]uint      `json:"fails,omitempty"`
	FailedAt map[string]time.Time `json:"failedAt,omitempty"`
	// Round is the unit being extracted now; a restart re-extracts it.
	Round string `json:"round,omitempty"`
	// Snaps are per-unit pre-extraction listings (see Extract.PreFiles). Kept in
	// memory; the round's listing is also saved as PreFiles.
	Snaps map[string]map[string]os.FileInfo `json:"-"`
}

// Label is the human-facing instance name, or the dialect when Name is empty.
func (e *Extract) Label() string {
	if e == nil {
		return ""
	}

	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}

	return string(e.App)
}

// Has reports whether a unit needs no more work: extracted, or failed for good.
func (u *Units) Has(name string) bool {
	return u != nil && (slices.Contains(u.Done, name) || slices.Contains(u.Dead, name))
}

// LastFail is when a unit's last round failed (zero if it never failed).
func (u *Units) LastFail(name string) time.Time {
	if u == nil {
		return time.Time{}
	}

	return u.FailedAt[name]
}
