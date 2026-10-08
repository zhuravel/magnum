package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// autoRefused is why auto-approval holds PR #n back, as the card reads it
// ("" when it does not, or for another head).
func (h *harness) autoRefused(n int) string {
	h.t.Helper()
	pr := h.pr(n)
	v, ok := h.e.getKV(h.ctx, KVPRAutoApproveRefused(pr.ID))
	if !ok {
		return ""
	}
	r, ok := ParseAutoApproveRefused(v)
	if !ok || r.Head != pr.HeadSHA {
		return ""
	}
	return r.Reason
}

// wantRefused fails unless PR #n was approved by nobody, is held back with
// a reason that contains want, and its review.auto_approve_refused event
// says the same, once.
func (h *harness) wantRefused(n int, want string) {
	h.t.Helper()
	if got := h.created(); len(got) != 0 {
		h.t.Fatalf("posted = %q", got)
	}
	if got := h.autoRefused(n); !strings.Contains(got, want) {
		h.t.Fatalf("held back for %q, want %q", got, want)
	}
	var evs []string
	for _, ev := range h.events("review.auto_approve_refused") {
		if strings.Contains(ev.Message, fmt.Sprintf("#%d", n)) {
			evs = append(evs, ev.Message)
		}
	}
	if len(evs) != 1 || !strings.Contains(evs[0], want) {
		h.t.Fatalf("review.auto_approve_refused events = %q, want one with %q", evs, want)
	}
}

// A review whose judge went without a reviewer's report (one that ran and
// wrote none, or was skipped as logged out) approves nothing as the
// operator: the App posted COMMENT, which says nothing to auto-approval. A
// round that heard every reviewer it ran, a continue whose paused round
// never ran some roles included, approves as before.
func TestAReviewThatDidNotHearEveryReviewerIsNotAutoApproved(t *testing.T) {
	h, plan := newAutoHarness(t)
	without := cleanRound
	without.missing = []pipeline.MissingReport{{Role: "codex-review", Status: "login_required"}}
	plan.set(2, without, cleanRound)
	h.reviewedPR(2, "b1")
	h.flush()
	h.flush()
	h.wantRefused(2, "magnum's review did not hear every reviewer: codex-review (login_required)")

	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b2:") {
		t.Fatalf("posted = %q, want the approval of b2", got)
	}
	if got := h.autoRefused(2); got != "" {
		t.Fatalf("still held back: %q", got)
	}
}

// withoutCodexReview is a clean round whose judge went without
// codex-review's report, and heardNothingReason why auto-approval refuses
// its review.
var (
	withoutCodexReview = autoRound{findings: map[string]int{}, verdict: store.VerdictClean, line: "No problems found. LGTM :shipit:",
		missing: []pipeline.MissingReport{{Role: "codex-review", Status: "login_required"}}}
	heardNothingReason = "magnum's review did not hear every reviewer: codex-review (login_required)"
)

// The live case (the 2026-10-08 probe approved b1): a clean round that did
// not hear codex-review is refused, then an author's reply starts a reply
// round, whose judge runs alone, records its own round and posts no
// review. The review auto-approval decides on is still the first round's,
// and so is the record it reads: the next ticks still refuse, with the same
// reason.
func TestAReplyRoundKeepsTheRefusalOfAReviewThatDidNotHearEveryReviewer(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(2, withoutCodexReview)
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, heardNothingReason)

	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{threadReply(h, "alice")}})
	h.advance(3 * time.Minute)
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || ins[1].Replies == 0 {
		t.Fatalf("rounds = %d, want the reply round", len(ins))
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	h.flush()
	h.wantRefused(2, heardNothingReason)
}

