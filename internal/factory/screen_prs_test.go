package factory

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// prHead is the head of the screen tests' PRs: a1b2c3d, short.
var prHead = "a1b2c3d" + strings.Repeat("0", 33)

// withMagnum makes the rig's data read magnum's registry (the fake).
func (r *screenRig) withMagnum() *fakeMagnum {
	r.magnum = &fakeMagnum{}
	return r.magnum
}

// pr makes magnum's registry say of example/talkable#number what edit
// makes of an open PR at prHead that magnum has, in a watch under autofix =
// "open", with its daemon up.
func (r *screenRig) pr(number int, edit func(*PRState)) {
	r.t.Helper()
	v := PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: prHead, State: store.PRReviewed}
	if edit != nil {
		edit(&v)
	}
	r.magnum.setPR(PRRef{Repo: "example/talkable", Number: number}, &v)
}

// verdict is an open verdict at prHead.
func verdict(of, why string) engine.OpenVerdict {
	return engine.OpenVerdict{Verdict: of, Why: why, Head: prHead, At: screenNow}
}

// publishedTask is a task of key published yesterday as #number.
func (r *screenRig) publishedTask(key string, number int) registry.Task {
	r.t.Helper()
	r.at(26 * time.Hour)
	return r.published(r.task(key, "bug", key+" title", registry.TaskActive, "publish", "", ""), number)
}

// A published task whose PR magnum's fixer works on says so on its row, as
// an agent at work (the moving mark), with the loop's cycle; before, every
// published task said "magnum: review of #N".
func TestAPublishedTaskShowsItsPRsFixLoop(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	task := r.publishedTask("PR-12320", 4530)
	r.pr(4530, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 2 of 5")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 2, Cap: 5, Phase: "fixing", Head: prHead}
	})
	d := r.data(time.Time{})
	row := rowOf(t, d, "PR-12320")
	want := tui.WaitsOn{Who: "agent", What: "magnum fixes #4530 (cycle 2 of 5)", Short: "magnum fixes #4530"}
	if row.Group != tui.GroupWaits || row.Mark != tui.MarkWorking || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
		t.Fatalf("the row of a PR in a fix loop: %+v, want it working with %+v", row, want)
	}
	if card := d.Cards[task.ID].Task; card.WaitingFor != "agent: magnum fixes #4530 (cycle 2 of 5)" {
		t.Errorf("its card waits for %q", card.WaitingFor)
	}

	r.pr(4530, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, re-review of b2c3d4e")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 2, Cap: 5, Phase: engine.FixPhaseReviewing, Head: "b2c3d4e" + strings.Repeat("0", 33)}
	})
	if w := rowOf(t, r.data(time.Time{}), "PR-12320").WaitsOn[0]; w.What != "magnum fixes #4530 (re-review of b2c3d4e)" {
		t.Errorf("the row of a loop's re-review: %+v", w)
	}
}

// A PR that needs you puts its task in NEEDS YOU, the verdict's words on
// its row and what needs you as links on its card.
func TestAPublishedTaskWithFixNeedsYouIsInNeedsYou(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	task := r.publishedTask("PR-12321", 4531)
	links := []string{"https://github.com/example/talkable/pull/4531#discussion_r1", "https://github.com/example/talkable/pull/4531#discussion_r2"}
	r.pr(4531, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, "2 items need you: 1 thread of a person and 1 skipped finding")
		v.Open.Needs = []engine.OpenNeed{{Kind: "thread", URL: links[0]}, {Kind: "finding", URL: links[1]}}
	})
	d := r.data(time.Time{})
	row := rowOf(t, d, "PR-12321")
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: "2 items need you: 1 thread of a person and 1 skipped finding", Short: "2 items need you", OnYou: true}
	if row.Group != tui.GroupNeedsYou || row.Mark != tui.MarkNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
		t.Fatalf("the row of a PR that needs you: %+v, want NEEDS YOU with %+v", row, want)
	}
	if d.Decisions != 1 {
		t.Errorf("%d decisions, want the PR's 1", d.Decisions)
	}
	card := d.Cards[task.ID].Task
	if !slices.Equal(card.Links, links) || !strings.HasPrefix(card.WaitingFor, "you: 2 items need you") {
		t.Errorf("its card: waits for %q, links %q, want the needs' links %q", card.WaitingFor, card.Links, links)
	}
}

