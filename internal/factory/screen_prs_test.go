package factory

import (
	"errors"
	"fmt"
	"reflect"
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
		"no verdict yet": {func(v *PRState) { v.UpdatedAt = screenNow.Add(-time.Minute) }, tui.WaitsOn{Who: "magnum", What: "looking at #4532"}},
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

// An optional call magnum's fixer left you holds nothing, so the PR is idle
// and merges as any approved one does, but you see it at the merge: the
// move ends with "1 optional call: <link>", and the card links it.
func TestTheMergeMoveNamesTheOptionalCalls(t *testing.T) {
	const link = "https://github.com/talkable/talkable/pull/4534#discussion_r4302866759"
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12324", 4534)
	why := "nothing open at a1b2c3d · approved, merge is yours · 1 optional call: " + link
	r.pr(4534, func(v *PRState) {
		v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, why)
		v.Open.Optional = []string{link}
	})
	row := rowOf(t, r.data(time.Time{}), "PR-12324")
	if row.Group != tui.GroupNeedsYou || row.Move != "merge #4534 on GitHub · 1 optional call: "+link || row.MoveShort != "merge it" {
		t.Errorf("its move %q / %q in %v, want the merge with the optional call", row.Move, row.MoveShort, row.Group)
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
// needs you after it, a PR magnum leaves alone is LEFT ALONE with why (fix
// off is not that: magnum still reviews it), one no watch covers needs you
// to finish it, and an unreadable registry is magnum
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
		}, nil, tui.GroupWaits, tui.MarkWaits,
			tui.WaitsOn{Who: "magnum", What: "review of #4540", Long: "review of #4540 · fix is off for #4540 (F turns it on)"}},
		"a draft": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenOff, "a draft")
			v.Open.Off = engine.OpenOffDraft
		}, nil, tui.GroupYours, tui.MarkWaits, tui.WaitsOn{Who: tui.WaitsOnYou, What: "#4540 is a draft: mark it ready", Short: "#4540 is a draft", OnYou: true}},
		"no watch": {func(v *PRState) { *v = PRState{DaemonUp: true} }, nil, tui.GroupNeedsYou, tui.MarkNeedsYou,
			tui.WaitsOn{Who: tui.WaitsOnYou, What: "magnum does not watch example/talkable: merge or close #4540 yourself, then x drops the task",
				Short: "magnum does not watch example/talkable", OnYou: true}},
		"not picked up a day after its publish": {func(v *PRState) { *v = PRState{DaemonUp: true, Watched: true} }, nil, tui.GroupNeedsYou,
			tui.MarkNeedsYou, tui.WaitsOn{Who: tui.WaitsOnYou, OnYou: true, Short: "magnum has not picked #4540 up since Nov 3 07:05",
				What: "magnum has not picked #4540 up since Nov 3 07:05: `magnum review example/talkable#4540` asks for it"}},
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
		"no verdict, reviewed at the head": {func(v *PRState) { v.OpenWatch, v.ReviewedSHA, v.ReviewEvent = false, prHead, "COMMENTED" }, nil,
			tui.GroupWaits, tui.MarkWaits, tui.WaitsOn{Who: "reviewers", What: "magnum commented at a1b2c3d · no approval yet", Short: "no approval yet"}},
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

// reviewedAt makes v a PR magnum reviewed at its head, outside autofix =
// "open" (no verdict): its review found counts, with CI green.
func reviewedAt(v *PRState, counts [4]int, verdict string) {
	v.OpenWatch, v.Autofix, v.ReviewedSHA = false, config.AutofixNever, prHead
	v.Review = &store.ReviewSummary{SHA: prHead, Counts: counts, Verdict: verdict, At: screenNow.Add(-2 * time.Hour)}
	v.CI = &tui.CIInfo{State: "passed", Total: 41, Passed: 41}
}