// A round of the judge alone that posts a review (a same-head re-review, a
// delta check of a new head) builds on the round before it, whose judge did
// not hear codex-review: its review is refused too.
func TestAJudgeAloneRoundAfterOneThatDidNotHearEveryReviewerIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, head string }{{"a same-head re-review", "b1"}, {"a delta check", "b2"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, plan := newAutoHarness(t)
			judgeAlone := cleanRound
			judgeAlone.judgeAlone = true
			plan.set(2, withoutCodexReview, judgeAlone)
			h.reviewedPR(2, "b1")
			h.flush()
			h.wantRefused(2, heardNothingReason)

			if tc.head == "b1" {
				h.reviewAgain(2)
			} else {
				h.push(2, tc.head)
				h.reviewTo(2, tc.head)
			}
			h.flush()
			h.flush()
			if n := len(h.rd.all()); n != 2 {
				t.Fatalf("rounds = %d, want 2", n)
			}
			if got := h.created(); len(got) != 0 {
				t.Fatalf("posted = %q", got)
			}
			if got := h.autoRefused(2); !strings.Contains(got, "magnum's review did not hear every reviewer: codex-review (login_required") {
				t.Fatalf("held back for %q", got)
			}
			if r, _ := ParseAutoApproveRefused(mustKV(t, h, KVPRAutoApproveRefused(h.pr(2).ID))); r.RunID != "run-2" {
				t.Fatalf("the reason names %s, want the judge-alone round's review (run-2)", r.RunID)
			}
		})
	}
}

// reviewAgain asks for a review of PR #n's head (magnum review), and ticks
// until its round ran.
func (h *harness) reviewAgain(n int) {
	h.t.Helper()
	before := len(h.rd.all())
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: strconv.Itoa(n)}})
	for i := 0; i < 20 && (len(h.rd.all()) == before || h.pr(n).State != store.PRReviewed); i++ {
		h.advance(time.Minute)
		h.tick()
	}
	if r := h.request(id); r.State != store.RequestDone || len(h.rd.all()) == before {
		h.t.Fatalf("review request: %s %q, rounds %d", r.State, deref(r.Result), len(h.rd.all()))
	}
	h.wantState(n, store.PRReviewed)
}

// The card's reason names the review it was decided on: a later round
// that leaves something to fix (here on the same head) clears what held
// the review before it back.
func TestTheCardsRefusalGoesWithALaterBlockingRound(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(2, cleanRound, blockingRound)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "FAILURE"})
	h.tick()
	h.reviewTo(2, "b1")
	h.flush()
	h.wantRefused(2, "its head's checks fail")

	h.reviewAgain(2)
	h.flush()
	if got := h.autoRefused(2); got != "" {
		t.Fatalf("the card still says %q after a review that found something to fix", got)
	}
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
}

// A forced round on a PR its watch would not review on its own (a
// manual repository's, a bot's, one skip_paths leaves out) approves
// nothing as the operator, however clean.
func TestAForcedCleanRoundOnAManualRepositoryIsNotAutoApproved(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) { h.cfg.Watches[0].ManualRepos = []string{"talkable"} })
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	wantManual(t, h, 2)
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.reviewTo(2, "b1")
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("review request: %s %q", r.State, deref(r.Result))
	}
	h.flush()
	h.wantRefused(2, "its watch would not review it on its own: manual repository (manual_repos)")
}

// A PR Codex flagged after its clean review (here while its checks still
// ran) is never approved as the operator: its watch's classify rejects it,
// the checks passing or not.
func TestACodexFlaggedPRIsNotAutoApproved(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "PENDING"})
	h.tick()
	h.reviewTo(2, "b1")
	h.flush()
	if err := h.e.setCodexFlag(h.ctx, h.pr(2).ID, CodexFlag{Kind: "codex", Head: "b1", At: h.clock.Now(), By: "magnum codex-flag"}); err != nil {
		t.Fatal(err)
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "SUCCESS"})
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
	if got := h.autoRefused(2); !strings.Contains(got, "its watch would not review it on its own: Codex flagged it as a possible cybersecurity risk") {
		t.Fatalf("held back for %q", got)
	}
}

