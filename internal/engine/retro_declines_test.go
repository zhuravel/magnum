package engine

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// retroSkill is the skill copy a judge prompt names (config.SnapshotPrompts:
// <state>/skill/<12 hex>/SKILL.md).
const retroSkill = "0123456789ab"

// ownRetroThread is a thread magnum's App started on retroShaA at
// app/models/gift.rb:line of PR n, in review reviewID, answered by replies
// (login, body); each reply carries SECRET-BODY, which no event may.
func ownRetroThread(n int, id int64, line int, reviewID int64, replies ...[2]string) github.Thread {
	th := retroThread(id, "talkable", "app/models/gift.rb", line, retroShaA)
	root := &th.Comments[0]
	root.AuthorType, root.ReviewID = "Bot", reviewID
	root.Body = "**[P2] Gift cards skip the balance check**\n\nThe balance is read after the charge."
	root.URL = "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n) + "#discussion_r" + itoa(id)
	for i, r := range replies {
		th.Comments = append(th.Comments, github.ThreadComment{ID: id*10 + int64(i), AuthorLogin: r[0], AuthorType: "User",
			Body: "SECRET-BODY " + r[1], CreatedAt: root.CreatedAt.Add(time.Duration(i+1) * time.Hour),
			URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n) + "#discussion_r" + itoa(id*10+int64(i))})
	}
	return th
}

// seedOwnThreads adds magnum's answered threads to PR n's: o301 declined,
// o302 fixed, both in the review magnum posted (retroPR: 9000+n).
func seedOwnThreads(h *harness, n int, extra ...github.Thread) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	review := int64(9000 + n)
	h.gh.threads[n] = append(h.gh.threads[n],
		ownRetroThread(n, 301, 12, review, [2]string{"alice", "Won't fix: the balance is locked by the caller."}),
		ownRetroThread(n, 302, 30, review, [2]string{"alice", "> **[P2] Gift cards skip the balance check**\n\nfixed in abc1234"}))
	h.gh.threads[n] = append(h.gh.threads[n], extra...)
}

// withJudgePrompt gives PR n's judge run a prompt that names the skill copy.
func withJudgePrompt(h *harness, n int) {
	h.t.Helper()
	text := "[$magnum-review](/state/skill/" + retroSkill + "/SKILL.md) Read /state/skill/" + retroSkill + "/SKILL.md first."
	if err := h.st.UpdateRun(h.ctx, "r-"+strconv.Itoa(n), func(u *store.RunUpdate) { u.Set("prompt_text", text) }); err != nil {
		h.t.Fatal(err)
	}
}

// threadsClassifier answers the candidates as the default fake does and
// each own thread with answers[id] (fixed when absent).
func threadsClassifier(answers map[string]learn.ThreadItem) *fakeClassifier {
	return &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) {
		if _, err := (&fakeClassifier{}).Classify(context.Background(), job); err != nil {
			return ClassifyResult{}, err
		}
		b, err := os.ReadFile(job.OutputPath)
		if err != nil {
			return ClassifyResult{}, err
		}
		var out learn.Output
		if err := json.Unmarshal(b, &out); err != nil {
			return ClassifyResult{}, err
		}
		for _, th := range job.Threads {
			it, ok := answers[th.ID]
			if !ok {
				it = learn.ThreadItem{Outcome: store.DeclineFixed}
			}
			it.ID = th.ID
			out.Threads = append(out.Threads, it)
		}
		b, _ = json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}}
}

// wrongDecline is the classifier's answer for a finding the author
// declined and magnum got wrong.
var wrongDecline = learn.ThreadItem{Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong,
	Lesson: "When the caller already holds the row lock, do not report a read after the write as a race.",
	Scope:  store.MissScopeGeneral, Match: []string{"balance", "race|lock"}}

// declinesOf reads PR prID's declines by their root comment id.
func (h *harness) declinesOf(prID int64) map[int64]store.Decline {
	h.t.Helper()
	ds, err := h.st.Declines(h.ctx, store.DeclineFilter{PRID: prID})
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[int64]store.Decline{}
	for _, d := range ds {
		out[d.CommentID] = d
	}
	return out
}

