package factory

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
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

// prRows are the rows of your own PRs in d (a task's row may carry its PR
// too: its id is the task's).
func prRows(d tui.FactoryData) []tui.FactoryRow {
	var out []tui.FactoryRow
	for _, r := range d.Rows {
		if r.PR != nil && r.ID < 0 {
			out = append(out, r)
		}
	}
	return out
}

// Your own open PRs show on the factory screen, read from magnum's
// registry: each one row of its own, in the group its wait says (WAITS ON
// OTHERS for one that waits on its reviewers), keyed by its number, with
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
	case row.Group != tui.GroupWaits || row.Mark != tui.MarkWaits || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want:
		t.Errorf("the row of your settling PR: %+v, want it in WAITS ON OTHERS with %+v", row, want)
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
// stale verdict never does: magnum looks at the PR, in WAITS ON OTHERS.
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
	if stale := rowOf(t, d, "#4604"); stale.Group != tui.GroupWaits || stale.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "looking at #4604"}) {
		t.Errorf("an own PR with a stale verdict: %+v, want magnum looking at it in WAITS ON OTHERS", stale)
	}
	if idle := rowOf(t, d, "#4605"); idle.Group != tui.GroupWaits || idle.WaitsOn[0].Who != "reviewers" {
		t.Errorf("an own PR idle without an approval: %+v, want it in WAITS ON OTHERS waiting on reviewers", idle)
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

// A PR magnum's look leaves alone (a baseline, fix off) waits in LEFT ALONE
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
		t.Errorf("a baseline PR: %+v, want it in LEFT ALONE with its reason", base)
	}
	if base.Move != "" || base.Mark == tui.MarkNeedsYou {
		t.Errorf("a baseline PR asks something: move %q, mark %v", base.Move, base.Mark)
	}
	off := rowOf(t, d, "#4607")
	if off.Group != tui.GroupYours || !off.PR.FixOff || off.WaitsOn[0].What != "fix is off for #4607 (F turns it on)" {
		t.Errorf("a PR with fix off: %+v (%+v)", off, off.PR)
	}
}

// Your PRs that wait on others sit in WAITS ON OTHERS, with the published
// tasks whose PRs wait alike: a fix loop at work moves, and a PR with no
// verdict yet under autofix = "open" is magnum looking. Before, they sat in
// YOUR PRS, so one state was in two groups.
func TestYourPRsThatWaitOnOthersSitInWaitsOnOthers(t *testing.T) {
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
	r.own(83, 4613, func(v *PRState) { v.UpdatedAt = screenNow.Add(-time.Minute) })
	d := r.data(time.Time{})
	for _, key := range []string{"#4611", "#4612", "#4613"} {
		if row := rowOf(t, d, key); row.Group != tui.GroupWaits {
			t.Errorf("%s in group %d, want WAITS ON OTHERS", key, row.Group)
		}
	}
	fixing := rowOf(t, d, "#4612")
	if fixing.Mark != tui.MarkWorking || !fixing.PR.Fixing || fixing.WaitsOn[0].What != "magnum fixes #4612 (cycle 1 of 5)" {
		t.Errorf("an own PR in a fix loop: %+v (%+v)", fixing, fixing.PR)
	}
	if looking := rowOf(t, d, "#4613"); looking.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "looking at #4613"}) {
		t.Errorf("an own PR with no verdict yet: %+v", looking)
	}
}

