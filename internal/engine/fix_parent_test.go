package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// fixOneFixed is a fixer's "## Replied" line for one fixed finding.
const fixOneFixed = "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)"

// The parent of a PR is its base branch, never the repository's default
// branch: a stacked PR's parent is the branch below it, and merging master
// into it would put master's commits into its own diff. A PR whose base
// neither GitHub nor the registry names gets no merge step at all, though
// the default branch's tip conflicts with it: the guard fetches nothing and
// checks no merge, the prompt names no merge and no fix-sync line, and the
// answer says that no parent is merged and why.
func TestAFixOnAPRWithNoKnownBaseMergesNoParent(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", noBase: true})
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET base_ref = NULL WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	git.conflict("app/models/offer.rb")
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	const note = "no parent merge: the PR's base branch is not known yet"
	if res := h.wantDone(id); !strings.Contains(res, note) {
		t.Errorf("answer = %q, want it to say %q", res, note)
	}
	if fetches, merges := git.calls(); len(fetches)+len(merges) != 0 {
		t.Errorf("fetches %v, merge checks %v: the job looked at a base the PR does not name", fetches, merges)
	}
	in := h.fixInput()
	if in.ConflictSHA != "" || in.ConflictBase != "" || in.FixSyncCommand != "" {
		t.Errorf("the prompt merges %q at %q, fix-sync line %q; want no merge and no fix-sync line", in.ConflictBase, in.ConflictSHA, in.FixSyncCommand)
	}
}

// A PR retargeted on GitHub after the poll's last read (its base merged, or
// its author moved it onto another branch) is merged with the base GitHub
// names when the job starts, not the one the registry still holds: the
// guard reads the PR again, fetches that base and checks the merge with its
// tip, and the prompt's merge and fix-sync line name it.
func TestAFixMergesTheBaseGitHubNamesWhenItsJobStarts(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", base: "release"})
	git.conflict("app/models/offer.rb")
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment\nb2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	if base := deref(h.pr(2).BaseRef); base != "master" {
		t.Fatalf("the registry's base = %q before the job, want master", base)
	}
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	slot := h.slot("review1")
	if fetches, _ := git.calls(); len(fetches) != 1 || fetches[0] != slot.MainClone+":release" {
		t.Errorf("fetches = %v, want the guard's of release alone", fetches)
	}
	in := h.fixInput()
	if in.ConflictBase != "release" || in.ConflictSHA != fixBaseTip {
		t.Errorf("the prompt merges %q at %q, want release at %s", in.ConflictBase, in.ConflictSHA, fixBaseTip)
	}
	if want := agents.FixSyncLine(h.d.Layout.Binary(), slot.Path, fixHead, "feature", "release"); in.FixSyncCommand != want {
		t.Errorf("fix-sync line = %q, want %q", in.FixSyncCommand, want)
	}
}