// TestTheRetroStoresADeclineForEachAnsweredFinding: magnum's threads that
// the author answered go to the classifier in candidates.json (with the
// replies) beside the other reviewers' comments, and each answer becomes a
// declines row: the outcome, the verdict and lesson of a declined finding,
// the run that posted the review, its findings row and skill copy, never a
// reply's text in an event.
func TestTheRetroStoresADeclineForEachAnsweredFinding(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	withJudgePrompt(h, 7)
	if err := h.st.RecordFindings(h.ctx, "r-7", pr.ID, 1, []store.Finding{
		{FindingID: "F1", Severity: "P2", Path: "app/models/coupon.rb", Line: 81, Verdict: store.FindingPosted}, // t105 is caught (retroFinding)
		{FindingID: "F2", Title: "Gift cards skip the balance check", Severity: "P2", Path: "app/models/gift.rb", Line: 13, Verdict: store.FindingPosted},
	}); err != nil {
		t.Fatal(err)
	}
	fs, err := h.st.FindingsByPR(h.ctx, pr.ID)
	if err != nil || len(fs) != 2 || fs[1].FindingID != "F2" {
		t.Fatalf("findings = %+v, %v", fs, err)
	}
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 || len(fc.jobs[0].Threads) != 2 {
		t.Fatalf("classifier jobs = %d, threads %v", len(fc.jobs), fc.jobs)
	}
	cands, err := readCandidates(fc.jobs[0].CandidatesPath)
	if err != nil || len(cands.Threads) != 2 || cands.Threads[0].ID != "o301" || len(cands.Threads[0].Replies) != 1 ||
		!strings.Contains(cands.Threads[0].Replies[0].Body, "Won't fix") || cands.Threads[0].Replies[0].Author != "alice" {
		t.Fatalf("candidates file threads = %+v, %v", cands.Threads, err)
	}
	ds := h.declinesOf(pr.ID)
	if len(ds) != 2 {
		t.Fatalf("declines = %+v, want o301 and o302", ds)
	}
	d := ds[301]
	if d.Outcome != store.DeclineDeclined || d.Verdict != store.VerdictWrong || d.Lesson != wrongDecline.Lesson ||
		d.Scope != store.MissScopeGeneral || !slices.Equal(d.Match, wrongDecline.Match) || d.State != store.MissNew {
		t.Fatalf("o301's answer = %+v", d)
	}
	if d.RunID != "r-7" || d.ReviewID != 9007 || d.FindingRowID == nil || *d.FindingRowID != fs[1].ID || d.SkillSHA != retroSkill ||
		d.Title != "Gift cards skip the balance check" || d.Priority != "P2" || d.Path != "app/models/gift.rb" || d.Line != 12 ||
		d.ReviewedSHA != retroShaA || d.SourceURL != "https://github.com/talkable/talkable/pull/7#discussion_r301" {
		t.Fatalf("o301's finding = %+v", d)
	}
	if f := ds[302]; f.Outcome != store.DeclineFixed || f.Verdict != "" || f.Lesson != "" || f.FindingRowID != nil || f.RunID != "r-7" {
		t.Fatalf("o302 = %+v", f)
	}
	// A decline is not a miss: only the other reviewers' comments are.
	if ms := h.misses(pr.ID); len(ms) != 4 {
		t.Fatalf("misses = %d, want the four of the comments", len(ms))
	}
	h.noCommentText("pr:talkable/talkable#7")
	h.noCommentText("")

	// Again: the answer changes, the state stays.
	if _, err := h.st.SetDeclineState(h.ctx, []int64{d.ID}, []string{store.MissNew}, store.MissDismissed); err != nil {
		t.Fatal(err)
	}
	h.requestRetro(RetroPayload{PRs: []int64{pr.ID}, Again: true})
	if again := h.declinesOf(pr.ID)[301]; again.ID != d.ID || again.State != store.MissDismissed || again.Outcome != store.DeclineDeclined {
		t.Fatalf("o301 after another retro = %+v", again)
	}
}

