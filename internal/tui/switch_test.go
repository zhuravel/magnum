package tui

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// runWithKeys runs a screen on a pipe that sends input once the screen
// loaded, and returns what the screen returned.
func runWithKeys(t *testing.T, input string, loaded func() bool, run func(ctx context.Context) error) error {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close(); r.Close() })
	old := extraProgramOptions
	extraProgramOptions = []tea.ProgramOption{tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	t.Cleanup(func() { extraProgramOptions = old })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	for !loaded() {
		select {
		case <-ctx.Done():
			t.Fatal("the screen did not load")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("the screen did not return")
		return nil
	}
}

func TestDashboardTabSwitchesToTheBoard(t *testing.T) {
	for _, k := range []string{"tab", "t"} {
		m, _, _ := newDash(t, 160, 30)
		m, cmd := send(t, m, keyMsg(k))
		if !m.switching || !isQuit(execCmd(cmd)) {
			t.Errorf("%s: switching %v, want a quit to switch", k, m.switching)
		}
	}
	m, _, _ := newDash(t, 160, 30)
	if m, _ = send(t, m, keyMsg("q")); m.switching {
		t.Error("q asked to switch")
	}
	mustContain(t, viewOf(m), "tab PRs")
	m, _ = send(t, m, keyMsg("?"), keyMsg("G")) // the end of the help
	mustContain(t, viewOf(m), "switch to the PR board")

	src := &fakeSource{data: dashData()}
	err := runWithKeys(t, "\t", func() bool { return src.count() > 0 }, func(ctx context.Context) error {
		return RunDashboard(ctx, src, nil, DashboardOptions{})
	})
	if !errors.Is(err, ErrSwitchToBoard) {
		t.Fatalf("RunDashboard after tab = %v, want ErrSwitchToBoard", err)
	}
	err = runWithKeys(t, "q", func() bool { return src.count() > 1 }, func(ctx context.Context) error {
		return RunDashboard(ctx, src, nil, DashboardOptions{})
	})
	if err != nil {
		t.Fatalf("RunDashboard after q = %v, want nil", err)
	}
}

func TestPRBoardTabSwitchesToTheDashboard(t *testing.T) {
	m, _, _ := newBoard(t, 200, 40, PRBoardOptions{})
	mustContain(t, viewOf(m), "tab overview")
	m, cmd := send(t, m, keyMsg("tab"))
	if !m.switching || !isQuit(execCmd(cmd)) {
		t.Fatal("tab on the table did not quit to switch")
	}

	m, _, _ = newBoard(t, 200, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("enter"))
	if m.mode != prbDetail {
		t.Fatal("enter did not open the card")
	}
	if m, _ = send(t, m, keyMsg("tab")); !m.switching {
		t.Error("tab on the card did not switch")
	}

	m, _, _ = newBoard(t, 200, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("/"))
	if m, _ = send(t, m, keyMsg("tab")); m.switching || !m.filtering {
		t.Error("tab while typing a filter switched screens")
	}
	m, _, _ = newBoard(t, 120, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "status dashboard")

	src := &fakeBoardSource{rows: boardRows()}
	err := runWithKeys(t, "\t", func() bool { return src.count() > 0 }, func(ctx context.Context) error {
		return RunPRBoard(ctx, src, nil, PRBoardOptions{})
	})
	if !errors.Is(err, ErrSwitchToDashboard) {
		t.Fatalf("RunPRBoard after tab = %v, want ErrSwitchToDashboard", err)
	}
	err = runWithKeys(t, "q", func() bool { return src.count() > 1 }, func(ctx context.Context) error {
		return RunPRBoard(ctx, src, nil, PRBoardOptions{})
	})
	if err != nil {
		t.Fatalf("RunPRBoard after q = %v, want nil", err)
	}
}
