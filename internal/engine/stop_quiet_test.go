package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

// quietHarness is a harness whose engine logs at Debug into the returned
// buffer.
func quietHarness(t *testing.T, mods ...func(*harness)) (*harness, *syncBuffer) {
	t.Helper()
	var buf syncBuffer
	logTo := func(h *harness) {
		h.d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return newHarness(t, append([]func(*harness){logTo}, mods...)...), &buf
}

// warnLines are the log lines at Warn or Error.
func warnLines(buf *syncBuffer) []string {
	var out []string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
			out = append(out, line)
		}
	}
	return out
}

// debugLines are the log lines at Debug that contain msg.
func debugLines(buf *syncBuffer, msg string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "level=DEBUG") && strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

func stoppedContext(h *harness) context.Context {
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	return ctx
}

// A daemon stop cancels the tick's context, and every registry read, write
// and event append the tick then made logged a warning ("kv read … context
// canceled": 640 lines, 88 more for events, 82 for writes since 10-05, half
// of all warnings). They are debug lines; a real failure still warns.
func TestAStoppingDaemonLogsItsRegistryAccessAtDebugNotWarn(t *testing.T) {
	h, buf := quietHarness(t)
	stopped := stoppedContext(h)

	if v, ok := h.e.getKV(stopped, "daemon.paused"); v != "" || ok {
		t.Fatalf("getKV on a cancelled context = %q, %v", v, ok)
	}
	h.e.setKV(stopped, "k", "v")
	h.e.delKV(stopped, "k", "k2")
	h.e.event(stopped, "info", "", "test.kind", "something happened", nil)

	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("a cancelled context logged warnings:\n%s", strings.Join(w, "\n"))
	}
	for _, msg := range []string{"kv read", "kv write", "kv delete", "append event"} {
		if debugLines(buf, msg) == 0 {
			t.Errorf("no debug line for %q:\n%s", msg, buf.String())
		}
	}
}

func TestARealRegistryFailureStillWarns(t *testing.T) {
	h, buf := quietHarness(t)
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.e.getKV(h.ctx, "k")
	h.e.setKV(h.ctx, "k", "v")
	h.e.delKV(h.ctx, "k")
	h.e.event(h.ctx, "info", "", "test.kind", "something happened", nil)
	if w := warnLines(buf); len(w) != 4 {
		t.Fatalf("a closed registry logged %d warnings, want 4:\n%s", len(w), buf.String())
	}
}

// A herdr snapshot that failed because the daemon is stopping says nothing
// about herdr: no "herdr unreachable" warning event, no herdr.down, no
// herdr_up=0 in the registry (23 such events since 10-05), and the next live
// tick still notes the transition when herdr really is down.
func TestAStoppingDaemonDoesNotNoteHerdrDown(t *testing.T) {
	h, buf := quietHarness(t)
	downs := func() int {
		evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "herdr.down")
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}

	h.e.noteHerdr(stoppedContext(h), fmt.Errorf("herdr snapshot: %w", context.Canceled))
	h.e.noteHerdr(h.ctx, fmt.Errorf("dial herdr: %w", context.Canceled)) // the error alone says it
	if n := downs(); n != 0 {
		t.Fatalf("%d herdr.down events for a cancelled context", n)
	}
	if v, ok, _ := h.st.GetKV(h.ctx, kvHerdrUp); ok {
		t.Fatalf("herdr_up = %q after a cancelled snapshot", v)
	}
	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("warnings for a cancelled snapshot:\n%s", strings.Join(w, "\n"))
	}

	h.e.noteHerdr(h.ctx, errors.New("dial unix herdr.sock: connection refused"))
	if n := downs(); n != 1 {
		t.Fatalf("herdr really down: %d herdr.down events, want 1", n)
	}
	if v, _, _ := h.st.GetKV(h.ctx, kvHerdrUp); v != "0" {
		t.Fatalf("herdr_up = %q, want 0", v)
	}
}

