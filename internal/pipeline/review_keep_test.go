package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeWritesReview is the standard posting judge (review id) that first
// writes body as its review file for magnum post-review, beside its result
// file.
func (e *env) judgeWritesReview(id int64, body string) behavior {
	post := e.judgePosts(id, "COMMENTED", "COMMENT").behavior(e.t)
	return func(f *fakeAgents, run store.Run, text string) error {
		file := filepath.Join(filepath.Dir(store.Deref(run.ReportPath)), agents.PostReviewFile)
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			return err
		}
		return post(f, run, text)
	}
}

// A round on a head an earlier round reviewed writes its review over the
// earlier one's review.json: 10 report directories held two or three posted
// reviews each, and only the last one's text stayed, so the improvement
// loop took 13 different reviews for the same. Each posted run keeps its
// own copy, review-<run id>.json, and review.json stays the latest, which
// its readers (the fixer's prompt, the improvement loop) read as before.
func TestEachPostedRunKeepsItsOwnReviewFile(t *testing.T) {
	e := newEnv(t)
	first, second := `{"event":"COMMENT","body":"first"}`, `{"event":"COMMENT","body":"second"}`
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgeWritesReview(601, first), e.judgeWritesReview(602, second)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("first RunRound = %+v, %v", res, err)
	}
	in := e.input(KindRereview)
	in.Round, in.SameHead, in.Roles = 2, true, []config.Role{e.judgeRole()}
	in.Previous = &PreviousReview{ID: 601, Event: "COMMENTED", SHA: target, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("second RunRound = %+v, %v", res, err)
	}
	want := map[int64]string{601: first, 602: second}
	kept := 0
	for _, r := range e.runs() {
		if r.Role != string(agents.RoleJudge) || r.ReviewID == nil {
			continue
		}
		kept++
		if got := readFile(t, filepath.Join(e.reportDir(), "review-"+r.ID+".json")); got != want[*r.ReviewID] {
			t.Errorf("review-%s.json = %q, want review %d's %q", r.ID, got, *r.ReviewID, want[*r.ReviewID])
		}
	}
	if kept != 2 {
		t.Fatalf("%d posted judge runs, want 2", kept)
	}
	if got := readFile(t, filepath.Join(e.reportDir(), agents.PostReviewFile)); got != second {
		t.Fatalf("%s = %q, want the latest review's", agents.PostReviewFile, got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
