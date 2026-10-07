package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// TestRetroLeavesThePRDueWhenTheRetroIsCutShort: a stop that is not the
// PR's fault (a classifier that cannot start or went away, a shutdown, a
// usage limit, a logout, a per-model limit, an overload) writes no
// retro_prs row for the PR in flight nor the ones after it, so the next
// retro takes them; only a usage limit and a logout pause the agent's CLI,
// as in rounds.
func TestRetroLeavesThePRDueWhenTheRetroIsCutShort(t *testing.T) {
	pause := func(kind string) func(*harness, ClassifyJob) (ClassifyResult, error) {
		return func(h *harness, _ ClassifyJob) (ClassifyResult, error) {
			return ClassifyResult{Pause: &pipeline.Pause{Kind: kind, Tool: "claude", Detail: "limit", Until: h.clock.Now().Add(time.Hour)}},
				errors.New("the retro's agent stopped")
		}
	}
	for name, tc := range map[string]struct {
		factoryErr bool
		answer     func(*harness, ClassifyJob) (ClassifyResult, error)
		paused     bool
	}{
		"the classifier cannot start": {factoryErr: true},
		"the classifier went away": {answer: func(*harness, ClassifyJob) (ClassifyResult, error) {
			return ClassifyResult{}, fmt.Errorf("%w: herdr is not running", ErrClassifierDown)
		}},
		"shutdown mid-turn": {answer: func(h *harness, _ ClassifyJob) (ClassifyResult, error) {
			h.e.stopRetro()
			return ClassifyResult{}, context.Canceled
		}},
		"usage limit":    {answer: pause("usage_limit"), paused: true},
		"login required": {answer: pause("login_required"), paused: true},
		"model limit":    {answer: pause("model_limit")},
		"overloaded":     {answer: pause("overloaded")},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeClassifier{}
			var h *harness
			h = newHarness(t, withClassifier(fc), func(h *harness) {
				if tc.factoryErr {
					h.d.Classifier = func(context.Context, RetroRun) (Classifier, error) { return nil, errors.New("no herdr") }
				}
			})
			if tc.answer != nil {
				fc.answer = func(job ClassifyJob) (ClassifyResult, error) { return tc.answer(h, job) }
			}
			pr7, pr8 := retroPR(h, 7, 24*time.Hour), retroPR(h, 8, 48*time.Hour)
			seedRetroGitHub(h, 7)
			seedRetroGitHub(h, 8)
			h.requestRetro(RetroPayload{})

			for _, pr := range []store.PR{pr7, pr8} {
				if rp, err := h.retroRecord(pr.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("PR %d has a retro row: %+v, %v", pr.Number, rp, err)
				}
			}
			if len(fc.jobs) > 1 {
				t.Fatalf("the retro went on after the stop: %d jobs", len(fc.jobs))
			}
			if _, ok := h.e.toolPause(h.ctx, "claude"); ok != tc.paused {
				t.Fatalf("claude paused = %v, want %v", ok, tc.paused)
			}
			if due, err := h.st.RetroDue(h.ctx, store.RetroQuery{Since: h.clock.Now().Add(-7 * 24 * time.Hour)}); err != nil || len(due) != 2 {
				t.Fatalf("due after the stop = %d PRs, %v; want both", len(due), err)
			}
			if name == "shutdown mid-turn" {
				// A retro the shutdown cut short is neither done nor failed:
				// the last finished retro's summary stays, and so does the day.
				if v, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); ok {
					t.Fatalf("a retro the shutdown cut short left a summary: %s", v)
				}
				return
			}
			if sum := h.retroLast(); sum.Failed != 0 || sum.Stopped == "" {
				t.Fatalf("summary = %+v", sum)
			}
		})
	}
}

// TestRetroRetriesAFailedPRUpToThreeTimes: an answer that stays invalid is
// the PR's failure: recorded, and tried again by the next retros until
// store.RetroMaxAttempts.
func TestRetroRetriesAFailedPRUpToThreeTimes(t *testing.T) {
	fc := &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) {
		return ClassifyResult{}, os.WriteFile(job.OutputPath, []byte(`{"items":[]}`), 0o600)
	}}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	for attempt := 1; attempt <= store.RetroMaxAttempts+1; attempt++ {
		h.advance(time.Second) // a new run directory per retro
		h.requestRetro(RetroPayload{})
		want := min(attempt, store.RetroMaxAttempts)
		if rp, err := h.retroRecord(pr.ID); err != nil || rp.Status != store.RetroFailed || rp.Attempts != want {
			t.Fatalf("retro %d: retro_prs = %+v, %v; want failed, %d attempts", attempt, rp, err, want)
		}
		if len(fc.jobs) != want {
			t.Fatalf("retro %d: classified %d times, want %d", attempt, len(fc.jobs), want)
		}
	}
}