// The day's retro was "not run today" for a daemon whose registry reads all
// failed at shutdown (a cancelled getKV answers ""), and it logged a warning
// for each read. A cancelled context starts no retro, reads nothing it
// would warn about and records nothing; the next live tick runs the retro.
func TestAStoppingDaemonStartsNoDailyRetroAndLogsNoWarning(t *testing.T) {
	h, buf := quietHarness(t, withClassifier(&fakeClassifier{}), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "09:00" // the clock is past it
	})
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)

	h.e.maybeRetro(stoppedContext(h))
	h.settle()
	if h.e.retroBusy() {
		t.Fatal("a retro is running")
	}
	if evs := retroEvents(t, h); len(evs) != 0 {
		t.Fatalf("retro events on a cancelled context: %v", evs)
	}
	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("warnings on a cancelled context:\n%s", strings.Join(w, "\n"))
	}

	h.e.maybeRetro(h.ctx)
	h.settle()
	if evs := retroEvents(t, h); len(evs) != 2 {
		t.Fatalf("the retro on the next live tick: %v", evs)
	}
}

// The tick's periodic reads (wait reasons, the requests, the PRs past their
// close grace and those releasing) failed with the cancelled context and
// each logged a warning at every stop.
func TestAStoppingDaemonLogsNoWarningFromItsPeriodicReads(t *testing.T) {
	h, buf := quietHarness(t)
	stopped := stoppedContext(h)

	h.e.noteWaits(stopped, tickState{})
	h.e.handleRequests(stopped)
	h.e.complete(stopped, 1, nil, "done")
	h.e.closeGrace(stopped)
	if !h.e.closedRoundsRunning(stopped) {
		t.Error("a read that failed must not let a cleanup go ahead")
	}
	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("a cancelled context logged warnings:\n%s", strings.Join(w, "\n"))
	}
}

// The reconcile found the same two orphan databases every 17 minutes and
// logged them every time (164 lines in a day). A finding is logged once per
// day; a new one at once; one that went away and came back is new again. The
// scan itself still reports every finding to status and doctor.
func TestDriftIsLoggedOncePerFindingPerDay(t *testing.T) {
	h, buf := quietHarness(t)
	finding := func(slug string) inventory.Finding {
		return inventory.Finding{Kind: inventory.KindOrphanDB, Subject: "slug:" + slug,
			Message: "1 database(s) belong to no slot or worktree", Safe: false}
	}
	logged := func() int { return strings.Count(buf.String(), "msg=drift ") }
	reconcile := func(slugs ...string) {
		var inv inventory.Inventory
		for _, s := range slugs {
			inv.Drift = append(inv.Drift, finding(s))
		}
		h.e.applyInventory(h.ctx, inv)
	}
	step := func(name string, want int) {
		t.Helper()
		if n := logged(); n != want {
			t.Fatalf("%s: %d drift lines, want %d:\n%s", name, n, want, buf.String())
		}
	}

	reconcile("old1", "old2")
	step("first reconcile", 2)
	reconcile("old1", "old2")
	step("the same findings again", 2)
	h.advance(3 * time.Hour)
	reconcile("old1", "old2")
	step("the same day, hours later", 2)
	reconcile("old1", "old2", "old3")
	step("a new finding", 3)
	reconcile("old1")
	reconcile("old1", "old2")
	step("old2 went away and came back", 4)
	h.advance(24 * time.Hour)
	reconcile("old1", "old2")
	step("the next day", 6)
	reconcile("old1", "old2")
	step("the next day, again", 6)
	if day, _, _ := h.st.GetKV(h.ctx, kvDriftLogged); !strings.Contains(day, store.DayKey(h.clock.Now())) {
		t.Errorf("the registry does not hold today (%s): %q", store.DayKey(h.clock.Now()), day)
	}
}

// A drift line is not worth a retry storm at shutdown either: a cancelled
// context logs nothing and does not mark the findings as logged.
func TestAStoppingDaemonDoesNotMarkDriftAsLogged(t *testing.T) {
	h, buf := quietHarness(t)
	inv := inventory.Inventory{Drift: []inventory.Finding{{Kind: inventory.KindOrphanDB, Subject: "slug:old1", Message: "orphan"}}}
	h.e.applyInventory(stoppedContext(h), inv)
	if n := strings.Count(buf.String(), "msg=drift "); n != 0 {
		t.Fatalf("a cancelled context logged %d drift lines", n)
	}
	h.e.applyInventory(h.ctx, inv)
	if n := strings.Count(buf.String(), "msg=drift "); n != 1 {
		t.Fatalf("the next live reconcile logged %d drift lines, want 1", n)
	}
}
