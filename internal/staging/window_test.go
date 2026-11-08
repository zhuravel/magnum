package staging

import (
	"testing"
	"time"
)

// A PR merged before the window is none: 90 days back, or 400 first-parent
// commits back, whichever ends first.
func TestAPRMergedBeyondTheWindowIsNone(t *testing.T) {
	t.Parallel()
	t.Run("older than 90 days", func(t *testing.T) {
		s := newScene(t)
		// The PR's commits and its merge are 100 days old; a newer merge
		// follows.
		old := s.chain(s.m1, 1, 101*24*time.Hour)[0]
		s.ref("refs/pull/7/head", old)
		s.push(100*24*time.Hour, old)
		s.push(24*time.Hour, s.commit(25*time.Hour, s.m1))
		c := s.client()
		s.refresh(c, 7)
		if got := one(c, PR{Repo: "acme/app", Number: 7, Head: old, Base: "master"}); got.State != StateNone {
			t.Errorf("merged 100 days ago: %s (in tip %q), want none", got.State, got.InTip)
		}
	})
	for _, tc := range []struct {
		newer int
		want  State
	}{
		{Window - 1, StatePushed}, // the merge is the window's last first-parent commit
		{Window, StateNone},       // the merge is the 401st
	} {
		t.Run("behind newer merges", func(t *testing.T) {
			s := newScene(t)
			s.push(time.Hour, s.p2)
			for range tc.newer {
				s.push(time.Minute, s.commit(2*time.Hour, s.m1))
			}
			c := s.client()
			s.refresh(c, 7)
			if got := one(c, s.pr()); got.State != tc.want {
				t.Errorf("behind %d newer merges: %s, want %s", tc.newer, got.State, tc.want)
			}
		})
	}
}

// A branch that merged staging in (by mistake, as happens) holds every PR
// staging held then; merged into staging within the window, it brings back
// none of those merged before the window: what entered staging before the
// window stays beyond it.
func TestABranchHoldingAnOldStagingTipBringsNoPRMergedBeforeTheWindow(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	old := s.chain(s.m1, 1, 101*24*time.Hour)[0]
	s.ref("refs/pull/7/head", old)
	oldTip := s.push(100*24*time.Hour, old)
	f1 := s.commit(3*time.Hour, s.m1)
	f2 := s.merge(2*time.Hour, f1, oldTip) // the feature branch merged staging
	s.push(time.Hour, f2)
	c := s.client()
	s.refresh(c, 7)
	if got := one(c, PR{Repo: "acme/app", Number: 7, Head: old, Base: "master"}); got.State != StateNone {
		t.Errorf("merged 100 days ago, held by a branch merged an hour ago: %s (pushed at %v), want none", got.State, got.PushedAt)
	}
}

// A merge of master into staging brings master's commits, never a PR's: the
// PRs off master stay none, and a PR that merged master in is only where its
// own commits are.
func TestAMasterMergeOnTheChainDoesNotPutEveryPRIn(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	// master moves on (m2, m3); PR 8 merged m2 into its branch; staging
	// merges master at m3 and nothing else.
	m2 := s.commit(5*time.Hour, s.m1)
	m3 := s.commit(4*time.Hour, m2)
	s.ref("refs/heads/master", m3)
	q1 := s.commit(6*time.Hour, s.m1)
	q2 := s.merge(3*time.Hour, q1, m2)
	s.ref("refs/pull/8/head", q2)
	s.push(time.Hour, m3)
	c := s.client()
	s.refresh(c, 7, 8)
	for _, pr := range []PR{s.pr(), {Repo: "acme/app", Number: 8, Head: q2, Base: "master"}} {
		if got := one(c, pr); got.State != StateNone {
			t.Errorf("PR %d after a master merge: %s (in tip %q), want none", pr.Number, got.State, got.InTip)
		}
	}
	// The same PR 8 merged after: in, at its head.
	s.push(30*time.Minute, q2)
	s.now = s.now.Add(RefreshEvery)
	s.refresh(c, 7, 8)
	if got := one(c, PR{Repo: "acme/app", Number: 8, Head: q2, Base: "master"}); got.State != StatePushed || got.InTip != q2 {
		t.Errorf("PR 8 once merged: %s at %q, want pushed at its head", got.State, got.InTip)
	}
	if got := one(c, s.pr()); got.State != StateNone {
		t.Errorf("PR 7 beside PR 8's merge: %s, want none", got.State)
	}
}