// A row never says a bare "reviewed": magnum's outcome at the head (the
// findings open by priority, all fixed, magnum approved, none), what comes
// next and whom it waits on, and the outcome decides the group. Findings
// no loop of magnum's takes wait on you (NEEDS YOU, f fixes them on your
// PR); a PR clean on magnum's side waits on the reviewers it asked, or on
// you to merge it, to ask one or to fix its CI, never on magnum. Before,
// each said "magnum: reviewed a1b2c3d", in YOUR PRS for your PR and in WAITS
// ON OTHERS for a task's.
func TestARowSaysMagnumsOutcomeAndWhomItWaitsOn(t *testing.T) {
	asked := screenNow.Add(-17 * time.Hour)
	for name, c := range map[string]struct {
		edit        func(*PRState)
		group       tui.FactoryGroup
		who, what   string
		short, move string
	}{
		"findings, no loop takes them": {func(v *PRState) { reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking) },
			tui.GroupNeedsYou, tui.WaitsOnYou, "4 findings open at a1b2c3d (P1 P2×3) · f fixes them", "4 findings open", "f fixes them"},
		"findings just posted under autofix own: its loop starts": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking)
			v.Autofix, v.Review.At = config.AutofixOwn, screenNow.Add(-30*time.Second)
		}, tui.GroupWaits, "magnum", "4 findings open at a1b2c3d (P1 P2×3) · magnum's fixer takes them", "4 findings open", ""},
		"findings under autofix own, its loop stopped or never started": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking)
			v.Autofix = config.AutofixOwn // reviewed 2h ago, and no loop runs: a K stopped it, or startFix refused
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "4 findings open at a1b2c3d (P1 P2×3) · f fixes them", "4 findings open", "f fixes them"},
		"findings under autofix own, its fix held": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking)
			v.Autofix, v.FixHeld = config.AutofixOwn, "when the GitHub budget recovers"
		}, tui.GroupWaits, "magnum", "4 findings open at a1b2c3d (P1 P2×3) · magnum's fixer takes them when the GitHub budget recovers",
			"4 findings open", ""},
		"findings under autofix open": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking)
			v.Autofix = config.AutofixOpen
		}, tui.GroupWaits, "magnum", "4 findings open at a1b2c3d (P1 P2×3) · magnum's fixer takes them", "4 findings open", ""},
		"findings under autofix own, fix off": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 3, 0}, store.VerdictBlocking)
			v.Autofix, v.FixOff, v.Review.At = config.AutofixOwn, true, screenNow.Add(-30*time.Second)
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "4 findings open at a1b2c3d (P1 P2×3) · f fixes them", "4 findings open", "f fixes them"},
		"clean, a reviewer asked for changes": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Gate = &store.ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []store.LatestReview{{Login: "rev-ann", State: "CHANGES_REQUESTED"}}}
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "no findings at a1b2c3d · rev-ann asked for changes", "rev-ann asked for changes", "answer rev-ann"},
		"clean, no review required": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Gate = &store.ReviewGate{}
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "no findings at a1b2c3d · no review required, merge is yours", "merge is yours", "merge #%d on GitHub"},
		"clean, a required check has not run yet": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI.Required = []tui.CheckState{{Name: "Completion", Label: "Completion", State: "missing"}}
			v.HeadChangedAt = screenNow.Add(-5 * time.Minute)
		}, tui.GroupWaits, "CI", "no findings at a1b2c3d · Completion has not run at a1b2c3d", "Completion has not run", ""},
		"clean, a required check never ran past settle_max": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI.Required = []tui.CheckState{{Name: "Completion", Label: "Completion", State: "missing"}}
			v.HeadChangedAt = screenNow.Add(-72 * time.Hour)
		}, tui.GroupNeedsYou, tui.WaitsOnYou,
			"no findings at a1b2c3d · Completion did not run at a1b2c3d: run its workflow on the PR's branch, or push",
			"Completion did not run", "run its workflow, or push"},
		"all fixed, a reviewer asked": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Review.Fixed = 3
			v.Gate = &store.ReviewGate{Decision: "REVIEW_REQUIRED"}
			v.Asked = []ReviewAsk{{Who: "rev-ann", At: asked}}
		}, tui.GroupWaits, "reviewers", "all 3 findings fixed at a1b2c3d · asked rev-ann 17h ago", "asked rev-ann 17h ago", ""},
		"magnum approved, a person too": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Review, v.ReviewEvent = nil, "APPROVED"
			v.Gate = &store.ReviewGate{Decision: "APPROVED", Opinions: []store.LatestReview{{Login: "rev-ann", State: "APPROVED", CommitSHA: prHead}}}
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "magnum approved a1b2c3d · approved by rev-ann, merge is yours", "approved, merge is yours",
			"merge #%d on GitHub"},
		"clean, nobody asked": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Gate = &store.ReviewGate{Decision: "REVIEW_REQUIRED"}
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "no findings at a1b2c3d · no reviewer asked", "no reviewer asked", "ask a reviewer"},
		"only optional findings, CI failed": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 0, 0, 2}, store.VerdictNonBlocking)
			v.CI = &tui.CIInfo{State: "failed", Failed: 1, Required: []tui.CheckState{{Name: "Completion", Label: "Completion", State: "failed"}}}
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "no problems at a1b2c3d, 2 optional (P3×2) · CI failed: Completion", "CI failed: Completion",
			"fix the CI"},
		"clean, CI of an older commit": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI = &tui.CIInfo{State: "failed", Failed: 1, Stale: true}
		}, tui.GroupWaits, "CI", "no findings at a1b2c3d · CI of a1b2c3d not read yet", "CI of a1b2c3d not read yet", ""},
		"clean, a required check runs": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI.Required = []tui.CheckState{{Name: "rspec*", Label: "rspec", State: "pending", Done: 3, Total: 8}}
		}, tui.GroupWaits, "CI", "no findings at a1b2c3d · CI runs: rspec", "CI runs: rspec", ""},
		"clean, CI runs": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI = &tui.CIInfo{State: "pending", Total: 41, Passed: 25, Pending: 16}
		}, tui.GroupWaits, "CI", "no findings at a1b2c3d · CI runs: 25/41", "CI runs: 25/41", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			r.pr(4540, c.edit)
			r.own(81, 4541, c.edit)
			d := r.data(time.Time{})
			for key, n := range map[string]int{"PR-12340": 4540, "#4541": 4541} {
				row := rowOf(t, d, key)
				w := row.WaitsOn[0]
				what, move := c.what, c.move
				if strings.Contains(move, "%d") {
					move = fmt.Sprintf(move, n)
				}
				switch {
				case strings.Contains(w.What, "reviewed "):
					t.Errorf("%s says a bare reviewed: %q", key, w.What)
				case row.Group != c.group || w.Who != c.who || w.What != what || w.Short != c.short || w.OnYou != (c.who == tui.WaitsOnYou):
					t.Errorf("%s: group %d, %+v; want group %d, %s: %q (short %q)", key, row.Group, w, c.group, c.who, what, c.short)
				case c.group == tui.GroupNeedsYou && row.Move != move:
					t.Errorf("%s: move %q, want %q", key, row.Move, move)
				}
			}
		})
	}
}