// TestRetroAgainKeepsClassificationsWhenTheClassifierFails: `--again` with
// a classifier that fails leaves the earlier classification in place.
func TestRetroAgainKeepsClassificationsWhenTheClassifierFails(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	if m := h.misses(pr.ID)["101"]; m.Class != store.MissMiss {
		t.Fatalf("first retro: t101 = %+v", m)
	}
	fc.answer = func(job ClassifyJob) (ClassifyResult, error) {
		return ClassifyResult{}, os.WriteFile(job.OutputPath, []byte(`not json`), 0o600)
	}
	h.advance(time.Second)
	h.requestRetro(RetroPayload{Again: true})
	if rp, _ := h.retroRecord(pr.ID); rp.Status != store.RetroFailed {
		t.Fatalf("second retro = %+v", rp)
	}
	ms := h.misses(pr.ID)
	if m := ms["101"]; m.Class != store.MissMiss || m.Title == "" || m.Severity != "P1" {
		t.Fatalf("t101 downgraded: %+v", m)
	}
	if ms["201"].Class != store.MissStyle {
		t.Fatalf("r201 downgraded: %+v", ms["201"])
	}
}

// TestRetroExplicitRefsImplyAgain: `magnum retro <ref>` looks at the PRs
// it names even when a retro already did.
func TestRetroExplicitRefsImplyAgain(t *testing.T) {
	h := newHarness(t)
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.requestRetro(RetroPayload{})
	reads := h.gh.count("threads:")
	h.advance(time.Second)
	h.requestRetro(RetroPayload{PRs: []int64{pr.ID}})
	if h.gh.count("threads:") != reads+1 {
		t.Fatal("a named PR was skipped because a retro had looked at it")
	}
}

// TestRetroCountsLaterCommentsOnlyOnDescendants: the comparison's status
// decides: a comment on a commit that descends from the reviewed one, with
// its file unchanged, applies to the reviewed commit; one on an older
// commit (behind) or a replaced history (diverged) is outside.
func TestRetroCountsLaterCommentsOnlyOnDescendants(t *testing.T) {
	for status, class := range map[string]string{"ahead": store.MissUnclassified, "identical": store.MissUnclassified,
		"behind": store.MissOutside, "diverged": store.MissOutside} {
		h := newHarness(t)
		pr := retroPR(h, 7, 24*time.Hour)
		seedRetroGitHub(h, 7)
		h.gh.mu.Lock()
		h.gh.statuses = map[string]string{retroShaA + "..." + retroShaB: status}
		h.gh.mu.Unlock()
		h.requestRetro(RetroPayload{})
		if m := h.misses(pr.ID)["103"]; m.Class != class {
			t.Errorf("%s: t103 = %s, want %s", status, m.Class, class)
		}
	}
}

// TestDaemonOnceDoesNotStartTheDailyRetro: `magnum daemon --once` runs one
// tick to inspect the daemon's decisions; it never starts the day's retro.
func TestDaemonOnceDoesNotStartTheDailyRetro(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Learn.Enabled = true })
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); ok {
		t.Fatal("a --once run started the daily retro")
	}
	if h.gh.count("threads:") != 0 {
		t.Fatal("a --once run read a PR's threads")
	}
}

// retroCandidatesOf reads the candidates file a classifier was given.
func retroCandidatesOf(t *testing.T, job ClassifyJob) []learn.Candidate {
	t.Helper()
	c, err := readCandidates(job.CandidatesPath)
	if err != nil {
		t.Fatal(err)
	}
	return c.Candidates
}

// TestRetroLeftSideCommentsKeepTheirHunkButNoLine: a comment on deleted
// lines reaches the classifier with its hunk and side, without a line, and
// is stored without one.
func TestRetroLeftSideCommentsKeepTheirHunkButNoLine(t *testing.T) {
	fc := &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) { return ClassifyResult{Unclassified: true}, nil }}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.gh.mu.Lock()
	th := retroThread(101, "rev-ann", "app/models/coupon.rb", 42, retroShaA)
	th.DiffSide = "LEFT"
	h.gh.threads[7] = []github.Thread{th}
	h.gh.allReviews[7] = nil
	h.gh.mu.Unlock()
	h.requestRetro(RetroPayload{})
	if len(fc.jobs) != 1 {
		t.Fatalf("jobs = %d", len(fc.jobs))
	}
	c := retroCandidatesOf(t, fc.jobs[0])
	b, _ := json.Marshal(c)
	if len(c) != 1 || c[0].Line != 0 || c[0].Side != "LEFT" || c[0].DiffHunk == "" || strings.Contains(string(b), `"line"`) {
		t.Fatalf("candidates = %s", b)
	}
	if m := h.misses(pr.ID)["101"]; m.Line != 0 || len(m.Lines) != 0 {
		t.Fatalf("stored = %+v", m)
	}
}
