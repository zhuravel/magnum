package factory

import (
	"slices"
	"strconv"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A copy's server runs the repository's server command in the copy with the
// copy's env, once however often it is started, and its pid and start are
// recorded; a stop ends its process group and forgets it.
func TestStartServerRecordsThePidAndTheStartOfTheCopysServer(t *testing.T) {
	r := newCopiesRig(t, 1)
	c := r.claim(r.task("PR-1"))
	for range 2 {
		if err := r.c.StartServer(r.ctx, c); err != nil {
			t.Fatalf("StartServer: %v", err)
		}
	}
	if len(r.spawned) != 1 {
		t.Fatalf("spawned %+v, want one server however often it was started", r.spawned)
	}
	s := r.spawned[0]
	if s.Dir != c.Path || s.Command != "bin/worktree-run" || s.Env["REDIS_DB_OFFSET"] != "16" || s.Env["WT_BRANCH"] != "factory1" {
		t.Fatalf("spawned %+v, want bin/worktree-run in %s with the copy's env", s, c.Path)
	}
	rec, ok, err := r.c.serverOf(r.ctx, c.Name)
	if err != nil || !ok || rec.PID != s.PID || rec.Start != lstart(r.clock.Now()) {
		t.Fatalf("record = %+v, %v, %v; want pid %d started %s", rec, ok, err, s.PID, lstart(r.clock.Now()))
	}
	if err := r.c.StopServer(r.ctx, c); err != nil {
		t.Fatalf("StopServer: %v", err)
	}
	if want := []string{strconv.Itoa(s.PID) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want %v", r.kills, want)
	}
	if _, ok, _ := r.c.serverOf(r.ctx, c.Name); ok {
		t.Fatal("a stopped server is still recorded")
	}
}

// After a restart every recorded server is a stray (no stage owns it):
// Reconcile kills one whose pid and start both match the record, and leaves
// alone a process that took a dead server's pid, with another start. Both
// records are forgotten.
func TestReconcileKillsAStrayServerOnlyWhenPidAndStartTimeMatch(t *testing.T) {
	r := newCopiesRig(t, 2)
	one, two := r.claim(r.task("PR-1")), r.claim(r.task("PR-2"))
	for _, c := range []Copy{one, two} {
		if err := r.c.StartServer(r.ctx, c); err != nil {
			t.Fatalf("StartServer %s: %v", c.Name, err)
		}
	}
	stray, dead := r.spawned[0].PID, r.spawned[1].PID
	r.mu.Lock()
	for i, p := range r.table {
		if p.PID == dead { // the server died; another process got its pid later
			r.table[i] = execx.Process{PID: dead, PPID: 1, PGID: dead, Start: "Thu Nov  5 10:00:00 2026", Name: "postgres"}
		}
	}
	r.mu.Unlock()

	r.c = r.newCopies() // a restart: nothing in memory
	if err := r.c.Reconcile(r.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := []string{strconv.Itoa(stray) + " terminated"}; !slices.Equal(r.kills, want) {
		t.Fatalf("kills %v, want only the stray %d (pid %d is another process now)", r.kills, stray, dead)
	}
	for _, c := range []Copy{one, two} {
		if _, ok, _ := r.c.serverOf(r.ctx, c.Name); ok {
			t.Errorf("%s's server is still recorded after the reconcile", c.Name)
		}
	}
	if err := r.c.StopServer(r.ctx, two); err != nil || len(r.kills) != 1 {
		t.Fatalf("StopServer of a forgotten server = %v, kills %v; want nothing killed", err, r.kills)
	}
}