// A task's PR and your PR in the same state sit in the same group, a look
// that leaves them alone included. Fix off is no reason to leave a PR
// alone: magnum still reviews it, so its row says what magnum's row says
// (its outcome, whom it waits on), with fix off and F named on the long
// form. A PR magnum does not review (muted, a baseline) is LEFT ALONE, a
// task's as yours. Before, fix off put a task's PR in NEEDS YOU ("you: fix
// is off for #4540 (F turns it on)") whatever it waited on, while your PR
// in that state sat in LEFT ALONE.
func TestATasksPRAndYoursInOneOffStateShareAGroup(t *testing.T) {
	fixOff := func(v *PRState) {
		v.HasOpen, v.FixOff, v.Open = true, true, verdict(engine.OpenOff, "fix is off (F)")
		v.Open.Off = engine.OpenOffFixOff
	}
	for name, c := range map[string]struct {
		edit      func(*PRState)
		group     tui.FactoryGroup
		who, what string
		long      string
	}{
		"fix off, the reviewers asked": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			fixOff(v)
			v.Gate = &store.ReviewGate{Decision: "REVIEW_REQUIRED"}
			v.Asked = []ReviewAsk{{Who: "rev-ann", At: screenNow.Add(-2 * time.Hour)}}
		}, tui.GroupWaits, "reviewers", "no findings at a1b2c3d · asked rev-ann 2h ago", "fix is off for #%d (F turns it on)"},
		"fix off, findings open": {func(v *PRState) {
			reviewedAt(v, [4]int{0, 1, 0, 0}, store.VerdictBlocking)
			fixOff(v)
		}, tui.GroupNeedsYou, tui.WaitsOnYou, "1 finding open at a1b2c3d (P1) · f fixes them", "fix is off for #%d (F turns it on)"},
		"fix off, magnum's review to come": {fixOff, tui.GroupWaits, "magnum", "review of #%d", "fix is off for #%d (F turns it on)"},
		"muted": {func(v *PRState) {
			v.HasOpen, v.Open = true, verdict(engine.OpenOff, "muted")
			v.Open.Off = engine.OpenOffMuted
		}, tui.GroupYours, tui.WaitsOnYou, "#%d is muted: U unmutes it", ""},
		"a baseline without a fresh verdict": {func(v *PRState) { v.OpenWatch, v.State = false, store.PRBaseline }, tui.GroupYours,
			tui.WaitsOnYou, "#%d is yours to follow: it was open before magnum first synced the repository", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			r.pr(4540, c.edit)
			r.own(81, 4541, c.edit)
			d := r.data(time.Time{})
			for key, n := range map[string]int{"PR-12340": 4540, "#4541": 4541} {
				row := rowOf(t, d, key)
				w := row.WaitsOn[0]
				what := strings.ReplaceAll(c.what, "%d", strconv.Itoa(n))
				if row.Group != c.group || w.Who != c.who || w.What != what {
					t.Errorf("%s: group %d, %+v; want group %d, %s: %q", key, row.Group, w, c.group, c.who, what)
				}
				if long := strings.ReplaceAll(c.long, "%d", strconv.Itoa(n)); long != "" &&
					(!strings.Contains(w.Long, long) || !strings.Contains(row.Help, long)) {
					t.Errorf("%s: long %q, help %q; want them to say %q", key, w.Long, row.Help, long)
				}
				if c.group == tui.GroupYours && (row.Move != "" || row.Mark == tui.MarkNeedsYou || row.PR == nil || row.PR.Off == "") {
					t.Errorf("%s is left alone, yet asks something: move %q, mark %v, PR %+v", key, row.Move, row.Mark, row.PR)
				}
				if c.group != tui.GroupYours && row.PR != nil && row.PR.Off != "" {
					t.Errorf("%s is not left alone, yet its PR says why: %q", key, row.PR.Off)
				}
			}
		})
	}
}

