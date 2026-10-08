package usage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// session copies a fixture (or writes raw content) to
// home/sessions/<date>/rollout-<name>.jsonl and sets its modification time.
func session(t *testing.T, home, date, name, content string, mtime time.Time) string {
	t.Helper()
	if strings.HasPrefix(content, "testdata/") {
		b, err := os.ReadFile(content)
		if err != nil {
			t.Fatal(err)
		}
		content = string(b)
	}
	dir := filepath.Join(home, "sessions", filepath.FromSlash(date))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+name+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func tokenCount(at time.Time, used float64, plan string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":%g,"window_minutes":10080,"resets_at":1791800000},"secondary":null,"plan_type":%q}}}`+"\n",
		at.Format(time.RFC3339Nano), used, plan)
}

func TestCodexReadsTheLastSnapshotOfASession(t *testing.T) {
	home := t.TempDir()
	path := session(t, home, "2026/10/04", "a", "testdata/current.jsonl", now.Add(-time.Hour))

	snap, err := Codex(context.Background(), home, now)
	if err != nil {
		t.Fatal(err)
	}
	want := Snapshot{
		Window: Window{UsedPercent: 42, WindowMinutes: 10080, ResetsAt: time.Unix(1791800000, 0).UTC()},
		Plan:   "pro",
		At:     time.Date(2026, 10, 4, 9, 2, 0, 250e6, time.UTC),
		Path:   path,
	}
	if snap.Window != want.Window || snap.Secondary != nil || snap.Plan != want.Plan || !snap.At.Equal(want.At) || snap.Path != want.Path {
		t.Errorf("snapshot = %+v\nwant       %+v", snap, want)
	}
}

// Older Codex reported a 5-hour primary and a weekly secondary, and some
// token_count events carry no rate limits at all.
func TestCodexLegacyTwoWindowsAndNullRateLimits(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2025/09/20", "old", "testdata/legacy.jsonl", now.Add(-time.Hour))
	early := time.Date(2025, 9, 20, 11, 0, 0, 0, time.UTC) // before both resets

	snap, err := Codex(context.Background(), home, early)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 12 || snap.WindowMinutes != 299 || snap.Plan != "" {
		t.Errorf("primary = %+v plan %q", snap.Window, snap.Plan)
	}
	if snap.Secondary == nil || snap.Secondary.UsedPercent != 63 || snap.Secondary.WindowMinutes != 10079 {
		t.Fatalf("secondary = %+v", snap.Secondary)
	}
	if got := snap.Used(); got != 63 {
		t.Errorf("Used = %g, want the binding (weekly) 63", got)
	}
}

func TestCodexWindowThatHasResetReadsZero(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2025/09/20", "old", "testdata/legacy.jsonl", now.Add(-time.Hour))
	afterPrimary := time.Unix(1758370000, 0) // the 5-hour window reset, the weekly not yet

	snap, err := Codex(context.Background(), home, afterPrimary)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 0 || snap.Secondary.UsedPercent != 63 {
		t.Errorf("used = %g / %g; want 0 (reset) / 63", snap.UsedPercent, snap.Secondary.UsedPercent)
	}
	snap, _ = Codex(context.Background(), home, now)
	if snap.Used() != 0 {
		t.Errorf("Used = %g after both resets, want 0", snap.Used())
	}
}

