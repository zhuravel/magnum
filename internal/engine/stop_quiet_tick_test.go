package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

// stoppedGH is a GitHub whose reads fail the way gh does when the daemon
// stops under it (the command is killed and the error says "context
// canceled") and, with a live context, the way a bad gateway does.
type stoppedGH struct{ GitHub }

func (stoppedGH) failure(ctx context.Context, what string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: gh api graphql --hostname github.com --input -: %w", what, context.Canceled)
	}
	return fmt.Errorf("%s: gh api graphql: HTTP 502", what)
}

func (g stoppedGH) Radar(ctx context.Context, org string) ([]github.RepoRadar, github.RateLimit, error) {
	return nil, github.RateLimit{}, g.failure(ctx, "github radar "+org+" page 1")
}

func (g stoppedGH) CIStates(ctx context.Context, _ []github.PRRadar) (map[string]github.CIState, github.RateLimit, error) {
	return nil, github.RateLimit{}, g.failure(ctx, "github ci states call 1")
}

// The daemon restarted 29 times in four days (a drain for each landing), and
// every stop that came mid-tick logged the cancelled calls as warnings: the
// Codex usage read, the auto approvals, the PRs that need you, the review
// machine's failures, the radar and CI reads, the startup's recovery and
// reclassification reads, the schema check and the notes' repository lists.
// A stop is no fault: those lines are debug, and the same failures with a
// live context still warn (TestAFailureThatIsNoStopStillWarns).
func TestAStopMidTickLogsNoWarning(t *testing.T) {
	h, buf := quietHarness(t, func(h *harness) {
		h.d.Usage = func(ctx context.Context, _ string, _ time.Time) (usage.Snapshot, error) {
			return usage.Snapshot{}, fmt.Errorf("read the codex sessions: %w", ctx.Err())
		}
	})
	live := h.e.d.GitHub
	h.e.d.GitHub = func(id string) GitHub {
		if g := live(id); g != nil {
			return stoppedGH{g}
		}
		return nil
	}
	stopped := stoppedContext(h)

	h.e.readBudget(stopped)
	h.e.autoApprove(stopped)
	h.e.noteNeedsMe(stopped)
	h.e.noteMachine(stopped)
	h.e.recoverRows(stopped)
	h.e.reclassifyIneligible(stopped)
	h.e.syncNotes(stopped, true)
	_ = h.e.Tick(stopped) // the schema check, the radar and CI reads

	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("a stop logged warnings:\n%s", strings.Join(w, "\n"))
	}
	if debugLines(buf, "codex usage") == 0 {
		t.Errorf("the cancelled usage read left no debug line:\n%s", buf.String())
	}

	// the stop did not use up the once-an-hour slot of the machine failures' line
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.e.noteMachine(h.ctx)
	if w := warnLines(buf); len(w) != 1 || !strings.Contains(w[0], "review machine failures") {
		t.Fatalf("a real failure after the stop logged %q, want its one warning", w)
	}
}

// The same reads failing for a reason of their own (the context is live)
// are the faults the warnings are for.
func TestAFailureThatIsNoStopStillWarns(t *testing.T) {
	h, buf := quietHarness(t, func(h *harness) {
		h.d.Usage = func(context.Context, string, time.Time) (usage.Snapshot, error) {
			return usage.Snapshot{}, fmt.Errorf("read the codex sessions: permission denied")
		}
	})
	h.e.readBudget(h.ctx)
	if w := warnLines(buf); len(w) != 1 || !strings.Contains(w[0], "codex usage") {
		t.Fatalf("a failed usage read logged %d warnings, want the one: %q", len(w), w)
	}

	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	h.e.noteNeedsMe(h.ctx)
	h.e.recoverRows(h.ctx)
	h.e.reclassifyIneligible(h.ctx)
	warned := strings.Join(warnLines(buf), "\n")
	for _, want := range []string{"prs that need you", "recovery: PRs", "baseline PRs", "waiting PRs", "ineligible PRs"} {
		if !strings.Contains(warned, want) {
			t.Errorf("a closed registry logged no warning %q:\n%s", want, warned)
		}
	}
}