// An idle verdict without an approval names whom it waits on: the
// reviewers asked, with how long ago; with nobody asked it needs you to ask
// one.
func TestAnIdleUnapprovedPRNamesItsReviewers(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12325", 4535)
	r.publishedTask("PR-12326", 4536)
	idle := func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenIdle, "nothing open at a1b2c3d · no approval yet")
		v.Gate = &store.ReviewGate{Decision: "REVIEW_REQUIRED"}
	}
	r.pr(4535, func(v *PRState) {
		idle(v)
		v.Asked = []ReviewAsk{{Who: "rev-ann", At: screenNow.Add(-17 * time.Hour)}, {Who: "team:core"}}
	})
	r.pr(4536, idle)
	d := r.data(time.Time{})
	if row := rowOf(t, d, "PR-12325"); row.Group != tui.GroupWaits ||
		row.WaitsOn[0] != (tui.WaitsOn{Who: "reviewers", What: "nothing open at a1b2c3d · asked rev-ann 17h ago and team:core",
			Short: "asked rev-ann 17h ago and team:core"}) {
		t.Errorf("an idle PR whose reviewers were asked: %+v", row.WaitsOn)
	}
	if row := rowOf(t, d, "PR-12326"); row.Group != tui.GroupNeedsYou || row.Move != "ask a reviewer" ||
		row.WaitsOn[0].What != "nothing open at a1b2c3d · no reviewer asked" {
		t.Errorf("an idle PR nobody was asked to review: %+v, move %q", row.WaitsOn, row.Move)
	}
}

