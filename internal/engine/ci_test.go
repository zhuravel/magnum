package engine

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// setCI changes what the fake GitHub reports for PR #n's head checks,
// keeping its updatedAt: a finished CI run does not move it.
func (h *harness) setCI(n int, state string, checks ...github.Check) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	prs := h.gh.repos["talkable/talkable"]
	for i := range prs {
		if prs[i].n == n {
			prs[i].ci, prs[i].checks = state, checks
		}
	}
}

func (h *harness) prEvents() int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRowContext(h.ctx, "SELECT COUNT(*) FROM events WHERE kind LIKE 'pr.%'").Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// TestCIChangeFetchesDetailsOnceAndQueuesNothing: CI going PENDING →
// FAILURE on a reviewed PR is fetched once per change and stored, but it is
// not a push: the PR stays reviewed with its timers, nothing is queued.
func TestCIChangeFetchesDetailsOnceAndQueuesNothing(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	if before.CI == nil || before.CI.SHA != "b1" || deref(before.CIState) != "" || len(before.CI.Checks) != 0 {
		t.Fatalf("CI without checks = %v %+v", before.CIState, before.CI)
	}
	own, all, events := h.gh.count("details:talkable/talkable:[2]"), h.gh.count("details:"), h.prEvents()
	step := func(want int) {
		t.Helper()
		h.advance(time.Minute)
		h.tick()
		if n := h.gh.count("details:talkable/talkable:[2]") - own; n != want {
			t.Fatalf("details of #2 = %d, want %d (calls %v)", n, want, h.gh.calls)
		}
		if n := h.gh.count("details:"); n != all+want {
			t.Fatalf("other details fetched: %v", h.gh.calls)
		}
	}

	h.setCI(2, "PENDING", github.Check{Name: "rspec (1)", State: github.CheckPassed}, github.Check{Name: "jest", State: github.CheckPending},
		github.Check{Name: "completion", State: github.CheckPending})
	step(1)
	step(1) // unchanged: not fetched again
	if pr := h.pr(2); deref(pr.CIState) != "PENDING" || pr.CI == nil || pr.CI.State != "PENDING" || pr.CI.Total != 3 || pr.CI.Pending != 2 {
		t.Fatalf("pending CI = %v %+v", pr.CIState, pr.CI)
	}

	failedAt := h.clock.Now().UTC()
	h.setCI(2, "FAILURE", github.Check{Name: "rspec (1)", State: github.CheckPassed},
		github.Check{Name: "jest", State: github.CheckFailed, Workflow: "CI", At: failedAt}, github.Check{Name: "completion", State: github.CheckSkipped})
	step(2)
	step(2)
	pr := h.wantState(2, store.PRReviewed)
	want := store.CIStatus{SHA: "b1", State: "FAILURE", Total: 3, Passed: 1, Failed: 1, Skipped: 1, Complete: true,
		Checks: []store.CheckResult{{Name: "rspec (1)", State: store.CheckPassed}, {Name: "jest", State: store.CheckFailed, Workflow: "CI", At: failedAt},
			{Name: "completion", State: store.CheckSkipped}}}
	if deref(pr.CIState) != "FAILURE" || pr.CI == nil || !reflect.DeepEqual(*pr.CI, want) {
		t.Fatalf("failed CI = %v %+v", pr.CIState, pr.CI)
	}
	if pr.HeadSHA != before.HeadSHA || !pr.HeadChangedAt.Equal(before.HeadChangedAt) || deref(pr.ReviewedSHA) != "b1" ||
		!equalTime(pr.PendingSince, before.PendingSince) || !equalTime(pr.NextEligibleAt, before.NextEligibleAt) ||
		pr.Attempts != before.Attempts || pr.RoundsToday != before.RoundsToday {
		t.Fatalf("CI changed the PR:\nbefore %+v\nafter  %+v", before, pr)
	}
	if n := h.prEvents(); n != events {
		t.Fatalf("PR events: %d new", n-events)
	}
}

// TestCIRetriedUntilDetailsSucceed: the radar's rollup is recorded at once,
// and the checks are asked for on every poll until a Details fetch succeeds.
func TestCIRetriedUntilDetailsSucceed(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	details := h.gh.count("details:")
	h.gh.fail(errors.New("boom"), nil)
	h.setCI(2, "SUCCESS", github.Check{Name: "completion", State: github.CheckPassed})
	for range 2 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := h.gh.count("details:") - details; n != 2 {
		t.Fatalf("details attempts = %d, want 2", n)
	}
	if pr := h.pr(2); deref(pr.CIState) != "SUCCESS" || pr.CI.State != "" {
		t.Fatalf("while Details fail: ci_state %v, checks %+v", pr.CIState, pr.CI)
	}
	h.gh.fail(nil, nil)
	for range 2 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := h.gh.count("details:") - details; n != 3 {
		t.Fatalf("details attempts = %d, want 3", n)
	}
	if pr := h.pr(2); pr.CI.State != "SUCCESS" || pr.CI.Passed != 1 {
		t.Fatalf("after the retry: %+v", pr.CI)
	}
}

