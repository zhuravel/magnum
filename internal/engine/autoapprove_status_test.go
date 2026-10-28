package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// The status comment's words for each class of refusal, as alice (the
// login magnum approves as in these tests).
const (
	aliceWorkflows   = "Not approved as alice: the PR changes its own CI workflows or actions, so alice must review and approve it in person."
	aliceAgentConfig = "Not approved as alice: the PR changes the instructions or configuration of the AI review agents, so alice must review " +
		"and approve it in person."
	aliceUnheard = "Not approved as alice: one of Magnum's reviewers did not finish on this head, and Magnum approves only a complete " +
		"review. The next review, or a review by alice, decides."
	aliceCIRuns     = "Approval as alice waits for the checks of this head to pass."
	aliceCIFails    = "Approval as alice waits for the checks of this head to pass. Some fail now."
	aliceStopped    = "Magnum stopped approving this PR as alice. A review by alice decides."
	alicePostFailed = "The approval as alice did not post. Magnum told alice."
)

// aliceWorkflowFiles are the files of a PR that edits its CI workflow.
var aliceWorkflowFiles = []string{"app/models/order.rb", ".github/workflows/ci.yml"}

// approvesAsAlice has the talkable watch auto-approve as alice, a gh
// identity of that login whose GitHub is the fake's, with the review
// status comment on. The PRs the tests open are bob-rev's: alice's own PR
// is no approval of hers.
func approvesAsAlice(h *harness) {
	statusOn(h)
	h.cfg.Identities = append(h.cfg.Identities, config.Identity{Name: "alice", Kind: "gh", Login: "alice"})
	h.cfg.Watches[0].AutoApproveAs = "alice"
	h.gh.postAs = "alice"
	live := h.d.GitHub
	h.d.GitHub = func(id string) GitHub {
		if id == "alice" {
			return h.gh
		}
		return live(id)
	}
}

// bobRevs is PR #2 as spec, authored by bob-rev.
func bobRevs(spec prSpec) prSpec {
	spec.n, spec.author = 2, "bob-rev"
	return spec
}

// reviewedFor opens PR #2 (bobRevs) after a first sync, ticks until
// magnum's review of its head posted, and twice more, so the status
// comment shows what auto-approval decided at the round's end.
func (h *harness) reviewedFor(spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, bobRevs(spec))
	h.tick()
	h.reviewTo(2, spec.head)
	h.flush()
	h.flush()
}

// moveTo has the fake GitHub list PR #2 as spec (bobRevs), updated now,
// next to the baseline.
func (h *harness) moveTo(spec prSpec) {
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, bobRevs(spec))
}

// statusLines are the lines of PR #2's status comment below its header.
func (h *harness) statusLines() []string {
	h.t.Helper()
	body := h.statusComment(2).body
	_, rest, ok := strings.Cut(body, "\n**Magnum review status**")
	if !ok {
		h.t.Fatalf("no status header:\n%s", body)
	}
	lines := strings.Split(rest, "\n")
	return lines[1:]
}

// wantApprovalLine fails unless PR #2's status comment is the posted line
// and, under it, want.
func (h *harness) wantApprovalLine(want string) {
	h.t.Helper()
	lines := h.statusLines()
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Review of `") || lines[1] != want {
		h.t.Fatalf("status comment lines = %q, want the posted line and %q", lines, want)
	}
}

// wantNoApprovalLine fails unless PR #2's status comment is the posted line
// alone.
func (h *harness) wantNoApprovalLine() {
	h.t.Helper()
	if lines := h.statusLines(); len(lines) != 1 || !strings.HasPrefix(lines[0], "Review of `") {
		h.t.Fatalf("status comment lines = %q, want the posted line alone", lines)
	}
}

// On talkable#12100 the PR edited .github/workflows/ci.yml, which magnum
// never approves, and the author saw a clean review and no approval. The
// status comment says under the posted line why, and who must approve: the
// login magnum approves as, as plain text (an @ would notify them on every
// edit), never the path.
func TestACleanReviewOfAWorkflowChangeSaysWhoMustApprove(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
	h.wantRefused(2, workflowsWords)
	h.wantApprovalLine(aliceWorkflows)
	body := h.statusComment(2).body
	for _, bad := range []string{"@alice", ".github/workflows", "approve by hand"} {
		if strings.Contains(body, bad) {
			t.Fatalf("the status comment says %q:\n%s", bad, body)
		}
	}
}

