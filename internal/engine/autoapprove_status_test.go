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
// role nor its status.
func TestAReviewThatMissedAReviewerSaysSoWithoutTheRole(t *testing.T) {
	h, plan := newAutoHarness(t, approvesAsAlice)
	plan.set(2, autoRound{findings: map[string]int{}, verdict: store.VerdictClean, line: "No problems found. LGTM :shipit:",
		missing: []pipeline.MissingReport{{Role: "codex-review", Status: pipeline.ReportBusy}}})
	h.reviewedFor(prSpec{head: "b1"})
	h.wantRefused(2, "codex-review (busy)")
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
}

// The author lists and the watch's filters say nothing on GitHub (the
// operator kept the rule of 2026-10-16): on a PR they hold back, which
// edits its CI workflow too, the status comment reads exactly as on a
// watch without auto_approve, where the same PR on a watch that
// auto-approves it shows the line.
func TestTheAuthorListsAndFiltersLeaveTheCommentAsWithoutAutoApproval(t *testing.T) {
	reviewed := func(h *harness) { h.reviewedFor(prSpec{head: "b1", files: aliceWorkflowFiles}) }
	forced := func(h *harness) { // a manual repository's PR, reviewed because magnum review asked for it
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		h.tick()
		h.open(prSpec{n: 1, head: "base1"}, bobRevs(prSpec{head: "b1", files: aliceWorkflowFiles}))
		h.tick()
		h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.holdRounds() // the comment opens while the round runs
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
		h.reviewTo(2, "b1")
		h.flush()
		h.flush()
	}
	manual := func(h *harness) { h.cfg.Watches[0].ManualRepos = []string{"talkable"} }
	comments := func(flow func(*harness), mods ...func(*harness)) []string {
		h, _ := newAutoHarness(t, append([]func(*harness){approvesAsAlice}, mods...)...)
		flow(h)
		var out []string
		for _, c := range h.gh.commentsOn(2) {
			out = append(out, c.body)
		}
		return out
	}
	off := func(h *harness) { h.cfg.Watches[0].AutoApprove = nil }
	if got := comments(reviewed); len(got) != 1 || !strings.HasSuffix(got[0], "\n"+aliceWorkflows) {
		t.Fatalf("the PR on a watch that auto-approves it: %q, want the line", got)
	}
	for _, tc := range []struct {
		name string
		flow func(*harness)
		mods []func(*harness)
		bare []func(*harness)
	}{
		{"auto_approve_skip_authors", reviewed, []func(*harness){func(h *harness) { h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"bob-rev"} }},
			[]func(*harness){off}},
		{"auto_approve_authors", reviewed, []func(*harness){func(h *harness) { h.cfg.Watches[0].AutoApproveAuthors = []string{"rev-ann"} }},
			[]func(*harness){off}},
		{"a manual repository's forced round", forced, []func(*harness){manual}, []func(*harness){manual, off}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, bare := comments(tc.flow, tc.mods...), comments(tc.flow, tc.bare...)
			if len(held) == 0 || !slices.Equal(held, bare) {
				t.Fatalf("status comments:\n%q\nwant those of a watch without auto_approve:\n%q", held, bare)
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
