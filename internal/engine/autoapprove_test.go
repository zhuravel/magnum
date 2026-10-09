package engine

import (
	"encoding/json"
	"errors"
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
	"github.com/zhuravel/magnum/internal/textx"
)

// autoRound is what a scripted round posts: its findings by priority, the
// earlier ones still open (openBy: by priority, as the skill writes them
// now), its verdict and the verdict line of its review; missing are the
// reports its judge went without (the pipeline's record of the round), and
// judgeAlone says the round ran no reviewer (a delta check's, a same-head
// re-review's).
type autoRound struct {
	findings   map[string]int
	open       int
	openBy     map[string]int
	verdict    string
	line       string
	missing    []pipeline.MissingReport
	judgeAlone bool
}

var (
	cleanRound    = autoRound{findings: map[string]int{}, verdict: store.VerdictClean, line: "No problems found. LGTM :shipit:"}
	optionalRound = autoRound{findings: map[string]int{"P3": 1}, verdict: store.VerdictNonBlocking, line: "No blocking problems. 1 optional: 1 P3."}
	fixRound      = autoRound{findings: map[string]int{"P2": 1}, verdict: store.VerdictNonBlocking, line: "Fix 1 problem before merging."}
	blockingRound = autoRound{findings: map[string]int{"P1": 1}, verdict: store.VerdictBlocking, line: "Blocking: 1 problem must be fixed before merging."}
)

// autoPlan is what each PR's rounds post, in turn (the last one repeats).
type autoPlan struct {
	mu     sync.Mutex
	rounds map[int][]autoRound
	done   map[int]int
}

func (p *autoPlan) set(n int, rs ...autoRound) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rounds[n] = rs
}

func (p *autoPlan) next(n int) autoRound {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done[n]++
	rs := p.rounds[n]
	if len(rs) == 0 {
		return cleanRound
	}
	return rs[min(p.done[n], len(rs))-1]
}