// Only an idle verdict says nothing is open: a wait, a look not done, a
// verdict to come under autofix = "open" and an unknown one never do.
func TestAPublishedTaskIsIdleOnlyWhenTheOpenRowSaysIdle(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12322", 4532)
	for name, c := range map[string]struct {
		edit func(*PRState)
		want tui.WaitsOn
	}{
		"idle": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · no approval yet")
		},
			tui.WaitsOn{Who: "reviewers", What: "nothing open at a1b2c3d · no approval yet", Short: "nothing open at a1b2c3d"}},
		"magnum reviews": {func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") },
			tui.WaitsOn{Who: "magnum", What: "magnum reviews a1b2c3d"}},
		"settling": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "settling until 09:20: a reviewer can still be at work")
		}, tui.WaitsOn{Who: "reviewers", What: "settling until 09:20: a reviewer can still be at work", Short: "settling until 09:20"}},
		"CI": {func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "CI at a1b2c3d") },
			tui.WaitsOn{Who: "CI", What: "CI at a1b2c3d"}},
		"unknown": {func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenUnknown, "reading GitHub") },
			tui.WaitsOn{Who: "magnum", What: "looking at #4532", Long: "looking at #4532: reading GitHub"}},
		"no verdict yet": {nil, tui.WaitsOn{Who: "magnum", What: "looking at #4532"}},
	} {
		r.pr(4532, c.edit)
		row := rowOf(t, r.data(time.Time{}), "PR-12322")
		if row.Group != tui.GroupWaits || row.Mark != tui.MarkWaits || len(row.WaitsOn) != 1 || row.WaitsOn[0] != c.want {
			t.Errorf("%s: %+v, want it waiting on others with %+v", name, row, c.want)
		}
	}
}

// A verdict that no longer tells the PR's state (stale: magnum's last tick
// long ago, another head, a row changed since) is never idle: the row says
// magnum is looking at the PR, and magnum's daemon down says so.
func TestAStaleOpenRowIsNeverIdle(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12323", 4533)
	r.pr(4533, func(v *PRState) {
		v.HasOpen, v.Stale, v.Approved = true, true, true
		v.Open = verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
	})
	row := rowOf(t, r.data(time.Time{}), "PR-12323")
	if row.Group != tui.GroupWaits || row.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "looking at #4533"}) {
		t.Errorf("a stale idle verdict: %+v, want magnum looking at #4533", row)
	}
	r.pr(4533, func(v *PRState) {
		v.HasOpen, v.Stale, v.DaemonUp = true, true, false
		v.Open = verdict(engine.OpenIdle, "nothing open at a1b2c3d · no approval yet")
	})
	row = rowOf(t, r.data(time.Time{}), "PR-12323")
	if w := row.WaitsOn[0]; row.Group != tui.GroupWaits || w.Who != "magnum" || w.What != "daemon down" || !strings.Contains(w.Long, "magnum's daemon is down") {
		t.Errorf("magnum's daemon down: %+v, want magnum's daemon down", row)
	}
}

// A PR in needs attention needs you whatever the look's verdict is: one to
// come under autofix = "open", or one gone stale, never hides it under
// WAITS ON OTHERS as "looking at".
func TestAPublishedTaskInNeedsAttentionNeedsYouWithoutAFreshVerdict(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12329", 4539)
	msg := "fix: 1 finding needs your call (5808905525 description)"
	for name, edit := range map[string]func(*PRState){
		"a verdict to come": func(v *PRState) { v.OpenWatch = true },
		"a stale verdict": func(v *PRState) {
			v.OpenWatch, v.HasOpen, v.Stale = true, true, true
			v.Open = verdict(engine.OpenNeedsYou, "needs your attention (fix)")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r.pr(4539, func(v *PRState) {
				v.State, v.Attention, v.AttentionMsg = store.PRNeedsAttention, "fix", msg
				edit(v)
			})
			row := rowOf(t, r.data(time.Time{}), "PR-12329")
			if row.Group != tui.GroupNeedsYou || len(row.WaitsOn) == 0 || !row.WaitsOn[0].OnYou {
				t.Fatalf("a PR in needs attention with %s: %+v, want NEEDS YOU", name, row)
			}
			if w := row.WaitsOn[0]; strings.Contains(w.What, "looking at") {
				t.Errorf("its wait says %q, want the attention", w.What)
			}
		})
	}
}

