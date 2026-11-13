package factory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The holds a daemon's pidfile names are its own from the start: a daemon
// that starts writes holds.json before its pidfile, then again once it took
// the controls that stay (a pause), so no reader ever pairs its pid with
// the holds of the daemon before it. Only Tick's deferred save wrote the
// file, so after every `magnum factory install --drain` and every reboot,
// the old daemon's drain was the wait of every active task with none of its
// own (StoppingHold) until the new daemon's first tick ended, which a long
// first stage holds up.
func TestADaemonThatStartsShowsNoneOfTheOldDaemonsHolds(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12541", registry.TaskActive, "investigate")
	old := []Hold{{Kind: HoldDrain, Reason: "draining for a restart since Nov 13 16:40 (pid 4242, `magnum factory install --drain`)",
		Since: h.clock.Now().Add(-time.Minute)}}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.layout.FactoryHolds()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.layout.FactoryHolds(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	pause := Pause{Generation: 1, Since: h.clock.Now().Add(-time.Hour), Reason: "lunch"}
	if err := WritePause(h.layout, pause.Value()); err != nil {
		t.Fatal(err)
	}
	first, release := make(chan struct{}), make(chan struct{})
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == task.ID {
			select {
			case first <- struct{}{}:
				<-release // the first tick's stage runs on: its deferred save waits
			default:
			}
		}
		return false, nil
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never ran the task's stage")
	}
	pid, _ := os.ReadFile(h.layout.FactoryPid())
	hs, err := ReadHolds(h.layout)
	close(release)
	if strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) || err != nil {
		t.Fatalf("pidfile %q, holds %v (%v); want this daemon's", pid, hs, err)
	}
	if got, ok := StoppingHold(hs, h.reload(task.ID)); ok && got.Kind == HoldDrain {
		t.Errorf("the task waits on %s's %q, the old daemon's drain, before the new daemon's first tick ends", got.Kind, got.Reason)
	}
	kinds := make([]string, 0, len(hs))
	for _, h := range hs {
		kinds = append(kinds, h.Kind)
	}
	if strings.Join(kinds, ",") != HoldPause {
		t.Errorf("holds.json holds %q, want the pause that stays alone", kinds)
	}
	(<-sigs) <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
}