// TestANudgedJudgesDeclineKeepsItsFirstRun: every judge run of a round
// that was nudged (or fell back to another model) carries the round's
// review id, while its findings are recorded under the first and only
// that one's prompt names the skill copy. The decline binds to that run:
// its findings row and skill hash, never the nudge's emptiness.
func TestANudgedJudgesDeclineKeepsItsFirstRun(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	withJudgePrompt(h, 7)
	h.advance(time.Minute)
	nudge, err := h.st.CreateRun(h.ctx, store.Run{ID: "r-7-nudge", PRID: pr.ID, Round: 1, Role: "codex-judge", Kind: store.RunNudge,
		TargetSHA: retroShaA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateRun(h.ctx, nudge.ID, func(u *store.RunUpdate) {
		u.Set("review_id", int64(9007))
		u.Set("review_commit", retroShaA)
		u.Set("prompt_text", "The judge stopped without a result: finish the review.")
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordFindings(h.ctx, "r-7", pr.ID, 1, []store.Finding{
		{FindingID: "F2", Title: "Gift cards skip the balance check", Severity: "P2", Path: "app/models/gift.rb", Line: 13, Verdict: store.FindingPosted},
	}); err != nil {
		t.Fatal(err)
	}
	fs, err := h.st.FindingsByPR(h.ctx, pr.ID)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings = %+v, %v", fs, err)
	}
	h.requestRetro(RetroPayload{})

	d := h.declinesOf(pr.ID)[301]
	if d.RunID != "r-7" || d.SkillSHA != retroSkill || d.FindingRowID == nil || *d.FindingRowID != fs[0].ID {
		t.Fatalf("o301 = run %q, skill %q, findings row %v; want r-7, %s, %d", d.RunID, d.SkillSHA, d.FindingRowID, retroSkill, fs[0].ID)
	}
}

// TestReviewRunsOfBindsAReviewToTheRunWithItsFindings: a review's run is
// the first of its runs (oldest first) with findings rows, else its first,
// so a recovery round that verified an earlier round's review binds to the
// recovery's judge; the skill hash is that run's, else the first of the
// review's runs whose prompt names a skill copy.
func TestReviewRunsOfBindsAReviewToTheRunWithItsFindings(t *testing.T) {
	review := func(id int64) *int64 { return &id }
	skill := func(sha string) string { return "Read /state/skill/" + sha + "/SKILL.md first." }
	runs := []store.Run{
		{ID: "r-1", ReviewID: review(1), PromptText: skill(retroSkill)},
		{ID: "r-1-nudge", ReviewID: review(1), PromptText: "nudge"},
		{ID: "r-2", ReviewID: review(2), PromptText: skill("abcdefabcdef")}, // failed before it recorded findings
		{ID: "r-3", ReviewID: review(2), PromptText: skill("0000000000ff")}, // the recovery: verified, recorded them
		{ID: "r-4", ReviewID: review(4), PromptText: "Read /checkout/skills/magnum-review/SKILL.md first."},
		{ID: "r-4b", ReviewID: review(4), PromptText: skill("111111111111")}, // a later run of the review that names a copy
		{ID: "r-5"}, // posted nothing
	}
	findings := []store.Finding{{RunID: "r-1", FindingID: "F1"}, {RunID: "r-3", FindingID: "F1"}}
	got := reviewRunsOf(runs, findings)
	want := map[int64]reviewRun{1: {ID: "r-1", SkillSHA: retroSkill}, 2: {ID: "r-3", SkillSHA: "0000000000ff"},
		4: {ID: "r-4", SkillSHA: "111111111111"}}
	if !maps.Equal(got, want) {
		t.Fatalf("reviewRunsOf = %+v, want %+v", got, want)
	}
}

// TestAMissingContextDeclineBecomesARepoMissForTheCuration: a finding the
// author declined because the repository does it otherwise (verdict
// missing_context, scope repo) is also stored as a miss for the notes
// (class miss, scope repo, its reviewer the author who answered), marks
// the repository, and reaches the curator in misses.json like any repo
// miss.
func TestAMissingContextDeclineBecomesARepoMissForTheCuration(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": {Outcome: store.DeclineDeclined, Verdict: store.VerdictMissingContext,
		Lesson: "Gift card charges run inside the ledger transaction, so a balance read after the charge is safe.",
		Scope:  store.MissScopeRepo, Match: []string{"balance"}}})
	cur := &fakeCurator{write: notedProposal}
	h, _ := missesHarness(t, fc, cur)
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	h.requestRetro(RetroPayload{})

	m, ok := h.misses(pr.ID)["301"]
	if !ok {
		t.Fatalf("no miss for o301: %v", h.misses(pr.ID))
	}
	if m.Class != store.MissMiss || m.Scope != store.MissScopeRepo || m.SourceKind != store.MissSourceThread || m.Severity != "P2" ||
		m.Title != "Gift cards skip the balance check" || !strings.Contains(m.Lesson, "ledger transaction") || m.Path != "app/models/gift.rb" ||
		!slices.Equal(m.Lines, []int{12, 12}) || !slices.Equal(m.Match, []string{"balance"}) || m.Reviewer != "alice" ||
		m.ReviewedSHA != retroShaA || m.State != store.MissNew {
		t.Fatalf("o301's miss = %+v", m)
	}
	if d := h.declinesOf(pr.ID)[301]; d.Verdict != store.VerdictMissingContext || d.Scope != store.MissScopeRepo {
		t.Fatalf("o301's decline = %+v", d)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesMisses("talkable/talkable")); v == "" {
		t.Fatal("the repository is not marked for a curation")
	}
	evs := notesEvents(t, h, "notes.misses")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "1 miss") || !strings.Contains(evs[0].Message, "1 from a declined finding") ||
		!strings.Contains(string(evs[0].Data), `"declines":1`) {
		t.Fatalf("notes.misses events = %+v", evs)
	}

	h.advance(curateCheckEvery)
	h.tick()
	ps := proposals(t, h)
	if len(ps) != 1 || ps[0].Trigger != CurateTriggerMisses {
		t.Fatalf("proposals = %+v", ps)
	}
	given := givenMisses(notes.Scratch{Dir: ps[0].Scratch})
	if len(given.Misses) != 1 || given.Misses[0].ID != m.ID || given.Misses[0].Where != "app/models/gift.rb:12" ||
		!strings.Contains(given.Misses[0].Lesson, "ledger transaction") {
		t.Fatalf("misses.json = %+v", given)
	}
}