// Only your PRs magnum's look leaves alone keep a group of their own, oldest
// first: they ask nothing and nothing works on them, so a short screen
// folds them first (the group comes after WAITS ON OTHERS and folds from its
// end); the ones at work wait on others.
func TestOnlyThePRsMagnumLeavesAloneKeepAGroupOfTheirOwn(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(76, 4606, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenOff, "baseline")
		v.Open.Off, v.Open.Since = engine.OpenOffBaseline, screenNow.Add(-26*time.Hour)
	})
	r.own(77, 4607, func(v *PRState) {
		v.HasOpen, v.FixOff, v.Open = true, true, verdict(engine.OpenOff, "fix off")
		v.Open.Off, v.Open.Since = engine.OpenOffFixOff, screenNow.Add(-5*time.Hour)
	})
	r.own(81, 4611, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "CI at a1b2c3d")
		v.Open.Since = screenNow.Add(-10 * time.Minute)
	})
	r.own(82, 4612, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 1 of 5")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 5, Phase: engine.FixPhaseFixing, Head: prHead}
		v.Open.Since = screenNow.Add(-3 * time.Hour)
	})
	var alone, waits []string
	for _, row := range r.data(time.Time{}).Rows {
		switch row.Group {
		case tui.GroupYours:
			alone = append(alone, row.Key)
		case tui.GroupWaits:
			waits = append(waits, row.Key)
		}
	}
	if want := []string{"#4606", "#4607"}; !slices.Equal(alone, want) {
		t.Errorf("the PRs magnum leaves alone: %q, want %q, oldest first", alone, want)
	}
	if want := []string{"#4612", "#4611"}; !slices.Equal(waits, want) { // the loop's row waits since an hour, CI's 10 minutes
		t.Errorf("WAITS ON OTHERS: %q, want %q, oldest first", waits, want)
	}
}

// With two repositories in the factory, your PR's key names its
// repository (talkable#12, widget#12), so two PRs of one number read apart
// in the y/N and to a double click; on home its row names the repository
// and its GitHub cell the number, #12. A factory of one repository keeps
// the bare #12. Before, both read #12.
func TestYourPRsOfTwoFactoryRepositoriesNameTheirRepository(t *testing.T) {
	r := newScreenRig(t)
	r.cfg.Factory.Repos = append(r.cfg.Factory.Repos, config.FactoryRepo{Repo: "example/widget", Base: "main", MainClone: t.TempDir()})
	r.withMagnum()
	r.own(91, 12, nil)
	r.own(92, 12, func(v *PRState) {
		v.Ref.Repo, v.URL = "example/widget", "https://github.com/example/widget/pull/12"
	})
	d := r.data(time.Time{})
	for id, want := range map[int64]string{-91: "talkable#12", -92: "widget#12"} {
		row, ok := rowByID(d, id)
		repo, _, _ := strings.Cut(want, "#")
		switch {
		case !ok:
			t.Errorf("no row of your PR %d: %+v", id, d.Rows)
		case row.Key != want || d.Cards[id].Task.Key != want:
			t.Errorf("your PR %d: key %q, card %q; want %q", id, row.Key, d.Cards[id].Task.Key, want)
		case row.GitHub != "#12" || row.Repo != "example/"+repo:
			t.Errorf("your PR %d: GitHub %q, repository %q; want #12 of example/%s", id, row.GitHub, row.Repo, repo)
		}
	}
}

// rowByID is d's row of id.
func rowByID(d tui.FactoryData, id int64) (tui.FactoryRow, bool) {
	i := slices.IndexFunc(d.Rows, func(r tui.FactoryRow) bool { return r.ID == id })
	if i < 0 {
		return tui.FactoryRow{}, false
	}
	return d.Rows[i], true
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

// A muted own PR, which magnum's look leaves alone, says so and how the
// mute is lifted, not that magnum does not review it.
func TestAMutedOwnPRSaysItIsMuted(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(78, 4608, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenOff, "muted")
		v.Open.Off = engine.OpenOffMuted
	})
	row := rowOf(t, r.data(time.Time{}), "#4608")
	if row.Group != tui.GroupYours || row.PR.Off == "" || row.WaitsOn[0].What != "#4608 is muted: U on the board unmutes it" {
		t.Errorf("a muted PR: %+v (%+v), want it dim in LEFT ALONE with the way to unmute it", row, row.PR)
	}
}

