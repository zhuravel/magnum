package engine

// What a restart waits for besides review rounds (DECISIONS "A restart waits
// for a running notes curation and the retro"): a restart for a new build
// and the CLI's daemon-restart, install, uninstall and daemon-stop counted
// only rounds as in flight, so they cut a running notes curation (whose
// proposal is then never made) or the retro short. The curation running is
// KVNotesCurating; the retro running is KVRetroRunning, which the engine
// keeps from its own state (noteRetroRunning).

import (
	"context"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
)

// KVRetroRunning holds the start of the retro running now (store.FormatTime),
// which the CLI reads before it stops the daemon: set by the tick or the
// request that sees it started, deleted by the first one that sees it ended,
// at a restart for a new build, at shutdown and at every start of the daemon.
const KVRetroRunning = "learn.retro_running"

// backgroundBusy names the background work in flight that a restart would
// cut short ("" = none): the notes curation running and the retro.
func (e *Engine) backgroundBusy() string {
	var busy []string
	e.curateMu.Lock()
	if e.curateCancel != nil {
		busy = append(busy, "the notes curation of "+e.curateRepo)
	}
	e.curateMu.Unlock()
	if e.retroBusy() {
		busy = append(busy, "the retro")
	}
	return strings.Join(busy, " and ")
}

// noteRetroRunning keeps KVRetroRunning to the retro's state: its start
// while one runs, nothing otherwise.
func (e *Engine) noteRetroRunning(ctx context.Context) {
	e.retroMu.Lock()
	running, started := e.retroCancel != nil, e.retroStarted
	e.retroMu.Unlock()
	cur, set := e.getKV(ctx, KVRetroRunning)
	switch {
	case running && cur != store.FormatTime(started):
		e.setKV(ctx, KVRetroRunning, store.FormatTime(started))
	case !running && set:
		e.delKV(ctx, KVRetroRunning)
	}
}
