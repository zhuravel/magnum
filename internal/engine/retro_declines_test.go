package engine

import (
	"context"
	"encoding/json"
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

// TestADeclineWithoutAFindingsRowIsStoredWithoutOne: a finding posted
// before the findings table (migration 0005) or by a review magnum has no
// run of has no findings row: its decline is stored without the link (and
// without a run and skill hash when the review is unknown), and the retro
// goes on.
func TestADeclineWithoutAFindingsRowIsStoredWithoutOne(t *testing.T) {
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": wrongDecline, "o303": wrongDecline})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7, ownRetroThread(7, 303, 50, 0, [2]string{"alice", "Not an issue."}))
	h.requestRetro(RetroPayload{})

	ds := h.declinesOf(pr.ID)
	if len(ds) != 3 {
		t.Fatalf("declines = %+v", ds)
	}
	if d := ds[301]; d.FindingRowID != nil || d.RunID != "r-7" || d.SkillSHA != "" || d.Outcome != store.DeclineDeclined {
		t.Fatalf("o301 (its run recorded no findings, its prompt names no skill) = %+v", d)
	}
	if d := ds[303]; d.FindingRowID != nil || d.RunID != "" || d.ReviewID != 0 || d.Verdict != store.VerdictWrong || d.ReviewedSHA != retroShaA {
		t.Fatalf("o303 (its review is unknown) = %+v", d)
	}
	if rp, err := h.retroRecord(pr.ID); err != nil || rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
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