// On talkable#12092 codex-review did not finish ("codex-review (busy)"):
// the line says that one of Magnum's reviewers did not finish, never which
// role nor its status. Here it stays busy through both completions of the
// head (complete_test.go), after which the line is the refusal's.
func TestAReviewThatMissedAReviewerSaysSoWithoutTheRole(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, autoRound{findings: map[string]int{}, verdict: store.VerdictClean, line: "No problems found. LGTM :shipit:",
		missing: []pipeline.MissingReport{{Role: "codex-review", Status: pipeline.ReportBusy}}})
	h.reviewedFor(prSpec{head: "b1"})
	if got := h.autoRefused(2); !strings.Contains(got, "codex-review (busy)") {
		t.Fatalf("held back for %q, want the reviewer it did not hear", got)
	}
	h.wantApprovalLine(aliceUnheard)
	body := h.statusComment(2).body
	for _, bad := range []string{"codex-review", "busy", "claude-review"} {
		if strings.Contains(body, bad) {
			t.Fatalf("the status comment says %q:\n%s", bad, body)
		}
	}
}

// While the head's checks run the line says that the approval waits for
// them; once they pass the approval posts, and one edit takes the line
// away.
func TestTheApprovalLineGoesWhenTheChecksPassAndTheApprovalPosts(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", ci: "PENDING"})
	h.wantApprovalLine(aliceCIRuns)
	edits := h.statusComment(2).edits

	h.moveTo(prSpec{head: "b1", ci: "SUCCESS"})
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval once the checks pass", got)
	}
	h.flush()
	h.flush()
	h.wantNoApprovalLine()
	if n := h.statusComment(2).edits - edits; n != 1 {
		t.Fatalf("edits after the approval = %d, want the one that took the line away", n)
	}
}

// The approval of b1 posted and took the line away; a later round of b2,
// which edits its CI workflow, withdraws it, and the line comes back under
// b2's posted line.
func TestALaterRoundsWithdrawalBringsTheLineBackOnTheNewHead(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", ci: "PENDING"})
	h.wantApprovalLine(aliceCIRuns)
	h.moveTo(prSpec{head: "b1", ci: "SUCCESS"})
	h.flush()
	h.flush()
	h.flush()
	h.wantNoApprovalLine()
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.HeadSHA != "b1" {
		t.Fatalf("approval = %+v", a)
	}

	h.moveTo(prSpec{head: "b2", files: aliceWorkflowFiles})
	h.reviewTo(2, "b2")
	h.flush()
	h.flush()
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum {
		t.Fatalf("approval = %+v, want magnum's withdrawal", a)
	}
	h.wantApprovalLine(aliceWorkflows)
	if lines := h.statusLines(); !strings.HasPrefix(lines[0], "Review of `b2` posted") {
		t.Fatalf("status comment lines = %q, want b2's posted line", lines)
	}
}

// A reason that changes within its class (a second file of the agents'
// configuration the head's session ran without) is a new event and a new
// card, and nothing on GitHub: the comment reads as before.
func TestAnUnchangedClassEditsNothing(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	declined := func(paths ...string) {
		b, _ := json.Marshal(agents.ProjectNote{Head: "b1", At: h.clock.Now(), Files: 1, Compared: true, Paths: paths})
		h.e.setKV(h.ctx, store.KVPRProject(h.pr(2).ID, agents.KindClaude), string(b))
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, bobRevs(prSpec{head: "b1"}))
	h.tick()
	declined(".claude/")
	h.reviewTo(2, "b1")
	h.flush()
	h.flush()
	h.wantRefused(2, "(.claude/)")
	h.wantApprovalLine(aliceAgentConfig)
	edits, calls := h.statusComment(2).edits, h.gh.count("edit_issue_comment:")

	declined(".claude/", ".mcp.json")
	h.flush()
	h.flush()
	h.flush()
	if got := h.autoRefused(2); !strings.Contains(got, "(.claude/, .mcp.json)") {
		t.Fatalf("held back for %q, want the new reason", got)
	}
	if n := len(h.events("review.auto_approve_refused")); n != 2 {
		t.Fatalf("refusal events = %d, want one per reason", n)
	}
	if c := h.statusComment(2); c.edits != edits || h.gh.count("edit_issue_comment:") != calls {
		t.Fatalf("the status comment was edited for a reason of the same class:\n%s", c.body)
	}
	h.wantApprovalLine(aliceAgentConfig)
}