// An own PR that needs your attention, or where magnum stopped arguing, is
// in NEEDS YOU with the attention's words whatever its verdict says or
// whether it has one: fix off (F) does not hide an attention, a verdict
// decided before the attention is stale, and a watch without autofix =
// "open" has no verdict. A loop at work on it (f on a PR that needs
// attention) moves, in WAITS ON OTHERS. Before, all of these sat in YOUR PRS and
// the chip counted them at work.
func TestAnOwnPRThatNeedsAttentionIsInNeedsYouWhateverItsVerdict(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	attn := func(v *PRState) {
		v.State, v.Attention, v.AttentionMsg = store.PRNeedsAttention, "fix", "fix: 2 findings need you"
	}
	r.own(91, 4701, func(v *PRState) { v.OpenWatch = false; attn(v) })
	r.own(92, 4702, func(v *PRState) {
		attn(v)
		v.FixOff, v.HasOpen, v.Open = true, true, verdict(engine.OpenOff, "fix is off (F)")
		v.Open.Off = engine.OpenOffFixOff
	})
	r.own(93, 4703, func(v *PRState) {
		attn(v)
		v.HasOpen, v.Stale, v.Open = true, true, verdict(engine.OpenWaits, "settling until 09:20")
	})
	r.own(94, 4704, func(v *PRState) { v.OpenWatch, v.ReviewedSHA, v.Stalemate = false, prHead, true })
	r.own(95, 4705, func(v *PRState) {
		attn(v)
		v.OpenWatch, v.Fixing, v.Fix = false, true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead}
	})
	d := r.data(time.Time{})
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum: fix: 2 findings need you", Short: "magnum: fix", OnYou: true}
	for _, key := range []string{"#4701", "#4702", "#4703"} {
		row := rowOf(t, d, key)
		if row.Group != tui.GroupNeedsYou || row.Mark != tui.MarkNeedsYou || row.WaitsOn[0] != want || row.Move == "" || row.PR.Off != "" {
			t.Errorf("%s: %+v (%+v), want NEEDS YOU with %+v and a move", key, row, row.PR, want)
		}
	}
	if !rowOf(t, d, "#4702").PR.FixOff {
		t.Error("#4702 lost its fix off")
	}
	if row := rowOf(t, d, "#4704"); row.Group != tui.GroupNeedsYou || !strings.HasPrefix(row.WaitsOn[0].What, "magnum stopped arguing on #4704") {
		t.Errorf("a stalemate: %+v, want NEEDS YOU", row)
	}
	if row := rowOf(t, d, "#4705"); row.Group != tui.GroupWaits || row.Mark != tui.MarkWorking || !row.PR.Fixing {
		t.Errorf("a loop at work on a PR that needs attention: %+v (%+v), want it moving in WAITS ON OTHERS", row, row.PR)
	}
}

// Without a verdict (the watch's autofix is not "open") an own PR says
// what magnum did: what its review of the head posted and whom it waits
// on next (never a bare "reviewed"), and one
// magnum does not review (include_own = false, a mute, a baseline) is dim
// with why, as one the look leaves alone is, and asks nothing. Before,
// each said "review of #N".
func TestAnOwnPRWithoutAVerdictSaysWhatMagnumDid(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(96, 4711, func(v *PRState) { v.OpenWatch, v.ReviewedSHA, v.ReviewEvent = false, prHead, "COMMENTED" })
	r.own(97, 4712, func(v *PRState) { v.OpenWatch, v.State = false, store.PRIneligible })
	r.own(98, 4713, func(v *PRState) { v.OpenWatch, v.State = false, store.PRBaseline })
	d := r.data(time.Time{})
	if row := rowOf(t, d, "#4711"); row.Group != tui.GroupWaits || row.PR.Off != "" ||
		row.WaitsOn[0] != (tui.WaitsOn{Who: "reviewers", What: "magnum commented at a1b2c3d · no approval yet", Short: "no approval yet"}) {
		t.Errorf("an own PR magnum reviewed at its head: %+v (%+v)", row, row.PR)
	}
	for key, want := range map[string]string{"#4712": "#4712 is yours to follow: magnum does not review it",
		"#4713": "#4713 is yours to follow: it was open before magnum first synced the repository"} {
		row := rowOf(t, d, key)
		if row.Group != tui.GroupYours || row.PR.Off != want || row.WaitsOn[0].What != want || row.Move != "" {
			t.Errorf("%s: %+v (%+v), want it dim in LEFT ALONE: %q", key, row, row.PR, want)
		}
	}
}