// "looking at #N" is said only while it is true: magnum's look owes a
// verdict (none yet under autofix = "open", or one gone stale) for at most
// four of magnum's polls since the PR's row changed, and the row waits
// since then, so WAITED gives its age. Past that the row says what
// magnum's row says (rowWait's outcome and whom it waits on, which decide
// its group) and its long form names the look that has not decided, with
// why when magnum's daemon stopped ticking. Before, a verdict that never
// came (a daemon on a build without the look) said "looking at" for good.
func TestLookingAtIsSaidOnlyWhileItIsTrue(t *testing.T) {
	clean := func(v *PRState) {
		v.ReviewedSHA, v.ReviewEvent, v.LastTick = prHead, "APPROVED", screenNow.Add(-10*time.Second)
		v.Gate = &store.ReviewGate{Decision: "APPROVED", Opinions: []store.LatestReview{{Login: "rev-ann", State: "APPROVED", CommitSHA: prHead}}}
	}
	stale := func(v *PRState) {
		v.HasOpen, v.Stale, v.Approved = true, true, true
		v.Open = verdict(engine.OpenIdle, "nothing open at a1b2c3d · approved, merge is yours")
		v.Open.At = screenNow.Add(-4 * time.Hour)
	}
	for name, c := range map[string]struct {
		edit    func(*PRState)
		looking bool
		since   time.Time
		long    string
	}{
		"a verdict to come, the row changed a minute ago": {func(v *PRState) { clean(v); v.UpdatedAt = screenNow.Add(-time.Minute) },
			true, screenNow.Add(-time.Minute), ""},
		"a verdict to come for 3h": {func(v *PRState) { clean(v); v.UpdatedAt = screenNow.Add(-3 * time.Hour) },
			false, time.Time{}, "magnum's look has not decided #4540 since 06:05"},
		"a stale verdict, the row changed a minute ago": {func(v *PRState) {
			clean(v)
			stale(v)
			v.UpdatedAt = screenNow.Add(-time.Minute)
		}, true, screenNow.Add(-time.Minute), ""},
		"a stale verdict for 3h": {func(v *PRState) {
			clean(v)
			stale(v)
			v.UpdatedAt = screenNow.Add(-3 * time.Hour)
		}, false, time.Time{}, "magnum's look has not decided #4540 since 06:05"},
		"a stale verdict, magnum's daemon stuck": {func(v *PRState) {
			clean(v)
			stale(v)
			v.UpdatedAt, v.LastTick = screenNow.Add(-time.Minute), screenNow.Add(-time.Hour)
		}, false, time.Time{}, "magnum's look has not decided #4540 since 09:04: magnum's daemon has not ticked since 08:05"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newScreenRig(t)
			r.withMagnum()
			r.publishedTask("PR-12340", 4540)
			r.pr(4540, c.edit)
			row := rowOf(t, r.data(time.Time{}), "PR-12340")
			w := row.WaitsOn[0]
			switch {
			case c.looking && (row.Group != tui.GroupWaits || w != tui.WaitsOn{Who: "magnum", What: "looking at #4540"} || !row.Since.Equal(c.since)):
				t.Errorf("%+v since %v, want magnum looking at #4540 since %v", w, row.Since, c.since)
			case !c.looking && (row.Group != tui.GroupNeedsYou || !strings.HasPrefix(w.What, "magnum approved a1b2c3d · approved by rev-ann") ||
				!strings.Contains(w.Long, c.long) || !strings.Contains(row.Help, c.long)):
				t.Errorf("group %d, %+v, help %q: want NEEDS YOU with the merge and the look named: %q", row.Group, w, row.Help, c.long)
			}
		})
	}
}

// A published PR magnum has not picked up ten minutes after its publish
// needs you, with the command that asks magnum for it; before that it is
// magnum looking at it. Before, it said "looking at #N" for good.
func TestAPublishedPRMagnumNeverPickedUpNeedsYou(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12340", 4540)
	r.at(2 * time.Minute)
	r.published(r.task("PR-12341", "bug", "PR-12341 title", registry.TaskActive, "publish", "", ""), 4541)
	r.at(0)
	for _, n := range []int{4540, 4541} {
		r.pr(n, func(v *PRState) { *v = PRState{DaemonUp: true, Watched: true, OpenWatch: true} })
	}
	d := r.data(time.Time{})
	if row := rowOf(t, d, "PR-12341"); row.Group != tui.GroupWaits || row.WaitsOn[0].What != "looking at #4541" {
		t.Errorf("a PR published 2 minutes ago: %+v", row.WaitsOn)
	}
	row := rowOf(t, d, "PR-12340")
	if w := row.WaitsOn[0]; row.Group != tui.GroupNeedsYou || !w.OnYou ||
		w.What != "magnum has not picked #4540 up since Nov 3 07:05: `magnum review example/talkable#4540` asks for it" ||
		row.Move != "magnum review example/talkable#4540" {
		t.Errorf("a PR magnum never picked up: %+v, move %q", row.WaitsOn, row.Move)
	}
}