// TestALaterRetroWithdrawsTheMissOfADeclineItNoLongerRoutes: when another
// retro (`magnum retro <ref>`, --again) answers a thread an earlier one
// routed to the notes with anything but missing_context and scope repo,
// the miss it routed, still new, is withdrawn (class not_issue, without
// the lesson), so the notes curation no longer takes the lesson the
// classifier dropped; answered missing_context again, it is a miss again.
func TestALaterRetroWithdrawsTheMissOfADeclineItNoLongerRoutes(t *testing.T) {
	missingContext := learn.ThreadItem{Outcome: store.DeclineDeclined, Verdict: store.VerdictMissingContext,
		Lesson: "Gift card charges run inside the ledger transaction, so a balance read after the charge is safe.",
		Scope:  store.MissScopeRepo, Match: []string{"balance"}}
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": missingContext})
	cur := &fakeCurator{write: notedProposal}
	h, _ := missesHarness(t, fc, cur)
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	h.requestRetro(RetroPayload{})
	if m := h.misses(pr.ID)["301"]; m.Class != store.MissMiss || m.Scope != store.MissScopeRepo || m.State != store.MissNew {
		t.Fatalf("o301's miss after the first retro = %+v", m)
	}

	answer := func(it learn.ThreadItem) {
		next := threadsClassifier(map[string]learn.ThreadItem{"o301": it})
		fc.mu.Lock()
		fc.answer = next.answer
		fc.mu.Unlock()
	}
	answer(wrongDecline)
	h.requestRetro(RetroPayload{PRs: []int64{pr.ID}, Again: true})
	m := h.misses(pr.ID)["301"]
	if m.Class != store.MissNotIssue || m.Lesson != "" || m.Scope != "" || len(m.Match) != 0 || m.State != store.MissNew {
		t.Fatalf("o301's miss after the verdict changed = %+v, want it withdrawn", m)
	}
	if d := h.declinesOf(pr.ID)[301]; d.Verdict != store.VerdictWrong || d.Scope != store.MissScopeGeneral {
		t.Fatalf("o301's decline = %+v", d)
	}
	h.advance(curateCheckEvery)
	h.tick()
	if ps := proposals(t, h); len(ps) != 0 {
		t.Fatalf("proposals = %+v, want none: the repository has no miss left for its notes", ps)
	}

	answer(missingContext)
	h.requestRetro(RetroPayload{PRs: []int64{pr.ID}, Again: true})
	if again := h.misses(pr.ID)["301"]; again.ID != m.ID || again.Class != store.MissMiss || again.Scope != store.MissScopeRepo ||
		!strings.Contains(again.Lesson, "ledger transaction") || again.State != store.MissNew {
		t.Fatalf("o301's miss answered missing_context again = %+v", again)
	}
}