// alice's own comment by hand stops auto-approval of the PR; the line says
// so in the third person, without the hold's words ("you commented on it by
// hand") or the review's link.
func TestAStoppedAutoApprovalSpeaksInTheThirdPerson(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.gh.addReview(2, github.Review{DatabaseID: 40, State: "COMMENTED", AuthorLogin: "alice", AuthorType: "User", Body: "Why this way?",
		CommitOid: "b1", SubmittedAt: h.clock.Now(), URL: prURL2 + "#pullrequestreview-40"})
	h.reviewedFor(prSpec{head: "b1"})
	if hd := h.hold(2); !hd.Held || !strings.Contains(hd.Reason, "you commented") {
		t.Fatalf("hold = %+v", hd)
	}
	h.flush()
	h.wantApprovalLine(aliceStopped)
	body := strings.ToLower(h.statusComment(2).body)
	for _, bad := range []string{"you", "pullrequestreview-40", "commented", "unapprove"} {
		if strings.Contains(body, bad) {
			t.Fatalf("the status comment says %q:\n%s", bad, body)
		}
	}
}

// Once GitHub lists alice's approval of the head (she approved it in
// person), the line goes: nothing waits for her any more.
func TestNoApprovalLineOnceTheLoginApprovedTheHead(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
	h.wantApprovalLine(aliceWorkflows)
	h.moveTo(prSpec{head: "b1", files: aliceWorkflowFiles, reviews: []github.LatestReview{
		{State: "APPROVED", AuthorLogin: "alice", AuthorType: "User", CommitOid: "b1", SubmittedAt: h.clock.Now()}}})
	h.flush()
	h.flush()
	h.wantNoApprovalLine()
	if got := h.autoRefused(2); got != workflowsWords {
		t.Fatalf("held back for %q, want the record standing: her approval alone silences the line", got)
	}
}

// forcedReviewFor opens PR #2 (bobRevs) on a repository its watch does not
// review on its own, asks for its review (magnum review), holds the round
// so that the status comment opens while it runs, and ticks until the
// review posted and twice more.
func (h *harness) forcedReviewFor(spec prSpec) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, bobRevs(spec))
	h.tick()
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.holdRounds()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	h.releaseRound()
	h.reviewTo(2, spec.head)
	h.flush()
	h.flush()
}

// manualTalkable makes the talkable watch's repository a manual one.
func manualTalkable(h *harness) { h.cfg.Watches[0].ManualRepos = []string{"talkable"} }

// skipBobRev puts bob-rev in the watch's auto_approve_skip_authors.
func skipBobRev(h *harness) { h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"bob-rev"} }

// The author lists and the watch's filters say nothing on GitHub (the
// operator kept the rule of 2026-10-16), whichever refusal auto-approval
// recorded: on a PR they hold back, which edits its CI workflow too or whose
// round missed a reviewer (a refusal the gates find before the watch's),
// the status comment reads exactly as on a watch without auto_approve, where
// the same PR on a watch that auto-approves it shows the line.
func TestTheAuthorListsAndFiltersLeaveTheCommentAsWithoutAutoApproval(t *testing.T) {
	reviewed := func(h *harness, _ *autoPlan) { h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles}) }
	forced := func(h *harness, _ *autoPlan) { h.forcedReviewFor(prSpec{head: "b1", files: aliceWorkflowFiles}) }
	forcedUnheard := func(h *harness, plan *autoPlan) {
		plan.set(2, withoutCodexReview)
		h.forcedReviewFor(prSpec{head: "b1"})
	}
	comments := func(t *testing.T, flow func(*harness, *autoPlan), mods ...func(*harness)) []string {
		t.Helper()
		h, plan := newAutoHarness(t, append([]func(*harness){approvesAsAlice}, mods...)...)
		flow(h, plan)
		var out []string
		for _, c := range h.gh.commentsOn(2) {
			out = append(out, c.body)
		}
		return out
	}
	off := func(h *harness) { h.cfg.Watches[0].AutoApprove = nil }
	if got := comments(t, reviewed); len(got) != 1 || !strings.HasSuffix(got[0], "\n"+aliceWorkflows) {
		t.Fatalf("the PR on a watch that auto-approves it: %q, want the line", got)
	}
	for _, tc := range []struct {
		name string
		flow func(*harness, *autoPlan)
		mods []func(*harness)
		bare []func(*harness)
	}{
		{"auto_approve_skip_authors", reviewed, []func(*harness){skipBobRev}, []func(*harness){off}},
		{"auto_approve_authors", reviewed, []func(*harness){func(h *harness) { h.cfg.Watches[0].AutoApproveAuthors = []string{"rev-ann"} }},
			[]func(*harness){off}},
		{"a manual repository's forced round", forced, []func(*harness){manualTalkable}, []func(*harness){manualTalkable, off}},
		{"a manual repository's forced round that missed a reviewer", forcedUnheard, []func(*harness){manualTalkable},
			[]func(*harness){manualTalkable, off}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, bare := comments(t, tc.flow, tc.mods...), comments(t, tc.flow, tc.bare...)
			if len(held) == 0 || !slices.Equal(held, bare) {
				t.Fatalf("status comments:\n%q\nwant those of a watch without auto_approve:\n%q", held, bare)
			}
		})
	}
}

