package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// renamesHarness is an auto-approval harness whose zhuravel client lists a
// PR's files through REST (filesGH), as the paths renames leave are read.
func renamesHarness(t *testing.T) (*harness, *filesGH) {
	t.Helper()
	fg := &filesGH{}
	h, _ := newAutoHarness(t, func(h *harness) {
		fg.fakeGH = h.gh
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return fg
			}
			return nil
		}
	})
	return h, fg
}

// A rename's previous path depends on the base GitHub compares the head
// with: a PR retargeted to another base reads the REST list again, though
// its head and the Details' new paths stay the same, so the path a rename
// leaves under the new base (.claude/hooks/x.rb) is in the list.
func TestARetargetReadsTheRenamesAgainstTheNewBase(t *testing.T) {
	h, fg := renamesHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	fg.set(2, "scripts/x.rb", "lib/x.rb")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", base: "feature", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	fg.set(2, "scripts/x.rb", ".claude/hooks/x.rb")
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID)
	if err != nil || !ok || !strings.Contains(strings.Join(f.Paths, " "), ".claude/hooks/x.rb") {
		t.Fatalf("the retargeted head's list = %+v, %v (%v), after %d REST reads: want the path its rename leaves under the new base",
			f, ok, err, fg.fetches(2))
	}
}

// A list kept for the same head (an earlier build's, without the paths its
// renames leave, or one GitHub listed before a retarget) is no list of the
// head once the Details name a rename and the REST list cannot be read: the
// poll drops it, so the gate refuses without a class (it cannot tell) and
// approves nothing, rather than read the stored list as the head's whole
// one. The next poll that reads the REST list refuses the head for the
// .claude/ path its rename leaves.
func TestARESTFailureLeavesASameHeadListAnOlderBuildKeptUnread(t *testing.T) {
	h, fg := renamesHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}})
	h.tick()
	fg.set(2, "scripts/x.rb", ".claude/hooks/x.rb")
	fg.fail(errors.New("GitHub is down"))
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	if f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID); err != nil || ok {
		t.Fatalf("the head's list after a failed REST read = %+v, %v (%v): want none", f, ok, err)
	}
	h.reviewTo(2, "b1")
	h.flush()
	h.wantRefused(2, "it has no list of the head's files")
	v, _ := h.e.getKV(h.ctx, KVPRAutoApproveRefused(h.pr(2).ID))
	if r, _ := ParseAutoApproveRefused(v); r.Public != "" {
		t.Fatalf("the refusal's class = %q: want none, the next poll decides again", r.Public)
	}
	fg.fail(nil)
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true})
	h.tick()
	h.flush()
	if got := h.autoRefused(2); !strings.Contains(got, "(.claude/hooks/x.rb)") {
		t.Fatalf("held back for %q once REST answers: want the path the rename leaves", got)
	}
}

// A REST read of the paths renames leave that fails ends the poll's REST
// reads of the repository, as a failed file list does (fetchChangedFiles):
// the next PR's renames wait for the next poll rather than ask a GitHub
// that just failed.
func TestARESTFailureEndsThePollsReadsOfTheRenamesPaths(t *testing.T) {
	h, fg := renamesHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	fg.fail(errors.New("GitHub is down"))
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true},
		prSpec{n: 3, head: "c1", files: []string{"lib/y.rb"}, renamed: true})
	h.tick()
	if n := fg.fetches(-1); n != 1 {
		t.Fatalf("ListFiles asked %d times in the poll, want 1: a failure ends the poll's reads", n)
	}
}

// A list GitHub's Details cut at 100 files stays cut once the REST list
// adds the paths renames leave, whatever the REST list says: it was read
// at another moment, and the first page of the Details is no whole list.
// The list served again for the same head stays cut too, and the gate
// refuses the head as one of more files than GitHub lists.
func TestTheRESTListKeepsTheDetailsCutOfTheFileList(t *testing.T) {
	h, fg := renamesHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	fg.set(2, "scripts/x.rb", "lib/x.rb", "app/y.rb")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true, filesTruncated: true})
	h.tick()
	if f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID); err != nil || !ok || !f.Truncated {
		t.Fatalf("the head's list = %+v, %v (%v): want it cut, as the Details said", f, ok, err)
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"scripts/x.rb"}, renamed: true, filesTruncated: true,
		labels: []string{"ready"}})
	h.tick()
	if f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID); err != nil || !ok || !f.Truncated || fg.fetches(2) != 1 {
		t.Fatalf("the head's list served again = %+v, %v (%v) after %d REST reads: want it cut, from the one read", f, ok, err, fg.fetches(2))
	}
	h.reviewTo(2, "b1")
	h.flush()
	h.wantRefused(2, "it changes more than the 3 files GitHub listed")
}