// An approval whose post the stop cut short is neither recorded as failed
// nor announced as an event that says "could not approve ... context
// canceled; tried again in 5m": the row stays posting, the restart's pass
// finds it, and the log has no warning.
func TestAStopWhilePostingAnAutoApprovalLogsNoWarning(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.createErr = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}
	h.reviewedPR(2, "b1")
	h.tick() // the first post fails for good reason: failed, attempt 1
	if a := h.autoApproval(2); a.State != store.AutoFailed {
		t.Fatalf("approval = %+v", a)
	}
	h.gh.createErr = nil
	h.advance(autoApproveRetry)

	ctx, stop := context.WithCancel(h.ctx)
	live := h.e.d.GitHub
	h.e.d.GitHub = func(id string) GitHub {
		if live(id) != nil {
			return stopsOnReview{fakeGH: h.gh, stop: stop}
		}
		return nil
	}
	var buf syncBuffer // from here on: the stop's lines alone
	h.e.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.e.autoApprove(ctx)

	if ctx.Err() == nil {
		t.Fatal("the retry never posted: the test stopped nothing")
	}
	if w := warnLines(&buf); len(w) != 0 {
		t.Fatalf("a stop while posting logged warnings:\n%s", strings.Join(w, "\n"))
	}
	if n := len(h.events("review.auto_approve_failed")); n != 1 {
		t.Errorf("review.auto_approve_failed events = %d, want the first failure's alone (the stop is no failure)", n)
	}
}

// stopsOnReview stops the daemon (cancels its context) as the review is
// posted and fails the way the cut gh command does.
type stopsOnReview struct {
	*fakeGH
	stop context.CancelFunc
}

func (g stopsOnReview) CreateReview(_ context.Context, owner, repo string, number int, _, _, _ string) (github.RESTReview, error) {
	g.stop()
	return github.RESTReview{}, fmt.Errorf("github review %s/%s#%d (APPROVE): gh api: %w", owner, repo, number, context.Canceled)
}

// A radar or CI read the stop cut short is no poll failure: no poll.error or
// poll.ci_error event, and the failure it would have reported still comes
// once with the next live tick.
func TestAStopDuringThePollWritesNoPollErrorEvent(t *testing.T) {
	h, buf := quietHarness(t)
	live := h.e.d.GitHub
	h.e.d.GitHub = func(id string) GitHub {
		if g := live(id); g != nil {
			return stoppedGH{g}
		}
		return nil
	}
	_ = h.e.Tick(stoppedContext(h))
	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("warnings:\n%s", strings.Join(w, "\n"))
	}

	// the CI rollups the same
	w := h.cfg.Watches[0]
	repos := []github.RepoRadar{{PRs: []github.PRRadar{{NodeID: "PR_1", Number: 1}}}}
	h.e.readCI(stoppedContext(h), w, stoppedGH{h.gh}, repos, []*config.Watch{&w}, h.clock.Now())
	if w := warnLines(buf); len(w) != 0 {
		t.Fatalf("warnings after the CI read:\n%s", strings.Join(w, "\n"))
	}
	if n := len(h.events("poll.ci_error")); n != 0 {
		t.Fatalf("poll.ci_error events for a stopped read = %d", n)
	}

	// the next live ticks report the same failures once per watch, as they always did
	_ = h.e.Tick(h.ctx)
	_ = h.e.Tick(h.ctx)
	if n, want := len(h.events("poll.error")), len(h.cfg.Watches); n != want {
		t.Fatalf("poll.error events after two live ticks = %d, want one per watch (%d)", n, want)
	}
	h.e.readCI(h.ctx, w, stoppedGH{h.gh}, repos, []*config.Watch{&w}, h.clock.Now())
	if n := len(h.events("poll.ci_error")); n != 1 {
		t.Fatalf("poll.ci_error events for a live failure = %d, want 1", n)
	}
}
