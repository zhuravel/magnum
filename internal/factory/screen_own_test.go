package factory

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// own adds to magnum's registry an own open PR of example/talkable,
// number n, magnum's id id, at prHead, in a watch under autofix = "open",
// with its daemon up, as edit shapes it.
func (r *screenRig) own(id int64, n int, edit func(*PRState)) {
	r.t.Helper()
	v := PRState{Ref: PRRef{Repo: "example/talkable", Number: n}, ID: id, Title: "Own change " + strconv.Itoa(n),
		URL: "https://github.com/example/talkable/pull/" + strconv.Itoa(n), Found: true, DaemonUp: true, Watched: true, OpenWatch: true,
		GHState: store.GHOpen, Head: prHead, State: store.PRReviewed, AuthorLogin: "zhuravel", UpdatedAt: screenNow.Add(-time.Hour)}
	if edit != nil {
		edit(&v)
	}
	r.magnum.mu.Lock()
	defer r.magnum.mu.Unlock()
	r.magnum.own = append(r.magnum.own, v)
}

// prRows are the rows of your own PRs in d.
func prRows(d tui.FactoryData) []tui.FactoryRow {
	var out []tui.FactoryRow
	for _, r := range d.Rows {
		if r.PR != nil {
			out = append(out, r)
		}
	}
	return out
}

// Your own open PRs show on the factory screen, read from magnum's
// registry: each one row of its own in YOUR PRS, keyed by its number, with
// magnum's id negated so no task's row shares it, its verdict's words, its
// reference for the board's requests, and its page for b. Before, the
// screen read no PR of magnum's but the published tasks'.
func TestOwnPRsShowOnTheFactoryScreenReadOnly(t *testing.T) {
	r := newScreenRig(t)
	m := r.withMagnum()
	r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "coding", "", "")
	r.own(71, 4601, func(v *PRState) {
		v.Title = "Coupon cache warmup"
		v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "settling until 09:20: a reviewer can still be at work")
		v.Open.Since = screenNow.Add(-40 * time.Minute)
	})
	d := r.data(time.Time{})
	if m.ownReads != 1 {
		t.Errorf("ScreenData read your PRs %d times, want once", m.ownReads)
	}
	rows := prRows(d)
	if len(rows) != 1 {
		t.Fatalf("%d rows of your PRs, want 1: %+v", len(rows), d.Rows)
	}
	row := rows[0]
	want := tui.WaitsOn{Who: "reviewers", What: "settling until 09:20: a reviewer can still be at work", Short: "settling until 09:20"}
	switch {
	case row.ID != -71 || row.Key != "#4601" || row.GitHub != "#4601" || row.Title != "Coupon cache warmup":
		t.Errorf("the row of your PR: id %d, key %q, GitHub %q, title %q", row.ID, row.Key, row.GitHub, row.Title)
	case row.Group != tui.GroupYours || row.Mark != tui.MarkWaits || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want:
		t.Errorf("the row of your settling PR: %+v, want it in YOUR PRS with %+v", row, want)
	case row.PR.Ref != "example/talkable#4601" || row.PR.Fixing || row.PR.FixOff || row.PR.Off != "":
		t.Errorf("its PR: %+v", *row.PR)
	case !row.Since.Equal(screenNow.Add(-40*time.Minute)) || row.Move != "":
		t.Errorf("since %v, move %q: want since the verdict began and no move", row.Since, row.Move)
	}
	if c := d.Cards[-71].Task; c.PRURL != "https://github.com/example/talkable/pull/4601" || c.PR != "#4601" {
		t.Errorf("its card: %+v, want its page for b", c)
	}
	if d.Decisions != 0 {
		t.Errorf("%d decisions: a PR that waits owes none", d.Decisions)
	}
}

// An own PR whose verdict needs you, or that is idle and approved (your
// merge), sits in NEEDS YOU keyed by its number, with its issue key from
// the title; it adds no decision, which counts answers to tasks only. A
// stale verdict never does: magnum looks at the PR, in YOUR PRS.
func TestAnOwnPRThatNeedsYouIsInNeedsYou(t *testing.T) {
	r := newScreenRig(t)
	r.cfg.Board.Trackers = []string{"https://example.atlassian.net/browse/PR-{num}"}
	r.withMagnum()
	links := []string{"https://github.com/example/talkable/pull/4602#discussion_r1"}
	r.own(72, 4602, func(v *PRState) {
		v.Title = "[PR-12400] Coupon rounding"
		v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, "1 item needs you: 1 thread of a person")
		v.Open.Needs = []engine.OpenNeed{{Kind: "thread", URL: links[0]}}
	})
	r.own(73, 4603, func(v *PRState) {
		v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
	})
	r.own(74, 4604, func(v *PRState) {
		v.HasOpen, v.Stale, v.Open = true, true, verdict(engine.OpenNeedsYou, "1 item needs you")
	})
	r.own(75, 4605, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · no approval yet")
	})
	d := r.data(time.Time{})
	needs := rowOf(t, d, "#4602")
	if needs.Group != tui.GroupNeedsYou || needs.Mark != tui.MarkNeedsYou || needs.Jira != "PR-12400" || !needs.WaitsOn[0].OnYou {
		t.Errorf("an own PR that needs you: %+v, want NEEDS YOU with its issue key", needs)
	}
	if needs.Move == "" || !slices.Equal(d.Cards[-72].Task.Links, links) {
		t.Errorf("its move %q and card links %q: want a move and the links of what needs you", needs.Move, d.Cards[-72].Task.Links)
	}
	if merge := rowOf(t, d, "#4603"); merge.Group != tui.GroupNeedsYou || merge.Move != "merge #4603 on GitHub" {
		t.Errorf("an own PR idle and approved: %+v, want NEEDS YOU with your merge", merge)
	}
	if stale := rowOf(t, d, "#4604"); stale.Group != tui.GroupYours || stale.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "looking at #4604"}) {
		t.Errorf("an own PR with a stale verdict: %+v, want magnum looking at it in YOUR PRS", stale)
	}
	if idle := rowOf(t, d, "#4605"); idle.Group != tui.GroupYours || idle.WaitsOn[0].Who != "reviewers" {
		t.Errorf("an own PR idle without an approval: %+v, want it in YOUR PRS waiting on reviewers", idle)
	}
	if d.Decisions != 0 {
		t.Errorf("%d decisions: your PRs owe no answers", d.Decisions)
	}
}