// Auto-approval waits while the head's checks fail or run, and approves
// once they pass.
func TestAutoApprovalWaitsForTheHeadsChecks(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "FAILURE"})
	h.tick()
	h.reviewTo(2, "b1")
	h.flush()
	h.wantRefused(2, "its head's checks fail")

	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "PENDING"})
	h.flush()
	if got := h.autoRefused(2); !strings.Contains(got, "its head's checks have not finished") {
		t.Fatalf("held back for %q", got)
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "SUCCESS"})
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval once the checks pass", got)
	}
	if got := h.autoRefused(2); got != "" {
		t.Fatalf("still held back: %q", got)
	}
}

// A PR that changes what steers the review agents (an AGENTS.md or
// CLAUDE.md at any depth and in any case, .claude/, .codex/, .mcp.json), or
// whose head's sessions ran without its project config, is never approved
// as the operator, and neither is one whose file list magnum cannot read
// whole. A as the operator still approves it: they decide by hand.
func TestAutoApprovalRefusesAPRThatChangesTheAgentsInstructions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     []string
		truncated bool
		declined  string // a kind whose head's session ran without the PR's project config
		// declinedPaths are the paths of its config the record names (none:
		// a record written before it named them)
		declinedPaths []string
		want          string
	}{
		{name: "a nested CLAUDE.md", files: []string{"app/x.rb", "devops/k8s/claude.MD"},
			want: "it changes the review agents' instructions or hooks (devops/k8s/claude.MD)"},
		{name: "AGENTS.override.md", files: []string{"lib/AGENTS.override.md"}, want: "(lib/AGENTS.override.md)"},
		{name: "CLAUDE.local.md", files: []string{"CLAUDE.local.md"}, want: "(CLAUDE.local.md)"},
		{name: "claude settings", files: []string{".Claude/settings.json"}, want: "(.Claude/settings.json)"},
		{name: "codex config", files: []string{".codex/config.toml"}, want: "(.codex/config.toml)"},
		{name: "mcp servers", files: []string{".mcp.json"}, want: "(.mcp.json)"},
		{name: "an unsafe path", files: []string{"docs/a`b\n/AGENTS.md"}, want: "(AGENTS.md)"},
		{name: "an unsafe settings path", files: []string{".claude/hooks/$(x).sh"}, want: "(.claude/)"},
		{name: "a declined project config", files: []string{"app/x.rb"}, declined: agents.KindClaude, declinedPaths: []string{".claude/"},
			want: "(.claude/)"},
		{name: "an older declined record", files: []string{"app/x.rb"}, declined: agents.KindCodex, want: "(.codex/"},
		{name: "a cut file list", files: []string{"app/x.rb"}, truncated: true,
			want: "magnum cannot tell whether it changes the review agents' instructions or hooks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t)
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: tc.files, filesTruncated: tc.truncated})
			h.tick()
			if tc.declined != "" {
				b, _ := json.Marshal(agents.ProjectNote{Head: "b1", At: h.clock.Now(), Files: 1, Compared: true, Paths: tc.declinedPaths})
				h.e.setKV(h.ctx, store.KVPRProject(h.pr(2).ID, tc.declined), string(b))
			}
			h.reviewTo(2, "b1")
			h.flush()
			h.wantRefused(2, tc.want)
			if strings.Contains(h.autoRefused(2), "\n") {
				t.Fatalf("the reason carries the PR's path as is: %q", h.autoRefused(2))
			}

			if r := h.verdictAs(ReqApprove, 2, "zhuravel"); r.State != store.RequestDone {
				t.Fatalf("A as the operator: %s %q", r.State, deref(r.Result))
			}
			if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q", got)
			}
		})
	}
}

