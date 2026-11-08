package staging

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A stacked PR, whose base is another PR's branch, gets a status: its
// commits are its base..refs/pull/N/head, the base fetched beside master,
// so the commits under it (the other PR's) never count as its own.
func TestAStackedPROnAnotherBaseGetsItsStatus(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.ref("refs/heads/feat-a", s.p2) // PR 7's branch, the base of PR 8
	q1 := s.commit(2*time.Hour, s.p2)
	s.ref("refs/pull/8/head", q1)
	s.push(time.Hour, s.p1) // staging has PR 7 at p1 only
	c := s.client()
	s.refreshPRs(c, OpenPR{Number: 7, Base: "master"}, OpenPR{Number: 8, Base: "feat-a"})
	if last := s.git.fetched[len(s.git.fetched)-1]; !slices.Contains(last, "+refs/heads/feat-a:refs/heads/feat-a") ||
		!slices.Contains(last, "+refs/heads/master:refs/heads/master") {
		t.Errorf("the fetch %q lacks a base", last)
	}
	stacked := PR{Repo: "acme/app", Number: 8, Head: q1, Base: "feat-a"}
	if got := one(c, stacked); got.State != StateNone {
		t.Errorf("PR 8 before its merge: %s (in tip %q, why %q), want none: PR 7's p1 is not its commit", got.State, got.InTip, got.Why)
	}
	s.push(30*time.Minute, q1)
	s.now = s.now.Add(RefreshEvery)
	s.refreshPRs(c, OpenPR{Number: 7, Base: "master"}, OpenPR{Number: 8, Base: "feat-a"})
	if got := one(c, stacked); got.State != StatePushed || got.InTip != q1 {
		t.Errorf("PR 8 merged: %s at %q (why %q), want pushed at its head", got.State, got.InTip, got.Why)
	}
	if got := one(c, s.pr()); got.State != StatePushed || got.InTip != s.p2 {
		t.Errorf("PR 7 under it: %s at %q, want pushed at its head p2, which PR 8's merge brought", got.State, got.InTip)
	}
	if got := one(c, PR{Repo: "acme/app", Number: 8, Head: q1, Base: "release"}); got.State != StateUnknown || !strings.Contains(got.Why, "feat-a") {
		t.Errorf("PR 8 asked on another base than the Refresh's: %s %q, want unknown naming both", got.State, got.Why)
	}
}

// A base the fetch cannot read (its branch deleted) leaves its PRs unknown,
// Why naming the base; the other PRs are read as ever.
func TestABaseTheFetchCannotReadIsUnknownNamingIt(t *testing.T) {
	t.Parallel()
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	q := s.commit(time.Hour, s.m1)
	s.ref("refs/pull/9/head", q)
	s.git.missing["refs/heads/gone"] = true
	c := s.client()
	s.refreshPRs(c, OpenPR{Number: 7, Base: "master"}, OpenPR{Number: 9, Base: "gone"})
	if got := one(c, PR{Repo: "acme/app", Number: 9, Head: q, Base: "gone"}); got.State != StateUnknown || !strings.Contains(got.Why, `"gone"`) {
		t.Errorf("PR 9 on a base the fetch cannot read: %s %q, want unknown naming it", got.State, got.Why)
	}
	if got := one(c, s.pr()); got.State != StatePushed {
		t.Errorf("PR 7 beside it: %s %q, want pushed", got.State, got.Why)
	}
	if last := s.git.fetched[len(s.git.fetched)-1]; slices.ContainsFunc(last, func(spec string) bool { return strings.Contains(spec, "gone") || strings.Contains(spec, "pull/9/") }) {
		t.Errorf("the retry still fetches the base or its PR: %q", last)
	}
}

// A base that is no plain branch name (git check-ref-format --branch, no
// leading '-') is refused in Refresh and never reaches git: its PR is
// unknown, Why naming it.
func TestAHostileBaseNameIsRefusedAndNeverReachesGit(t *testing.T) {
	t.Parallel()
	hostile := []string{"--upload-pack=touch /tmp/x", "-x", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[b", `a\b`,
		"a/", "/a", "a//b", "a.lock", ".a", "a/.b", "a.", "@", "a@{1}", "HEAD", "a\x00b", "a\nb"}
	for _, base := range []string{"master", "feat/a-1", "release-2026.11", "user/topic_x"} {
		if !validBranch(base) {
			t.Errorf("validBranch(%q) = false, want a branch", base)
		}
	}
	s := newScene(t)
	s.push(10*time.Minute, s.p2)
	open := []OpenPR{{Number: 7, Base: "master"}}
	for i, base := range hostile {
		if validBranch(base) {
			t.Errorf("validBranch(%q) = true, want refused", base)
		}
		n := 100 + i
		s.ref(pullRef(n), s.p2)
		open = append(open, OpenPR{Number: n, Base: base})
	}
	c := s.client()
	s.refreshPRs(c, open...)
	for _, spec := range slices.Concat(s.git.fetched...) {
		if !strings.HasPrefix(spec, "+refs/heads/staging-*") && spec != "+refs/heads/master:refs/heads/master" && spec != "+refs/pull/7/head:refs/pull/7/head" {
			t.Errorf("a fetch carries %q", spec)
		}
	}
	for i, base := range hostile {
		got := one(c, PR{Repo: "acme/app", Number: 100 + i, Head: s.p2, Base: base})
		if got.State != StateUnknown || !strings.Contains(got.Why, "not a branch name") {
			t.Errorf("base %q: %s %q, want unknown, not a branch name", base, got.State, got.Why)
		}
	}
	if got := one(c, s.pr()); got.State != StatePushed {
		t.Errorf("PR 7 beside them: %s %q, want pushed", got.State, got.Why)
	}
}