// A stop of auto-approval says nothing either where the author lists or
// the watch's filters keep the PR from it: a hold on a skip-listed
// author's PR (magnum unapprove, or a dismissal of an approval posted
// before the list named them) or on a manual repository's.
func TestAHoldOnAPRTheListsOrFiltersKeepSaysNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*harness)
		flow func(*harness)
	}{
		{"a skip-listed author", skipBobRev, func(h *harness) { h.reviewedFor(prSpec{head: "b1"}) }},
		{"a manual repository", manualTalkable, func(h *harness) { h.forcedReviewFor(prSpec{head: "b1"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, approvesAsAlice, tc.mod)
			tc.flow(h)
			h.wantNoApprovalLine()
			if err := h.st.SetAutoApproveHold(h.ctx, store.AutoApproveHold{PRID: h.pr(2).ID, Held: true,
				Reason: "withdrawn with magnum unapprove", At: h.clock.Now()}); err != nil {
				t.Fatal(err)
			}
			h.flush()
			h.flush()
			h.wantNoApprovalLine()
		})
	}
}

// A PR that turns into a draft, or is muted, on the head its line spoke of
// leaves auto-approval (it no longer decides on it, so its record stands
// as it was): the line goes, though the posted line stays, and does not
// come back while it stays a draft or muted, its checks passing or not. A
// watch that reviews drafts (include_drafts) would review the draft on its
// own, so its filters do not silence the line: the draft does.
func TestAStaleLineGoesOnADraftOrAMutedPR(t *testing.T) {
	toDraft := func(h *harness) { h.moveTo(prSpec{head: "b1", ci: "FAILURE", draft: true}) }
	for _, tc := range []struct {
		name  string
		mod   func(h *harness)
		turn  func(h *harness)
		draft bool
	}{
		{"a draft", nil, toDraft, true},
		{"a draft on a watch that reviews drafts", func(h *harness) { h.cfg.Watches[0].IncludeDrafts = new(true) }, toDraft, true},
		{"muted", nil, func(h *harness) { h.muteRequest(ReqMute, 2) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods := []func(*harness){approvesAsAlice}
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			h, _ := newAutoHarness(t, mods...)
			h.reviewedFor(prSpec{head: "b1", ci: "FAILURE"})
			h.wantApprovalLine(aliceCIFails)
			tc.turn(h)
			h.flush()
			h.flush()
			h.wantNoApprovalLine()
			h.moveTo(prSpec{head: "b1", ci: "SUCCESS", draft: tc.draft})
			h.flush()
			h.flush()
			h.wantNoApprovalLine()
			if got := h.created(); len(got) != 0 {
				t.Fatalf("posted = %q", got)
			}
		})
	}
}

// A reply round on the reviewed head takes the PR out of reviewed while it
// is due and runs (auto-approval waits for it): the line goes meanwhile and
// comes back once the round ends with the review's record unchanged.
func TestAReplyRoundOnTheReviewedHeadHidesTheLineWhileItRuns(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
	h.wantApprovalLine(aliceWorkflows)
	h.advance(10 * time.Minute)
	h.holdRounds()
	before := len(h.rd.all())
	reqPollNoSettle(h, bobRevs(prSpec{head: "b1", files: aliceWorkflowFiles, remarks: []github.Remark{threadReply(h, "bob-rev")}}))
	h.advance(3 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	if ins := h.rd.all(); ins[len(ins)-1].Replies == 0 {
		t.Fatalf("round %d is no reply round", len(ins))
	}
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.pr(2).State; st == store.PRReviewed {
		t.Fatalf("state = %s while the reply round runs", st)
	}
	h.wantNoApprovalLine()
	h.releaseRound()
	h.flush()
	h.flush()
	h.wantState(2, store.PRReviewed)
	h.wantApprovalLine(aliceWorkflows)
}

// A round whose result cannot tell whether something is left to fix (an
// older result: earlier findings open, their priorities not given) says
// nothing, though the gates recorded a refusal of its head's checks before
// auto-approval read its verdict line on GitHub.
func TestAnUnknownVerdictSaysNothingWhateverTheRecord(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, autoRound{findings: map[string]int{}, open: 1, verdict: store.VerdictNonBlocking, line: "Looks fine to me."})
	h.reviewedFor(prSpec{head: "b1", ci: "FAILURE"})
	if got := h.autoRefused(2); got != ciFailsWords {
		t.Fatalf("held back for %q, want the checks' refusal recorded", got)
	}
	h.wantNoApprovalLine()
}

