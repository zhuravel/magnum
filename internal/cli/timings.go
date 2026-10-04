package cli

// Stage timings of a PR's last review round, for the PR board's card and
// `magnum status <ref>`: fetch/checkout from the slot's step events, each
// role's runs from submitted to ended, the judge's verification and the
// total, all read from the registry.

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// timingCheckoutWindow bounds how long before a round's first prompt its
// checkout may have ended; an older checkout belongs to an earlier round.
const timingCheckoutWindow = time.Hour

// timingSteps is how many of a PR's newest checkout step events are read.
const timingSteps = 200

// timingStageCheckout names the checkout stage.
const timingStageCheckout = "fetch/checkout"

// checkoutSpan is one checkout's step events: from the first begin to the
// last ok or fail.
type checkoutSpan struct {
	start, end time.Time
	failed     bool
	found      bool
}

// lastCheckout finds the checkout that ended last at or before before, in
// steps (oldest first): the step events of that subject since its latest
// step reset. A checkout that ended more than timingCheckoutWindow before
// before is not the round's.
func lastCheckout(steps []store.Event, before time.Time) checkoutSpan {
	subject := ""
	last := -1
	for i := len(steps) - 1; i >= 0; i-- {
		e := steps[i]
		if e.Kind == store.KindStep && e.Subject != nil && !e.At.After(before) {
			subject, last = *e.Subject, i
			break
		}
	}
	if last < 0 {
		return checkoutSpan{}
	}
	var sp checkoutSpan
	for i := last; i >= 0; i-- {
		e := steps[i]
		if e.Subject == nil || *e.Subject != subject {
			continue
		}
		if e.Kind == store.KindStepReset {
			break
		}
		switch store.Deref(e.Phase) {
		case store.PhaseBegin:
			sp.start = e.At
		case store.PhaseOK, store.PhaseFail:
			if sp.end.IsZero() {
				sp.end, sp.failed = e.At, store.Deref(e.Phase) == store.PhaseFail
			}
		}
	}
	if sp.start.IsZero() || sp.end.IsZero() || before.Sub(sp.end) > timingCheckoutWindow {
		return checkoutSpan{}
	}
	sp.found = true
	return sp
}

// roundStart is when the round's first run was created; zero without runs.
func roundStart(runs []store.Run) time.Time {
	var t time.Time
	for _, r := range runs {
		if t.IsZero() || r.CreatedAt.Before(t) {
			t = r.CreatedAt
		}
	}
	return t
}

// runActive reports whether a run is still waiting for or in its turn.
func runActive(r store.Run) bool {
	return slices.Contains([]string{store.RunPending, store.RunSubmitted, store.RunWorking}, r.State)
}

// roundTimings computes the stages of a round from its runs (one round,
// any order) and the checkout before it: fetch/checkout, each role from its
// first submission (or creation) to its last end in the order the roles
// started (the judge last), the judge's verification (its run's end to the
// verification) and the total. A stage still in flight counts to now. Nil
// without runs.
func roundTimings(runs []store.Run, co checkoutSpan, isJudge func(string) bool, now time.Time) *tui.RoundTimings {
	if len(runs) == 0 {
		return nil
	}
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b store.Run) int { return a.CreatedAt.Compare(b.CreatedAt) })
	t := &tui.RoundTimings{Round: runs[0].Round, Kind: runs[0].Kind}
	first, last := runs[0].CreatedAt, time.Time{}
	if co.found {
		t.Stages = append(t.Stages, tui.StageTiming{Name: timingStageCheckout, Duration: co.end.Sub(co.start), Failed: co.failed})
		first = co.start
		last = co.end
	}

	type roleSpan struct {
		name       string
		start, end time.Time
		running    bool
		lastState  string
		judge      bool
		lastRun    store.Run
	}
	var roles []*roleSpan
	byRole := map[string]*roleSpan{}
	for _, r := range runs {
		rs := byRole[r.Role]
		if rs == nil {
			rs = &roleSpan{name: actRoleName(r.Role), judge: isJudge != nil && isJudge(r.Role)}
			byRole[r.Role] = rs
			roles = append(roles, rs)
		}
		start := r.CreatedAt
		if r.SubmittedAt != nil {
			start = *r.SubmittedAt
		}
		if rs.start.IsZero() || start.Before(rs.start) {
			rs.start = start
		}
		if r.EndedAt != nil && r.EndedAt.After(rs.end) {
			rs.end = *r.EndedAt
		}
		rs.running = rs.running || runActive(r)
		rs.lastState, rs.lastRun = r.State, r
		for _, at := range []*time.Time{r.EndedAt, r.VerifiedAt} {
			if at != nil && at.After(last) {
				last = *at
			}
		}
		t.Running = t.Running || runActive(r) || r.State == store.RunEnded // ended: its report or review not collected yet
	}
	slices.SortStableFunc(roles, func(a, b *roleSpan) int {
		if a.judge != b.judge {
			if a.judge {
				return 1
			}
			return -1
		}
		return 0
	})
	var verify *tui.StageTiming
	for _, rs := range roles {
		st := tui.StageTiming{Name: rs.name, Running: rs.running,
			Failed: !rs.running && slices.Contains([]string{store.RunFailed, store.RunAbandoned}, rs.lastState)}
		switch {
		case rs.running:
			st.Duration = now.Sub(rs.start)
		case !rs.end.IsZero():
			st.Duration = rs.end.Sub(rs.start)
		}
		t.Stages = append(t.Stages, st)
		if !rs.judge {
			continue
		}
		j := rs.lastRun
		switch {
		case j.EndedAt != nil && j.VerifiedAt != nil:
			verify = &tui.StageTiming{Name: "verify", Duration: j.VerifiedAt.Sub(*j.EndedAt)}
		case j.EndedAt != nil && j.State == store.RunEnded:
			verify = &tui.StageTiming{Name: "verify", Duration: now.Sub(*j.EndedAt), Running: true}
		}
	}
	if verify != nil {
		verify.Duration = max(verify.Duration, 0)
		t.Stages = append(t.Stages, *verify)
	}
	for i := range t.Stages {
		t.Stages[i].Duration = max(t.Stages[i].Duration, 0)
	}
	switch {
	case t.Running:
		t.Total = max(now.Sub(first), 0)
	case !last.IsZero():
		t.Total = max(last.Sub(first), 0)
	}
	return t
}

