package slots

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// saves lists the saves in folder, by their folders' names ("" for none).
func saves(t *testing.T, folder string) []string {
	t.Helper()
	ents, err := os.ReadDir(folder)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, filepath.Join(folder, e.Name()))
		}
	}
	return out
}

// onlySave is the one save in folder, which the slot's one slot.discarded
// event names.
func (h *harness) onlySave(t *testing.T, folder, slot string) string {
	t.Helper()
	got := saves(t, folder)
	if len(got) != 1 {
		t.Fatalf("saves in %s = %q, want one", folder, got)
	}
	evs := h.discarded(t, slot)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, got[0]) {
		t.Fatalf("slot.discarded events = %+v, want one that names the save %s", evs, got[0])
	}
	return got[0]
}

// applied puts the patch of a save back on a worktree of the main clone at
// head and returns that worktree.
func (h *harness) applied(t *testing.T, save, head string) string {
	t.Helper()
	wt := filepath.Join(h.root, "applied")
	gitT(t, h.main, "worktree", "add", "--quiet", "--detach", wt, head)
	gitT(t, wt, "apply", filepath.Join(save, "changes.diff"))
	return wt
}

// A switch with --discard-changes saves what the slot's tree holds first,
// in the folder of the PR the slot holds: the binary patch of the tracked
// changes against HEAD, which `git apply` puts back on that HEAD, binary
// files included; the slot.discarded event names the save, and saved.txt
// says what was saved, from where and how to put it back, and names the
// untracked files git does not ignore, which the switch keeps. Ignored
// files are not named. Only the event's list of up to 20 paths was left.
func TestASwitchSavesItsTrackedChangesBeforeItDiscardsThem(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	binary := []byte{0, 1, 2, 3, 0xff, 'b', 'i', 'n', 0}
	if err := os.WriteFile(filepath.Join(sl.Path, "README"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	strayEdit(t, sl.Path)
	gitT(t, sl.Path, "rm", "--quiet", "pnpm-lock.yaml")
	writeFile(t, filepath.Join(sl.Path, "notes", "mine.txt"), "my notes\n")
	writeFile(t, filepath.Join(sl.Path, "tmp", "cache.log"), "ignored\n")

	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaPR7 {
		t.Fatalf("HEAD = %s, want the switch to %s", head, h.shaPR7)
	}
	save := h.onlySave(t, h.layout.PRSaves("talkable", "talkable", pr.Number), sl.Name)

	wt := h.applied(t, save, h.shaPR8)
	if got, err := os.ReadFile(filepath.Join(wt, "README")); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("README put back = %q, %v, want the binary change", got, err)
	}
	if got := readFile(t, filepath.Join(wt, "Gemfile.lock")); got != "GEM stray\n" {
		t.Fatalf("Gemfile.lock put back = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(wt, "pnpm-lock.yaml")); !os.IsNotExist(err) {
		t.Fatalf("pnpm-lock.yaml put back: %v, want it deleted", err)
	}
	if got := readFile(t, filepath.Join(sl.Path, "notes", "mine.txt")); got != "my notes\n" {
		t.Fatalf("the untracked file the switch keeps = %q", got)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	for _, want := range []string{sl.Path, h.shaPR8, "3 tracked", "git -C", "apply", "kept in the tree", "notes/"} {
		if !strings.Contains(note, want) {
			t.Fatalf("saved.txt does not say %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "tmp/") || strings.Contains(note, ".mise.local.toml") {
		t.Fatalf("saved.txt names an ignored file:\n%s", note)
	}
	if fi, err := os.Stat(save); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the save's folder: %v, %v, want 0700", fi, err)
	}

	// A clean tree saves nothing.
	if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR8); err != nil {
		t.Fatalf("Resync of a clean tree: %v", err)
	}
	if got := saves(t, h.layout.PRSaves("talkable", "talkable", pr.Number)); len(got) != 1 {
		t.Fatalf("saves after a clean switch = %q, want still one", got)
	}
}

// The abort of an operation left in progress and a release's reset save
// the tracked changes they drop first, each in its own save.
func TestAnAbortAndAReleaseSaveWhatTheyDrop(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotClaimed}, store.SlotHeld, nil); err != nil {
		t.Fatal(err)
	}
	midMerge(t, sl.Path, h.shaMaster, h.shaPR8)
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got := saves(t, h.layout.PRSaves("talkable", "talkable", pr.Number))
	if len(got) != 1 {
		t.Fatalf("saves = %q, want the abort's", got)
	}
	if patch := readFile(t, filepath.Join(got[0], "changes.diff")); !strings.Contains(patch, "README") {
		t.Fatalf("the abort's save does not hold the merge's README:\n%s", patch)
	}
	if note := readFile(t, filepath.Join(got[0], "saved.txt")); !strings.Contains(note, "merge") {
		t.Fatalf("saved.txt does not name the merge's abort:\n%s", note)
	}
}

// The save keeps at most its cap (50 MB; 1500 bytes here) and names what
// it left out in the event and in saved.txt: a patch past the cap goes
// path by path, each path's patch that still fits.
func TestThePatchPastTheCapGoesPathByPath(t *testing.T) {
	h := newHarness(t)
	h.m.saveCap = 1500
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	writeFile(t, filepath.Join(sl.Path, "Gemfile.lock"), "GEM small\n")
	writeFile(t, filepath.Join(sl.Path, "db", "schema.rb"), strings.Repeat("big schema line\n", 200))

	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("talkable", "talkable", pr.Number), sl.Name)
	patch := readFile(t, filepath.Join(save, "changes.diff"))
	if !strings.Contains(patch, "GEM small") || strings.Contains(patch, "big schema line") {
		t.Fatalf("the patch past the cap kept:\n%s\nwant Gemfile.lock's alone", patch)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	if ev := h.discarded(t, sl.Name)[0].Message; !strings.Contains(note, "db/schema.rb (") || !strings.Contains(ev, "db/schema.rb") {
		t.Fatalf("saved.txt or the event does not name db/schema.rb as left out:\n%s\n%s", note, ev)
	}

	// No path's patch fits: no patch, and every path is named.
	h.m.saveCap = 10
	h.now = h.now.Add(time.Minute)
	strayEdit(t, sl.Path)
	if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR8); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	var second string
	for _, s := range saves(t, h.layout.PRSaves("talkable", "talkable", pr.Number)) {
		if s != save {
			second = s
		}
	}
	if _, err := os.Lstat(filepath.Join(second, "changes.diff")); !os.IsNotExist(err) {
		t.Fatalf("a save that kept no path's patch has changes.diff: %v", err)
	}
	if note := readFile(t, filepath.Join(second, "saved.txt")); !strings.Contains(note, "Gemfile.lock (") {
		t.Fatalf("saved.txt does not name Gemfile.lock as left out:\n%s", note)
	}
}

