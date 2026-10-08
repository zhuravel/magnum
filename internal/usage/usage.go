// Package usage reads how much of an agent CLI's subscription budget is used.
//
// Codex writes a token_count event into its session file
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl) after every turn; the
// event carries the account's rate-limit windows as Codex last saw them:
//
//	{"timestamp":"…","type":"event_msg","payload":{"type":"token_count",
//	 "rate_limits":{"primary":{"used_percent":7.0,"window_minutes":10080,
//	 "resets_at":1791710538},"secondary":null,"plan_type":"pro"}}}
//
// Codex reads the newest of those snapshots without starting Codex or calling
// any API, CodexAt the newest one at or before a past time. Decide turns a
// snapshot into a soft/hard verdict for the scheduler. Everything here is
// read-only.
package usage

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Bounds of one Codex read.
const (
	// MaxFiles is how many session files, newest modification first, are
	// searched for a snapshot.
	MaxFiles = 8
	// MaxTailBytes is how far from its end a session file is read.
	MaxTailBytes = 4 << 20
	// MaxHeads is how many session files written at or after the time
	// CodexAt asks about have their first line read, to tell whether the
	// session began before that time.
	MaxHeads   = 256
	headBytes  = 512
	chunkBytes = 64 << 10
)

// ErrNoData means no rate-limit snapshot was found: no session directory, no
// session files, or none of the newest files carries one.
var ErrNoData = errors.New("usage: no Codex rate-limit snapshot found")

// Window is one rate-limit window as Codex reported it.
type Window struct {
	// UsedPercent is the share of the window's budget used, 0–100. Codex
	// reports it as of At; a window whose ResetsAt has passed reads 0.
	UsedPercent float64
	// WindowMinutes is the window's length (10080 for the weekly limit).
	WindowMinutes int
	// ResetsAt is when the window resets; zero when Codex did not say.
	ResetsAt time.Time
}

// Snapshot is the newest rate-limit report found. The embedded Window is
// Codex's primary limit; Secondary is the other one when Codex reports two
// (older Codex versions report a 5-hour primary and a weekly secondary).
type Snapshot struct {
	Window
	Secondary *Window
	// Plan is the account's plan_type ("pro", "plus", …); empty when absent.
	// Sessions under different logins can report different plans; the
	// newest snapshot wins whatever its plan.
	Plan string
	// At is the timestamp of the event the snapshot came from.
	At time.Time
	// Path is the session file it came from.
	Path string
}

// Used is the higher used percentage of the primary and secondary windows:
// the budget that runs out first.
func (s Snapshot) Used() float64 {
	if s.Secondary != nil {
		return max(s.UsedPercent, s.Secondary.UsedPercent)
	}
	return s.UsedPercent
}

// Level is a scheduling verdict on a snapshot.
type Level int

const (
	// OK means below the soft threshold.
	OK Level = iota
	// Soft means at or above the soft threshold: defer optional work (first
	// reviews), keep what is already promised.
	Soft
	// Hard means at or above the hard threshold: start nothing that needs
	// this agent kind.
	Hard
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Soft:
		return "soft"
	case Hard:
		return "hard"
	}
	return "unknown"
}

// Decide compares snap.Used() with the thresholds (percentages). A
// threshold of zero or less is off.
func Decide(snap Snapshot, soft, hard float64) Level {
	used := snap.Used()
	switch {
	case hard > 0 && used >= hard:
		return Hard
	case soft > 0 && used >= soft:
		return Soft
	}
	return OK
}

// DefaultCodexHome is $CODEX_HOME, or ~/.codex when it is unset.
func DefaultCodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// Codex returns the newest rate-limit snapshot in home's session files (home
// is a CODEX_HOME; empty means DefaultCodexHome). It looks at the MaxFiles
// most recently modified rollout files, reads each from its end (at most
// MaxTailBytes), skips malformed lines and token_count events without rate
// limits, and returns the snapshot with the latest timestamp. Windows whose
// reset time is at or before now read 0% used. It returns ErrNoData when
// nothing is found.
func Codex(ctx context.Context, home string, now time.Time) (Snapshot, error) {
	return codex(ctx, cmp.Or(home, DefaultCodexHome()), now, MaxFiles, MaxTailBytes)
}