// roundTimingsFor reads a PR's last round from the registry and computes
// its timings; nil when no round ran. runs are the PR's runs when the
// caller has them (any rounds; nil reads the latest round's).
func roundTimingsFor(ctx context.Context, st *store.Store, cfg *config.Config, prID int64, runs []store.Run, now time.Time) (*tui.RoundTimings, error) {
	if runs == nil {
		byPR, err := st.LatestRoundRuns(ctx, prID)
		if err != nil {
			return nil, err
		}
		runs = byPR[prID]
	} else {
		runs = latestRound(runs)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	co, err := roundCheckout(ctx, st, prID, runs)
	if err != nil {
		return nil, err
	}
	return roundTimings(runs, co, func(role string) bool { return actIsJudge(cfg, role) }, now), nil
}

// latestRound keeps the runs of the highest round.
func latestRound(runs []store.Run) []store.Run {
	top := 0
	for _, r := range runs {
		top = max(top, r.Round)
	}
	var out []store.Run
	for _, r := range runs {
		if r.Round == top {
			out = append(out, r)
		}
	}
	return out
}

// roundCheckout is the checkout before a round's first run; a continue
// round reuses the paused round's checkout and has none.
func roundCheckout(ctx context.Context, st *store.Store, prID int64, runs []store.Run) (checkoutSpan, error) {
	start := roundStart(runs)
	if start.IsZero() || !slices.ContainsFunc(runs, func(r store.Run) bool { return r.Kind != store.RunContinue && r.Kind != store.RunNudge }) {
		return checkoutSpan{}, nil
	}
	steps, err := st.CheckoutSteps(ctx, prID, timingSteps)
	if err != nil {
		return checkoutSpan{}, err
	}
	return lastCheckout(steps, start), nil
}

// boardTimings fills the board rows' last-round timings: one query for the
// latest round of every row's PR, and each round's checkout read once (a
// round's checkout never changes once its first run exists).
type boardTimings struct {
	mu        sync.Mutex
	checkouts map[string]checkoutSpan // by the round's first run id
}

// fill sets LastRound on rows (ids[i] is rows[i]'s PR id).
func (b *boardTimings) fill(ctx context.Context, st *store.Store, cfg *config.Config, ids []int64, rows []tui.PRBoardRow, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	byPR, err := st.LatestRoundRuns(ctx, ids...)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	next := make(map[string]checkoutSpan, len(byPR)) // only the rounds still listed stay cached
	defer func() { b.checkouts = next }()
	isJudge := func(role string) bool { return actIsJudge(cfg, role) }
	for i, id := range ids {
		runs := byPR[id]
		if len(runs) == 0 {
			continue
		}
		key := runs[0].ID
		co, ok := b.checkouts[key]
		if !ok {
			// A PR whose checkout cannot be read keeps its other stages; the
			// next load tries again.
			if c, err := roundCheckout(ctx, st, id, runs); err == nil {
				co, ok = c, true
			}
		}
		if ok {
			next[key] = co
		}
		rows[i].LastRound = roundTimings(runs, co, isJudge, now)
	}
	return nil
}

// statusRenderTimings prints the last round's stage timings under the
// review history of `magnum status <ref>`.
func statusRenderTimings(w io.Writer, t *tui.RoundTimings) {
	if t == nil {
		return
	}
	head := fmt.Sprintf("round %d", t.Round)
	if t.Kind != "" {
		head += " " + t.Kind
	}
	fmt.Fprintf(w, "  timings:   %s: %s\n", statusSafe(head, 0), statusSafe(tui.TimingsText(*t), 0))
}