// newAutoHarness is a harness whose talkable watch auto-approves as the
// operator (the gh identity zhuravel), whose fake GitHub lists what is
// posted as zhuravel, and whose rounds post what plan says for their PR
// (clean by default): each records its judge run with its result, as the
// pipeline does, and its record of the reports its judge went without, and
// lists its review, with its verdict line, as magnum's App. A reply round
// records its judge's, which runs alone, and ends replied: no review. Every
// PR changes one application file (the Details' file list). No toast for
// every review.
func newAutoHarness(t *testing.T, mods ...func(*harness)) (*harness, *autoPlan) {
	t.Helper()
	plan := &autoPlan{rounds: map[int][]autoRound{}, done: map[int]int{}}
	setup := func(h *harness) {
		h.cfg.Herdr.ToastEveryReview = false
		h.cfg.Watches[0].AutoApprove = []string{"talkable"}
		h.cfg.Watches[0].AutoApproveAs = "zhuravel"
		h.gh.postAs = "zhuravel"
		h.gh.clock = h.clock.Now
		h.gh.defaultFiles = []string{"app/models/order.rb"}
		h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
			n := len(h.rd.all())
			if in.Replies > 0 {
				if err := recordAutoRound(h, in, n, autoRound{judgeAlone: true}); err != nil {
					return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
				}
				return pipeline.RoundResult{Outcome: pipeline.OutcomeReplied, Round: n, TargetSHA: in.TargetSHA, JudgePromptedAt: h.clock.Now(),
					Replies: []pipeline.PostedReply{{CommentID: 101, ID: 800, Kind: "rebuttal"}}, ThreadsRead: true}, nil
			}
			r := plan.next(in.PR.Number)
			id := int64(500 + n)
			url := fmt.Sprintf("https://github.com/talkable/talkable/pull/%d#pullrequestreview-%d", in.PR.Number, id)
			event := map[string]string{store.VerdictBlocking: "CHANGES_REQUESTED"}[r.verdict]
			if event == "" {
				event = "COMMENTED" // the App's no_findings_event is COMMENT
			}
			var open any = r.open
			if r.openBy != nil {
				open = r.openBy
			}
			result, _ := json.Marshal(map[string]any{"event": event, "verdict": r.verdict, "findings": r.findings,
				"previous_findings": map[string]any{"open": open}})
			if err := recordAutoRound(h, in, n, r); err != nil {
				return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
			}
			if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES (?, ?, ?, 'codex-judge', 'initial', ?, ?, 'talkable[bot]', 'verified', 'posted', ?, ?, 'p', ?, ?)`,
				"run-"+strconv.Itoa(n), in.PR.ID, n, in.TargetSHA, in.PR.Identity, id, url, string(result), store.FormatTime(h.clock.Now())); err != nil {
				return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
			}
			h.gh.addReview(in.PR.Number, github.Review{DatabaseID: id, State: event, URL: url, CommitOid: in.TargetSHA,
				AuthorLogin: "talkable", AuthorType: "Bot", SubmittedAt: h.clock.Now(),
				Body: r.line + "\n\n<!-- magnum:run=run-" + strconv.Itoa(n) + " head=" + textx.ShortSHA(in.TargetSHA) + " -->"})
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: n, ReviewID: id, ReviewURL: url, Event: event,
				ReviewCommit: in.TargetSHA, TargetSHA: in.TargetSHA, Findings: r.findings}, nil
		}
	}
	return newHarness(t, append([]func(*harness){setup}, mods...)...), plan
}

// recordAutoRound keeps the PR's record of round n's missing reports, as
// its judge's prompt does: claude-review's report and r.missing, none in a
// round of the judge alone.
func recordAutoRound(h *harness, in pipeline.RoundInput, n int, r autoRound) error {
	var reports []agents.Report
	if !r.judgeAlone {
		reports = append(reports, agents.Report{Role: string(agents.RoleClaude), Path: "claude-review.md", Status: pipeline.ReportOK})
		for _, m := range r.missing {
			reports = append(reports, agents.Report{Role: m.Role, Status: m.Status, Missing: true})
		}
	}
	_, err := pipeline.RecordMissingReports(h.ctx, h.st, in.PR.ID, n, in.TargetSHA, reports)
	return err
}

// created lists the reviews posted through the fake GitHub:
// "<event>@<sha>:<body>".
func (h *harness) created() []string {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	return slices.Clone(h.gh.created)
}

// calls lists the fake GitHub's calls that start with prefix.
func (h *harness) ghCalls(prefix string) []string {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	var out []string
	for _, c := range h.gh.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// autoApproval is PR #n's latest automatic approval.
func (h *harness) autoApproval(n int) store.AutoApproval {
	h.t.Helper()
	pr := h.pr(n)
	all, err := h.st.LatestAutoApprovals(h.ctx, []int64{pr.ID})
	if err != nil {
		h.t.Fatal(err)
	}
	a, ok := all[pr.ID]
	if !ok {
		h.t.Fatalf("#%d has no automatic approval", n)
	}
	return a
}

// hold is PR #n's auto-approval hold (zero when none).
func (h *harness) hold(n int) store.AutoApproveHold {
	h.t.Helper()
	hd, _, err := h.st.AutoApproveHold(h.ctx, h.pr(n).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return hd
}

// reviewTo ticks until PR #n's review covers head.
func (h *harness) reviewTo(n int, head string) {
	h.t.Helper()
	for i := 0; i < 20 && (h.pr(n).State != store.PRReviewed || deref(h.pr(n).ReviewedSHA) != head); i++ {
		h.advance(5 * time.Minute)
		h.tick()
	}
	if pr := h.wantState(n, store.PRReviewed); deref(pr.ReviewedSHA) != head {
		h.t.Fatalf("#%d reviewed %s, want %s", n, deref(pr.ReviewedSHA), head)
	}
}

// push moves PR #n (next to the baseline #1) to head, updated now.
func (h *harness) push(n int, head string) {
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
}

func autoBody(sha, reviewURL string) string {
	return "Auto-approved: magnum's review of `" + sha + "` found no blocking problems ([review](" + reviewURL + ")).\n\n" +
		"<!-- magnum:auto-approval head=" + sha + " -->"
}

const prURL2 = "https://github.com/talkable/talkable/pull/2"

// A PR whose review of its head found nothing to fix is approved as the
// operator, once: on the head magnum reviewed, with the default body and
// the marker magnum recognises its approvals by, recorded as standing with
// begin and ok events, and toasted with its link.
func TestACleanReviewIsApprovedAsTheOperator(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	want := []string{"APPROVE@b1:" + autoBody("b1", prURL2+"#pullrequestreview-501")}
	if got := h.created(); !slices.Equal(got, want) {
		t.Fatalf("posted = %q\nwant %q", got, want)
	}
	a := h.autoApproval(2)
	if a.State != store.AutoStanding || a.ReviewID != 9001 || a.HeadSHA != "b1" || a.Login != "zhuravel" || a.RunID != "run-1" ||
		a.SourceReviewID != 501 || a.PostedAt == nil {
		t.Fatalf("approval = %+v", a)
	}
	for _, kind := range []string{"review.auto_approve_begin", "review.auto_approved"} {
		if !h.hasEvent("pr:talkable/talkable#2", kind) {
			t.Errorf("no %s event", kind)
		}
	}
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 1 {
		t.Fatalf("approved again: %q", got)
	}
	toasts := toastsWith(h, "approved as you: talkable#2")
	if len(toasts) != 1 || !strings.Contains(toasts[0], prURL2) {
		t.Fatalf("toasts = %q (all %q)", toasts, h.nh.shown())
	}
}

// Nothing is approved as the operator unless the watch names the
// repository.
func TestAutoApprovalIsOffUnlessTheWatchOptsIn(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) { h.cfg.Watches[0].AutoApprove = nil })
	h.reviewedPR(2, "b1")
	h.flush()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted without auto_approve: %q", got)
	}
	if n := h.ghCalls("all_reviews:"); len(n) != 0 {
		t.Fatalf("read the reviews without auto_approve: %q", n)
	}
}

// Only a review that leaves nothing to fix before merging is approved: no
// P0, P1 or P2 finding, still-open earlier ones included. P3 findings are
// optional; still-open earlier findings, whose priorities the result does
// not give, count by the review's verdict line, and a line magnum does not
// know is not a clean one.
func TestOnlyAReviewWithNothingToFixIsAutoApproved(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(3, optionalRound)
	plan.set(4, fixRound)
	plan.set(5, blockingRound)
	plan.set(6, autoRound{findings: map[string]int{}, open: 1, verdict: store.VerdictNonBlocking, line: "Fix 1 problem before merging."})
	plan.set(7, autoRound{findings: map[string]int{}, open: 1, verdict: store.VerdictNonBlocking,
		line: "**Re-review a1a1a1a → b7b7b7b:** No blocking problems. 1 optional: 1 P3."})
	plan.set(8, autoRound{findings: map[string]int{}, open: 1, verdict: store.VerdictNonBlocking, line: "Looks fine to me."})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // #1 baseline
	specs := []prSpec{{n: 1, head: "base1"}}
	for n := 2; n <= 8; n++ {
		specs = append(specs, prSpec{n: n, head: fmt.Sprintf("b%d", n)})
	}
	h.open(specs...)
	h.tick()
	for n := 2; n <= 8; n++ {
		h.reviewTo(n, fmt.Sprintf("b%d", n))
	}
	h.tick()
	var heads []string
	for _, c := range h.created() {
		heads = append(heads, strings.SplitN(strings.TrimPrefix(c, "APPROVE@"), ":", 2)[0])
	}
	slices.Sort(heads)
	if want := []string{"b2", "b3", "b7"}; !slices.Equal(heads, want) {
		t.Fatalf("approved %q, want %q", heads, want)
	}
}

// The PRs magnum never approves as the operator, whatever its review said.
func TestAutoApprovalRefusesPRsItMustLeaveAlone(t *testing.T) {
	clean := &store.ReviewSummary{RunID: "r1", ReviewID: 501, SHA: "h1", Verdict: store.VerdictClean}
	ok := func() autoFacts {
		return autoFacts{Login: "zhuravel", Sum: clean, PR: store.PR{GHState: store.GHOpen, State: store.PRReviewed, HeadSHA: "h1",
			ReviewedSHA: new("h1"), LastReviewID: new(int64(501)), AuthorLogin: new("alice"), AuthorType: new("User")}}
	}
	if why := autoApproveRefusal(ok()); why != "" {
		t.Fatalf("a clean reviewed PR is refused: %s", why)
	}
	cases := map[string]func(*autoFacts){
		"draft":             func(f *autoFacts) { f.PR.IsDraft = true },
		"the operator's":    func(f *autoFacts) { f.PR.AuthorLogin = new("Zhuravel") },
		"closed":            func(f *autoFacts) { f.PR.GHState = store.GHClosed },
		"merged":            func(f *autoFacts) { f.PR.GHState = store.GHMerged },
		"head moved":        func(f *autoFacts) { f.PR.HeadSHA = "h2" },
		"never reviewed":    func(f *autoFacts) { f.PR.ReviewedSHA = nil },
		"a round in flight": func(f *autoFacts) { f.PR.State = store.PRReviewing },
		"due again":         func(f *autoFacts) { f.PR.State = store.PRRereviewPending },
		"muted":             func(f *autoFacts) { f.PR.Muted = true },
		"no posted round":   func(f *autoFacts) { f.Sum = nil },
		"a manual verdict":  func(f *autoFacts) { f.PR.LastReviewID = new(int64(777)) },
		"held":              func(f *autoFacts) { f.Hold = &store.AutoApproveHold{Held: true, Reason: "you reviewed it by hand"} },
		"blocking": func(f *autoFacts) {
			f.Sum = &store.ReviewSummary{RunID: "r1", ReviewID: 501, Counts: [4]int{0, 0, 1, 0}}
		},
	}
	for name, edit := range cases {
		f := ok()
		edit(&f)
		if why := autoApproveRefusal(f); why == "" {
			t.Errorf("%s: not refused", name)
		}
	}
	f := ok()
	f.PR.AuthorLogin, f.PR.AuthorType = new("zhuravel"), new("Bot") // an App named after the operator
	if why := autoApproveRefusal(f); why != "" {
		t.Errorf("a bot named zhuravel[bot] is refused: %s", why)
	}
	f = ok()
	f.Hold = &store.AutoApproveHold{Held: false, Reason: "resumed"}
	if why := autoApproveRefusal(f); why != "" {
		t.Errorf("a resumed PR is refused: %s", why)
	}
}

// What the operator's own reviews on GitHub say: magnum's reviews posted as
// them (its rounds', its automatic approvals) are not theirs; any other
// review of theirs, since a resume, is a review by hand; their approval of
// the head (its own, by its marker) means there is nothing to post; a
// pending review of theirs waits.
func TestTheOperatorsWordOnGitHub(t *testing.T) {
	since := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return since.Add(time.Duration(h) * time.Hour) }
	rev := func(id int64, login, typ, state, commit, body string, h int) github.Review {
		return github.Review{DatabaseID: id, AuthorLogin: login, AuthorType: typ, State: state, CommitOid: commit, Body: body, SubmittedAt: at(h),
			URL: fmt.Sprintf("u%d", id)}
	}
	magnumRound := rev(1, "zhuravel", "User", "CHANGES_REQUESTED", "h0", "Blocking: 1 problem.\n<!-- magnum:run=r1 head=h0 -->", 1)
	ownApproval := rev(2, "zhuravel", "User", "APPROVED", "h1", autoBody("h1", "u"), 2)
	others := []github.Review{rev(3, "alice", "User", "CHANGES_REQUESTED", "h1", "no", 3), rev(4, "zhuravel", "Bot", "COMMENTED", "h1", "x", 3)}

	w := operatorWord(append([]github.Review{magnumRound, ownApproval}, others...), "zhuravel", "h1", time.Time{})
	if w.hand != nil || w.pending || w.approved == nil || w.approved.DatabaseID != 2 || !w.approvedByMagnum {
		t.Fatalf("magnum's own reviews: %+v", w)
	}
	for state, want := range map[string]string{"CHANGES_REQUESTED": "requested changes", "APPROVED": "approved", "COMMENTED": "commented",
		"DISMISSED": "reviewed"} {
		hand := rev(5, "Zhuravel", "User", state, "h0", "by hand", 4)
		w := operatorWord([]github.Review{magnumRound, hand}, "zhuravel", "h1", time.Time{})
		if w.hand == nil || w.hand.DatabaseID != 5 || !strings.Contains(w.handReason(), want) {
			t.Errorf("%s by hand: %+v (%q)", state, w, w.handReason())
		}
	}
	verdict := rev(6, "zhuravel", "User", "APPROVED", "h1", "Approved after magnum's review.\n<!-- magnum:verdict=APPROVE head=h1 -->", 5)
	if w := operatorWord([]github.Review{verdict}, "zhuravel", "h1", time.Time{}); w.hand == nil {
		t.Error("a verdict the operator posted through magnum is not theirs")
	}
	pending := rev(7, "zhuravel", "User", "PENDING", "h1", "", 0)
	pending.SubmittedAt = time.Time{}
	if w := operatorWord([]github.Review{pending}, "zhuravel", "h1", time.Time{}); !w.pending || w.hand != nil {
		t.Errorf("a pending review: %+v", w)
	}
	old := rev(8, "zhuravel", "User", "COMMENTED", "h0", "earlier", -1)
	if w := operatorWord([]github.Review{old}, "zhuravel", "h1", since); w.hand != nil {
		t.Errorf("a review before the resume counts: %+v", w)
	}
}

// The verdict line of magnum's review says whether something must be fixed
// before merging, past a re-review's lead.
func TestReviewVerdictLine(t *testing.T) {
	for body, want := range map[string]verdictKind{
		"Blocking: 2 problems must be fixed before merging.\n\n- …":          verdictBlocking,
		"**Re-review 9be04f2 → 4c1d2e3:**\nFix 1 problem before merging.":    verdictFix,
		"**Re-review 9be04f2 → 4c1d2e3:** No blocking problems. 1 optional.": verdictOptional,
		"No problems found. LGTM :shipit:":                                   verdictClean,
		"**Re-review a → b:** No new problems since 9be04f2. LGTM :shipit:":  verdictClean,
		"Looks fine.":                         verdictUnknown,
		"":                                    verdictUnknown,
		"Fixed the typo.\nNo problems found.": verdictUnknown,
		"\n\nNo blocking problems.\n<!-- magnum:run=r1 head=h1 -->": verdictOptional,
		"No problems found in the merged commits. :shipit:":         verdictClean,
		"Fix the wording?": verdictUnknown,
		"Blocking: 1 problem must be fixed before merging.\nNo problems found.": verdictBlocking,
	} {
		if got := reviewVerdictLine(body); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

// New commits alone leave the approval standing (the next round decides),
// and a clean review of them leaves it standing without a second one.
func TestNewCommitsAndACleanRoundLeaveTheAutoApprovalStanding(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	h.push(2, "b2")
	h.tick()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("a push dismissed it: %q", d)
	}
	h.reviewTo(2, "b2")
	h.flush()
	if got := h.created(); len(got) != 1 {
		t.Fatalf("approved again after a clean round: %q", got)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.HeadSHA != "b1" {
		t.Fatalf("approval = %+v", a)
	}
}

// A later round that leaves something to fix (a blocking finding, on any
// head) withdraws the operator's automatic approval with the reason and a
// link to that review, and toasts it; the next clean round approves again.
func TestABlockingRoundWithdrawsTheAutoApprovalAndACleanOneApprovesAgain(t *testing.T) {
	h, plan := newAutoHarness(t)
	plan.set(2, cleanRound, blockingRound, cleanRound)
	h.reviewedPR(2, "b1")
	h.tick()
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	want := "dismiss:talkable/talkable#2:9001:magnum's review of b2 found blocking problems; this automatic approval is withdrawn ([review](" +
		prURL2 + "#pullrequestreview-502))."
	if d := h.ghCalls("dismiss:"); !slices.Equal(d, []string{want}) {
		t.Fatalf("dismissals = %q\nwant %q", d, want)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum || a.EndedAt == nil {
		t.Fatalf("approval = %+v", a)
	}
	for _, kind := range []string{"review.auto_approval_withdraw_begin", "review.auto_approval_withdrawn"} {
		if !h.hasEvent("pr:talkable/talkable#2", kind) {
			t.Errorf("no %s event", kind)
		}
	}
	h.flush()
	if toasts := toastsWith(h, "withdrew your approval: talkable#2"); len(toasts) != 1 || !strings.Contains(toasts[0], prURL2) {
		t.Fatalf("toasts = %q", h.nh.shown())
	}
	if hd := h.hold(2); hd.Held {
		t.Fatalf("magnum's own withdrawal stopped auto-approval: %+v", hd)
	}

	h.push(2, "b3")
	h.reviewTo(2, "b3")
	h.tick()
	got := h.created()
	if len(got) != 2 || !strings.HasPrefix(got[1], "APPROVE@b3:") {
		t.Fatalf("posted = %q, want a second approval of b3", got)
	}
	if d := h.ghCalls("dismiss:"); len(d) != 1 {
		t.Fatalf("dismissed again: %q", d)
	}
}

// The operator's own review by hand stops auto-approval of the PR for good:
// recorded with why, shown on the card; a review magnum is asked for does
// not lift it, `magnum unapprove --resume` does (counting only what the
// operator does from then on).
func TestTheOperatorsOwnReviewStopsAutoApproval(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.addReview(2, github.Review{DatabaseID: 40, State: "COMMENTED", AuthorLogin: "zhuravel", AuthorType: "User", Body: "Why this way?",
		CommitOid: "b1", SubmittedAt: h.clock.Now(), URL: prURL2 + "#pullrequestreview-40"})
	h.reviewedPR(2, "b1")
	h.tick()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("approved despite the operator's own review: %q", got)
	}
	hd := h.hold(2)
	if !hd.Held || !strings.Contains(hd.Reason, "commented") {
		t.Fatalf("hold = %+v", hd)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "review.auto_approve_stopped") {
		t.Error("no review.auto_approve_stopped event")
	}

	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.reviewTo(2, "b1")
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("review request: %s %q", r.State, deref(r.Result))
	}
	if got := h.created(); len(got) != 0 {
		t.Fatalf("a forced review lifted the hold: %q", got)
	}

	h.advance(time.Minute)
	if res := h.unapprove(2, true); !strings.Contains(res, "resumed") {
		t.Fatalf("resume: %q", res)
	}
	h.tick()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("after the resume: %q", got)
	}
}

// The operator's changes request by hand stops it too (W27's "lift your ✗"
// shows instead), and their approval of the head leaves nothing to post.
func TestTheOperatorsVerdictByHandStopsAutoApproval(t *testing.T) {
	for state, n := range map[string]int{"CHANGES_REQUESTED": 2, "APPROVED": 3} {
		t.Run(state, func(t *testing.T) {
			h, _ := newAutoHarness(t)
			h.gh.addReview(n, github.Review{DatabaseID: 40, State: state, AuthorLogin: "zhuravel", AuthorType: "User", Body: "",
				CommitOid: "b1", SubmittedAt: h.clock.Now()})
			h.reviewedPR(n, "b1")
			h.tick()
			if got := h.created(); len(got) != 0 {
				t.Fatalf("posted = %q", got)
			}
			if hd := h.hold(n); !hd.Held {
				t.Fatalf("hold = %+v", hd)
			}
		})
	}
}

// The operator dismissing one of magnum's automatic approvals by hand stops
// auto-approval of the PR; GitHub dismissing it as stale on a push does not.
func TestTheOperatorDismissingAnAutoApprovalStopsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       github.ReviewDismissal
		by      string
		stopped bool
	}{
		{"by the operator", github.ReviewDismissal{ReviewID: 9001, Actor: "zhuravel"}, store.AutoEndedOperator, true},
		{"by someone else", github.ReviewDismissal{ReviewID: 9001, Actor: "bob-rev"}, store.AutoEndedSomeone, true},
		{"on a push", github.ReviewDismissal{ReviewID: 9001, Actor: "alice", ByPush: true, Commit: "b2"}, store.AutoEndedPush, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t)
			h.reviewedPR(2, "b1")
			h.tick()
			h.gh.setReviewState(2, 9001, "DISMISSED")
			h.gh.dismissals = map[int][]github.ReviewDismissal{2: {tc.d}}
			h.push(2, "b2")
			h.tick()
			a := h.autoApproval(2)
			if a.State != store.AutoDismissed || a.EndedBy != tc.by {
				t.Fatalf("approval = %+v", a)
			}
			if hd := h.hold(2); hd.Held != tc.stopped {
				t.Fatalf("hold = %+v, want held %v", hd, tc.stopped)
			}
			h.reviewTo(2, "b2")
			h.tick()
			if got := h.created(); (len(got) == 2) == tc.stopped {
				t.Fatalf("posted = %q (stopped %v)", got, tc.stopped)
			}
			if d := h.ghCalls("dismiss:"); len(d) != 0 {
				t.Fatalf("magnum dismissed: %q", d)
			}
		})
	}
}

// unapprove asks the daemon to withdraw (or, resume, to allow again) PR
// #n's automatic approval and returns its answer.
func (h *harness) unapprove(n int, resume bool) string {
	h.t.Helper()
	id := h.enqueue(ReqUnapprove, UnapprovePayload{PRTarget: PRTarget{Ref: strconv.Itoa(n)}, Resume: resume})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("unapprove #%d: %s %q", n, r.State, deref(r.Result))
	}
	return deref(r.Result)
}

// `magnum unapprove` withdraws the automatic approval as the operator and
// stops auto-approval of the PR; without one standing it only stops it.
func TestUnapproveWithdrawsTheAutoApprovalAndStopsIt(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	res := h.unapprove(2, false)
	if !strings.Contains(res, "withdrew") || !strings.Contains(res, "talkable/talkable#2") {
		t.Fatalf("unapprove = %q", res)
	}
	want := "dismiss:talkable/talkable#2:9001:" + UnapproveMessage
	if d := h.ghCalls("dismiss:"); !slices.Equal(d, []string{want}) {
		t.Fatalf("dismissals = %q, want %q", d, want)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedOperator {
		t.Fatalf("approval = %+v", a)
	}
	if hd := h.hold(2); !hd.Held || !strings.Contains(hd.Reason, "unapprove") {
		t.Fatalf("hold = %+v", hd)
	}
	h.flush()
	if got := h.created(); len(got) != 1 {
		t.Fatalf("approved again after unapprove: %q", got)
	}
	if res := h.unapprove(2, false); !strings.Contains(res, "no automatic approval") {
		t.Fatalf("a second unapprove = %q", res)
	}
	if d := h.ghCalls("dismiss:"); len(d) != 1 {
		t.Fatalf("dismissed twice: %q", d)
	}
	if res := h.unapprove(2, true); !strings.Contains(res, "resumed") {
		t.Fatalf("resume = %q", res)
	}
	if hd := h.hold(2); hd.Held {
		t.Fatalf("hold after the resume = %+v", hd)
	}
}

// A post GitHub refused is tried again after a while, never twice at once,
// and one whose answer was lost is found on GitHub instead of posted again.
func TestAFailedAutoApprovalIsRetriedOnce(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.createErr = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}
	h.reviewedPR(2, "b1")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoFailed || !strings.Contains(a.Error, "Bad Gateway") {
		t.Fatalf("approval = %+v", a)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "review.auto_approve_failed") {
		t.Error("no review.auto_approve_failed event")
	}
	h.advance(time.Minute)
	h.tick()
	if n := len(h.ghCalls("review:")); n != 1 {
		t.Fatalf("retried at once: %d posts", n)
	}
	h.gh.createTakes = true // GitHub takes the next one, its answer is lost
	h.advance(autoApproveRetry)
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoFailed {
		t.Fatalf("approval after a lost answer = %+v", a)
	}
	h.gh.createErr = nil
	h.advance(autoApproveRetry)
	h.tick()
	if n := len(h.ghCalls("review:")); n != 2 {
		t.Fatalf("posted %d times, want the lost one found instead of a third", n)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.ReviewID != 9001 {
		t.Fatalf("approval = %+v", a)
	}
}

// A refusal (403) is not retried for that review.
func TestARefusedAutoApprovalIsNotRetried(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.createErr = fmt.Errorf("post: %w", github.ErrForbidden)
	h.reviewedPR(2, "b1")
	h.tick()
	h.advance(time.Hour)
	h.tick()
	if n := len(h.ghCalls("review:")); n != 1 {
		t.Fatalf("a refusal was retried: %d posts", n)
	}
	if !errors.Is(h.gh.createErr, github.ErrForbidden) {
		t.Fatal("test setup")
	}
}

// A PR magnum approved as the operator no longer toasts that it needs the
// operator's approval.
func TestAnAutoApprovedPRDoesNotNeedTheOperator(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", gate: gateRequired})
	h.tick()
	h.reviewTo(2, "b1")
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 1 {
		t.Fatalf("posted = %q", got)
	}
	if toasts := toastsWith(h, "needs your approval"); len(toasts) != 0 {
		t.Fatalf("toasts = %q", toasts)
	}
}
