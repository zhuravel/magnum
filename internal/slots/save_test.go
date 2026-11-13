package slots

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// what is left is skipped with its size, a symlink is named with its
// target. Ignored files are never saved, nor named.
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
	for _, p := range []string{"large.txt", "link", filepath.Join("tmp", "huge.log")} {
		if _, err := os.Lstat(filepath.Join(save, "untracked", p)); !os.IsNotExist(err) {
			t.Fatalf("%s saved: %v", p, err)
		}
	}
	note := readFile(t, filepath.Join(save, "saved.txt"))
	ev := h.discarded(t, sl.Name)[0].Message
	for _, want := range []string{"large.txt", "link", "/etc/hosts"} {
		if !strings.Contains(note, want) || !strings.Contains(ev, want) {
			t.Fatalf("saved.txt or the event does not name %q as skipped:\n%s\n%s", want, note, ev)
		}
	}
	if strings.Contains(note, "huge.log") || strings.Contains(ev, "huge.log") {
		t.Fatalf("an ignored file is named:\n%s\n%s", note, ev)
	}
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