type sessionFile struct {
	path  string
	mtime time.Time
}

func codex(ctx context.Context, home string, now time.Time, maxFiles int, maxTail int64) (Snapshot, error) {
	files, err := sessions(ctx, filepath.Join(home, "sessions"))
	if err != nil {
		return Snapshot{}, err
	}
	best, err := newestSnapshot(ctx, files[:min(maxFiles, len(files))], maxTail, time.Time{})
	if err != nil {
		return Snapshot{}, err
	}
	best.Window = best.expire(now)
	if best.Secondary != nil {
		w := best.Secondary.expire(now)
		best.Secondary = &w
	}
	return best, nil
}

// CodexAt returns the newest rate-limit snapshot in home's session files
// (empty means DefaultCodexHome) stamped at or before at: the budget as
// Codex last reported it then. Its windows read as reported, one that has
// reset since included; the caller compares ResetsAt. It reads, each from
// its end (at most MaxTailBytes), the files last written before at as Codex
// reads the newest (the newest one usually settles it) and the sessions
// spanning at, written at or after it but started at or before it; at most
// MaxFiles of each. Of a file written at or after at only the first line,
// the session_meta record stamped when the session started, is read to tell
// (of the MaxHeads such files written closest to at). It returns ErrNoData
// when nothing is found.
func CodexAt(ctx context.Context, home string, at time.Time) (Snapshot, error) {
	return codexAt(ctx, cmp.Or(home, DefaultCodexHome()), at, MaxHeads, MaxFiles, MaxTailBytes)
}

func codexAt(ctx context.Context, home string, at time.Time, maxHeads, maxFiles int, maxTail int64) (Snapshot, error) {
	files, err := sessions(ctx, filepath.Join(home, "sessions"))
	if err != nil {
		return Snapshot{}, err
	}
	// files is newest first: those written at or after at, then the rest.
	before := slices.IndexFunc(files, func(f sessionFile) bool { return f.mtime.Before(at) })
	if before < 0 {
		before = len(files)
	}
	var read []sessionFile
	for i := before - 1; i >= max(before-maxHeads, 0) && len(read) < maxFiles; i-- {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if started, ok := sessionStart(files[i].path); ok && !started.After(at) {
			read = append(read, files[i])
		}
	}
	slices.Reverse(read) // newest first again
	older := files[before:]
	read = append(read, older[:min(maxFiles, len(older))]...)
	return newestSnapshot(ctx, read, maxTail, at)
}

// newestSnapshot is the snapshot with the latest timestamp at or before
// until (any time when until is zero) in files, which are sorted newest
// modification first. A file last written before the best snapshot found
// cannot hold a newer one: the files from it on are not read. It returns
// ErrNoData when there is none.
func newestSnapshot(ctx context.Context, files []sessionFile, maxTail int64, until time.Time) (Snapshot, error) {
	var best Snapshot
	found := false
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if found && f.mtime.Before(best.At) {
			break
		}
		snap, ok := lastSnapshot(f.path, maxTail, until)
		if ok && (!found || snap.At.After(best.At)) {
			best, found = snap, true
		}
	}
	if !found {
		return Snapshot{}, ErrNoData
	}
	return best, nil
}

// expire zeroes the used share of a window that has reset since the report.
func (w Window) expire(now time.Time) Window {
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		w.UsedPercent = 0
	}
	return w
}

// sessions lists dir's YYYY/MM/DD/rollout-*.jsonl files, newest
// modification first. It stats every session file (a few thousand stats, no
// reads): a resumed session appends to its original file under an old date,
// so the date directories alone do not say what is new.
func sessions(ctx context.Context, dir string) ([]sessionFile, error) {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real // WalkDir does not follow a symlinked root
		}
	}
	var files []sessionFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil // an unreadable subtree is skipped
		}
		if d.IsDir() {
			if path != dir && !isDigits(d.Name()) {
				return filepath.SkipDir // index/ and anything that is not a date
			}
			return ctx.Err()
		}
		name := d.Name()
		if !d.Type().IsRegular() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, sessionFile{path: path, mtime: info.ModTime()})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoData
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b sessionFile) int {
		return cmp.Or(b.mtime.Compare(a.mtime), strings.Compare(b.path, a.path))
	})
	return files, nil
}

