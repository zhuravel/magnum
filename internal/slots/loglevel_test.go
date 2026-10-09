package slots

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
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

// A slot's warn event reaches the log at Warn with its subject and kind:
// slot events never reached it.
func TestASlotsWarnEventLogsAtWarnWithItsSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	logs := &levelLog{}
	m := New(Deps{Store: st, Run: &execx.Fake{}, Layout: paths.Layout{Home: t.TempDir()}, Log: logs,
		LookPath: func(file string) (string, error) { return "/fake/bin/" + file, nil }})
	sl, err := st.CreateSlot(ctx, store.Slot{Name: "talkable-worktree", RepoFullName: "talkable/talkable",
		Kind: store.SlotKindPerPR, Path: t.TempDir(), State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	// The PR number is unknown, so the teardown is skipped; git has no rules,
	// so the removal fails after it.
	if err := m.RemovePRWorktree(ctx, sl, true); err == nil {
		t.Fatal("the removal must fail on the fake git")
	}
	want := "WARN slots: slot:talkable-worktree slot.hook_failed: teardown skipped: the PR number of talkable-worktree is unknown " +
		"subject=slot:talkable-worktree kind=slot.hook_failed"
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if !slices.Contains(logs.recs, want) {
		t.Fatalf("no record %q in %q", want, logs.recs)
	}
}