// TestADeclineWithoutAFindingsRowIsStoredWithoutOne: a finding posted
// before the findings table (migration 0005), or by a run that recorded
// none, has no findings row: its decline is stored without the link, and
// the retro goes on. A thread of a review no run of magnum's posted is not
// magnum's finding at all: no decline.
func TestADeclineWithoutAFindingsRowIsStoredWithoutOne(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline, "o303": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7, ownRetroThread(7, 303, 50, 0, [2]string{"alice", "Not an issue."}))
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 || len(fc.jobs[0].Threads) != 2 {
		t.Fatalf("classifier jobs = %d, want one with o301 and o302", len(fc.jobs))
	}
	ds := h.declinesOf(pr.ID)
	if len(ds) != 2 {
		t.Fatalf("declines = %+v, want o301 and o302", ds)
	}
	if d := ds[301]; d.FindingRowID != nil || d.RunID != "r-7" || d.SkillSHA != "" || d.Outcome != store.DeclineDeclined {
		t.Fatalf("o301 (its run recorded no findings, its prompt names no skill) = %+v", d)
	}
	if d, ok := ds[303]; ok {
		t.Fatalf("o303 (its review is unknown) = %+v, want no decline", d)
	}
	if rp, err := h.retroRecord(pr.ID); err != nil || rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
}

// TestTheOperatorsOwnCommentIsNoDecline: a gh identity's login is the
// operator's own. A comment he writes by hand in a review of his own that
// someone answers is no finding of magnum's, so the retro stores no
// decline for it, and his reply by hand in magnum's thread is an answer
// (under his login), while magnum's marked reply is magnum's.
func TestTheOperatorsOwnCommentIsNoDecline(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline, "o304": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	byHand := ownRetroThread(7, 304, 50, 5555, [2]string{"alice", "No, the caller handles it."})
	byHand.Comments[0].AuthorLogin, byHand.Comments[0].AuthorType = "zhuravel", "User"
	byHand.Comments[0].Body = "Could you rename this variable?"
	answered := ownRetroThread(7, 305, 70, 9007, [2]string{"zhuravel", "Agreed with the author, drop it."})
	marked := ownRetroThread(7, 306, 90, 9007, [2]string{"zhuravel", "The caller does not lock.\n\n<!-- magnum:reply run=r-7 kind=rebuttal -->"})
	seedOwnThreads(h, 7, byHand, answered, marked)
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 {
		t.Fatalf("classifier jobs = %d", len(fc.jobs))
	}
	var got []string
	for _, th := range fc.jobs[0].Threads {
		got = append(got, th.ID)
	}
	if !slices.Equal(got, []string{"o301", "o302", "o305"}) {
		t.Fatalf("own threads = %v, want o301, o302 and o305 (the operator answered it)", got)
	}
	if r := fc.jobs[0].Threads[2].Replies; len(r) != 1 || r[0].Author != "zhuravel" {
		t.Fatalf("o305's replies = %+v", r)
	}
	ds := h.declinesOf(pr.ID)
	if _, ok := ds[304]; ok || len(ds) != 3 {
		t.Fatalf("declines = %+v, want none for the operator's own comment", ds)
	}
	if n, err := h.st.CountDeclines(h.ctx, store.DeclineFilter{Outcomes: []string{store.DeclineDeclined}}); err != nil || n != 1 {
		t.Fatalf("declined = %d, %v; want o301's only", n, err)
	}
}