// A result that gives the earlier findings' priorities decides without the
// review's body: an open P1 with no new finding is no approval, and GitHub
// is not even asked; open P3s alone are clean whatever the verdict line.
func TestAnOpenP1IsNotApprovedWithoutReadingTheBody(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(2, autoRound{findings: map[string]int{}, open: 1, openBy: map[string]int{"P1": 1}, verdict: store.VerdictNonBlocking,
		line: "No problems found. LGTM :shipit:"})
	plan.set(3, autoRound{findings: map[string]int{}, open: 2, openBy: map[string]int{"P3": 2}, verdict: store.VerdictNonBlocking,
		line: "Looks fine to me."})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "b3"})
	h.tick()
	h.reviewTo(2, "b2")
	h.reviewTo(3, "b3")
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b3:") {
		t.Fatalf("posted = %q, want only #3's approval", got)
	}
	if calls := h.ghCalls("all_reviews:talkable/talkable#2"); len(calls) != 0 {
		t.Fatalf("read #2's reviews: %q", calls)
	}
}

// roundBlocks reads the earlier findings by priority when the result has
// them, and the verdict line only when it does not.
func TestRoundBlocksReadsTheOpenFindingsByPriority(t *testing.T) {
	for name, tc := range map[string]struct {
		sum           store.ReviewSummary
		body          string
		blocks, known bool
	}{
		"an open P1":            {store.ReviewSummary{Open: 1, OpenCounts: &[4]int{0, 1, 0, 0}}, "", true, true},
		"an open P2":            {store.ReviewSummary{Open: 1, OpenCounts: &[4]int{0, 0, 1, 0}, Verdict: store.VerdictNonBlocking}, "", true, true},
		"open P3s":              {store.ReviewSummary{Open: 2, OpenCounts: &[4]int{0, 0, 0, 2}, Verdict: store.VerdictNonBlocking}, "", false, true},
		"open P3s, line fix":    {store.ReviewSummary{Open: 2, OpenCounts: &[4]int{0, 0, 0, 2}}, "Fix 1 problem before merging.", true, true},
		"open, no priorities":   {store.ReviewSummary{Open: 1}, "", false, false},
		"open, a clean line":    {store.ReviewSummary{Open: 1}, "No blocking problems.", false, true},
		"nothing open or found": {store.ReviewSummary{}, "", false, true},
	} {
		if blocks, known := roundBlocks(tc.sum, tc.body); blocks != tc.blocks || known != tc.known {
			t.Errorf("%s: blocks %v known %v, want %v %v", name, blocks, known, tc.blocks, tc.known)
		}
	}
}

// A PR with more reviews than magnum reads is not approved: what it cannot
// read may be the operator's review by hand.
func TestAnIncompleteReviewListApprovesNothing(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.reviewsCut = map[int]bool{2: true}
	h.reviewedPR(2, "b1")
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
	evs := h.events("review.auto_approve_refused")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "magnum cannot read all of its reviews on GitHub") {
		t.Fatalf("events = %+v", evs)
	}
	if n := len(h.ghCalls("all_reviews:talkable/talkable#2")); n != 1 {
		t.Fatalf("read the reviews %d times, want once until the PR moves", n)
	}
}

// approvedThenBlocked approves PR #2 at b1, then reviews b2 with a
// blocking round, whose withdrawal GitHub answers with err; the PR's
// timeline lists dismissals.
func approvedThenBlocked(t *testing.T, err error, takes bool, dismissals ...github.ReviewDismissal) *harness {
	t.Helper()
	h, plan := newAutoHarness(t)
	plan.set(2, cleanRound, blockingRound)
	h.reviewedPR(2, "b1")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
	h.gh.mu.Lock()
	h.gh.dismissals = map[int][]github.ReviewDismissal{2: dismissals}
	h.gh.mu.Unlock()
	h.gh.dismissErr, h.gh.dismissTakes = err, takes
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	return h
}

var errUnprocessable = &github.APIError{Op: "dismiss review", Status: 422, Message: "Unprocessable Entity"}

