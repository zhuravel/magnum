package engine

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
)

// corpusPR is fullName#n, closed and reviewed, in h's registry.
func corpusPR(h *harness, fullName string, n int) store.PR {
	h.t.Helper()
	owner, name, _ := strings.Cut(fullName, "/")
	repo, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_" + fullName, Owner: owner, Name: name, WatchOwner: owner, Mode: store.RepoModePerPR})
	if err != nil {
		h.t.Fatal(err)
	}
	closed := h.clock.Now().Add(-48 * time.Hour)
	up, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{
		RepoID: repo.ID, NodeID: "PR_" + fullName + strconv.Itoa(n), Number: n, URL: "https://github.com/" + fullName + "/pull/" + strconv.Itoa(n),
		HeadSHA: retroShaB, AuthorLogin: new("alice"), AuthorType: new("User"),
		GHState: store.GHMerged, MergedAt: &closed, ClosedAt: &closed, InitialState: store.PRReviewed, Identity: "zhuravel",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return up.PR
}

// corpusMiss stores a miss of pr of scope, P1, at the reviewed commit retroShaA.
func corpusMiss(h *harness, pr store.PR, n int, scope string) store.Miss {
	h.t.Helper()
	m, err := h.st.UpsertMiss(h.ctx, store.Miss{PRID: pr.ID, SourceURL: pr.URL + "#discussion_r" + strconv.Itoa(n), SourceKind: store.MissSourceThread,
		Reviewer: "rev-ann", Path: "app/models/coupon.rb", Line: 41, ReviewedSHA: retroShaA, Class: store.MissMiss, Severity: "P1",
		Title: "Coupon lookup ignores the site", Lesson: "When a finder takes a code from the request, check the site scope.",
		Scope: scope, Lines: []int{40, 42}, Match: []string{"site"}})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// corpusDecline stores a general decline of PR prID, declined as wrong at
// P1 on the reviewed commit retroShaA, of comment n.
func corpusDecline(h *harness, prID int64, n int) store.Decline {
	h.t.Helper()
	pr, err := h.st.PRByID(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	d, err := h.st.UpsertDecline(h.ctx, store.Decline{PRID: pr.ID, SourceURL: pr.URL + "#discussion_d" + strconv.Itoa(n), CommentID: int64(n),
		Path: "lib/token.rb", Line: 30, Title: "Token can be nil", Priority: "P1", ReviewedSHA: retroShaA,
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Lesson: "When the caller checks a value, do not ask for a guard.",
		Scope: store.MissScopeGeneral, Match: []string{"nil"}})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

// learnedCases are the case names of the learned corpus; nil when there is no file.
func learnedCases(h *harness) []string {
	h.t.Helper()
	c, err := eval.LoadCorpus(h.layout.LearnCorpus())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, cs := range c.Cases {
		out = append(out, cs.Name)
	}
	return out
}

// A retro ends by writing the learned corpus: the general P1 miss it stored is a case at once.
func TestARetroSyncsTheLearnedCorpus(t *testing.T) {
	h := newHarness(t, withClassifier(&fakeClassifier{}))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	m := h.misses(pr.ID)["101"]
	if m.Scope != store.MissScopeGeneral || m.Class != store.MissMiss {
		t.Fatalf("the retro stored %+v for t101, want a general miss", m)
	}
	if got := learnedCases(h); !slices.Equal(got, []string{"miss-" + strconv.FormatInt(m.ID, 10)}) {
		t.Fatalf("learned cases after the retro: %v", got)
	}
}

func (h *harness) syncCorpus() {
	h.t.Helper()
	if err := h.e.syncLearnedCorpus(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// The learned corpus holds one case per general P0..P2 miss and per general wrong or over-ranked
// decline whose PR a watch still covers and Codex never flagged: a replay of an unwatched PR has no
// roles or identity, and a flagged one is never replayed. A file that would not change is not
// rewritten, a PR flagged since drops out, and no case left removes the file.
func TestSyncLearnedCorpusLeavesOutUnwatchedAndFlaggedPRs(t *testing.T) {
	h := newHarness(t)
	watched, flagged := corpusPR(h, "talkable/talkable", 11), corpusPR(h, "talkable/talkable", 12)
	unwatched, other := corpusPR(h, "example/widgets", 3), corpusPR(h, "zhuravel/widgets", 4)
	in := corpusMiss(h, watched, 1, store.MissScopeGeneral)
	corpusMiss(h, watched, 2, store.MissScopeRepo)
	corpusMiss(h, flagged, 3, store.MissScopeGeneral)
	corpusMiss(h, unwatched, 4, store.MissScopeGeneral)
	d, err := h.st.UpsertDecline(h.ctx, store.Decline{PRID: other.ID, SourceURL: other.URL + "#discussion_r5", CommentID: 5,
		Path: "lib/token.rb", Line: 30, Title: "Token can be nil", Priority: "P1", ReviewedSHA: retroShaA,
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Lesson: "When the caller checks a value, do not ask for a guard.",
		Scope: store.MissScopeGeneral, Match: []string{"nil"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.setCodexFlag(h.ctx, flagged.ID, CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-9", At: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}

	h.syncCorpus()
	missName, declineName := "miss-"+strconv.FormatInt(in.ID, 10), "decline-"+strconv.FormatInt(d.ID, 10)
	if got := learnedCases(h); !slices.Equal(got, []string{missName, declineName}) {
		t.Fatalf("learned cases %v, want %s and %s", got, missName, declineName)
	}

	path := h.layout.LearnCorpus()
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	h.syncCorpus()
	if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("an unchanged corpus was rewritten (mtime %v, %v)", st.ModTime(), err)
	}

	if err := h.e.setCodexFlag(h.ctx, watched.ID, CodexFlag{Kind: "codex", Role: "codex-judge", Run: "r-10", At: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.syncCorpus()
	if got := learnedCases(h); !slices.Equal(got, []string{declineName}) {
		t.Fatalf("after the PR of %s was flagged: %v", missName, got)
	}
	if n, err := h.st.SetDeclineState(h.ctx, []int64{d.ID}, []string{store.MissNew}, store.MissDismissed); err != nil || n != 1 {
		t.Fatalf("dismiss the decline: %d, %v", n, err)
	}
	h.syncCorpus()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no case left, the corpus file stays: %v", err)
	}
	h.syncCorpus() // nothing to remove: no error
}