// TestCIUnreadableRollupIsNotPolled: a PR whose rollup the radar cannot
// read (a fork's) is not fetched on every poll; its checks refresh with its
// next Details fetch.
func TestCIUnreadableRollupIsNotPolled(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.gh.mu.Lock()
	for i := range h.gh.repos["talkable/talkable"] {
		h.gh.repos["talkable/talkable"][i].ciUnknown = true
	}
	h.gh.mu.Unlock()
	details := h.gh.count("details:")
	h.setCI(2, "FAILURE", github.Check{Name: "completion", State: github.CheckFailed})
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := h.gh.count("details:") - details; n != 0 {
		t.Fatalf("details calls = %d, want 0", n)
	}
	if pr := h.pr(2); deref(pr.CIState) != "" || pr.CI.State != "" {
		t.Fatalf("CI = %v %+v", pr.CIState, pr.CI)
	}
}

func equalTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// TestRequiredChecksReadFromGitHub: the checks GitHub requires on a
// repository's default branch are read once on its first sync, reused for
// requiredChecksTTL, kept when a read fails (retried after
// requiredChecksRetry), and not read for a repository without open PRs.
func TestRequiredChecksReadFromGitHub(t *testing.T) {
	h := newHarness(t)
	h.gh.required = map[string][]string{"talkable/talkable": {"Completion"}}
	h.gh.set("zhuravel/empty")
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	reads := func() int { return h.gh.count("required:talkable/talkable@main") }
	required := func() store.RequiredChecks {
		t.Helper()
		r, err := h.st.RequiredChecks(h.ctx, "talkable/talkable", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if n := reads(); n != 1 {
		t.Fatalf("reads on the first sync = %d (calls %v)", n, h.gh.calls)
	}
	first := h.clock.Now()
	if r := required(); !r.FetchedAt.Equal(first) || // read back from the registry in UTC; the clock is local
		!reflect.DeepEqual(r.Checks, []string{"Completion"}) || r.Source != store.RequiredFromGitHub {
		t.Fatalf("required = %+v", r)
	}
	if !h.hasEvent("repo:talkable/talkable", "repo.required_checks") {
		t.Fatal("no repo.required_checks event")
	}
	if n := h.gh.count("required:zhuravel/empty"); n != 0 {
		t.Fatalf("a repository without open PRs was read %d times", n)
	}

	h.advance(time.Hour)
	h.tick()
	if n := reads(); n != 1 {
		t.Fatalf("reads within the TTL = %d", n)
	}

	h.gh.mu.Lock()
	h.gh.requiredErr = errors.New("boom")
	h.gh.mu.Unlock()
	h.advance(requiredChecksTTL)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if n := reads(); n != 2 {
		t.Fatalf("reads after the TTL = %d", n)
	}
	if r := required(); !slices.Equal(r.Checks, []string{"Completion"}) || !r.FetchedAt.Equal(first) {
		t.Fatalf("a failed read must keep the list: %+v", r)
	}
	if g, _, _ := h.st.GitHubRequiredChecks(h.ctx, "talkable/talkable"); !strings.Contains(g.Error, "boom") {
		t.Fatalf("cached = %+v", g)
	}
	if !h.hasEvent("repo:talkable/talkable", "poll.required_checks_error") {
		t.Fatal("no poll.required_checks_error event")
	}

	// Retried after requiredChecksRetry; GitHub no longer says.
	h.gh.mu.Lock()
	h.gh.requiredErr, h.gh.required = nil, nil
	h.gh.mu.Unlock()
	h.advance(requiredChecksRetry)
	h.tick()
	if n := reads(); n != 3 {
		t.Fatalf("reads after the retry delay = %d", n)
	}
	if r := required(); r.Source != "" || r.Checks != nil {
		t.Fatalf("unknown = %+v", r)
	}
	// The configuration overrides whatever GitHub says.
	r, err := h.st.RequiredChecks(h.ctx, "talkable/talkable", []string{"workflow:CI"})
	if err != nil || r.Source != store.RequiredFromConfig {
		t.Fatalf("configured = %+v, %v", r, err)
	}
}