// A withdrawal GitHub refuses with a 422 reads the review again: one
// dismissed meanwhile (or gone) ends the row instead of a retry every 5
// minutes for good, as a dismissal by someone (its timeline does not say
// by whom).
func TestARefusedWithdrawalOfADismissedReviewEndsTheRow(t *testing.T) {
	h := approvedThenBlocked(t, errUnprocessable, true)
	if d := h.ghCalls("dismiss:"); len(d) != 1 {
		t.Fatalf("dismissals = %q", d)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedSomeone || a.EndedAt == nil {
		t.Fatalf("approval = %+v", a)
	}
	h.advance(time.Hour)
	h.tick()
	if d := h.ghCalls("dismiss:"); len(d) != 1 {
		t.Fatalf("dismissed again: %q", d)
	}
	if toasts := toastsWith(h, "could not withdraw"); len(toasts) != 0 {
		t.Fatalf("toasts = %q", toasts)
	}
}

// A withdrawal GitHub refuses (422) because the approval was dismissed
// meanwhile counts as that dismissal, as one magnum finds before it
// withdraws does: the operator's, or anyone else's, stops auto-approval of
// the PR; GitHub's on a push does not. magnum withdrew nothing: no toast
// says it did.
func TestARefusedWithdrawalOfAnApprovalDismissedByHandStopsAutoApproval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dismissal github.ReviewDismissal
		by        string
		stop      string // the hold's reason ("" = none)
	}{
		{"by the operator", github.ReviewDismissal{ReviewID: 9001, Actor: "zhuravel"}, store.AutoEndedOperator, "you dismissed it (review 9001)"},
		{"by someone else", github.ReviewDismissal{ReviewID: 9001, Actor: "alice"}, store.AutoEndedSomeone, "alice dismissed it (review 9001)"},
		{"by a push", github.ReviewDismissal{ReviewID: 9001, ByPush: true, Commit: "b2"}, store.AutoEndedPush, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := approvedThenBlocked(t, errUnprocessable, true, tc.dismissal)
			if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != tc.by || a.EndedAt == nil {
				t.Fatalf("approval = %+v, want ended by %s", a, tc.by)
			}
			if hd := h.hold(2); hd.Held != (tc.stop != "") || hd.Reason != tc.stop {
				t.Fatalf("hold = %+v, want %q", hd, tc.stop)
			}
			h.flush()
			if toasts := toastsWith(h, "withdrew your approval"); len(toasts) != 0 {
				t.Fatalf("toasts = %q", toasts)
			}
		})
	}
}

// raceGH is the fake GitHub whose changes request, posted as magnum's App,
// lets a round's end in (autoApproveRound) once GitHub took it and before
// magnum records it: at once when nothing holds auto-approval, else as soon
// as it is free.
type raceGH struct {
	*fakeGH
	h    *harness
	prID int64
	wg   sync.WaitGroup
}

func (g *raceGH) CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error) {
	if event != "REQUEST_CHANGES" {
		return g.fakeGH.CreateReview(ctx, owner, repo, number, commitID, event, body)
	}
	g.mu.Lock()
	g.created = append(g.created, event+"@"+commitID+":"+body)
	id := int64(9000 + len(g.created))
	g.mu.Unlock()
	url := fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", owner, repo, number, id)
	g.addReview(number, github.Review{DatabaseID: id, State: "CHANGES_REQUESTED", Body: body, URL: url, CommitOid: commitID,
		AuthorLogin: "talkable", AuthorType: "Bot", SubmittedAt: g.h.clock.Now()})
	if e := g.h.e; e.autoMu.TryLock() {
		e.autoMu.Unlock()
		e.autoApproveRound(g.h.ctx, g.prID)
	} else {
		g.wg.Go(func() { e.autoApproveRound(g.h.ctx, g.prID) })
	}
	return github.RESTReview{ID: id, UserLogin: "talkable[bot]", UserType: "Bot", State: "CHANGES_REQUESTED", CommitID: commitID, HTMLURL: url}, nil
}