// An idle PR a person approved needs only your merge: NEEDS YOU, the
// merge your move, and the commits pushed after the approval named.
func TestAnIdleApprovedPRWaitsOnYouToMerge(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12324", 4534)
	why := "nothing open at a1b2c3d · approved, merge is yours · 2 commits after the approval"
	r.pr(4534, func(v *PRState) { v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, why) })
	row := rowOf(t, r.data(time.Time{}), "PR-12324")
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: why, Short: "nothing open at a1b2c3d", OnYou: true}
	if row.Group != tui.GroupNeedsYou || row.Mark != tui.MarkNeedsYou || row.WaitsOn[0] != want {
		t.Fatalf("an idle approved PR: %+v, want NEEDS YOU with %+v", row, want)
	}
	if row.Move != "merge #4534 on GitHub" || row.MoveShort != "merge it" {
		t.Errorf("its move %q / %q, want the merge", row.Move, row.MoveShort)
	}
}

// An idle PR nobody approved waits on its reviewers.
func TestAnIdleUnapprovedPRWaitsOnReviewers(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12325", 4535)
	r.pr(4535, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · no approval yet")
	})
	row := rowOf(t, r.data(time.Time{}), "PR-12325")
	if row.Group != tui.GroupWaits || row.Mark != tui.MarkWaits || row.WaitsOn[0].Who != "reviewers" || row.Move != "" {
		t.Errorf("an idle PR without an approval: %+v, want it waiting on reviewers", row)
	}
}

// A task published today is not done: its PR is open, so its row says
// what the PR waits on, as on any other day; only a merge is DONE TODAY.
func TestAPublishedTaskIsNoLongerDoneOnItsDay(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.clock.Set(time.Date(2026, 11, 4, 8, 40, 0, 0, time.Local))
	r.published(r.task("PR-12326", "bug", "Coupon CSV header names", registry.TaskActive, "publish", "", ""), 4536)
	r.pr(4536, func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") })
	row := rowOf(t, r.data(time.Time{}), "PR-12326")
	if row.Group != tui.GroupWaits || row.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "magnum reviews a1b2c3d"}) {
		t.Errorf("a task published today: %+v, want it waiting on magnum's review", row)
	}
}

// Without magnum's registry to read (no Magnum), a published task's row
// says what it said before: magnum reviews its PR.
func TestWithoutMagnumTheRowKeepsTodaysWords(t *testing.T) {
	r := newScreenRig(t)
	r.publishedTask("PR-12327", 4537)
	row := rowOf(t, r.data(time.Time{}), "PR-12327")
	if row.Group != tui.GroupWaits || row.Mark != tui.MarkWaits || row.WaitsOn[0] != (tui.WaitsOn{Who: "magnum", What: "review of #4537"}) {
		t.Errorf("without magnum's registry: %+v, want magnum: review of #4537", row)
	}
}