// alice replies on a thread of a PR magnum approved as her: GitHub lists her
// comment as her latest review, auto-approval of the PR stops, and her
// approval stands. The comment says nothing of a stop while magnum's
// approval as her stands on the head.
func TestNoStoppedLineWhileTheApprovalStands(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1"})
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
	h.gh.addReview(2, github.Review{DatabaseID: 41, State: "COMMENTED", AuthorLogin: "alice", AuthorType: "User", Body: "a question",
		CommitOid: "b1", SubmittedAt: h.clock.Now().Add(time.Minute), URL: prURL2 + "#pullrequestreview-41"})
	h.moveTo(prSpec{head: "b1", reviews: []github.LatestReview{
		{State: "COMMENTED", AuthorLogin: "alice", AuthorType: "User", CommitOid: "b1", SubmittedAt: h.clock.Now()}}})
	h.flush()
	h.flush()
	h.flush()
	if hd := h.hold(2); !hd.Held {
		t.Fatalf("hold = %+v, want auto-approval stopped", hd)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v, want it standing", a)
	}
	h.wantNoApprovalLine()
}

// A post that failed for good, recorded as such, says nothing once the
// author lists or the watch's filters keep the PR from auto-approval (here
// after a restart on a changed configuration): auto-approval no longer
// decides on the review, and the record stands as it was.
func TestAFailedPostSaysNothingOnceTheListsOrFiltersKeepThePR(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*harness)
	}{
		{"the author newly skipped", skipBobRev},
		{"the repository newly manual", manualTalkable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, approvesAsAlice)
			h.gh.createErr = fmt.Errorf("post: %w", github.ErrForbidden)
			h.reviewedFor(prSpec{head: "b1"})
			h.wantApprovalLine(alicePostFailed)
			tc.mod(h)
			h.e = New(h.d) // the daemon restarts on the new configuration
			h.startup()
			h.flush()
			h.flush()
			h.wantNoApprovalLine()
		})
	}
}

// A file list GitHub cut short (more files than it lists) and failing
// checks each say their own words.
func TestTheExactWordsOfTheFileListAndTheFailingChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec prSpec
		want string
	}{
		{"more files than GitHub lists", prSpec{head: "b1", files: []string{"app/models/order.rb"}, filesTruncated: true},
			"Not approved as alice: the PR changes more files than Magnum can check, so alice must review and approve it in person."},
		{"failing checks", prSpec{head: "b1", ci: "FAILURE"}, aliceCIFails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, approvesAsAlice)
			h.reviewedFor(tc.spec)
			h.wantApprovalLine(tc.want)
		})
	}
}

// A post a stopped daemon left without its answer on its last attempt,
// which the next daemon finds absent from GitHub's whole list of reviews
// (resumeAutoPost), is the last failed post: the line says so.
func TestALastAttemptTheNextDaemonSettlesAsFailedSaysTheLoginWasTold(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.gh.createErr = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}
	h.reviewedFor(prSpec{head: "b1"})
	a := h.autoApproval(2)
	if a.State != store.AutoFailed || a.Attempts != 1 {
		t.Fatalf("approval = %+v", a)
	}
	h.wantNoApprovalLine()
	if err := h.st.TransitionAutoApproval(h.ctx, a.ID, []string{store.AutoFailed}, store.AutoPosting, func(u *store.AutoApprovalUpdate) {
		u.Set("attempts", autoApproveAttempts) // its last attempt went out, and the daemon stopped before GitHub answered
	}); err != nil {
		t.Fatal(err)
	}
	h.e = New(h.d)
	h.startup()
	h.flush()
	if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts != autoApproveAttempts {
		t.Fatalf("approval = %+v, want its last attempt failed", a)
	}
	h.flush()
	h.wantApprovalLine(alicePostFailed)
}