var sessionStartPrefix = []byte(`{"timestamp":"`)

// sessionStart is when the session in path began: the timestamp of its first
// line, the session_meta record, which Codex writes with the timestamp
// first. Only the first headBytes are read (the record carries the session's
// instructions and runs to tens of kilobytes); ok is false when they hold no
// such timestamp.
func sessionStart(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	head := make([]byte, headBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return time.Time{}, false
	}
	stamp, ok := bytes.CutPrefix(head[:n], sessionStartPrefix)
	if !ok {
		return time.Time{}, false
	}
	stamp, _, ok = bytes.Cut(stamp, []byte(`"`))
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, string(stamp))
	return at, err == nil
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// record is the part of a session line this package reads.
type record struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary   *rawWindow `json:"primary"`
			Secondary *rawWindow `json:"secondary"`
			PlanType  string     `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type rawWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes int      `json:"window_minutes"`
	ResetsAt      int64    `json:"resets_at"` // unix seconds
}

func (r *rawWindow) window() (Window, bool) {
	if r == nil || r.UsedPercent == nil {
		return Window{}, false
	}
	w := Window{UsedPercent: *r.UsedPercent, WindowMinutes: r.WindowMinutes}
	if r.ResetsAt > 0 {
		w.ResetsAt = time.Unix(r.ResetsAt, 0).UTC()
	}
	return w, true
}

var tokenCountMarker = []byte(`"token_count"`)

// parseSnapshot reads one session line; ok is false for anything that is
// not a token_count event with a primary rate-limit window and a timestamp.
func parseSnapshot(line []byte) (Snapshot, bool) {
	if !bytes.Contains(line, tokenCountMarker) {
		return Snapshot{}, false
	}
	var rec record
	if json.Unmarshal(line, &rec) != nil || rec.Payload.Type != "token_count" || rec.Payload.RateLimits == nil {
		return Snapshot{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return Snapshot{}, false
	}
	rl := rec.Payload.RateLimits
	primary, ok := rl.Primary.window()
	if !ok {
		return Snapshot{}, false
	}
	snap := Snapshot{Window: primary, Plan: rl.PlanType, At: at}
	if sec, ok := rl.Secondary.window(); ok {
		snap.Secondary = &sec
	}
	return snap, true
}

// lastSnapshot is the last snapshot line in path's final maxTail bytes
// stamped at or before until (any time when until is zero).
func lastSnapshot(path string, maxTail int64, until time.Time) (Snapshot, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Snapshot{}, false
	}
	var snap Snapshot
	found := false
	_ = scanBackward(f, info.Size(), maxTail, func(line []byte) bool {
		snap, found = parseSnapshot(line)
		found = found && (until.IsZero() || !snap.At.After(until))
		return found
	})
	if found {
		snap.Path = path
	}
	return snap, found
}

// scanBackward calls fn with each complete line of r (whose size is size),
// last line first, until fn returns true or maxBytes have been read. A line
// cut by the maxBytes bound is not passed.
func scanBackward(r io.ReaderAt, size, maxBytes int64, fn func(line []byte) bool) error {
	stop := max(size-maxBytes, 0)
	pos := size
	var carry []byte // the start of a line whose end was read already
	buf := make([]byte, chunkBytes)
	for pos > stop {
		n := min(int64(chunkBytes), pos-stop)
		pos -= n
		if _, err := r.ReadAt(buf[:n], pos); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		data := append(buf[:n:n], carry...)
		for {
			i := bytes.LastIndexByte(data, '\n')
			if i < 0 {
				break
			}
			if line := bytes.TrimSpace(data[i+1:]); len(line) > 0 && fn(line) {
				return nil
			}
			data = data[:i]
		}
		carry = append([]byte(nil), data...)
	}
	if pos == 0 {
		if line := bytes.TrimSpace(carry); len(line) > 0 {
			fn(line)
		}
	}
	return nil
}