// `magnum request-changes` at the moment a round's end approves the PR as
// the operator: the approval does not stand next to the changes request
// (here the round's auto-approval waits for it, then finds the PR's
// latest review is the changes request).
func TestRequestChangesAndARoundsAutoApprovalAtOnceLeaveNoApprovalStanding(t *testing.T) {
	race := &raceGH{}
	h, _ := newAutoHarness(t, func(h *harness) {
		h.cfg.Watches[0].AutoApprove = nil // until the changes request
		race.fakeGH, race.h = h.gh, h
		h.d.GitHub = func(string) GitHub { return race }
	})
	h.reviewedPR(2, "b1")
	h.flush()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q", got)
	}
	h.cfg.Watches[0].AutoApprove = []string{"talkable"}
	race.prID = h.pr(2).ID
	if _, err := h.e.requestVerdict(h.ctx, VerdictPayload{PRTarget: PRTarget{Ref: "2"}}, "REQUEST_CHANGES"); err != nil {
		t.Fatal(err)
	}
	race.wg.Wait()
	h.flush()
	if a, ok, err := h.st.LiveAutoApproval(h.ctx, h.pr(2).ID); err != nil || ok {
		t.Fatalf("an approval as the operator stands next to the changes request: %+v %v", a, err)
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "REQUEST_CHANGES@b1:") {
		t.Fatalf("posted = %q", got)
	}
}

// A manual verdict GitHub took that the registry could not record ends its
// begin event with a failure that says it was posted, for a changes request
// as the PR's identity and for an approval as the operator.
func TestAManualVerdictPostedButNotRecordedWritesItsFailure(t *testing.T) {
	for _, tc := range []struct {
		name, kind, as, trigger string
	}{
		{"a changes request", ReqRequestChanges, "",
			"CREATE TRIGGER no_record BEFORE UPDATE OF last_review_id ON prs WHEN NEW.last_review_id >= 9000 BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END"},
		{"an approval as the operator", ReqApprove, "zhuravel",
			"CREATE TRIGGER no_record BEFORE UPDATE OF state ON auto_approvals WHEN NEW.state = 'standing' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, verdictClients, func(h *harness) { h.cfg.Watches[0].AutoApprove = nil })
			h.reviewedPR(2, "b1")
			h.flush()
			if _, err := h.st.DB().ExecContext(h.ctx, tc.trigger); err != nil {
				t.Fatal(err)
			}
			if r := h.verdictAs(tc.kind, 2, tc.as); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "but not recorded") {
				t.Fatalf("verdict: %s %q", r.State, deref(r.Result))
			}
			begun, failed := h.events("review.manual_verdict_begin"), h.events("review.manual_verdict_failed")
			if len(begun) != 1 || len(failed) != 1 || !strings.Contains(failed[0].Message, "but could not record it") ||
				!strings.Contains(failed[0].Message, prURL2+"#pullrequestreview-9001") {
				t.Fatalf("begin %+v, failed %+v", begun, failed)
			}
			if evs := h.events("review.manual_verdict"); len(evs) != 0 {
				t.Fatalf("ok events %+v", evs)
			}
		})
	}
}