// A standing approval a later round's gates refuse is dismissed on GitHub
// in the class's words alone, also when the first dismissal fails and a
// later one (after a restart) sends it again, while the operator's
// withdrawal events and toast keep the gate's own words ("(AGENTS.md)").
func TestAWithdrawalKeepsItsDetailLocalAndItsGitHubWordsPublic(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1"})
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
	h.gh.dismissErr = &github.APIError{Op: "dismiss review", Status: 502, Message: "Bad Gateway"}
	h.moveTo(prSpec{head: "b2", files: []string{"app/models/order.rb", "AGENTS.md"}})
	h.reviewTo(2, "b2")
	h.tick()
	const (
		public = "the PR changes the instructions or configuration of the AI review agents, so alice must review and approve it in person"
		local  = "it changes the review agents' instructions or hooks (AGENTS.md)"
	)
	if a := h.autoApproval(2); a.State != store.AutoDismissing || !strings.Contains(a.EndReason, public) || strings.Contains(a.EndReason, "AGENTS.md") {
		t.Fatalf("approval = %+v, want it being withdrawn with the public words", a)
	}
	h.gh.dismissErr = nil
	h.e = New(h.d) // the daemon restarts before the next attempt
	h.startup()
	h.advance(autoApproveRetry)
	h.tick()
	h.flush()
	d := h.ghCalls("dismiss:")
	if len(d) != 2 {
		t.Fatalf("dismissals = %q, want the failed one and its retry", d)
	}
	for _, m := range d {
		if !strings.Contains(m, public) || strings.Contains(m, "AGENTS.md") || strings.Contains(m, "instructions or hooks") {
			t.Fatalf("a dismissal on GitHub says %q", m)
		}
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || strings.Contains(a.EndReason, "AGENTS.md") {
		t.Fatalf("approval = %+v", a)
	}
	begins, withdrawn := h.events("review.auto_approval_withdraw_begin"), h.events("review.auto_approval_withdrawn")
	if len(begins) != 2 || len(withdrawn) != 1 {
		t.Fatalf("begin events = %d, withdrawn events = %d", len(begins), len(withdrawn))
	}
	for _, ev := range append(begins, withdrawn...) {
		if !strings.Contains(ev.Message, local) {
			t.Fatalf("a withdrawal event lost the gate's words: %q", ev.Message)
		}
	}
	if toasts := toastsWith(h, "withdrew your approval: talkable#2"); len(toasts) != 1 || !strings.Contains(toasts[0], local) {
		t.Fatalf("toasts = %q", toasts)
	}
}

// The note a withdrawal keeps for its later attempts (kvAutoWithdrawNote)
// goes with it at each of its ends: GitHub took a later attempt
// (dismissed), refused every one (failed for good), or the PR was merged
// before one went through.
func TestEveryEndOfAWithdrawalDropsItsNote(t *testing.T) {
	failing := &github.APIError{Op: "dismiss review", Status: 502, Message: "Bad Gateway"}
	for _, tc := range []struct {
		name  string
		end   func(h *harness)
		state string
		by    string
	}{
		{"dismissed", func(h *harness) {
			h.gh.dismissErr = nil
			h.advance(autoApproveRetry)
			h.tick()
		}, store.AutoDismissed, store.AutoEndedMagnum},
		{"failed for good", func(h *harness) {
			for range autoApproveAttempts - 1 {
				h.advance(autoApproveRetry)
				h.tick()
			}
		}, store.AutoFailed, store.AutoEndedMagnum},
		{"the PR merged", func(h *harness) {
			h.advance(time.Minute)
			h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list
			h.gh.closed[2] = "MERGED"
			for range 3 {
				h.advance(30 * time.Second)
				h.tick()
			}
		}, store.AutoDismissed, store.AutoEndedClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, approvesAsAlice)
			h.reviewedFor(prSpec{head: "b1"})
			h.gh.dismissErr = failing
			h.moveTo(prSpec{head: "b2", files: []string{"app/models/order.rb", "AGENTS.md"}})
			h.reviewTo(2, "b2")
			h.tick()
			key := kvAutoWithdrawNote(h.pr(2).ID)
			if a := h.autoApproval(2); a.State != store.AutoDismissing {
				t.Fatalf("approval = %+v, want it being withdrawn", a)
			}
			if v, ok := h.kvOf(key); !ok || !strings.Contains(v, "(AGENTS.md)") {
				t.Fatalf("%s = %q, %v: want the gate's words kept for the next attempt", key, v, ok)
			}
			tc.end(h)
			if a := h.autoApproval(2); a.State != tc.state || a.EndedBy != tc.by {
				t.Fatalf("approval = %+v, want %s by %s", a, tc.state, tc.by)
			}
			if v, ok := h.kvOf(key); ok {
				t.Fatalf("%s = %q after the withdrawal ended", key, v)
			}
		})
	}
}

