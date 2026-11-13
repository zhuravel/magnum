package slots

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// stoppedAfterWorktreeAdd provisions pool slot 1 of h with a render that
// fails: the provisioning stops after its worktree_add step, which recorded
// that it made the worktree (noteTreeAdded), and before any setup ran.
func (h *harness) stoppedAfterWorktreeAdd(t *testing.T) store.Slot {
	t.Helper()
	trust := execx.Rule{Prefix: []string{"mise", "-C", h.pool.Path(1), "trust"}, Result: execx.Result{Code: 1, Stderr: []byte("boom\n")}}
	h.fake.Rules = append([]execx.Rule{trust}, h.fake.Rules...)
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); err == nil {
		t.Fatal("ProvisionPool with a render that fails: no error")
	}
	h.fake.Rules = h.fake.Rules[1:]
	sl := h.slot(h.pool.Slot(1))
	if _, ok, err := h.st.GetKV(h.ctx, treeAddedKey(sl.ID)); err != nil || !ok || len(h.scriptCalls("")) != 0 {
		t.Fatalf("record %v (%v), %d setup commands; want the record that the provisioning made the worktree, and no setup run",
			ok, err, len(h.scriptCalls("")))
	}
	return sl
}

// sql runs stmt on h's registry.
func (h *harness) sql(t *testing.T, stmt string) {
	t.Helper()
	if _, err := h.st.DB().ExecContext(h.ctx, stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// heldBackSetup checks that the provisioning of sl, whose record that it
// made the worktree could not be taken (the error err of ProvisionPool,
// whose words name the record as what), ran no setup and left the record
// standing, and that logs has a line naming it; then that the retry, once
// the registry answers, is the first setup over the worktree: it takes the
// record, and a removal saves a person's file and nothing the setup wrote.
func (h *harness) heldBackSetup(t *testing.T, sl store.Slot, err error, logs *levelLog, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), what) {
		t.Fatalf("ProvisionPool = %v, want the setup step failed on %q", err, what)
	}
	if n := len(h.scriptCalls("touch .env.setup")); n != 0 {
		t.Fatalf("the setup ran %d times over the record it could not take, want none", n)
	}
	if got := h.slot(sl.Name); got.State != store.SlotProvisioning {
		t.Fatalf("state = %s, want provisioning, to resume", got.State)
	}
	if _, ok, err := h.st.GetKV(h.ctx, treeAddedKey(sl.ID)); err != nil || !ok {
		t.Fatalf("record %v (%v), want it standing for the setup that runs first", ok, err)
	}
	if !slices.ContainsFunc(logs.lines(), func(l string) bool { return strings.Contains(l, what) && strings.Contains(l, sl.Name) }) {
		t.Errorf("log %q, want a line on %q of %s", logs.lines(), what, sl.Name)
	}

	sl = h.provisioned(1)
	if n := len(h.scriptCalls("touch .env.setup")); n != 1 {
		t.Fatalf("the setup ran %d times, want once", n)
	}
	if _, ok, err := h.st.GetKV(h.ctx, treeAddedKey(sl.ID)); err != nil || ok {
		t.Fatalf("record %v (%v), want it taken by the first setup", ok, err)
	}
	writeFile(t, filepath.Join(sl.Path, "notes.txt"), "mine\n")
	if err := h.m.Remove(h.ctx, sl, h.pool, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	save := h.onlySave(t, h.layout.SlotSaves(sl.Name), sl.Name)
	if got := savedUntracked(t, save); !maps.Equal(got, map[string]string{"notes.txt": "mine\n"}) {
		t.Fatalf("the save's untracked files = %q; want the person's notes.txt alone", got)
	}
}

// lines are the lines l kept.
func (l *levelLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs)
}

// A setup that cannot read the record that its provisioning made the
// worktree does not run: the setup step fails before its commands, the
// log names the read, and the record stands. Run, it left the record
// unread: a later run of the setup (the retry of one that failed), over a
// worktree a person may be at work in by then, took it as the first and
// recorded the person's file written while it ran as magnum's, which a
// removal deletes unsaved. The read's failure was not logged either. The
// retry, once the registry reads again, is the first setup over the
// worktree.
func TestASetupThatCannotReadTheRecordOfItsNewWorktreeDoesNotRun(t *testing.T) {
	h := keyedPool(t)
	logs := &levelLog{}
	h.m = h.newManager(Deps{Log: logs})
	sl := h.stoppedAfterWorktreeAdd(t)
	h.sql(t, "ALTER TABLE kv RENAME TO kv_away") // every read of the kv table fails
	err := h.m.ProvisionPool(h.ctx, h.pool, 1)
	h.sql(t, "ALTER TABLE kv_away RENAME TO kv")
	h.heldBackSetup(t, sl, err, logs, "read the record")
}