// A PR that is a factory task's own stays that task's row and is not
// listed again; a stopped task's PR is your PR again.
func TestAFactoryPRIsListedOnceAsItsTask(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12320", 4530)
	r.pr(4530, func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") })
	r.own(70, 4530, func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") })
	stopped := r.publishedTask("PR-12399", 4599)
	r.move(stopped, registry.TaskStopped, "", "")
	r.own(79, 4599, func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") })
	d := r.data(time.Time{})
	if hasRow(d, "#4530") {
		t.Errorf("the task's PR is listed again: %+v", d.Rows)
	}
	if !hasRow(d, "PR-12320") {
		t.Errorf("the task's row is gone: %+v", d.Rows)
	}
	if !hasRow(d, "#4599") {
		t.Errorf("a stopped task's open PR is not listed as yours: %+v", d.Rows)
	}
}

// A PR magnum's look leaves alone (a baseline, fix off) waits in YOUR PRS
// with its reason and asks nothing, never in NEEDS YOU: a baseline open
// for weeks would wear that group out. Fix off says that F turns it on.
func TestAnOffOwnPRWaitsInYourPRs(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(76, 4606, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenOff, "baseline")
		v.Open.Off = engine.OpenOffBaseline
	})
	r.own(77, 4607, func(v *PRState) {
		v.HasOpen, v.FixOff, v.Open = true, true, verdict(engine.OpenOff, "fix off")
		v.Open.Off = engine.OpenOffFixOff
	})
	d := r.data(time.Time{})
	base := rowOf(t, d, "#4606")
	if base.Group != tui.GroupYours || base.PR.Off == "" || !strings.Contains(base.WaitsOn[0].What, "open before magnum first synced") {
		t.Errorf("a baseline PR: %+v, want it in YOUR PRS with its reason", base)
	}
	if base.Move != "" || base.Mark == tui.MarkNeedsYou {
		t.Errorf("a baseline PR asks something: move %q, mark %v", base.Move, base.Mark)
	}
	off := rowOf(t, d, "#4607")
	if off.Group != tui.GroupYours || !off.PR.FixOff || off.WaitsOn[0].What != "fix is off for #4607 (F turns it on)" {
		t.Errorf("a PR with fix off: %+v (%+v)", off, off.PR)
	}
}

// Your PRs wait in YOUR PRS oldest first, a fix loop at work moves, and a
// PR with no verdict yet under autofix = "open" is magnum looking.
func TestYourPRsWaitOldestFirst(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(81, 4611, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "CI at a1b2c3d")
		v.Open.Since = screenNow.Add(-10 * time.Minute)
	})
	r.own(82, 4612, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 1 of 5")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 5, Phase: engine.FixPhaseFixing, Head: prHead}
		v.Open.Since = screenNow.Add(-3 * time.Hour)
	})
	r.own(83, 4613, nil)
	d := r.data(time.Time{})
	var keys []string
	for _, row := range d.Rows {
		if row.Group == tui.GroupYours {
			keys = append(keys, row.Key)
		}
	}
	if !slices.Equal(keys, []string{"#4612", "#4613", "#4611"}) {
		t.Errorf("YOUR PRS in the order %q, want the oldest wait first", keys)
	}
	fixing := rowOf(t, d, "#4612")
	if fixing.Mark != tui.MarkWorking || !fixing.PR.Fixing || fixing.WaitsOn[0].What != "magnum fixes #4612 (cycle 1 of 5)" {
		t.Errorf("an own PR in a fix loop: %+v (%+v)", fixing, fixing.PR)
	}
	if looking := rowOf(t, d, "#4613"); looking.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "looking at #4613"}) {
		t.Errorf("an own PR with no verdict yet: %+v", looking)
	}
}

// A registry of magnum's that cannot be read leaves your PRs out and says
// so on home's blocker line; the tasks are drawn.
func TestYourPRsNotReadSaySo(t *testing.T) {
	r := newScreenRig(t)
	m := r.withMagnum()
	r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "coding", "", "")
	m.ownErr = errors.New("magnum's registry is at schema 80 and this build needs 81")
	d := r.data(time.Time{})
	if len(prRows(d)) != 0 || !hasRow(d, "PR-12330") {
		t.Errorf("rows %+v: want the task and no PR", d.Rows)
	}
	if !slices.Contains(d.Blocking, "your PRs not read") || !strings.Contains(strings.Join(d.BlockingLong, " "), "schema 80") {
		t.Errorf("blocking %q / %q: want your PRs not read, with why", d.Blocking, d.BlockingLong)
	}
}

// Without magnum (no Magnum), the screen reads no PR of yours.
func TestWithoutMagnumNoPRsOfYoursShow(t *testing.T) {
	r := newScreenRig(t)
	r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "coding", "", "")
	if rows := prRows(r.data(time.Time{})); len(rows) != 0 {
		t.Errorf("rows of your PRs without magnum: %+v", rows)
	}
}
