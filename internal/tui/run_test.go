package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTracedScreensReturnTheirModels: with MAGNUM_TUI_TRACE set the
// program runs a *traceModel; every screen must still read its own final
// model (the switch request, the cleanup outcome, the pick, the watch's
// stop error) through runProgram's unwrap.
func TestTracedScreensReturnTheirModels(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace.log")
	t.Setenv("MAGNUM_TUI_TRACE", trace)
	ready := func() bool { return true }

	src := &fakeSource{data: dashData()}
	err := runWithKeys(t, "\t", func() bool { return src.count() > 0 }, func(ctx context.Context) error {
		return RunDashboard(ctx, src, nil, DashboardOptions{})
	})
	if !errors.Is(err, ErrSwitchToBoard) {
		t.Errorf("dashboard: %v, want ErrSwitchToBoard", err)
	}

	bsrc := &fakeBoardSource{rows: boardRows()}
	err = runWithKeys(t, "\t", func() bool { return bsrc.count() > 0 }, func(ctx context.Context) error {
		return RunPRBoard(ctx, bsrc, nil, PRBoardOptions{})
	})
	if !errors.Is(err, ErrSwitchToDashboard) {
		t.Errorf("board: %v, want ErrSwitchToDashboard", err)
	}

	var plan CleanupOutcome
	err = runWithKeys(t, "\ry", ready, func(ctx context.Context) (err error) {
		plan, err = RunCleanupPlan(ctx, cleanupFixture())
		return err
	})
	if err != nil || !plan.Apply {
		t.Errorf("cleanup: %+v, %v; want the plan applied", plan, err)
	}

	var pick PickOutcome
	err = runWithKeys(t, "talkable#7\ry", ready, func(ctx context.Context) (err error) {
		pick, err = RunPicker(ctx, nil, PickerOptions{})
		return err
	})
	if err != nil || pick.Action != PickActionReview || pick.Query != "talkable#7" {
		t.Errorf("picker: %+v, %v; want a review of talkable#7", pick, err)
	}

	gone := errors.New("the pane closed")
	err = runWithKeys(t, "", ready, func(ctx context.Context) error {
		return RunWatch(ctx, func(context.Context) (WatchFrame, error) { return WatchFrame{}, StopWatch(gone) }, WatchOptions{})
	})
	if !errors.Is(err, gone) {
		t.Errorf("watch: %v, want the StopWatch error", err)
	}

	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "trace start"); n != 5 {
		t.Errorf("trace has %d starts, want 5:\n%s", n, b)
	}
}
