package settings

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// A save asks the daemon to restart on it (engine.KVConfigRestart, which
// the daemon's idle restart reads), and the screen says "restart pending"
// exactly when the file on disk is not the one the daemon recorded at
// start.
func TestRequestRestartMarksTheSaveAndRestartPendingComparesContent(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	st := storetest.Open(t, filepath.Join(t.TempDir(), "magnum.db"))
	ctx := t.Context()
	pending := func() bool {
		t.Helper()
		got, err := RestartPending(ctx, st, s)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if pending() {
		t.Error("pending with no daemon's record")
	}
	if err := st.SetKV(ctx, KVDaemonConfigSHA, hex.EncodeToString(s.Sum[:])); err != nil {
		t.Fatal(err)
	}
	if pending() {
		t.Error("pending while the daemon runs the file on disk")
	}
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Commit(p)
	if err != nil {
		t.Fatal(err)
	}
	if !pending() {
		t.Error("not pending after a save the daemon does not run")
	}
	if err := RequestRestart(ctx, st, res.Sum); err != nil {
		t.Fatal(err)
	}
	m, ok := engine.ReadConfigRestart(ctx, st)
	if !ok || m.SHA256 != hex.EncodeToString(res.Sum[:]) || m.SavedAt.IsZero() {
		t.Errorf("mark = %+v, %v", m, ok)
	}
	// The daemon read no user config, and there is none: nothing pending.
	if err := os.Remove(s.Path); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, KVDaemonConfigSHA, ""); err != nil {
		t.Fatal(err)
	}
	if pending() {
		t.Error("pending with neither a file nor a recorded one")
	}
}