// A setup that cannot delete the record that its provisioning made the
// worktree (a store error on that one write) does not run either: the
// record would stand past it, and the setup's next run, over a worktree a
// person may be at work in, would take it as the first.
func TestASetupThatCannotDeleteTheRecordOfItsNewWorktreeDoesNotRun(t *testing.T) {
	h := keyedPool(t)
	logs := &levelLog{}
	h.m = h.newManager(Deps{Log: logs})
	sl := h.stoppedAfterWorktreeAdd(t)
	h.sql(t, `CREATE TRIGGER keep_tree_added BEFORE DELETE ON kv WHEN OLD.key LIKE 'slot.%.tree_added'
  BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	err := h.m.ProvisionPool(h.ctx, h.pool, 1)
	h.sql(t, "DROP TRIGGER keep_tree_added")
	h.heldBackSetup(t, sl, err, logs, "take the record")
}

// perPRTakeFails creates f's per-PR worktree, whose [[repo]] copies the
// main clone's master.key and whose setup writes .env.setup, with fail run
// as its render trusts the local mise file (after the worktree_add step
// recorded that it made the worktree, before the setup step) and mend as
// the setup's command runs, which also writes notes/repro.rb there first:
// a person at work in the worktree meanwhile.
func perPRTakeFails(t *testing.T, f *perPRFixture, logs *levelLog, fail, mend string) (store.Slot, error) {
	t.Helper()
	h := f.h
	writeFile(t, filepath.Join(f.main, "master.key"), "main clone secret\n")
	writeFile(t, filepath.Join(f.main, MiseLocal), "[env]\nKEEP = \"k\"\n")
	h.m = h.newManager(Deps{Log: logs, Repos: []config.Repo{{Repo: f.repoName(),
		CopyFiles: []string{"master.key"}, Setup: []string{"touch .env.setup"}}}})
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"mise", "-C", PRWorktreePath(f.main, f.pr.Number), "trust"},
		Fn: func(execx.Cmd) (execx.Result, error) {
			h.sql(t, fail)
			return execx.Result{}, nil
		}}}, h.fake.Rules...)
	h.onScript = func(script, dir string) {
		if script == "touch .env.setup" {
			h.sql(t, mend)
			writeFile(t, filepath.Join(dir, "notes", "repro.rb"), "written while the setup ran\n")
		}
	}
	return h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
}

// A per-PR worktree whose setup cannot read or delete the record that its
// provisioning made the worktree (SQLITE_BUSY past the busy timeout, an
// I/O error) logs that and goes on as over a kept worktree: the setup
// runs, records the copy_files alone, and the row is claimed, so a removal
// saves what the setup wrote and the person's file written while it ran,
// never the copied key. The setup step failed instead, which broke the
// row, charged the round and could dismiss a kept approval, though the
// row's revival found the worktree listed, dropped the record and ran the
// same setup as over a kept worktree. A pool slot's setup still fails:
// TestASetupThatCannotReadTheRecordOfItsNewWorktreeDoesNotRun.
func TestAPerPRSetupThatCannotTakeTheRecordOfItsNewWorktreeRunsAsOverAKeptOne(t *testing.T) {
	for _, c := range []struct{ name, what, fail, mend string }{
		{"the read", "read the record",
			"ALTER TABLE kv RENAME TO kv_away", "ALTER TABLE kv_away RENAME TO kv"},
		{"the delete", "take the record",
			`CREATE TRIGGER keep_tree_added BEFORE DELETE ON kv WHEN OLD.key LIKE 'slot.%.tree_added'
  BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`, "DROP TRIGGER keep_tree_added"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			logs := &levelLog{}
			sl, err := perPRTakeFails(t, f, logs, c.fail, c.mend)
			if err != nil {
				t.Fatalf("CreatePRWorktree = %v, want the setup run as over a kept worktree", err)
			}
			if sl.State != store.SlotClaimed {
				t.Fatalf("state = %s, want claimed", sl.State)
			}
			if n := len(h.scriptCalls("touch .env.setup")); n != 1 {
				t.Fatalf("the setup ran %d times, want once", n)
			}
			if !slices.ContainsFunc(logs.lines(), func(l string) bool { return strings.Contains(l, c.what) && strings.Contains(l, sl.Name) }) {
				t.Errorf("log %q, want a line on %q of %s", logs.lines(), c.what, sl.Name)
			}
			if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
				t.Fatalf("RemovePRWorktree: %v", err)
			}
			save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
			want := map[string]string{"notes/repro.rb": "written while the setup ran\n", ".env.setup": "written by setup\n"}
			if got := savedUntracked(t, save); !maps.Equal(got, want) {
				t.Fatalf("the save's untracked files = %q; want %q, the copied key alone recorded as magnum's", got, want)
			}
		})
	}
}