// A repository magnum does not auto-approve, and a login it does not know,
// get no line, whatever the refusal record says.
func TestNoApprovalLineWithoutAutoApproveOrALogin(t *testing.T) {
	t.Run("auto_approve dropped", func(t *testing.T) {
		h, _ := newAutoHarness(t, approvesAsAlice)
		h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
		h.wantApprovalLine(aliceWorkflows)
		h.cfg.Watches[0].AutoApprove = nil
		h.flush()
		h.wantNoApprovalLine()
	})
	t.Run("no login", func(t *testing.T) {
		h, _ := newAutoHarness(t, approvesAsAlice, func(h *harness) { h.cfg.IdentityByName("alice").Login = "" })
		h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
		if got := h.autoRefused(2); got != workflowsWords {
			t.Fatalf("held back for %q", got)
		}
		h.wantNoApprovalLine()
	})
}

// A refusal recorded before the records named a class (an older build's)
// gains it on the next pass without a second event: its reason did not
// change. The line follows.
func TestAnOldRefusalRecordGainsItsClassWithoutASecondEvent(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles})
	h.wantRefused(2, workflowsWords)
	key := KVPRAutoApproveRefused(h.pr(2).ID)
	rec, ok := ParseAutoApproveRefused(mustKV(t, h, key))
	if !ok || rec.Public != string(publicWorkflows) {
		t.Fatalf("record = %+v, want the class %s", rec, publicWorkflows)
	}
	rec.Public = ""
	b, _ := json.Marshal(rec)
	h.e.setKV(h.ctx, key, string(b))
	if strings.Contains(string(b), `"public"`) {
		t.Fatalf("an older record names a class: %s", b)
	}

	h.e = New(h.d) // the daemon restarts on the same registry
	h.startup()
	h.flush()
	got, ok := ParseAutoApproveRefused(mustKV(t, h, key))
	if !ok || got.Public != string(publicWorkflows) || got.Reason != rec.Reason || !got.At.Equal(rec.At) {
		t.Fatalf("record = %+v, want %+v with the class", got, rec)
	}
	h.wantRefused(2, workflowsWords) // one event still
	h.flush()
	h.wantApprovalLine(aliceWorkflows)
}

// GitHub refused the approval for good (403): the line says it did not post
// and that magnum told alice, with no refusal event (the failure's own
// records it). A post that is tried again says nothing yet.
func TestAFinalPostFailureSaysTheLoginWasTold(t *testing.T) {
	t.Run("for good", func(t *testing.T) {
		h, _ := newAutoHarness(t, approvesAsAlice)
		h.gh.createErr = fmt.Errorf("post: %w", github.ErrForbidden)
		h.reviewedFor(prSpec{head: "b1"})
		if a := h.autoApproval(2); a.State != store.AutoFailed {
			t.Fatalf("approval = %+v", a)
		}
		h.wantApprovalLine(alicePostFailed)
		if evs := h.events("review.auto_approve_refused"); len(evs) != 0 {
			t.Fatalf("refusal events = %+v", evs)
		}
		if !h.hasEvent("pr:talkable/talkable#2", "review.auto_approve_failed") {
			t.Fatal("no review.auto_approve_failed event")
		}
	})
	t.Run("tried again", func(t *testing.T) {
		h, _ := newAutoHarness(t, approvesAsAlice)
		h.gh.createErr = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}
		h.reviewedFor(prSpec{head: "b1"})
		if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts >= autoApproveAttempts {
			t.Fatalf("approval = %+v", a)
		}
		h.wantNoApprovalLine()
	})
}