// Each fact of the PR has its row: a merge is done (before the tick moves
// the task), a close waits for a reopen within [daemon] close_grace and
// needs you after it, a PR magnum leaves alone needs you with why, one no
// watch covers needs you to finish it, and an unreadable registry is magnum
// looking; without a verdict (autofix is not "open") the loop, the
// attention and magnum's review still show.
func TestAPublishedTasksRowFollowsItsPR(t *testing.T) {
	merged := time.Date(2026, 11, 4, 8, 50, 0, 0, time.Local)
	closedNow, closedLong := screenNow.Add(-2*time.Minute), screenNow.Add(-time.Hour)
	for name, c := range map[string]struct {
		edit  func(*PRState)
		err   error
		group tui.FactoryGroup
		mark  tui.FactoryMark
		want  tui.WaitsOn
	}{
		"merged": {func(v *PRState) { v.GHState, v.MergedAt, v.ClosedAt = store.GHMerged, &merged, &merged },
			nil, tui.GroupDone, tui.MarkDone, tui.WaitsOn{Who: "nothing", What: "merged 08:50"}},
		"closed within the grace": {func(v *PRState) { v.GHState, v.ClosedAt = store.GHClosed, &closedNow }, nil, tui.GroupWaits, tui.MarkWaits,
			tui.WaitsOn{Who: "magnum", What: "#4540 closed 09:03; the task waits until 09:13 for a reopen", Short: "#4540 closed 09:03"}},
		"closed past the grace": {func(v *PRState) { v.GHState, v.ClosedAt = store.GHClosed, &closedLong }, nil, tui.GroupNeedsYou, tui.MarkNeedsYou,
			tui.WaitsOn{Who: tui.WaitsOnYou, What: "PR #4540 closed without a merge", OnYou: true,
				Long: "PR #4540 closed without a merge: reopen it, or x drops the task"}},
		"fix off": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenOff, "fix is off (F)")
			v.Open.Off = engine.OpenOffFixOff
		}, nil, tui.GroupNeedsYou, tui.MarkNeedsYou,
			tui.WaitsOn{Who: tui.WaitsOnYou, What: "fix is off for #4540 (F on the board turns it on)", Short: "fix is off for #4540", OnYou: true}},
		"a draft": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenOff, "a draft")
			v.Open.Off = engine.OpenOffDraft
		}, nil, tui.GroupNeedsYou, tui.MarkNeedsYou, tui.WaitsOn{Who: tui.WaitsOnYou, What: "#4540 is a draft: mark it ready", Short: "#4540 is a draft", OnYou: true}},
		"no watch": {func(v *PRState) { *v = PRState{DaemonUp: true} }, nil, tui.GroupNeedsYou, tui.MarkNeedsYou,
			tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum does not watch example/talkable: merge or close #4540 yourself, then x drops the task",
				Short: "magnum does not watch example/talkable", OnYou: true}},
		"not found yet": {func(v *PRState) { *v = PRState{DaemonUp: true, Watched: true} }, nil, tui.GroupWaits, tui.MarkWaits,
			tui.WaitsOn{Who: "magnum", What: "looking at #4540"}},
		"registry unreadable": {nil, errors.New("magnum's registry is at schema 40 and this build needs 41"), tui.GroupWaits, tui.MarkWaits,
			tui.WaitsOn{Who: "magnum", What: "looking at #4540",
				Long: "looking at #4540: magnum's registry cannot be read: magnum's registry is at schema 40 and this build needs 41"}},
		"no verdict, a loop": {func(v *PRState) {
			v.OpenWatch, v.Fixing, v.Fix = false, true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead}
		}, nil, tui.GroupWaits, tui.MarkWorking, tui.WaitsOn{Who: "agent", What: "magnum fixes #4540 (cycle 1 of 3)", Short: "magnum fixes #4540"}},
		"no verdict, attention": {func(v *PRState) {
			v.OpenWatch, v.State, v.Attention, v.AttentionMsg = false, store.PRNeedsAttention, "blocked", "judge blocked: the slot's database is gone"
		}, nil, tui.GroupNeedsYou, tui.MarkNeedsYou,
			tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum: judge blocked: the slot's database is gone", Short: "magnum: judge blocked", OnYou: true}},
		"no verdict": {func(v *PRState) { v.OpenWatch = false }, nil, tui.GroupWaits, tui.MarkWaits, tui.WaitsOn{Who: "magnum", What: "review of #4540"}},
		"no verdict, reviewed at the head": {func(v *PRState) { v.OpenWatch, v.ReviewedSHA = false, prHead }, nil, tui.GroupWaits, tui.MarkWaits,
			tui.WaitsOn{Who: "magnum", What: "reviewed a1b2c3d"}},
		"no verdict, a stalemate": {func(v *PRState) { v.OpenWatch, v.ReviewedSHA, v.Stalemate = false, prHead, true }, nil, tui.GroupNeedsYou,
			tui.MarkNeedsYou, tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum stopped arguing on #4540: the thread is yours",
				Short: "magnum stopped arguing on #4540", OnYou: true}},
		"a loop started after a fresh verdict": {func(v *PRState) {
			v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
			v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead}
		}, nil, tui.GroupWaits, tui.MarkWorking, tui.WaitsOn{Who: "agent", What: "magnum fixes #4540 (cycle 1 of 3)", Short: "magnum fixes #4540"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			r.pr(4540, c.edit)
			r.magnum.setPRErr(c.err)
			row := rowOf(t, r.data(time.Time{}), "PR-12340")
			if row.Group != c.group || row.Mark != c.mark || len(row.WaitsOn) != 1 || row.WaitsOn[0] != c.want {
				t.Errorf("%+v, want group %v, mark %v, %+v", row, c.group, c.mark, c.want)
			}
		})
	}
}