// TestAFixerDeclineMakesNoLearnedCase: magnum's fixer replies in magnum's
// threads as the PR's author, each reply ending with its marker
// (postreview.FixReplyMarker). A thread only the fixer answered goes to
// the classifier marked fixer, its reply under learn.FixerAuthor, and its
// answer is stored with the outcome and verdict but without a lesson,
// scope or patterns, even when the classifier wrote them: it makes no
// learned case, and a missing-context one no repository miss. The author's
// own decline beside it still teaches, and so does a thread a person
// answered beside the fixer, whose miss names that person, never the fixer.
func TestAFixerDeclineMakesNoLearnedCase(t *testing.T) {
	missingContext := learn.ThreadItem{Outcome: store.DeclineDeclined, Verdict: store.VerdictMissingContext,
		Lesson: "Gift card charges run inside the ledger transaction, so a balance read after the charge is safe.",
		Scope:  store.MissScopeRepo, Match: []string{"balance"}}
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline, "o307": wrongDecline, "o308": missingContext, "o309": missingContext})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	fixer := "Won't fix: net 1, the caller holds the row lock at `app/models/gift.rb:40`.\n\n" + postreview.FixReplyMarker
	seedOwnThreads(h, 7, ownRetroThread(7, 307, 50, 9007, [2]string{"alice", fixer}),
		ownRetroThread(7, 308, 70, 9007, [2]string{"alice", fixer}),
		ownRetroThread(7, 309, 90, 9007, [2]string{"alice", fixer}, [2]string{"rev-ann", "The ledger wraps every charge in its transaction."}))
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 {
		t.Fatalf("classifier jobs = %d", len(fc.jobs))
	}
	cands, err := readCandidates(fc.jobs[0].CandidatesPath)
	if err != nil {
		t.Fatal(err)
	}
	fixerOnly := map[string]bool{}
	for _, th := range cands.Threads {
		fixerOnly[th.ID] = th.Fixer
		if th.ID == "o307" && (len(th.Replies) != 1 || th.Replies[0].Author != learn.FixerAuthor) {
			t.Fatalf("o307's replies = %+v, want the fixer's", th.Replies)
		}
	}
	if want := map[string]bool{"o301": false, "o302": false, "o307": true, "o308": true, "o309": false}; !maps.Equal(fixerOnly, want) {
		t.Fatalf("threads answered by the fixer alone = %v, want %v", fixerOnly, want)
	}

	ds := h.declinesOf(pr.ID)
	for id, verdict := range map[int64]string{307: store.VerdictWrong, 308: store.VerdictMissingContext} {
		d, ok := ds[id]
		if !ok || d.Outcome != store.DeclineDeclined || d.Verdict != verdict || d.Lesson != "" || d.Scope != "" || len(d.Match) != 0 {
			t.Fatalf("o%d's decline = %+v (stored %v), want the fixer's outcome and verdict without a lesson", id, d, ok)
		}
		if c, ok := learn.CaseFromDecline(d); ok {
			t.Fatalf("o%d's decline makes the learned case %s", id, c.Name)
		}
	}
	if d := ds[301]; d.Lesson != wrongDecline.Lesson || d.Scope != store.MissScopeGeneral {
		t.Fatalf("the author's decline o301 = %+v, want its lesson kept", d)
	}
	cases := learnedCases(h)
	if !slices.Contains(cases, "decline-"+strconv.FormatInt(ds[301].ID, 10)) || slices.Contains(cases, "decline-"+strconv.FormatInt(ds[307].ID, 10)) {
		t.Fatalf("learned cases = %v, want the author's decline o301 and not the fixer's o307", cases)
	}
	if m, ok := h.misses(pr.ID)["308"]; ok {
		t.Fatalf("the fixer's missing-context decline became the repository miss %+v", m)
	}
	if m := h.misses(pr.ID)["309"]; m.Class != store.MissMiss || m.Scope != store.MissScopeRepo || m.Reviewer != "rev-ann" || m.Lesson != missingContext.Lesson {
		t.Fatalf("o309, which a person answered beside the fixer, = %+v, want a repository miss of rev-ann's", m)
	}
	h.noCommentText("pr:talkable/talkable#7")
}