// A standing approval a later clean round's gates refuse is dismissed with
// the class's words, never the gate's own ("it changes the review agents'
// instructions or hooks (AGENTS.md)"); a gate without a class says only
// that magnum no longer approves the PR automatically.
func TestTheWithdrawalMessageUsesThePublicWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		next prSpec
		mod  func(*harness)
		want string
		raw  []string
	}{
		{name: "the agents' instructions", next: prSpec{head: "b2", files: []string{"app/models/order.rb", "AGENTS.md"}},
			want: "the PR changes the instructions or configuration of the AI review agents, so alice must review and approve it in person",
			raw:  []string{"AGENTS.md", "instructions or hooks"}},
		{name: "a gate without a class", next: prSpec{head: "b2"}, mod: func(h *harness) { h.gh.defaultFiles = nil },
			want: "Magnum no longer approves it automatically", raw: []string{"no list of the head's files", "cannot tell"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, approvesAsAlice)
			h.reviewedFor(prSpec{head: "b1"})
			if a := h.autoApproval(2); a.State != store.AutoStanding {
				t.Fatalf("approval = %+v", a)
			}
			if tc.mod != nil {
				tc.mod(h)
			}
			h.moveTo(tc.next)
			h.reviewTo(2, "b2")
			h.tick()
			d := h.ghCalls("dismiss:")
			want := "dismiss:talkable/talkable#2:9001:Magnum's review of b2 found nothing to fix, but " + tc.want +
				"; this automatic approval is withdrawn ([review](" + prURL2 + "#pullrequestreview-502))."
			if len(d) != 1 || d[0] != want {
				t.Fatalf("dismissals = %q\nwant %q", d, want)
			}
			for _, bad := range tc.raw {
				if strings.Contains(d[0], bad) {
					t.Fatalf("the dismissal says %q: %q", bad, d[0])
				}
			}
		})
	}
}

// Whatever the record's reason says (a path, a role and its status, a
// login, an @, the operator's "you", a link), the line says the class's
// fixed words alone, for every class, and a class it does not know (a gate
// added later) says nothing.
func TestApprovalLineNeverCarriesTheRecordsWords(t *testing.T) {
	h, _ := newAutoHarness(t, approvesAsAlice)
	h.reviewedFor(prSpec{head: "b1", ci: "FAILURE"})
	pr := h.pr(2)
	key := KVPRAutoApproveRefused(pr.ID)
	base, ok := ParseAutoApproveRefused(mustKV(t, h, key))
	if !ok || base.Public != string(publicCIFails) {
		t.Fatalf("record = %+v", base)
	}
	const raw = "it changes .github/workflows/ci.yml (codex-review (busy)) for @bob-rev: you commented on it by hand " +
		"(https://github.com/talkable/talkable/pull/2#pullrequestreview-40)"
	bad := []string{".github", "codex-review", "busy", "@", "bob-rev", "pullrequestreview-40", "commented", " you ", "You "}
	classes := []publicRefusal{publicWorkflows, publicAgentConfig, publicTooManyFiles, publicUnheard, publicCIRuns, publicCIFails,
		publicStopped, publicPostFailed}
	for _, class := range append(classes, "a_gate_added_later") {
		rec := base
		rec.Reason, rec.Public = raw, string(class)
		if class == publicStopped {
			rec.Public = string(publicCIFails)
			if err := h.st.SetAutoApproveHold(h.ctx, store.AutoApproveHold{PRID: pr.ID, Held: true, Reason: raw, At: h.clock.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(rec)
		h.e.setKV(h.ctx, key, string(b))
		h.advance(statusEvery)
		h.e.noteStatusComments(h.ctx)
		lines := h.statusLines()
		if class == "a_gate_added_later" {
			if len(lines) != 1 {
				t.Fatalf("%s: status comment lines = %q, want the posted line alone", class, lines)
			}
			continue
		}
		if len(lines) != 2 || lines[1] == "" {
			t.Fatalf("%s: status comment lines = %q, want the posted line and the class's", class, lines)
		}
		for _, w := range bad {
			if strings.Contains(strings.Join(lines, "\n"), w) {
				t.Fatalf("%s: the status comment says %q: %q", class, w, lines)
			}
		}
		if strings.Contains(lines[1], "http") || !strings.Contains(lines[1], "alice") {
			t.Fatalf("%s: the line %q", class, lines[1])
		}
		if class == publicStopped {
			if lines[1] != aliceStopped {
				t.Fatalf("a stop says %q, want %q", lines[1], aliceStopped)
			}
			if err := h.st.SetAutoApproveHold(h.ctx, store.AutoApproveHold{PRID: pr.ID, Held: false, Reason: "resumed", At: h.clock.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