// A loop that f started writes magnum's fix row at once, and the look
// writes fixing only at its next tick: until then the verdict is still
// fresh. The row moves with the loop, never in NEEDS YOU as "merge #N on
// GitHub", a merge that would race the fixer's push.
func TestAnOwnPRWhoseLoopStartedAfterItsVerdictMoves(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(99, 4721, func(v *PRState) {
		v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead}
	})
	row := rowOf(t, r.data(time.Time{}), "#4721")
	if row.Group != tui.GroupWaits || row.Mark != tui.MarkWorking || !row.PR.Fixing || row.Move != "" ||
		row.WaitsOn[0].What != "magnum fixes #4721 (cycle 1 of 3)" {
		t.Errorf("an own PR whose loop started after its idle verdict: %+v (%+v), want it moving in WAITS ON OTHERS", row, row.PR)
	}
}

// Home names every repository the factory has, in its config's order, never
// the first task's alone; and every row and its card name its own: a
// task's, a published task's by its PR, and your PR's, whose number is its
// GitHub cell while its key keeps naming the repository for the y/N.
func TestScreenDataNamesTheRepositoryOfEveryRow(t *testing.T) {
	r := newScreenRig(t)
	r.cfg.Factory.Repos = append(r.cfg.Factory.Repos, config.FactoryRepo{Repo: "example/widget", Base: "main", MainClone: t.TempDir()})
	r.withMagnum()
	r.at(3 * time.Hour)
	widget, err := r.reg.CreateTask(r.ctx, registry.Task{Kind: "bug", Repo: "example/widget", Title: "offer page link", Prompt: "fix the offer page link",
		State: registry.TaskActive, Stage: "coding", Branch: "zhuravel-offer-link"})
	if err != nil {
		t.Fatal(err)
	}
	r.at(2 * time.Hour)
	talkable := r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "coding", "", "")
	published := r.publishedTask("PR-12320", 4530)
	r.own(91, 12, func(v *PRState) {
		v.Ref.Repo, v.URL = "example/helm", "https://github.com/example/helm/pull/12"
	})
	d := r.data(time.Time{})
	if want := []string{"example/talkable", "example/widget"}; !slices.Equal(d.Repos, want) {
		t.Errorf("home's repositories %q, want %q", d.Repos, want)
	}
	for id, want := range map[int64]string{widget.ID: "example/widget", talkable.ID: "example/talkable",
		published.ID: "example/talkable", -91: "example/helm"} {
		if row, ok := rowByID(d, id); !ok || row.Repo != want {
			t.Errorf("row %d: %+v, want repository %q", id, row, want)
		}
		if got := d.Cards[id].Task.Repo; got != want {
			t.Errorf("the card of row %d names repository %q, want %q", id, got, want)
		}
	}
	if row, _ := rowByID(d, -91); row.Key != "helm#12" || row.GitHub != "#12" {
		t.Errorf("your PR's row: key %q, GitHub %q; want helm#12 and #12", row.Key, row.GitHub)
	}
}