// A resumed session appends to its file under the old date directory: the
// newest snapshot wins whatever directory it is in and whatever plan it is
// on.
func TestCodexPrefersTheNewestSnapshotAcrossFiles(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2026/10/04", "today", tokenCount(now.Add(-3*time.Hour), 50, "plus"), now.Add(-3*time.Hour))
	session(t, home, "2026/09/20", "resumed", tokenCount(now.Add(-10*time.Minute), 80, "pro"), now.Add(-10*time.Minute))
	// Modified most recently, but its last snapshot is older than the
	// resumed session's (the file's tail is other events).
	session(t, home, "2026/10/03", "chatty", tokenCount(now.Add(-2*time.Hour), 10, "pro")+`{"timestamp":"x","type":"response_item","payload":{}}`+"\n", now.Add(-time.Minute))

	snap, err := Codex(context.Background(), home, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 80 || snap.Plan != "pro" || !strings.Contains(snap.Path, "resumed") {
		t.Errorf("snapshot = %+v; want the resumed session's 80%%", snap)
	}
}

func TestCodexReadsOnlyTheNewestFiles(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2026/10/01", "old", tokenCount(now.Add(-72*time.Hour), 30, "pro"), now.Add(-72*time.Hour))
	session(t, home, "2026/10/04", "b", "{}\n", now.Add(-2*time.Minute))
	session(t, home, "2026/10/04", "c", "{}\n", now.Add(-time.Minute))

	if _, err := codex(context.Background(), home, now, 2, MaxTailBytes); !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v; the old file is beyond the newest two and must not be read", err)
	}
	snap, err := codex(context.Background(), home, now, 3, MaxTailBytes)
	if err != nil || snap.UsedPercent != 30 {
		t.Fatalf("with three files: %+v, %v", snap, err)
	}
}

func TestCodexReadsOnlyTheTail(t *testing.T) {
	home := t.TempDir()
	filler := strings.Repeat(`{"timestamp":"x","type":"response_item","payload":{"type":"message"}}`+"\n", 2000)
	session(t, home, "2026/10/04", "long", tokenCount(now.Add(-time.Hour), 70, "pro")+filler, now)

	if _, err := codex(context.Background(), home, now, MaxFiles, int64(len(filler))); !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v; the snapshot lies before the tail bound", err)
	}
	if snap, err := codex(context.Background(), home, now, MaxFiles, int64(len(filler))+4096); err != nil || snap.UsedPercent != 70 {
		t.Fatalf("snapshot inside the bound: %+v, %v", snap, err)
	}
}

func TestCodexNoData(t *testing.T) {
	for name, setup := range map[string]func(home string){
		"no sessions directory": func(string) {},
		"empty sessions":        func(h string) { _ = os.MkdirAll(filepath.Join(h, "sessions", "2026", "10", "04"), 0o755) },
		"no token_count": func(h string) {
			session(t, h, "2026/10/04", "a", `{"timestamp":"2026-10-04T09:00:00Z","type":"session_meta"}`+"\n", now)
		},
		"only null rate limits": func(h string) {
			session(t, h, "2026/10/04", "a", `{"timestamp":"2026-10-04T09:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}`+"\n", now)
		},
		"outside the date tree": func(h string) { session(t, h, "index/by-dir", "a", tokenCount(now, 5, "pro"), now) },
		"bad timestamp": func(h string) {
			session(t, h, "2026/10/04", "a", strings.Replace(tokenCount(now, 5, "pro"), now.Format(time.RFC3339Nano), "yesterday", 1), now)
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			setup(home)
			if snap, err := Codex(context.Background(), home, now); !errors.Is(err, ErrNoData) {
				t.Fatalf("Codex = %+v, %v; want ErrNoData", snap, err)
			}
		})
	}
}