// TestTheRetroOKEventCountsDeclines: the PR's retro.ok event counts the
// declines it stored and how many of them the author declined or argued,
// and the retro's summary and retro.done event add them up.
func TestTheRetroOKEventCountsDeclines(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	h.requestRetro(RetroPayload{})

	evs := h.noCommentText("pr:talkable/talkable#7")
	ok := evs[len(evs)-1]
	if ok.Kind != "retro.ok" || !strings.Contains(string(ok.Data), `"declines":2`) || !strings.Contains(string(ok.Data), `"declined":1`) ||
		!strings.Contains(ok.Message, "2 answered finding(s), 1 declined") {
		t.Fatalf("retro.ok = %s %s", ok.Message, ok.Data)
	}
	if sum := h.retroLast(); sum.Declines != 2 || sum.Declined != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	done, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "retro.done")
	if err != nil || len(done) != 1 || !strings.Contains(string(done[0].Data), `"declines":2`) || !strings.Contains(string(done[0].Data), `"declined":1`) ||
		!strings.Contains(done[0].Message, "2 answered finding(s), 1 declined") {
		t.Fatalf("retro.done = %+v, %v", done, err)
	}
}

// TestARetroClassifiesAPRWithOnlyAnsweredFindings: a PR no other reviewer
// commented on but whose author answered magnum's findings still goes to
// the classifier.
func TestARetroClassifiesAPRWithOnlyAnsweredFindings(t *testing.T) {
	fc := threadsClassifier(nil)
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.gh.mu.Lock()
	h.gh.threads[7], h.gh.allReviews[7] = nil, nil
	h.gh.mu.Unlock()
	seedOwnThreads(h, 7)
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 || len(fc.jobs[0].Candidates) != 0 || len(fc.jobs[0].Threads) != 2 {
		t.Fatalf("classifier jobs = %+v", fc.jobs)
	}
	if ds := h.declinesOf(pr.ID); len(ds) != 2 || ds[301].Outcome != store.DeclineFixed {
		t.Fatalf("declines = %+v", ds)
	}
	if rp, err := h.retroRecord(pr.ID); err != nil || rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
}