// prData is a PR's CI, magnum's last review and its people's reviews as
// PRStates reads them: CI green, P1 P2×3 at the head, one approval of the
// head and one of an older commit, one changes request.
func prData(v *PRState) {
	v.CI = &tui.CIInfo{State: "passed", Total: 3, Passed: 3}
	v.Review = &store.ReviewSummary{SHA: prHead, Counts: [4]int{0, 1, 3, 0}, Verdict: store.VerdictBlocking, Event: "COMMENT"}
	v.Findings = &tui.FindingsInfo{Counts: [4]int{0, 1, 3, 0}, Verdict: store.VerdictBlocking, SHA: prHead}
	v.Gate = &store.ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []store.LatestReview{
		{Login: "rev-ann", State: "APPROVED", CommitSHA: prHead}, {Login: "rev-bob", State: "APPROVED", CommitSHA: "0ld"},
		{Login: "rev-cid", State: "CHANGES_REQUESTED", CommitSHA: prHead}, {Login: "rev-dee", State: "DISMISSED", CommitSHA: prHead}}}
}

// The row of your PR and of a published task carry their PR's CI, magnum's
// findings and the people's reviews for the CI, FINDINGS and APPR columns,
// as the board's columns read them; a task without a PR carries none.
// Before, a row said none of them.
func TestPRRowsCarryCIFindingsAndApprovals(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12320", 4530)
	r.pr(4530, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "CI at a1b2c3d")
		prData(v)
	})
	r.task("PR-12330", "bug", "Advocate offer email locale", registry.TaskActive, "coding", "", "")
	r.own(71, 4601, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "CI at a1b2c3d")
		prData(v)
	})
	d := r.data(time.Time{})
	want := tui.FactoryApprovals{Approved: 2, Changes: 1, Stale: 1, Decision: "CHANGES_REQUESTED"}
	for _, key := range []string{"PR-12320", "#4601"} {
		row := rowOf(t, d, key)
		switch {
		case row.CI == nil || row.CI.State != "passed" || row.CI.Total != 3:
			t.Errorf("%s: CI %+v", key, row.CI)
		case row.Findings == nil || row.Findings.Counts != [4]int{0, 1, 3, 0}:
			t.Errorf("%s: findings %+v", key, row.Findings)
		case row.Approvals == nil || *row.Approvals != want:
			t.Errorf("%s: approvals %+v, want %+v", key, row.Approvals, want)
		}
	}
	if row := rowOf(t, d, "PR-12330"); row.CI != nil || row.Findings != nil || row.Approvals != nil {
		t.Errorf("a task without a PR: CI %+v, findings %+v, approvals %+v", row.CI, row.Findings, row.Approvals)
	}
}

// Your PR's WAITED counts from its last activity on GitHub when no verdict
// says since when it waits, never from the last write of magnum's row
// (any write moves it, which made a baseline open since August read 17d).
func TestAnOwnPRWithoutAVerdictWaitsSinceItsLastActivity(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	active := screenNow.Add(-50 * 24 * time.Hour)
	r.own(98, 4713, func(v *PRState) {
		v.OpenWatch, v.State, v.ActivityAt = false, store.PRBaseline, active
		v.UpdatedAt = screenNow.Add(-17 * 24 * time.Hour)
	})
	if row := rowOf(t, r.data(time.Time{}), "#4713"); !row.Since.Equal(active) {
		t.Errorf("a baseline's row waits since %v, want its last activity %v", row.Since, active)
	}
}

// A baseline or a PR magnum does not review is left alone without a fresh
// verdict under autofix = "open" too: no review comes, so nothing is at
// work on it. Before, a verdict still to come made a baseline "magnum:
// looking at" and the chip counted it at work.
func TestABaselineWithoutAFreshVerdictIsLeftAlone(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.own(98, 4713, func(v *PRState) { v.State = store.PRBaseline })
	r.own(99, 4714, func(v *PRState) {
		v.State, v.HasOpen, v.Stale = store.PRIneligible, true, true
		v.Open = verdict(engine.OpenWaits, "magnum reviews a1b2c3d")
	})
	d := r.data(time.Time{})
	for _, key := range []string{"#4713", "#4714"} {
		if row := rowOf(t, d, key); row.Group != tui.GroupYours || row.PR.Off == "" {
			t.Errorf("%s: group %d, %+v (%+v), want it left alone", key, row.Group, row.WaitsOn, row.PR)
		}
	}
}