func TestCodexStopsOnCancelledContext(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2026/10/04", "a", "testdata/current.jsonl", now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Codex(ctx, home, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestScanBackwardAcrossChunks(t *testing.T) {
	long := strings.Repeat("x", chunkBytes+100) // one line spanning two chunks
	content := "first\n" + long + "\nmiddle\n\nlast"
	var got []string
	err := scanBackward(strings.NewReader(content), int64(len(content)), int64(len(content)), func(line []byte) bool {
		got = append(got, string(line))
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"last", "middle", long, "first"}
	if len(got) != len(want) {
		t.Fatalf("lines = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %.20q…, want %.20q…", i, got[i], want[i])
		}
	}

	// A bound inside the long line: lines after it are passed, the cut one
	// and everything before it are not.
	got = nil
	_ = scanBackward(strings.NewReader(content), int64(len(content)), int64(len("middle\n\nlast"))+10, func(line []byte) bool {
		got = append(got, string(line))
		return false
	})
	if len(got) != 2 || got[0] != "last" || got[1] != "middle" {
		t.Errorf("bounded lines = %q", got)
	}

	// fn returning true stops the scan.
	got = nil
	_ = scanBackward(strings.NewReader(content), int64(len(content)), int64(len(content)), func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	if len(got) != 1 {
		t.Errorf("scan continued after fn returned true: %d lines", len(got))
	}
}

func TestDecide(t *testing.T) {
	snap := func(primary float64, secondary ...float64) Snapshot {
		s := Snapshot{Window: Window{UsedPercent: primary}}
		if len(secondary) > 0 {
			s.Secondary = &Window{UsedPercent: secondary[0]}
		}
		return s
	}
	for _, c := range []struct {
		name       string
		snap       Snapshot
		soft, hard float64
		want       Level
	}{
		{"below soft", snap(79.9), 80, 95, OK},
		{"at soft", snap(80), 80, 95, Soft},
		{"between", snap(94), 80, 95, Soft},
		{"at hard", snap(95), 80, 95, Hard},
		{"full", snap(100), 80, 95, Hard},
		{"secondary binds", snap(10, 96), 80, 95, Hard},
		{"soft off", snap(90), 0, 95, OK},
		{"hard off", snap(99), 80, 0, Soft},
		{"both off", snap(100), 0, 0, OK},
	} {
		if got := Decide(c.snap, c.soft, c.hard); got != c.want {
			t.Errorf("%s: Decide = %s, want %s", c.name, got, c.want)
		}
	}
	if Level(9).String() != "unknown" {
		t.Errorf("unknown level string")
	}
}

func TestDefaultCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/example-codex")
	if got := DefaultCodexHome(); got != "/tmp/example-codex" {
		t.Errorf("DefaultCodexHome = %q", got)
	}
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "/tmp/example-home")
	if got := DefaultCodexHome(); got != filepath.Join("/tmp/example-home", ".codex") {
		t.Errorf("DefaultCodexHome = %q", got)
	}
}