// TestTheRetroWritesTheFileOfAnAnsweredFinding: the classifier judges an
// answered finding against the code, so the retro copies the finding's
// file at its reviewed commit, as it does a commented file, even when no
// other reviewer commented on it: a thread's file names the copy, fetched
// once for the threads that share it, and one GitHub has no copy of says
// why in file_skipped. A path that climbs out of the PR's directory is
// never written outside it.
func TestTheRetroWritesTheFileOfAnAnsweredFinding(t *testing.T) {
	fc := threadsClassifier(nil)
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	gone := ownRetroThread(7, 303, 5, 9007, [2]string{"alice", "Won't fix, the caller already checks it."})
	gone.Path = "app/models/gone.rb"
	escape := ownRetroThread(7, 304, 5, 9007, [2]string{"alice", "Won't fix."})
	escape.Path = "../escape.rb"
	h.gh.mu.Lock()
	h.gh.threads[7], h.gh.allReviews[7] = nil, nil
	h.gh.contents["app/models/gift.rb@"+retroShaA] = []byte("class Gift\nend\n")
	h.gh.contents["../escape.rb@"+retroShaA] = []byte("x")
	h.gh.mu.Unlock()
	seedOwnThreads(h, 7, gone, escape)
	h.requestRetro(RetroPayload{})

	if len(fc.jobs) != 1 || len(fc.jobs[0].Threads) != 4 {
		t.Fatalf("classifier jobs = %+v", fc.jobs)
	}
	job := fc.jobs[0]
	cands, err := readCandidates(job.CandidatesPath)
	if err != nil || len(cands.Threads) != 4 {
		t.Fatalf("candidates file threads = %+v, %v", cands.Threads, err)
	}
	gift := learn.FilePath(retroShaA, "app/models/gift.rb")
	for i, th := range cands.Threads {
		wantFile, wantSkipped := gift, ""
		switch th.ID {
		case "o303":
			wantFile, wantSkipped = "", "not on GitHub at the reviewed commit"
		case "o304":
			wantFile, wantSkipped = "", "GitHub could not be read"
		}
		if th.File != wantFile || th.FileSkipped != wantSkipped {
			t.Errorf("%s (%s): file %q, skipped %q; want %q, %q", th.ID, th.Path, th.File, th.FileSkipped, wantFile, wantSkipped)
		}
		if j := job.Threads[i]; j.File != th.File || j.FileSkipped != th.FileSkipped {
			t.Errorf("%s: the job's thread has file %q, skipped %q; the candidates file %q, %q", th.ID, j.File, j.FileSkipped, th.File, th.FileSkipped)
		}
	}
	if b, err := os.ReadFile(filepath.Join(job.Dir, gift)); err != nil || string(b) != "class Gift\nend\n" {
		t.Fatalf("the finding's file: %q, %v", b, err)
	}
	if n := h.gh.count("file:talkable/talkable:app/models/gift.rb@"); n != 1 {
		t.Errorf("gift.rb read %d times, want once for the two threads on it", n)
	}
	err = filepath.WalkDir(h.layout.Home, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "escape.rb" {
			t.Errorf("escape.rb written: %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPaneClassifierChecksAndNudgesTheOwnThreads: the pane classifier's
// answer must account for the job's own threads too, and its nudge names
// their ids.
func TestPaneClassifierChecksAndNudgesTheOwnThreads(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	job.Threads = []learn.OwnThread{{ID: "o301"}, {ID: "o302"}}
	p.ag.reply = func(n int, _ string) bool {
		validAnswer(t, job) // no threads: invalid for this job
		if n == 2 {
			b, _ := os.ReadFile(job.OutputPath)
			var out learn.Output
			_ = json.Unmarshal(b, &out)
			out.Threads = []learn.ThreadItem{{ID: "o301", Outcome: store.DeclineFixed}, {ID: "o302", Outcome: store.DeclineQuestion}}
			b, _ = json.Marshal(out)
			_ = os.WriteFile(job.OutputPath, b, 0o600)
		}
		return true
	}
	if _, err := p.classifier(run).Classify(p.ctx, job); err != nil {
		t.Fatalf("after a nudge: %v", err)
	}
	sent := p.ag.sent()
	if len(sent) != 2 || !strings.Contains(sent[1], "does not follow the answer format") || !strings.Contains(sent[1], "o301, o302") ||
		!strings.Contains(sent[1], `"threads"`) {
		t.Fatalf("prompts = %q", sent)
	}
}

// TestSkillSHAOfReadsTheSkillCopyPath: a judge prompt names its skill copy
// <state>/skill/<12 hex>/SKILL.md; the hash is those 12 characters, "" when
// the prompt names none.
func TestSkillSHAOfReadsTheSkillCopyPath(t *testing.T) {
	for _, tc := range []struct{ prompt, want string }{
		{"[$magnum-review](/Users/x/.local/state/magnum/skill/" + retroSkill + "/SKILL.md) Read it first.", retroSkill},
		{"Read /state/skill/abcdefabcdef/SKILL.md, then /state/skill/" + retroSkill + "/SKILL.md.", "abcdefabcdef"},
		{"Read /checkout/skills/magnum-review/SKILL.md first.", ""},
		{"/state/skill/0123456789abcdef/SKILL.md", ""},
		{"/state/skill/0123456789AB/SKILL.md", ""},
		{"/state/skill/" + retroSkill, ""},
		{"", ""},
	} {
		if got := skillSHAOf(tc.prompt); got != tc.want {
			t.Errorf("skillSHAOf(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}
