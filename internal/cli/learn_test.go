package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	learnSkill = "# Skill\n\n## 4. Findings\nProve five facts.\n"
	learnRule  = "When two writers share a row, check the lock.\n"
	learnRun   = "20261009-070000"
)

// learnHarness is an act harness whose home is a git checkout of magnum's
// shape (git runs for real there, in a hermetic environment), a general
// miss and a general decline in the registry, and a held proposal whose
// branch adds a rule to the skill.
type learnHarness struct {
	*actHarness
	git            *execx.Real
	base, cand     string
	miss           store.Miss
	decline        store.Decline
	proposal       store.LearnProposal
	reviewDecision string
}

func (h *learnHarness) gitOut(args ...string) string {
	h.t.Helper()
	res, err := h.git.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", h.home}, args...)})
	if err != nil {
		h.t.Fatalf("git %v: %v", args, err)
	}
	return res.Out()
}

func (h *learnHarness) write(rel, text string) {
	h.t.Helper()
	p := filepath.Join(h.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func newLearnHarness(t *testing.T) *learnHarness {
	t.Helper()
	h := &learnHarness{actHarness: newActHarness(t)}
	h.git = &execx.Real{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com", "LC_ALL=C"}}
	if _, err := h.git.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	h.d.Run = h.git
	h.gitOut("init", "--quiet", "-b", "master")
	h.write(learn.SkillFile, learnSkill)
	h.write("prompts/judge-initial.md", "Review {{.URL}}.\n")
	h.gitOut("add", "skills", "prompts")
	h.gitOut("commit", "--quiet", "-m", "c1")
	h.base = h.gitOut("rev-parse", "master")
	h.gitOut("checkout", "--quiet", "-b", "learn/"+learnRun)
	h.write(learn.SkillFile, learnSkill+learnRule)
	h.gitOut("commit", "--quiet", "-am", "The judge checks the lock.")
	h.cand = h.gitOut("rev-parse", "HEAD")
	h.gitOut("checkout", "--quiet", "master")

	pr := h.seedPR("talkable/talkable", 11, store.PRReviewed)
	var err error
	h.miss, err = h.st.UpsertMiss(h.ctx, store.Miss{PRID: pr.ID, SourceURL: pr.URL + "#discussion_r1", SourceKind: store.MissSourceThread,
		Reviewer: "rev-ann", ReviewedSHA: "abc", Class: store.MissMiss, Severity: "P1", Title: "Lost update on a shared row",
		Lesson: "When two writers share a row, check the lock.", Scope: store.MissScopeGeneral})
	if err != nil {
		t.Fatal(err)
	}
	h.decline, err = h.st.UpsertDecline(h.ctx, store.Decline{PRID: pr.ID, SourceURL: pr.URL + "#discussion_r5", CommentID: 5,
		Title: "Token can be nil", Priority: "P2", ReviewedSHA: "abc", Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong,
		Lesson: "When the caller checks a value, do not ask for a guard.", Scope: store.MissScopeGeneral})
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(h.home, "state", "learn", "skill", learnRun)
	h.reviewDecision = "The judge checks the lock of a row two writers share. A reviewer caught a lost update."
	b, _ := json.Marshal(learn.Changes{Decision: h.reviewDecision})
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, learn.ChangesFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h.proposal, err = h.st.CreateLearnProposal(h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: h.base,
		Branch: "learn/" + learnRun, CandidateSHA: h.cand, Files: []string{learn.SkillFile}, State: store.LearnHeld, Model: "sonnet",
		Gate: []byte(`{"rule":"targets could not be replayed","cases":[]}`), RunDir: runDir, At: h.now,
		Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: h.miss.ID, Outcome: store.MissNoted, Where: "SKILL.md section 4"},
			{Kind: store.LearnInputDecline, InputID: h.decline.ID, Outcome: store.MissSkipped, Reason: "the receiver rule covers it"}}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLearnListsVersionsAndWaitingProposals(t *testing.T) {
	h := newActHarness(t)
	for i, sha := range []string{"aaaaaaa1111", "bbbbbbb2222"} {
		if _, err := h.st.CreateLearnVersion(h.ctx, store.LearnVersion{LandedSHA: sha, SkillSHA: "s", PromptsSHA: "p",
			LandedAt: h.now.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(state, reason string, at time.Duration) store.LearnProposal {
		t.Helper()
		p, err := h.st.CreateLearnProposal(h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerEvidence, BaseSHA: "base",
			Branch: "learn/" + state, State: state, Reason: reason, At: h.now.Add(at)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	add(store.LearnFailed, "an old failure", -3*time.Hour)
	failed := add(store.LearnFailed, "no target improved", -2*time.Hour)
	invalid := add(store.LearnInvalid, "SKILL.md lost a pinned sentence", -time.Hour)
	held := add(store.LearnHeld, "", 0)
	gating := add(store.LearnGating, "", time.Minute)
	add(store.LearnRejected, "too broad", 2*time.Minute)

	if code := h.cmd("learn"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "skill versions, newest first:", "bbbbbbb", "aaaaaaa", "proposals waiting, oldest first:",
		"learn/"+store.LearnHeld, "learn/"+store.LearnGating, "last failed: proposal "+itoa(failed.ID), "no target improved",
		"last invalid: proposal "+itoa(invalid.ID), "SKILL.md lost a pinned sentence", "next edit: on request",
		"`magnum learn --review` reviews proposal "+itoa(held.ID))
	if strings.Index(out, "bbbbbbb") > strings.Index(out, "aaaaaaa") || strings.Index(out, "learn/held") > strings.Index(out, "learn/gating") {
		t.Errorf("order:\n%s", out)
	}
	if strings.Contains(out, "too broad") || strings.Contains(out, "an old failure") {
		t.Errorf("the list shows a rejection or an older failure:\n%s", out)
	}

	h.out.Reset()
	if code := h.cmd("learn", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var data learnListData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatalf("--json: %v\n%s", err, h.out.String())
	}
	if len(data.Versions) != 2 || data.Versions[0].LandedSHA != "bbbbbbb2222" || len(data.Waiting) != 2 || data.Waiting[0].ID != held.ID ||
		data.Waiting[1].ID != gating.ID || data.LastFailed == nil || data.LastFailed.ID != failed.ID || data.LastInvalid == nil ||
		data.LastInvalid.ID != invalid.ID || data.Next == "" {
		t.Fatalf("--json = %+v", data)
	}
}

func TestLearnReviewShowsTheDiffAndTheGateAndAsksYN(t *testing.T) {
	h := newLearnHarness(t)
	m, d := learn.MissInputID(h.miss.ID), learn.DeclineInputID(h.decline.ID)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "proposal 1 cannot land before the daemon restarted on skill version 1 (each version runs "+
			"before the next lands); ask again after the restart")
	}
	h.d.StdinTTY = true
	h.stdin("y\n")
	code := h.cmd("learn", "--review")
	out := h.out.String()
	actContains(t, out, "skill proposal "+itoa(h.proposal.ID), "learn/"+learnRun, m+" P1 Lost update on a shared row → noted in SKILL.md section 4",
		d+" P2 wrong Token can be nil → skipped: the receiver rule covers it", "lesson: When the caller checks a value",
		"+"+strings.TrimSuffix(learnRule, "\n"), "decision:", h.reviewDecision, "gate:", "targets could not be replayed",
		"Land proposal "+itoa(h.proposal.ID)+" on master?")
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqLearnLand {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.LearnDecisionPayload](t, reqs[0].Payload); p.ID != h.proposal.ID || p.Reason != "" {
		t.Fatalf("payload = %+v", p)
	}
	if code != 1 || !strings.Contains(h.errb.String(), "ask again after the restart") {
		t.Fatalf("exit %d, stderr %q: the daemon's refusal is the answer", code, h.errb.String())
	}

	// --json prints the data and asks nothing; without a terminal --review
	// refuses.
	h.out.Reset()
	h.errb.Reset()
	if code := h.cmd("learn", "--review", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var data learnReviewData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Proposal.ID != h.proposal.ID || len(data.Inputs) != 2 || len(data.Diffs) != 1 || !strings.Contains(data.Diffs[0].Diff, "+When two writers") ||
		data.Decision != h.reviewDecision || data.DiffError != "" {
		t.Fatalf("--json = %+v", data)
	}
	h.d.StdinTTY = false
	if code := h.cmd("learn", "--review"); code != 2 || !strings.Contains(h.errb.String(), "--review asks y/N on a terminal") {
		t.Fatalf("no terminal: exit %d, %s", code, h.errb.String())
	}
	if len(h.requests()) != 1 {
		t.Fatalf("requests = %+v", h.requests())
	}
}

func TestLearnReviewNSendsARejectWithTheReason(t *testing.T) {
	h := newLearnHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "rejected proposal 1; the next edit reads why; 2 inputs back to new for the next edit")
	}
	h.d.StdinTTY = true
	h.stdin("n\n")
	if code := h.cmd("learn", "--review", "--reason", "the rule is too broad"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqLearnReject {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.LearnDecisionPayload](t, reqs[0].Payload); p.ID != h.proposal.ID || p.Reason != "the rule is too broad" {
		t.Fatalf("payload = %+v", p)
	}
	actContains(t, h.out.String(), "rejected proposal 1; the next edit reads why")

	// Anything else leaves it, and sends nothing.
	h.out.Reset()
	h.stdin("maybe\n")
	if code := h.cmd("learn", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "left for later: `magnum learn --review`")
	if len(h.requests()) != 1 {
		t.Fatalf("requests = %+v", h.requests())
	}
}

// The review shows every file the branch changes, git's word, not only the
// proposal's list, and names one the list lacks: magnum checked only those.
func TestLearnReviewNamesAFileTheBranchChangesBeyondTheProposal(t *testing.T) {
	h := newLearnHarness(t)
	h.gitOut("checkout", "--quiet", "learn/"+learnRun)
	h.write("prompts/judge-initial.md", "Review {{.URL}} now.\n")
	h.gitOut("commit", "--quiet", "-am", "an unlisted change")
	cand := h.gitOut("rev-parse", "HEAD")
	h.gitOut("checkout", "--quiet", "master")
	if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE learn_proposals SET candidate_sha = ? WHERE id = ?", cand, h.proposal.ID); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("learn", "--review", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var data learnReviewData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Diffs) != 2 || len(data.Unlisted) != 1 || data.Unlisted[0] != "prompts/judge-initial.md" {
		t.Fatalf("diffs %d, unlisted %q", len(data.Diffs), data.Unlisted)
	}
	h.out.Reset()
	h.d.StdinTTY = true
	h.stdin("\n")
	if code := h.cmd("learn", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "WARNING: the branch also changes prompts/judge-initial.md", "+Review {{.URL}} now.")
}

func TestLearnProposeIsRefusedWhileAProposalWaits(t *testing.T) {
	h := newLearnHarness(t)
	if code := h.cmd("learn", "--propose"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "proposal "+itoa(h.proposal.ID)+" is held", "`magnum learn --review` lands or rejects it first")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("a refused edit queued %+v", reqs)
	}

	// Once nothing waits, the request goes to the daemon, which answers.
	if _, err := h.st.DecideLearnProposal(h.ctx, h.proposal.ID, []string{store.LearnHeld}, store.LearnRejected, "", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	h.errb.Reset()
	h.pid = 4242
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "skill edit started from 2 inputs") }
	if code := h.cmd("learn", "--propose"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqLearnPropose {
		t.Fatalf("requests = %+v", reqs)
	}
	actContains(t, h.out.String(), "skill edit started from 2 inputs")

	// Without a daemon the request stays queued and the command says so.
	h.pid, h.onSleep = 0, nil
	h.errb.Reset()
	if code := h.cmd("learn", "--propose"); code != 1 || !strings.Contains(h.errb.String(), "no daemon is running") {
		t.Fatalf("no daemon: exit %d, %s", code, h.errb.String())
	}
}

func TestLearnUsageLineNamesItsFlags(t *testing.T) {
	h := newActHarness(t)
	cmd, _, err := newRoot(h.c).Find([]string{"learn"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Use != "learn [--review [--reason <text>] | --propose | --revert <version> [--reason <text>]] [--json]" {
		t.Fatalf("usage = %q", cmd.Use)
	}
	for _, f := range []string{"review", "reason", "propose", "revert", "json"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("no --%s", f)
		}
	}
	for _, args := range [][]string{{"learn", "x"}, {"learn", "--review", "--propose"}, {"learn", "--reason", "why"},
		{"learn", "--revert", "1", "--propose"}, {"learn", "--revert", "0"}} {
		h.errb.Reset()
		if code := h.cmd(args[0], args[1:]...); code != 2 || !strings.Contains(h.errb.String(), "usage: magnum learn [--review") {
			t.Errorf("%v: exit %d, %s", args, code, h.errb.String())
		}
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("a usage error queued %+v", reqs)
	}
}

// `magnum learn --revert <version>` asks the daemon for a proposal that
// restores the files of the version before it (engine.ReqLearnRevert, with
// --reason) and prints its answer; a version that never landed is refused
// before anything is queued.
func TestLearnRevertProposesThePreviousVersion(t *testing.T) {
	h := newActHarness(t)
	v, err := h.st.CreateLearnVersion(h.ctx, store.LearnVersion{LandedSHA: "aaaaaaa1111", SkillSHA: "s", PromptsSHA: "p", LandedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "the revert of skill version 1 runs; the gate holds its proposal for `magnum learn --review`")
	}
	if code := h.cmd("learn", "--revert", itoa(v.ID), "--reason", "authors declined twice as much"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "the revert of skill version 1 runs")
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqLearnRevert {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.LearnRevertPayload](t, reqs[0].Payload); p.Version != v.ID || p.Reason != "authors declined twice as much" {
		t.Fatalf("payload = %+v", p)
	}
	h.errb.Reset()
	if code := h.cmd("learn", "--revert", "99"); code != 1 || !strings.Contains(h.errb.String(), "no skill version 99") {
		t.Fatalf("an unknown version: exit %d, %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("an unknown version queued %+v", reqs[1:])
	}
}

// The list shows what each landed version's reviews did since the daemon
// restarted on it: the posted reviews, their posted findings, the authors'
// fixes and declines (declined and argued, with their share) and the
// misses other reviewers found; --json carries each version's measure.
func TestLearnShowsAVersionsNumbers(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 21, store.PRReviewed)
	var vs []store.LearnVersion
	for i, sha := range []string{"aaaaaaa1111", "bbbbbbb2222"} {
		at := h.now.Add(time.Duration(i-2) * 24 * time.Hour)
		v, err := h.st.CreateLearnVersion(h.ctx, store.LearnVersion{LandedSHA: sha, SkillSHA: "skill" + itoa(int64(i)), PromptsSHA: "p",
			LandedAt: at.Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.MarkLearnRestarted(h.ctx, v.ID, at); err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	// Version 2's window: two posted reviews, five posted findings, of
	// which the authors fixed one and declined or argued two, and a miss.
	for i, n := range []int{3, 2} {
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: i + 1, Role: store.RoleJudge, Kind: store.RunInitial,
			State: store.RunVerified, TargetSHA: "head" + itoa(int64(i)), Identity: "talkable-app", ReviewerLogin: "talkable[bot]",
			PromptText: "p", Outcome: new("posted"), ReviewID: new(int64(70 + i)), CreatedAt: h.now.Add(-time.Duration(i+1) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		var fs []store.Finding
		for j := range n {
			fs = append(fs, store.Finding{FindingID: "F" + itoa(int64(j)), Severity: "P2", Verdict: store.FindingPosted})
		}
		if err := h.st.RecordFindings(h.ctx, run.ID, pr.ID, i+1, fs); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			continue
		}
		for j, outcome := range []string{store.DeclineFixed, store.DeclineDeclined, store.DeclineArgued} {
			if _, err := h.st.UpsertDecline(h.ctx, store.Decline{PRID: pr.ID, SourceURL: pr.URL + "#d" + itoa(int64(j)), CommentID: int64(j + 1),
				RunID: run.ID, ReviewedSHA: run.TargetSHA, SkillSHA: "skill1", Outcome: outcome}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.st.UpsertMiss(h.ctx, store.Miss{PRID: pr.ID, SourceURL: pr.URL + "#m1", SourceKind: store.MissSourceThread,
			Reviewer: "rev-ann", ReviewedSHA: run.TargetSHA, Class: store.MissMiss, Severity: "P1", Scope: store.MissScopeGeneral}); err != nil {
			t.Fatal(err)
		}
	}
	if code := h.cmd("learn"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "ROUNDS", "POSTED", "FIXED", "DECLINED", "MISSES")
	rows := map[string][]string{}
	for line := range strings.Lines(out) {
		for _, sha := range []string{"aaaaaaa", "bbbbbbb"} {
			if strings.Contains(line, sha) {
				f := strings.Fields(line)
				rows[sha] = f[max(0, len(f)-6):]
			}
		}
	}
	if got := strings.Join(rows["bbbbbbb"], " "); got != "2 5 1 2 (40%) 1" {
		t.Errorf("version 2's numbers = %q\n%s", got, out)
	}
	if got := strings.Join(rows["aaaaaaa"], " "); !strings.HasSuffix(got, "0 0 0 0 0") {
		t.Errorf("version 1's numbers = %q\n%s", got, out)
	}

	h.out.Reset()
	if code := h.cmd("learn", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var data learnListData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatalf("--json: %v\n%s", err, h.out.String())
	}
	if len(data.Versions) != 2 || data.Versions[0].ID != vs[1].ID || data.Versions[0].Measure.JudgeRuns != 2 || data.Versions[0].Measure.Posted != 5 ||
		data.Versions[0].Measure.Outcomes[store.DeclineArgued] != 1 || data.Versions[0].Measure.MissesGeneral != 1 ||
		data.Versions[1].Measure.Posted != 0 || data.Versions[1].Measure.To == nil {
		t.Fatalf("--json versions = %+v", data.Versions)
	}
}