// ScreenData reads magnum's registry once for all the published tasks of
// a frame (and its tasks done today), and not at all when there is none.
func TestScreenDataReadsMagnumOncePerFrame(t *testing.T) {
	r := newScreenRig(t)
	m := r.withMagnum()
	r.task("PR-12350", "bug", "At work", registry.TaskActive, "investigate", "", "")
	r.data(time.Time{})
	if n := m.prReads(); n != 0 {
		t.Fatalf("%d reads of magnum's registry without a published task", n)
	}
	r.publishedTask("PR-12351", 4551)
	r.publishedTask("PR-12352", 4552)
	r.data(time.Time{})
	if n := m.prReads(); n != 1 {
		t.Errorf("%d reads of magnum's registry for 2 published tasks, want 1", n)
	}
}

// A published row waited since its PR began to wait as it does, not since
// the task was published: a verdict's Since (its At in a row of an older
// build), a loop's start, a close's time and the grace's end, the attention's
// row change; a reopen after it is the later start. A row that needs you
// since you were last seen is new for you in the digest.
func TestAPublishedRowWaitedSinceItsPRBeganToWaitSo(t *testing.T) {
	since, at := screenNow.Add(-2*time.Hour), screenNow.Add(-5*time.Minute)
	closed := screenNow.Add(-time.Hour)
	needs := func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenNeedsYou, "1 item needs you: 1 thread of a person")
		v.Open.At, v.Open.Since = at, since
	}
	for name, c := range map[string]struct {
		edit   func(*PRState)
		reopen bool
		want   time.Time
	}{
		"needs you": {needs, false, since},
		"approved, idle": {func(v *PRState) {
			v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
			v.Open.At, v.Open.Since = at, since
		}, false, since},
		"fixing": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 1 of 3")
			v.Open.At, v.Open.Since = at, since
		}, false, since},
		"a verdict of an older build": {func(v *PRState) {
			needs(v)
			v.Open.Since = time.Time{}
		}, false, at},
		"a reopen after the verdict began": {needs, true, screenNow.Add(-time.Hour)},
		"no verdict, a loop": {func(v *PRState) {
			v.OpenWatch, v.Fixing, v.Fix = false, true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead, StartedAt: since}
		}, false, since},
		"no verdict, attention": {func(v *PRState) {
			v.OpenWatch, v.State, v.Attention, v.UpdatedAt = false, store.PRNeedsAttention, "blocked", since
		}, false, since},
		"closed within the grace": {func(v *PRState) {
			closedNow := screenNow.Add(-2 * time.Minute)
			v.GHState, v.ClosedAt = store.GHClosed, &closedNow
		}, false, screenNow.Add(-2 * time.Minute)},
		"closed past the grace, before the tick": {func(v *PRState) { v.GHState, v.ClosedAt = store.GHClosed, &closed },
			false, closed.Add(10 * time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			task := r.publishedTask("PR-12360", 4560)
			if c.reopen {
				r.at(time.Hour)
				r.move(task, registry.TaskPublished, "", "", func(u *registry.TaskUpdate) { u.Because("PR #4560 reopened") })
			}
			r.pr(4560, c.edit)
			if row := rowOf(t, r.data(time.Time{}), "PR-12360"); !row.Since.Equal(c.want) {
				t.Errorf("waited since %s, want %s", row.Since.Format(time.TimeOnly), c.want.Format(time.TimeOnly))
			}
		})
	}

	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12361", 4561)
	r.pr(4561, needs)
	r.at(time.Hour)
	r.published(r.task("PR-12362", "bug", "Pushed while away", registry.TaskActive, "publish", "", ""), 4562)
	r.pr(4562, func(v *PRState) { v.HasOpen, v.Open = true, verdict(engine.OpenWaits, "magnum reviews a1b2c3d") })
	d := r.data(screenNow.Add(-3 * time.Hour))
	if d.Digest == nil || d.Digest.Chip != "away 3h: 1 done, 1 new for you" {
		t.Errorf("the digest %+v, want the PR that came to need you counted new for you", d.Digest)
	}
}

