package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// pushDuringReview reviews PR #2 at b1 while one commit (b2, changing
// files; nil = GitHub lists none) is pushed during the round, runs
// meanwhile before the review posts, and returns the notes the round added
// to its review.
func pushDuringReview(t *testing.T, h *harness, files []github.FileDelta, meanwhile func()) []string {
	t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(time.Minute)
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	if files != nil {
		h.gh.files = map[string][]github.FileDelta{"b1...b2": files}
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if meanwhile != nil {
		meanwhile()
	}
	close(h.rd.gate)
	h.settle()
	if pr := h.wantState(2, store.PRRereviewPending); deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("reviewed_sha %q", deref(pr.ReviewedSHA))
	}
	return h.rd.appended()
}

// The note on a review that commits outran promises a re-review only when one
// starts by itself, and says when when it can; otherwise it says the commits
// are not reviewed yet.
func TestMovedHeadNoteFollowsThePRsWait(t *testing.T) {
	const (
		follows    = "101:_Reviewed b1; 1 commit arrived during the review, re-review follows._"
		afterQuiet = "101:_Reviewed b1; 1 commit arrived during the review, re-review follows after the quiet period._"
		notYet     = "101:_Reviewed b1; 1 commit arrived during the review and is not reviewed yet._"
		checkQuiet = "101:_Reviewed b1; 1 commit arrived during the review, a short check of that commit follows after the quiet period._"
	)
	for _, tc := range []struct {
		name      string
		minLines  int // [daemon] rereview_min_lines; 0 = no threshold
		noCheck   bool
		files     []github.FileDelta
		meanwhile func(h *harness)
		want      string
	}{
		{name: "push past its quiet period", meanwhile: func(h *harness) { h.advance(6 * time.Minute) }, want: follows},
		{name: "push inside its quiet period", want: afterQuiet},
		{name: "magnum paused", meanwhile: func(h *harness) {
			h.advance(6 * time.Minute)
			if err := h.st.SetKV(h.ctx, KVDaemonPaused, "1"); err != nil {
				h.t.Fatal(err)
			}
		}, want: notYet},
		{name: "small delta, delta_check off", minLines: 30, noCheck: true, files: rubyMixed, want: notYet},
		{name: "small delta: a delta check", minLines: 30, files: rubyMixed, want: checkQuiet},
		{name: "small delta: a delta check, magnum paused", minLines: 30, files: rubyMixed, meanwhile: func(h *harness) {
			if err := h.st.SetKV(h.ctx, KVDaemonPaused, "1"); err != nil {
				h.t.Fatal(err)
			}
		}, want: notYet},
		{name: "quiet hours", meanwhile: func(h *harness) { h.cfg.Daemon.QuietHours = "00:00-23:59" }, want: notYet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { h.cfg.Daemon.RereviewMinLines, h.cfg.Daemon.DeltaCheck = tc.minLines, !tc.noCheck })
			var meanwhile func()
			if tc.meanwhile != nil {
				meanwhile = func() { tc.meanwhile(h) }
			}
			if got := pushDuringReview(t, h, tc.files, meanwhile); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("notes = %q, want %q", got, tc.want)
			}
			evs, _ := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
			i := slices.IndexFunc(evs, func(e store.Event) bool { return e.Kind == "round.review_noted" })
			if i < 0 {
				t.Fatal("no round.review_noted event")
			}
			wantEvent := "not reviewed yet"
			switch tc.want {
			case checkQuiet:
				wantEvent = "a short check of that commit follows"
			case follows, afterQuiet:
				wantEvent = "re-review follows"
			}
			if !strings.Contains(evs[i].Message, wantEvent) {
				t.Errorf("event %q lacks %q", evs[i].Message, wantEvent)
			}
		})
	}
}