// A withdrawal GitHub keeps refusing is tried three times, then its row
// ends failed, with an event and one toast.
func TestAWithdrawalGitHubKeepsRefusingEndsFailedAfterThreeAttempts(t *testing.T) {
	h := approvedThenBlocked(t, errUnprocessable, false)
	for range 4 {
		h.advance(autoApproveRetry)
		h.tick()
	}
	if d := h.ghCalls("dismiss:"); len(d) != autoApproveAttempts {
		t.Fatalf("dismissals = %d, want %d", len(d), autoApproveAttempts)
	}
	a := h.autoApproval(2)
	if a.State != store.AutoFailed || a.EndedBy != store.AutoEndedMagnum || !strings.Contains(a.Error, "422") {
		t.Fatalf("approval = %+v", a)
	}
	var gaveUp []string
	for _, ev := range h.events("review.auto_approval_withdraw_failed") {
		if strings.Contains(ev.Message, "gave up") {
			gaveUp = append(gaveUp, ev.Message)
		}
	}
	if len(gaveUp) != 1 {
		t.Fatalf("give-up events = %q", gaveUp)
	}
	h.flush()
	if toasts := toastsWith(h, "could not withdraw your approval of talkable#2"); len(toasts) != 1 {
		t.Fatalf("toasts = %q (all %q)", toasts, h.nh.shown())
	}
	if live, ok, err := h.st.LiveAutoApproval(h.ctx, h.pr(2).ID); err != nil || ok {
		t.Fatalf("a live approval stays: %+v %v %v", live, ok, err)
	}
}

// The approval of a PR that was merged (or closed) is history: its row ends
// instead of being read on every tick for good.
func TestTheApprovalOfAMergedPREnds(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list
	h.gh.closed[2] = "MERGED"
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	if pr := h.pr(2); pr.GHState != store.GHMerged {
		t.Fatalf("gh state = %s", pr.GHState)
	}
	a := h.autoApproval(2)
	if a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedClosed || a.EndedAt == nil {
		t.Fatalf("approval = %+v", a)
	}
	if live, err := h.st.LiveAutoApprovals(h.ctx); err != nil || len(live) != 0 {
		t.Fatalf("live = %+v, %v", live, err)
	}
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("dismissed a merged PR's approval: %q", d)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "review.auto_approval_ended") {
		t.Error("no review.auto_approval_ended event")
	}
}

// `magnum request-changes` withdraws the operator's standing automatic
// approval before it posts the changes request, and both write begin and
// ok events.
func TestRequestChangesWithdrawsTheOperatorsAutoApproval(t *testing.T) {
	h, _ := newAutoHarness(t, verdictClients)
	h.reviewedPR(2, "b1")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
	r := h.verdictAs(ReqRequestChanges, 2, "")
	if r.State != store.RequestDone {
		t.Fatalf("request changes: %s %q", r.State, deref(r.Result))
	}
	var order []string
	for _, c := range h.ghCalls("") {
		if strings.HasPrefix(c, "dismiss:") || strings.HasPrefix(c, "review:") {
			order = append(order, c)
		}
	}
	want := []string{"review:talkable/talkable#2:APPROVE", "dismiss:talkable/talkable#2:9001:" + RequestChangesMessage,
		"review:talkable/talkable#2:REQUEST_CHANGES"}
	if !slices.Equal(order, want) {
		t.Fatalf("calls = %q\nwant %q", order, want)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedOperator {
		t.Fatalf("approval = %+v", a)
	}
	for _, kind := range []string{"review.auto_approval_withdraw_begin", "review.auto_approval_withdrawn", "review.manual_verdict_begin",
		"review.manual_verdict"} {
		if !h.hasEvent("pr:talkable/talkable#2", kind) {
			t.Errorf("no %s event", kind)
		}
	}
}

// A changes request GitHub refuses writes a failure event, after its begin.
func TestAFailedManualVerdictWritesItsFailure(t *testing.T) {
	h := newHarness(t, verdictClients)
	h.reviewedPR(2, "b1")
	h.gh.createErr = errUnprocessable
	if r := h.verdictAs(ReqRequestChanges, 2, ""); r.State != store.RequestFailed {
		t.Fatalf("request changes: %s %q", r.State, deref(r.Result))
	}
	for _, kind := range []string{"review.manual_verdict_begin", "review.manual_verdict_failed"} {
		if !h.hasEvent("pr:talkable/talkable#2", kind) {
			t.Errorf("no %s event", kind)
		}
	}
}
