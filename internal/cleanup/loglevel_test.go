package cleanup

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// levelLog is an execx.AttrLogger that keeps each line with its level and
// attributes.
type levelLog struct {
	mu   sync.Mutex
	recs []string
}

func (l *levelLog) Printf(format string, args ...any) { l.Logf(slog.LevelInfo, format, args...) }

func (l *levelLog) Logf(level slog.Level, format string, args ...any) {
	l.LogAttrs(level, fmt.Sprintf(format, args...))
}

func (l *levelLog) LogAttrs(level slog.Level, msg string, attrs ...slog.Attr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := level.String() + " " + msg
	for _, a := range attrs {
		rec += " " + a.String()
	}
	l.recs = append(l.recs, rec)
}

// An action that fails reaches the log at Error with its subject and kind
// (cleanup's events never reached it); one that succeeds keeps its single
// "done" line at Info, and its info event stays in the registry only.
func TestAFailedCleanupActionLogsAtError(t *testing.T) {
	f := newFixture(t)
	logs := &levelLog{}
	f.p.Log = logs
	a := f.closedPR(f.talkable, 1, store.GHMerged, -2*time.Minute)
	f.poolSlot("review1", store.SlotHeld, a.ID, true, nil)
	b := f.closedPR(f.talkable, 2, store.GHMerged, -time.Minute)
	f.poolSlot("review2", store.SlotHeld, b.ID, true, nil)
	f.slots.errs["release review1"] = errBoom

	if _, err := f.apply(f.plan(Options{}), false); err == nil {
		t.Fatal("the failed release must fail the apply")
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	var failed, events []string
	for _, r := range logs.recs {
		if strings.Contains(r, " kind=") {
			events = append(events, r)
		}
		if strings.HasPrefix(r, "ERROR cleanup: slot:review1 cleanup.release: release ") && strings.Contains(r, "failed: ") &&
			strings.HasSuffix(r, " subject=slot:review1 kind=cleanup.release") {
			failed = append(failed, r)
		}
	}
	if len(failed) != 1 || len(events) != 1 {
		t.Fatalf("records = %q, want the failed release's only, at error", logs.recs)
	}
	if !slices.ContainsFunc(logs.recs, func(r string) bool {
		return strings.HasPrefix(r, "INFO cleanup: release ") && strings.HasSuffix(r, " done")
	}) {
		t.Fatalf("records = %q, want the done line of review2 at info", logs.recs)
	}
}