// A task done by the tick's follow of its merged PR says when the PR
// merged, not when the tick saw it: its row and the digest name the
// merge's time, and DONE TODAY and the digest keep it by that time, so a
// PR merged last week whose task the first tick after an install made done
// is neither done today nor news.
func TestADoneTaskSaysWhenItsPRMerged(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.at(8 * 24 * time.Hour)
	lastWeek := r.published(r.task("PR-12371", "chore", "Merged last week", registry.TaskActive, "publish", "", ""), 4571)
	today := r.publishedTask("PR-12370", 4570)
	mergedToday, mergedLastWeek := time.Date(2026, 11, 4, 8, 50, 0, 0, time.Local), screenNow.Add(-7*24*time.Hour)
	r.pr(4570, func(v *PRState) { v.GHState, v.MergedAt, v.ClosedAt = store.GHMerged, &mergedToday, &mergedToday })
	r.pr(4571, func(v *PRState) { v.GHState, v.MergedAt, v.ClosedAt = store.GHMerged, &mergedLastWeek, &mergedLastWeek })
	r.at(time.Minute) // the tick follows both merges at 09:04
	r.move(today, registry.TaskDone, "", "")
	r.move(lastWeek, registry.TaskDone, "", "")

	d := r.data(screenNow.Add(-3 * time.Hour))
	want := tui.WaitsOn{Who: "nothing", What: "merged 08:50"}
	if row := rowOf(t, d, "PR-12370"); row.Group != tui.GroupDone || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
		t.Errorf("a task whose PR merged at 08:50: %+v, want DONE TODAY with %+v", row, want)
	}
	if hasRow(d, "PR-12371") {
		t.Errorf("a task whose PR merged last week is DONE TODAY")
	}
	if d.Digest == nil || !slices.Equal(d.Digest.Done, []string{"PR-12370 PR-12370 title · merged 08:50"}) {
		t.Errorf("the digest %+v, want only the merge of 08:50", d.Digest)
	}

	r.magnum.setPRErr(errors.New("magnum's registry is locked"))
	if row := rowOf(t, r.data(time.Time{}), "PR-12370"); row.WaitsOn[0].What != "merged 09:04" {
		t.Errorf("without magnum's registry: %+v, want the time the task was done", row.WaitsOn[0])
	}
}

// A PR in a repository no [[watch]] covers is yours to finish, and nothing
// can tell the task when you did: its row says that x then ends the task,
// and x is its move, as for a closed PR.
func TestAnUnwatchedPRsRowNamesHowItsTaskEnds(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12380", 4580)
	r.pr(4580, func(v *PRState) { *v = PRState{DaemonUp: true} })
	row := rowOf(t, r.data(time.Time{}), "PR-12380")
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum does not watch example/talkable: merge or close #4580 yourself, then x drops the task",
		Short: "magnum does not watch example/talkable", OnYou: true}
	if row.Group != tui.GroupNeedsYou || row.WaitsOn[0] != want {
		t.Errorf("the row of an unwatched PR: %+v, want NEEDS YOU with %+v", row, want)
	}
	if row.Move != "merge or close #4580, then x drops the task" || row.MoveShort != "then x" {
		t.Errorf("its move %q / %q, want x once you merged or closed it", row.Move, row.MoveShort)
	}
}