// The title line says when magnum's daemon runs an older build than the
// one on disk, with the command that restarts it, and a row whose verdict
// the look owes past its polls names it as the cause.
func TestTheTitleLineSaysMagnumsDaemonRunsAnOlderBuild(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12340", 4540)
	old := &engine.Build{Version: "dev", Revision: strings.Repeat("c8fcd4c6", 5), StartedAt: time.Date(2026, 11, 3, 20, 29, 0, 0, time.Local)}
	r.pr(4540, func(v *PRState) {
		v.UpdatedAt, v.LastTick, v.OldBuild = screenNow.Add(-3*time.Hour), screenNow, old
	})
	d := r.data(time.Time{})
	if !slices.Contains(d.Blocking, "magnum's daemon runs an old build") ||
		!slices.Contains(d.BlockingLong, "magnum's daemon runs dev (c8fcd4c) since Nov 3 20:29: verdicts wait for `magnum daemon-restart --drain`") {
		t.Errorf("blocking %q, long %q: want magnum's daemon's old build", d.Blocking, d.BlockingLong)
	}
	if row := rowOf(t, d, "PR-12340"); !strings.Contains(row.Help, "magnum's look has not decided #4540 since 06:05: magnum's daemon runs an old build") {
		t.Errorf("the row's help %q: want the old build named as the cause", row.Help)
	}
}

// A task's row whose PR magnum's registry has carries it (FactoryRow.PR),
// so f, F and K act on it as on your PR's row: its reference, whether
// magnum's fix loop runs, fix off, and why magnum's look leaves it alone.
// A task without a PR, and one whose PR magnum has not read, carry none.
// Its words name the screen's own keys, not the board's. Before, a task's
// row never carried its PR, and f, F and x refused it.
func TestATaskRowWithAPRCarriesItForTheFixKeys(t *testing.T) {
	r := newScreenRig(t)
	r.withMagnum()
	r.publishedTask("PR-12340", 4540)
	r.pr(4540, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenFixing, "fixing, cycle 1 of 3")
		v.Fixing, v.Fix = true, engine.FixState{Cycle: 1, Cap: 3, Phase: "fixing", Head: prHead}
		v.OpenRestores, v.OpenHold = true, 30*time.Minute
	})
	r.publishedTask("PR-12341", 4541)
	r.pr(4541, func(v *PRState) {
		v.HasOpen, v.Open, v.FixOff = true, verdict(engine.OpenOff, "fix is off (F)"), true
		v.Open.Off = engine.OpenOffFixOff
	})
	r.publishedTask("PR-12342", 4542)
	r.pr(4542, func(v *PRState) {
		v.HasOpen, v.Open = true, verdict(engine.OpenOff, "a draft")
		v.Open.Off = engine.OpenOffDraft
	})
	r.publishedTask("PR-12343", 4543) // magnum has not read it
	r.pr(4543, func(v *PRState) { *v = PRState{DaemonUp: true, Watched: true} })
	r.task("PR-12344", "bug", "Not pushed yet", registry.TaskActive, "coding", "", "")
	d := r.data(time.Time{})
	for key, want := range map[string]*tui.FactoryPR{
		"PR-12340": {Ref: "example/talkable#4540", Fixing: true, Restores: true, Hold: 30 * time.Minute},
		"PR-12341": {Ref: "example/talkable#4541", FixOff: true},
		"PR-12342": {Ref: "example/talkable#4542", Off: "#4542 is a draft: mark it ready"},
		"PR-12343": nil,
		"PR-12344": nil,
	} {
		row := rowOf(t, d, key)
		switch {
		case want == nil && row.PR != nil:
			t.Errorf("%s carries a PR %+v, want none", key, *row.PR)
		case want != nil && (row.PR == nil || !reflect.DeepEqual(*row.PR, *want)):
			t.Errorf("%s carries %+v, want %+v", key, row.PR, *want)
		}
	}
	if w := rowOf(t, d, "PR-12341").WaitsOn[0]; !strings.HasSuffix(w.Long, "fix is off for #4541 (F turns it on)") {
		t.Errorf("a task's PR with fix off says %q, want the screen's F", w.Long)
	}
}