// A removal saves the untracked files it deletes within what the cap
// leaves, and names the rest in the event and in saved.txt: a file past
// what is left is skipped with its size; a symlink is saved as the link.
// Ignored files are never saved, nor named.
func TestARemovalSavesTheUntrackedFilesWithinTheCap(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	h.m.saveCap = 1500
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
	writeFile(t, filepath.Join(sl.Path, "small.txt"), "small\n")
	writeFile(t, filepath.Join(sl.Path, "large.txt"), strings.Repeat("x", 2000))
	writeFile(t, filepath.Join(sl.Path, "tmp", "huge.log"), strings.Repeat("y", 4000))
	if err := os.Symlink("/etc/hosts", filepath.Join(sl.Path, "link")); err != nil {
		t.Fatal(err)
	}

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	if patch := readFile(t, filepath.Join(save, "changes.diff")); !strings.Contains(patch, "residue") {
		t.Fatalf("the patch = %q, want README's", patch)
	}
	if got := readFile(t, filepath.Join(save, "untracked", "small.txt")); got != "small\n" {
		t.Fatalf("small.txt saved = %q", got)
	}
	for _, p := range []string{"large.txt", filepath.Join("tmp", "huge.log")} {
		if _, err := os.Lstat(filepath.Join(save, "untracked", p)); !os.IsNotExist(err) {
			t.Fatalf("%s saved: %v", p, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(save, "untracked", "link")); err != nil || target != "/etc/hosts" {
		t.Fatalf("the symlink saved: %q, %v; want the link itself", target, err)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	ev := h.discarded(t, sl.Name)[0].Message
	if !strings.Contains(note, "large.txt (") || !strings.Contains(ev, "large.txt") {
		t.Fatalf("saved.txt or the event does not name large.txt as skipped:\n%s\n%s", note, ev)
	}
	if strings.Contains(note, "huge.log") || strings.Contains(ev, "huge.log") {
		t.Fatalf("an ignored file is named:\n%s\n%s", note, ev)
	}
}

// One cap covers the whole save: the patch and the untracked files take
// from it together, and each file saved takes from what is left. A file
// that fits the cap but not what the patch left is left out, and of two
// files that each fit the cap but not together, the second is. The cap
// test kept one file far past the cap and one far under it, so a cap per
// file or one the patch did not share passed it too.
func TestTheCapIsSharedByThePatchAndTheFiles(t *testing.T) {
	remove := func(t *testing.T, write func(dir string)) (save, note string) {
		f := newPerPR(t, true)
		h := f.h
		h.m.saveCap = 1500
		sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		write(sl.Path)
		if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		save = h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
		return save, readFile(t, filepath.Join(save, "saved.txt"))
	}
	t.Run("the patch's share", func(t *testing.T) {
		save, note := remove(t, func(dir string) {
			writeFile(t, filepath.Join(dir, "README"), strings.Repeat("a tracked line\n", 60)) // a patch of about 1 KB
			writeFile(t, filepath.Join(dir, "mid.txt"), strings.Repeat("m", 1000))
		})
		if fi, err := os.Stat(filepath.Join(save, "changes.diff")); err != nil || fi.Size() < 800 || fi.Size() > 1400 {
			t.Fatalf("changes.diff: %v, %v; want the patch, of 800 to 1400 bytes", fi, err)
		}
		if _, err := os.Lstat(filepath.Join(save, "untracked", "mid.txt")); !os.IsNotExist(err) || !strings.Contains(note, "mid.txt (") {
			t.Fatalf("mid.txt, which fits the cap but not what the patch left: %v, saved.txt:\n%s", err, note)
		}
	})
	t.Run("the files' shares", func(t *testing.T) {
		save, note := remove(t, func(dir string) {
			writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat("a", 900))
			writeFile(t, filepath.Join(dir, "b.txt"), strings.Repeat("b", 900))
		})
		if got := readFile(t, filepath.Join(save, "untracked", "a.txt")); len(got) != 900 {
			t.Fatalf("a.txt saved: %d bytes", len(got))
		}
		if _, err := os.Lstat(filepath.Join(save, "untracked", "b.txt")); !os.IsNotExist(err) || !strings.Contains(note, "b.txt (") {
			t.Fatalf("b.txt, which fits the cap but not beside a.txt: %v, saved.txt:\n%s", err, note)
		}
	})
}

// Untracked files whose names alone pass the runner's output (8 MB, far
// past the cap) are none saved by a removal, and the save says so; the
// tracked changes are saved and the removal goes on.
func TestASaveNamesTheUntrackedFilesItCannotList(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
	h.fake.Rules = append(h.fake.Rules, execx.Rule{Prefix: []string{"git"},
		Result: execx.Result{Stdout: []byte("a.txt\x00"), Truncated: true}})
	prev := h.run.fakeGit
	h.run.fakeGit = func(c execx.Cmd) bool {
		return slices.Contains(c.Args, "--exclude-standard") || prev != nil && prev(c)
	}

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	if patch := readFile(t, filepath.Join(save, "changes.diff")); !strings.Contains(patch, "residue") {
		t.Fatalf("the patch = %q, want the tracked change", patch)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	if !strings.Contains(note, "every untracked file") || !strings.Contains(h.discarded(t, sl.Name)[0].Message, "every untracked file") {
		t.Fatalf("saved.txt or the event does not say the untracked files were left out:\n%s", note)
	}
}

// unreadable writes path with mode 000, put back for the temporary
// directory's removal; it skips the test under root, which reads it anyway.
func unreadable(t *testing.T, path string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	writeFile(t, path, "locked\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

// longName is a file name of 240 bytes: the file itself fits APFS's 255,
// but the temporary name of an atomic write (".<name>.tmp-<12>") does not.
var longName = strings.Repeat("n", 236) + ".txt"

// A switch (and a reset, an abort) keeps the untracked files: its save is
// the patch alone, and saved.txt names the untracked files the tree keeps.
// One untracked file that cannot be copied (unreadable, or a name whose
// temporary file passes 255 bytes) failed the save, so the switch failed
// on every retry and the round's PR was parked, for a file the switch never
// touches.
func TestASwitchKeepsItsUntrackedFilesAndOnlyNamesThem(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	strayEdit(t, sl.Path)
	unreadable(t, filepath.Join(sl.Path, "locked.txt"))
	writeFile(t, filepath.Join(sl.Path, longName), "long\n")
	writeFile(t, filepath.Join(sl.Path, "notes", "mine.txt"), "my notes\n")

	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, h.shaPR7)
	for _, p := range []string{"locked.txt", longName, filepath.Join("notes", "mine.txt")} {
		if _, err := os.Lstat(filepath.Join(sl.Path, p)); err != nil {
			t.Fatalf("the switch did not keep %s: %v", p, err)
		}
	}
	save := h.onlySave(t, h.layout.PRSaves("talkable", "talkable", pr.Number), sl.Name)
	if patch := readFile(t, filepath.Join(save, "changes.diff")); !strings.Contains(patch, "GEM stray") {
		t.Fatalf("the patch = %q, want the tracked change", patch)
	}
	if _, err := os.Lstat(filepath.Join(save, "untracked")); !os.IsNotExist(err) {
		t.Fatalf("a switch copied untracked files: %v", err)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	for _, want := range []string{"locked.txt", longName, "notes/", "kept"} {
		if !strings.Contains(note, want) {
			t.Fatalf("saved.txt does not name %q:\n%s", want, note)
		}
	}
}

// A removal copies the untracked files it deletes, and one it cannot read
// or write is left out with its reason, as a listing too large is: the
// removal goes on. It failed, on every retry.
func TestARemovalLeavesOutAnUntrackedFileItCannotCopy(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
	unreadable(t, filepath.Join(sl.Path, "locked.txt"))
	writeFile(t, filepath.Join(sl.Path, "ok.txt"), "ok\n")

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
		t.Fatalf("state %s, directory left %v; want removed", got.State, fsx.Exists(sl.Path))
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	if got := readFile(t, filepath.Join(save, "untracked", "ok.txt")); got != "ok\n" {
		t.Fatalf("ok.txt saved = %q", got)
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	if !strings.Contains(note, "locked.txt (cannot be read: permission denied)") ||
		!strings.Contains(h.discarded(t, sl.Name)[0].Message, "locked.txt") {
		t.Fatalf("saved.txt or the event does not name locked.txt as left out:\n%s", note)
	}
}

// A removal writes each untracked file it saves straight into the save's
// own folder, with no temporary file and no disk flush per file: a name of
// 238 to 255 bytes is saved (an atomic write's temporary name passed 255
// bytes and the file was left out), and a tree of many small files no
// longer pays two full disk flushes for each.
func TestARemovalSavesAFileWhoseNameLeavesNoRoomForATemporaryOne(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, longName), "long\n")

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	if got := readFile(t, filepath.Join(save, "untracked", longName)); got != "long\n" {
		t.Fatalf("the file of a 240-byte name saved = %q", got)
	}
	ents, err := os.ReadDir(filepath.Join(save, "untracked"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("untracked/ holds %v, %v; want the one file and no temporary one", ents, err)
	}
}

// A tracked change whose patch is empty (a submodule with untracked
// content: git status says " M sub", git diff writes nothing) writes no
// changes.diff, and saved.txt does not say `git apply` puts anything back:
// it would fail with "No valid patches in input".
func TestAWholePatchOfNoBytesWritesNoChangesDiff(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	strayEdit(t, sl.Path)
	writeFile(t, filepath.Join(sl.Path, "notes.txt"), "kept\n") // saved.txt names it: the save is kept
	h.fake.Rules = append(h.fake.Rules, execx.Rule{Prefix: []string{"git"}})
	h.run.fakeGit = func(c execx.Cmd) bool { return slices.Contains(c.Args, "--binary") }

	if err := h.m.Resync(h.ctx, sl, h.shaPR7); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("talkable", "talkable", pr.Number), sl.Name)
	if _, err := os.Lstat(filepath.Join(save, "changes.diff")); !os.IsNotExist(err) {
		t.Fatalf("an empty patch was kept as changes.diff: %v", err)
	}
	if note := readFile(t, filepath.Join(save, "saved.txt")); strings.Contains(note, "apply") || strings.Contains(note, "tracked path(s)") {
		t.Fatalf("saved.txt says an empty patch puts something back:\n%s", note)
	}
}

// The local mise file magnum rendered is magnum's own, never a person's:
// a removal does not save it (the fixture's .gitignore names only the other
// spelling, so git lists it), a removal whose only change it is makes no
// save, and a switch's saved.txt does not name it among the files it
// keeps. A file of that name without magnum's marker is a person's, and
// is saved. The old check used the ignored spelling and could not fail.
func TestASaveLeavesOutOnlyTheMiseFileMagnumRendered(t *testing.T) {
	newRendered := func(t *testing.T) (*perPRFixture, store.Slot) {
		f := newPerPR(t, true)
		writeFile(t, filepath.Join(f.main, MiseLocal), "[env]\nA = \"1\"\n")
		sl, err := f.h.m.CreatePRWorktree(f.h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		if ch, err := f.h.m.worktreeChanges(f.h.ctx, sl.Path); err != nil || !slices.Equal(ch.untracked, []string{MiseLocal}) {
			t.Fatalf("untracked = %q, %v; want %s, which the fixture does not ignore", ch.untracked, err, MiseLocal)
		}
		return f, sl
	}
	folder := func(f *perPRFixture) string { return f.h.layout.PRSaves("zhuravel", "widget", f.pr.Number) }

	t.Run("a switch does not name it", func(t *testing.T) {
		f, sl := newRendered(t)
		writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
		writeFile(t, filepath.Join(sl.Path, "mine.txt"), "mine\n")
		if err := f.h.m.Resync(f.h.ctx, sl, f.sha7); err != nil {
			t.Fatalf("Resync: %v", err)
		}
		note := readFile(t, filepath.Join(f.h.onlySave(t, folder(f), sl.Name), "saved.txt"))
		if !strings.Contains(note, "mine.txt") || strings.Contains(note, MiseLocal) {
			t.Fatalf("saved.txt names %s, or not mine.txt:\n%s", MiseLocal, note)
		}
	})
	t.Run("a removal does not save it", func(t *testing.T) {
		f, sl := newRendered(t)
		writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
		if err := f.h.m.RemovePRWorktree(f.h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		save := f.h.onlySave(t, folder(f), sl.Name)
		if _, err := os.Lstat(filepath.Join(save, "untracked", MiseLocal)); !os.IsNotExist(err) {
			t.Fatalf("the rendered %s was saved: %v", MiseLocal, err)
		}
	})
	t.Run("a removal of it alone makes no save", func(t *testing.T) {
		f, sl := newRendered(t)
		if err := f.h.m.RemovePRWorktree(f.h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		if got := saves(t, folder(f)); len(got) != 0 {
			t.Fatalf("saves = %q, want none for magnum's own file", got)
		}
		if got := f.h.slot(sl.Name); got.State != store.SlotRemoved {
			t.Fatalf("state = %s, want removed", got.State)
		}
	})
	t.Run("a person's file of that name is saved", func(t *testing.T) {
		f, sl := newRendered(t)
		writeFile(t, filepath.Join(sl.Path, MiseLocal), "[env]\nMINE = \"1\"\n")
		if err := f.h.m.RemovePRWorktree(f.h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		save := f.h.onlySave(t, folder(f), sl.Name)
		if got := readFile(t, filepath.Join(save, "untracked", MiseLocal)); got != "[env]\nMINE = \"1\"\n" {
			t.Fatalf("the person's %s saved = %q", MiseLocal, got)
		}
	})
}

// A removal never saves the files magnum wrote into the worktree, as it
// never saves the mise file it rendered: the [[repo]] copy_files (a file,
// and a folder's files), which hold the main clone's secrets, and what the
// setup wrote; a removal whose only untracked files they are makes no
// save. A symlink is saved as the link, never what it points to, so a link
// to a folder of keys copies no key into the saves.
func TestARemovalSavesNothingMagnumWroteAndASymlinkAsTheLink(t *testing.T) {
	setup := func(t *testing.T) (*perPRFixture, store.Slot) {
		f := newPerPR(t, true)
		h := f.h
		writeFile(t, filepath.Join(f.main, "master.key"), "main clone secret\n")
		writeFile(t, filepath.Join(f.main, "fixtures", "a.json"), "{}\n")
		h.m = h.newManager(Deps{Repos: []config.Repo{{Repo: f.repoName(),
			CopyFiles: []string{"master.key", "fixtures"}, Setup: []string{"touch .env.setup"}}}})
		sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"master.key", filepath.Join("fixtures", "a.json"), ".env.setup"} {
			if !fsx.Exists(filepath.Join(sl.Path, p)) {
				t.Fatalf("the worktree's setup did not write %s", p)
			}
		}
		return f, sl
	}
	folder := func(f *perPRFixture) string { return f.h.layout.PRSaves("zhuravel", "widget", f.pr.Number) }

	t.Run("a removal saves a person's files alone", func(t *testing.T) {
		f, sl := setup(t)
		keys := filepath.Join(f.h.root, "keys")
		writeFile(t, filepath.Join(keys, "id_ed25519"), "a key\n")
		if err := os.Symlink(keys, filepath.Join(sl.Path, "keys")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(sl.Path, "notes.txt"), "mine\n")
		if err := f.h.m.RemovePRWorktree(f.h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		save := f.h.onlySave(t, folder(f), sl.Name)
		if got := readFile(t, filepath.Join(save, "untracked", "notes.txt")); got != "mine\n" {
			t.Fatalf("notes.txt saved = %q", got)
		}
		for _, p := range []string{"master.key", "fixtures", ".env.setup"} {
			if _, err := os.Lstat(filepath.Join(save, "untracked", p)); !os.IsNotExist(err) {
				t.Fatalf("%s saved: %v", p, err)
			}
		}
		if target, err := os.Readlink(filepath.Join(save, "untracked", "keys")); err != nil || target != keys {
			t.Fatalf("the symlink saved: %q, %v; want the link to %s", target, err, keys)
		}
		var files []string // WalkDir follows no link
		if err := filepath.WalkDir(save, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				files = append(files, filepath.Base(p))
			}
			return err
		}); err != nil || !slices.Equal(files, []string{"saved.txt", "notes.txt"}) {
			t.Fatalf("the save's files = %q, %v; want saved.txt and notes.txt alone, no key", files, err)
		}
		note := readFile(t, filepath.Join(save, "saved.txt"))
		if strings.Contains(note, "master.key") || strings.Contains(note, ".env.setup") {
			t.Fatalf("saved.txt names magnum's own files:\n%s", note)
		}
	})
	t.Run("a removal of magnum's files alone makes no save", func(t *testing.T) {
		f, sl := setup(t)
		if err := f.h.m.RemovePRWorktree(f.h.ctx, sl, false); err != nil {
			t.Fatalf("RemovePRWorktree: %v", err)
		}
		if got := saves(t, folder(f)); len(got) != 0 {
			t.Fatalf("saves = %q, want none for magnum's own files", got)
		}
		if got := f.h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
			t.Fatalf("state %s, directory left %v; want removed", got.State, fsx.Exists(sl.Path))
		}
	})
}

// A removal whose untracked files are all left out keeps no untracked/
// folder in its save, as a save with no path's patch keeps no changes.diff.
func TestASaveThatCopiesNoUntrackedFileHasNoUntrackedFolder(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	h.m.saveCap = 1000
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "residue\n")
	writeFile(t, filepath.Join(sl.Path, "large.txt"), strings.Repeat("x", 2000))

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	if note := readFile(t, filepath.Join(save, "saved.txt")); !strings.Contains(note, "large.txt (") {
		t.Fatalf("saved.txt does not name large.txt as left out:\n%s", note)
	}
	if _, err := os.Lstat(filepath.Join(save, "untracked")); !os.IsNotExist(err) {
		t.Fatalf("a save that copied no file has untracked/: %v", err)
	}
}

// A saved file keeps its owner's bits, so the `cp -R` saved.txt gives puts
// an executable script back executable; group and others get nothing. Every
// file was saved 0600.
func TestARemovalKeepsTheOwnersBitsOfASavedFile(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "bin", "dev-check"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(sl.Path, "bin", "dev-check"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "notes.txt"), "notes\n")
	if err := os.Chmod(filepath.Join(sl.Path, "notes.txt"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	save := h.onlySave(t, h.layout.PRSaves("zhuravel", "widget", f.pr.Number), sl.Name)
	for p, want := range map[string]os.FileMode{filepath.Join("bin", "dev-check"): 0o700, "notes.txt": 0o600} {
		if fi, err := os.Stat(filepath.Join(save, "untracked", p)); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s saved: %v, %v; want mode %04o", p, fi, err, want)
		}
	}
}
