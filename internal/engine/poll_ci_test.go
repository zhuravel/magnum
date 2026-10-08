package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// TestCIChangeWithoutUpdatedAtShowsOnTheNextPoll: a CI run that ends does
// not move the PR's updatedAt and the radar carries no rollup; the poll's
// CI read (one call for the watched repository's open PRs) still shows the
// new state on the next poll.
func TestCIChangeWithoutUpdatedAtShowsOnTheNextPoll(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	updated := h.pr(2).GHUpdatedAt
	reads := h.gh.count("ci:")
	h.setCI(2, "PENDING", github.Check{Name: "rspec", State: github.CheckPending})
	h.advance(30 * time.Second)
	h.tick()
	pr := h.pr(2)
	if deref(pr.CIState) != "PENDING" || pr.CI == nil || pr.CI.State != "PENDING" || pr.CI.Pending != 1 {
		t.Fatalf("CI after the run started = %v %+v", pr.CIState, pr.CI)
	}
	if !equalTime(pr.GHUpdatedAt, updated) {
		t.Fatalf("updatedAt moved: %v -> %v", updated, pr.GHUpdatedAt)
	}
	if n := h.gh.count("ci:") - reads; n != 1 {
		t.Fatalf("CI reads in one poll = %d, want 1: %v", n, h.gh.calls)
	}
	h.setCI(2, "SUCCESS", github.Check{Name: "rspec", State: github.CheckPassed})
	h.advance(30 * time.Second)
	h.tick()
	if pr := h.pr(2); deref(pr.CIState) != "SUCCESS" || pr.CI.State != "SUCCESS" || pr.CI.Passed != 1 {
		t.Fatalf("CI after the run passed = %v %+v", pr.CIState, pr.CI)
	}
}

// TestCIReadAsksOnlyAboutWatchedRepositories: a repository of the owner
// that no watch includes is in the radar, but its PRs' CI is never read.
func TestCIReadAsksOnlyAboutWatchedRepositories(t *testing.T) {
	h := newHarness(t)
	h.gh.set("talkable/other", prSpec{n: 7, head: "o1", updated: h.clock.Now()})
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.startup()
	h.tick()
	got := callsWith(h.gh, "ci:")
	if len(got) != 1 || got[0] != "ci:PR_talkable/talkable_1,PR_talkable/talkable_2" {
		t.Fatalf("CI reads = %v, want the two watched PRs in one call", got)
	}
}

// TestCIReadFailureDoesNotFailThePoll: when the CI read fails the poll
// still applies the radar (a new PR is recorded), the stored CI is kept,
// nothing is fetched for CI, and the failure is one warning.
func TestCIReadFailureDoesNotFailThePoll(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.gh.mu.Lock()
	h.gh.ciErr = &github.APIError{Op: "ci states call 1", Status: 502}
	h.gh.repos["talkable/talkable"] = append(h.gh.repos["talkable/talkable"], prSpec{n: 3, head: "c1", updated: h.clock.Now()})
	h.gh.mu.Unlock()
	h.setCI(2, "FAILURE", github.Check{Name: "rspec", State: github.CheckFailed})
	details := h.gh.count("details:talkable/talkable:[2]")
	for range 3 {
		h.advance(30 * time.Second)
		if err := h.e.Tick(h.ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		h.settle()
	}
	if pr := h.pr(3); pr.HeadSHA != "c1" {
		t.Fatalf("new PR = %+v", pr)
	}
	if pr := h.pr(2); deref(pr.CIState) != "" || pr.CI == nil || pr.CI.State != "" {
		t.Fatalf("stored CI changed without a read: %v %+v", pr.CIState, pr.CI)
	}
	if n := h.gh.count("details:talkable/talkable:[2]") - details; n != 0 {
		t.Fatalf("details of #2 fetched %d times", n)
	}
	if n := h.eventCount("watch:talkable", "poll.ci_error"); n != 1 {
		t.Fatalf("poll.ci_error events = %d, want 1", n)
	}
}

// TestPollErrorLoggedOncePerErrorPerHour: a radar failing poll after poll
// is one warning per distinct error per hour, even when polls succeed in
// between.
func TestPollErrorLoggedOncePerErrorPerHour(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	e502 := &github.APIError{Op: "radar talkable page 1", Status: 502}
	e504 := &github.APIError{Op: "radar talkable page 1", Status: 504, Message: "We couldn't respond to your request in time."}
	poll := func(err error, wantEvents int) {
		t.Helper()
		h.gh.mu.Lock()
		h.gh.radarErr = err
		h.gh.mu.Unlock()
		h.advance(30 * time.Second)
		if tickErr := h.e.Tick(h.ctx); (tickErr != nil) != (err != nil) {
			t.Fatalf("tick error = %v, radar error %v", tickErr, err)
		}
		h.settle()
		if n := h.eventCount("watch:talkable", "poll.error"); n != wantEvents {
			t.Fatalf("poll.error events = %d, want %d", n, wantEvents)
		}
	}
	for range 5 {
		poll(e502, 1)
	}
	poll(nil, 1)
	poll(e502, 1) // the same error within the hour, after a good poll
	poll(e504, 2) // another error
	poll(e502, 2)
	h.advance(time.Hour)
	poll(e502, 2) // the tick after an hour without one follows a sleep: it records nothing
	poll(e502, 3) // an hour later
}

// TestTickErrorLoggedOncePerErrorPerHour: the daemon's "tick finished with
// errors" warning names each distinct error once an hour; a TCP address
// and port that change between occurrences do not make an error new.
func TestTickErrorLoggedOncePerErrorPerHour(t *testing.T) {
	var buf syncBuffer
	h := newHarness(t, func(h *harness) {
		h.d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	reset := func(port string) error {
		return errors.New(`poll talkable: Post "https://api.github.com/graphql": read tcp 10.0.0.2:` + port + `->140.82.121.6:443: read: connection reset by peer`)
	}
	logged := func() int { return strings.Count(buf.String(), "tick finished with errors") }
	for i, step := range []struct {
		err   error
		after time.Duration
		want  int
	}{
		{reset("61647"), 0, 1},
		{reset("61648"), 30 * time.Second, 1},
		{errors.New("poll talkable: github radar talkable page 1 (HTTP 502)"), 30 * time.Second, 2},
		{reset("61649"), 30 * time.Minute, 2},
		{reset("61650"), 31 * time.Minute, 3},
	} {
		h.advance(step.after)
		h.e.warnTick(step.err)
		if n := logged(); n != step.want {
			t.Fatalf("step %d: warnings = %d, want %d:\n%s", i, n, step.want, buf.String())
		}
	}
}

// eventCount counts subject's events of kind.
func (h *harness) eventCount(subject, kind string) int {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// syncBuffer is a bytes.Buffer the engine's goroutines can log to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