func TestCodexFollowsASymlinkedSessionsDirectory(t *testing.T) {
	real := t.TempDir()
	session(t, real, "2026/10/04", "a", "testdata/current.jsonl", now)
	home := t.TempDir()
	if err := os.Symlink(filepath.Join(real, "sessions"), filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	if snap, err := Codex(context.Background(), home, now); err != nil || snap.UsedPercent != 42 {
		t.Fatalf("Codex = %+v, %v", snap, err)
	}
}

// sessionMeta is a rollout's first line: the session_meta record stamped
// when the session started, longer than the head CodexAt reads of it (a
// real one carries the instructions and runs to tens of kilobytes).
func sessionMeta(started time.Time) string {
	return fmt.Sprintf(`{"timestamp":%q,"ordinal":0,"type":"session_meta","payload":{"instructions":%q}}`+"\n",
		started.Format(time.RFC3339Nano), strings.Repeat("x", 4096))
}

// TestCodexAtTakesTheSnapshotBeforeAtFromASessionSpanningIt: a session that
// started before at and was written after it holds the newest snapshot at
// or before at, newer than the last one of the file last written before at;
// its snapshots after at do not count.
func TestCodexAtTakesTheSnapshotBeforeAtFromASessionSpanningIt(t *testing.T) {
	home := t.TempDir()
	at := now.Add(-24 * time.Hour)
	session(t, home, "2026/10/02", "old", sessionMeta(at.Add(-6*time.Hour))+tokenCount(at.Add(-3*time.Hour), 20, "pro"), at.Add(-2*time.Hour))
	spanning := session(t, home, "2026/10/03", "spanning",
		sessionMeta(at.Add(-5*time.Hour))+tokenCount(at.Add(-time.Hour), 30, "pro")+tokenCount(at.Add(time.Hour), 40, "pro"), at.Add(time.Hour))

	snap, err := CodexAt(context.Background(), home, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 30 || !snap.At.Equal(at.Add(-time.Hour)) || snap.Path != spanning {
		t.Fatalf("snapshot = %+v; want the spanning session's 30%% an hour before at", snap)
	}
}

// TestCodexAtReadsOnlyTheFirstLineOfASessionStartedAfterAt: a session that
// began after at cannot hold a snapshot from before it; only its first line
// is read, so a token_count in it stamped before at (bogus here) is never
// seen.
func TestCodexAtReadsOnlyTheFirstLineOfASessionStartedAfterAt(t *testing.T) {
	home := t.TempDir()
	at := now.Add(-24 * time.Hour)
	session(t, home, "2026/10/02", "old", tokenCount(at.Add(-3*time.Hour), 20, "pro"), at.Add(-2*time.Hour))
	session(t, home, "2026/10/04", "later", sessionMeta(at.Add(time.Hour))+tokenCount(at.Add(-10*time.Minute), 99, "pro"), now)

	snap, err := CodexAt(context.Background(), home, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 20 || !strings.Contains(snap.Path, "old") {
		t.Fatalf("snapshot = %+v; want the old file's 20%%", snap)
	}
}

// TestCodexAtTakesTheLastSnapshotOfAFileWrittenBeforeAt: the file last
// written before at gives its last snapshot, its windows as Codex reported
// them even when they reset since (the caller compares the reset).
func TestCodexAtTakesTheLastSnapshotOfAFileWrittenBeforeAt(t *testing.T) {
	home := t.TempDir()
	session(t, home, "2025/09/20", "legacy", "testdata/legacy.jsonl", now.Add(-48*time.Hour))
	at := now.Add(-24 * time.Hour) // after both of the fixture's resets

	snap, err := CodexAt(context.Background(), home, at)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UsedPercent != 12 || snap.Secondary == nil || snap.Secondary.UsedPercent != 63 {
		t.Fatalf("snapshot = %+v (secondary %+v); want 12%% / 63%% as reported", snap, snap.Secondary)
	}
}

// TestCodexAtBoundsTheFilesItOpens: of the files written after at, only the
// maxHeads written closest to at have their first line read, and only
// maxFiles of the sessions spanning at are read for a snapshot.
func TestCodexAtBoundsTheFilesItOpens(t *testing.T) {
	home := t.TempDir()
	at := now.Add(-24 * time.Hour)
	started := sessionMeta(at.Add(-5 * time.Hour))
	session(t, home, "2026/10/03", "near", started+tokenCount(at.Add(-time.Hour), 30, "pro"), at.Add(time.Hour))
	session(t, home, "2026/10/03", "far", started+tokenCount(at.Add(-30*time.Minute), 35, "pro"), at.Add(2*time.Hour))

	for _, c := range []struct {
		heads, files int
		want         float64
	}{
		{heads: 1, files: MaxFiles, want: 30},
		{heads: 2, files: 1, want: 30},
		{heads: 2, files: 2, want: 35},
	} {
		snap, err := codexAt(context.Background(), home, at, c.heads, c.files, MaxTailBytes)
		if err != nil || snap.UsedPercent != c.want {
			t.Errorf("heads %d, files %d: %+v, %v; want %g%%", c.heads, c.files, snap, err, c.want)
		}
	}
}

func TestCodexAtNoData(t *testing.T) {
	at := now.Add(-24 * time.Hour)
	for name, setup := range map[string]func(home string){
		"no sessions directory": func(string) {},
		"only sessions started after at": func(h string) {
			session(t, h, "2026/10/04", "a", sessionMeta(at.Add(time.Minute))+tokenCount(at.Add(time.Hour), 5, "pro"), now)
		},
		"spanning session without a snapshot before at": func(h string) {
			session(t, h, "2026/10/03", "a", sessionMeta(at.Add(-time.Hour))+tokenCount(at.Add(time.Hour), 5, "pro"), now)
		},
		"no first-line timestamp": func(h string) {
			session(t, h, "2026/10/03", "a", `{"type":"session_meta"}`+"\n"+tokenCount(at.Add(-time.Hour), 5, "pro"), now)
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			setup(home)
			if snap, err := CodexAt(context.Background(), home, at); !errors.Is(err, ErrNoData) {
				t.Fatalf("CodexAt = %+v, %v; want ErrNoData", snap, err)
			}
		})
	}
}