// Your PR's card, and a published task's, says what the PR is (CardPR):
// its state, head, branches and last activity; its CI; magnum's last review
// with what it kept open of the earlier findings; the people's reviews,
// whom it asks and how long ago; the replies magnum's judge has not read;
// and magnum's fix loop. Before, your PR's row had no card, and a task's
// card said none of it.
func TestThePRCardSaysWhatThePRIs(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	edit := func(v *PRState) {
		prData(v)
		v.HeadRef, v.BaseRef, v.ActivityAt = "zhuravel-coupon-rounding", "master", screenNow.Add(-50*time.Minute)
		v.Review.At, v.Review.Fixed, v.Review.Open = screenNow.Add(-40*time.Minute), 1, 2
		v.Asked = []ReviewAsk{{Who: "rev-eve", At: screenNow.Add(-2 * time.Hour)}}
		v.Replies = []store.Reply{{By: "rev-ann", Thread: true, At: screenNow.Add(-5 * time.Minute)}, {By: "alice", At: screenNow.Add(-time.Minute)}}
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 1 of 5")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 5, Phase: engine.FixPhaseFixing, Role: "claude-fix", Head: prHead,
			StartedAt: screenNow.Add(-10 * time.Minute)}
	}
	r.own(72, 4602, edit)
	task := r.publishedTask("PR-12320", 4530)
	r.pr(4530, edit)
	d := r.data(time.Time{})
	want := tui.CardPR{
		State:     "open · head a1b2c3d · zhuravel-coupon-rounding → master · last activity 08:15",
		Review:    "magnum commented at a1b2c3d, 08:25 · earlier findings: 1 fixed, 2 open",
		Reviewers: "approved by rev-ann and rev-bob (an older commit) · changes asked by rev-cid · asked rev-eve 2h ago",
		Threads:   "2 replies magnum's judge has not read: rev-ann (in magnum's thread) and alice",
		FixLoop:   "cycle 1 of 5 by claude-fix since 08:55 · K stops it",
	}
	for name, c := range map[string]tui.TaskCard{"your PR": d.Cards[-72].Task, "a published task": d.Cards[task.ID].Task} {
		got := c.PRInfo
		switch {
		case got == nil:
			t.Errorf("%s's card has no PR lines", name)
		case got.CI == nil || got.CI.Total != 3 || got.Findings == nil || got.Findings.Counts != [4]int{0, 1, 3, 0}:
			t.Errorf("%s's card: CI %+v, findings %+v", name, got.CI, got.Findings)
		case got.Approvals == nil || got.Approvals.Approved != 2 || got.Approvals.Changes != 1:
			t.Errorf("%s's card: approvals %+v", name, got.Approvals)
		default:
			g := *got
			g.CI, g.Findings, g.Approvals = nil, nil, nil
			if g != want {
				t.Errorf("%s's card says\n%+v\nwant\n%+v", name, g, want)
			}
		}
	}
	r.own(73, 4603, nil) // nothing read yet but the PR: its line alone
	if c := rowCard(t, r.data(time.Time{}), -73); c.PRInfo == nil || c.PRInfo.State != "open · head a1b2c3d" || c.PRInfo.Review != "" ||
		c.PRInfo.Reviewers != "" || c.PRInfo.Threads != "" {
		t.Errorf("a PR with nothing read but its head: %+v", c.PRInfo)
	}
}

// rowCard is the task card of row id in d.
func rowCard(t *testing.T, d tui.FactoryData, id int64) tui.TaskCard {
	t.Helper()
	c, ok := d.Cards[id]
	if !ok {
		t.Fatalf("no card %d", id)
	}
	return c.Task
}
