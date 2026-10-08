package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// defaultRelated is [pipeline]'s related_lookback and related_ignore.
var defaultRelated = config.Defaults().RelatedFor(nil)

// ownFiles stores the env's PR's file list at head.
func (e *env) ownFiles(head string, paths ...string) {
	e.t.Helper()
	pr := e.pr
	if _, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.repo.ID, NodeID: pr.NodeID, Number: pr.Number, URL: pr.URL,
		HeadSHA: pr.HeadSHA, GHState: store.GHOpen, Files: &store.PRFiles{HeadSHA: head, Paths: paths}}); err != nil {
		e.t.Fatal(err)
	}
}

// otherPR records PR number of the env's repository, with a title only the
// registry knows and its file list at its head, merged at mergedAt when set.
func (e *env) otherPR(number int, draft bool, mergedAt *time.Time, paths ...string) store.PR {
	e.t.Helper()
	n := fmt.Sprint(number)
	res, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.repo.ID, NodeID: "PR_" + n, Number: number,
		URL: "https://github.com/talkable/talkable/pull/" + n, HeadSHA: "head" + n, IsDraft: draft, Title: store.Ptr("Secret title " + n),
		GHState: store.GHOpen, InitialState: store.PRQueued, Identity: "talkable-app",
		Files: &store.PRFiles{HeadSHA: "head" + n, Paths: paths}})
	if err != nil {
		e.t.Fatal(err)
	}
	if mergedAt != nil {
		if err := e.st.UpdatePR(e.ctx, res.PR.ID, func(u *store.PRUpdate) {
			u.Set("gh_state", store.GHMerged)
			u.Set("merged_at", *mergedAt)
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	return res.PR
}

// reviewedBy records magnum's posted review of pr (blocking) with a posted
// finding on each of findingPaths and a rejected one on the first.
func (e *env) reviewedBy(pr store.PR, url string, findingPaths ...string) {
	e.t.Helper()
	run, err := e.st.CreateRun(e.ctx, store.Run{PRID: pr.ID, Round: 1, Role: "codex-judge", Kind: store.RunInitial, State: store.RunVerified,
		TargetSHA: pr.HeadSHA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "p", CreatedAt: t0.Add(-time.Hour),
		Outcome: store.Ptr(OutcomePosted), ReviewID: store.Ptr(int64(pr.Number)), ReviewEvent: store.Ptr("REQUEST_CHANGES"), ReviewURL: store.Ptr(url),
		ResultJSON: store.Ptr(`{"event":"REQUEST_CHANGES","verdict":"blocking","findings":{"P1":1}}`)})
	if err != nil {
		e.t.Fatal(err)
	}
	var fs []store.Finding
	for i, p := range findingPaths {
		fs = append(fs, store.Finding{FindingID: fmt.Sprintf("F%d", i+1), Severity: "P1", Path: p, Verdict: store.FindingPosted})
	}
	if len(findingPaths) > 0 {
		fs = append(fs, store.Finding{FindingID: "R1", Severity: "P2", Path: findingPaths[0], Verdict: store.FindingRejected, ReasonCode: "speculative"})
	}
	if err := e.st.RecordFindings(e.ctx, run.ID, pr.ID, 1, fs); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.UpdatePR(e.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("reviewed_sha", pr.HeadSHA) }); err != nil {
		e.t.Fatal(err)
	}
}

// judgeAlone is the round's judge alone among the configured roles.
func judgeAlone(e *env) []config.Role {
	roles := e.r.Config.RolesFor(nil)
	return roles[slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge }):][:1]
}

// active sets pr's last activity (prs.activity_at, the board's UPDATED).
func (e *env) active(pr store.PR, at time.Time) {
	e.t.Helper()
	if err := e.st.UpdatePR(e.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("activity_at", at) }); err != nil {
		e.t.Fatal(err)
	}
}

// relatedRound runs round n of kind on head with the judge alone, the PR
// changing own at head, as a delta check after the review of prev when set,
// and returns the round's report directory and its judge prompts.
func (e *env) relatedRound(n int, kind, head, prev string, own ...string) (string, []submitCall) {
	e.t.Helper()
	before := len(e.ag.submitsFor(agents.RoleJudge))
	e.clock.Add(time.Hour)
	e.ownFiles(head, own...)
	e.git.head = head
	post := e.judgePosts(int64(900+n), "COMMENTED", "COMMENT")
	post.commit = head
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(e.t)}
	in := e.input(kind)
	in.Round, in.TargetSHA, in.Related, in.Roles = n, head, defaultRelated, judgeAlone(e)
	if prev != "" {
		in.Previous = &PreviousReview{ID: int64(900 + n - 1), Event: "COMMENTED", SHA: prev, SubmittedAt: e.clock.Now().Add(-time.Minute),
			Login: "talkable[bot]"}
		in.DeltaCheck = &DeltaCheck{Lines: 4, Files: []DeltaFile{{Path: own[0], Status: "modified"}}}
	}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		e.t.Fatalf("round %d: RunRound = %+v, %v", n, res, err)
	}
	return res.ReportDir, e.ag.submitsFor(agents.RoleJudge)[before:]
}

// relatedNumbers are the PRs related.json in dir names (nil without one),
// checking that every judge prompt names the file, or none when there is
// none.
func relatedNumbers(t *testing.T, dir string, prompts []submitCall) []int {
	t.Helper()
	path := filepath.Join(dir, RelatedFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		for _, p := range prompts {
			if strings.Contains(p.Text, "related_prs") {
				t.Errorf("a judge prompt names related_prs without %s:\n%s", RelatedFile, p.Text)
			}
		}
		return nil
	}
	if len(prompts) == 0 {
		t.Fatal("no judge prompt")
	}
	for i, p := range prompts {
		mustContain(t, fmt.Sprintf("judge prompt %d", i), p.Text, "\nrelated_prs: "+path+"\n")
	}
	got, _ := readRelated(t, path)
	nums := []int{}
	for _, r := range got.Related {
		nums = append(nums, r.Number)
	}
	return nums
}

func readRelated(t *testing.T, path string) (RelatedPRs, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r RelatedPRs
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return r, string(b)
}

// The related PRs are the candidates sharing a changed path that
// related_ignore does not list, most shared paths first, then the newest PR:
// at most ten, with at most twenty of their paths (sorted), the rest counted.
func TestRelatedPRsRankByOverlapAndCap(t *testing.T) {
	own := []string{"Gemfile.lock", "app/a.rb", "app/b.rb", "app/c.rb"}
	var lib []string
	for i := range 25 {
		lib = append(lib, fmt.Sprintf("lib/f%02d.rb", i))
	}
	own = append(own, lib...)
	cand := func(n int, merged bool, truncated bool, paths ...string) store.FilesPR {
		p := store.FilesPR{ID: int64(n), Number: n, URL: fmt.Sprintf("u%d", n), GHState: store.GHOpen,
			Files: store.PRFiles{HeadSHA: fmt.Sprintf("h%d", n), Paths: paths, Truncated: truncated}}
		if merged {
			p.GHState, p.MergedAt = store.GHMerged, store.Ptr(t0.Add(-time.Duration(n)*time.Hour))
		}
		return p
	}
	cands := []store.FilesPR{
		cand(1, false, false, "app/a.rb", "README.md"),
		cand(2, true, true, "app/b.rb", "app/a.rb"),
		cand(3, false, false, "Gemfile.lock"), // only an ignored path
		cand(4, false, false, append([]string{"app/c.rb"}, lib...)...),
		cand(5, false, false, "docs/x.md"), // nothing shared
	}
	for n := 6; n <= 15; n++ {
		cands = append(cands, cand(n, false, false, "app/a.rb"))
	}
	got, more := relatedPRs(own, cands, config.DefaultRelatedIgnore())
	var nums []int
	for _, r := range got {
		nums = append(nums, r.Number)
	}
	if want := []int{4, 2, 15, 14, 13, 12, 11, 10, 9, 8}; !slices.Equal(nums, want) || more != 3 {
		t.Fatalf("related = %v (+%d more), want %v (+3 more)", nums, more, want)
	}
	if top := got[0]; top.Overlap != 26 || len(top.Paths) != 20 || top.Paths[0] != "app/c.rb" || top.Paths[19] != "lib/f18.rb" ||
		top.State != "open" || top.MergedAt != nil || top.HeadSHA != "h4" || top.FilesTruncated {
		t.Fatalf("#4 = %+v", top)
	}
	if m := got[1]; m.Overlap != 2 || !slices.Equal(m.Paths, []string{"app/a.rb", "app/b.rb"}) || m.State != "merged" || m.MergedAt == nil ||
		!m.FilesTruncated {
		t.Fatalf("#2 = %+v", m)
	}
	if got, more := relatedPRs([]string{"Gemfile.lock"}, cands, config.DefaultRelatedIgnore()); len(got) != 0 || more != 0 {
		t.Fatalf("an own list of ignored paths relates %v (+%d)", got, more)
	}
	if got, _ := relatedPRs(own, cands[2:3], []string{}); len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("related_ignore = []: %v", got)
	}
}

// Every judge prompt of a round (the own pass and the candidates phase
// alike) names related.json: the open PRs (drafts too) and those merged
// within related_lookback that change the PR's paths at the head under
// review, lockfiles aside, with whether magnum reviewed each, its last
// review's URL and verdict and magnum's posted findings on the shared
// paths; never a title.
func TestTheJudgeLearnsTheRelatedPRs(t *testing.T) {
	e := newEnv(t)
	e.ownFiles(target, "Gemfile.lock", "app/models/order.rb", "spec/models/order_spec.rb")
	draft := e.otherPR(7, true, nil, "Gemfile.lock", "app/models/order.rb", "app/other.rb")
	e.reviewedBy(draft, "https://github.com/talkable/talkable/pull/7#pullrequestreview-7", "app/models/order.rb", "app/models/order.rb", "app/other.rb")
	e.otherPR(8, false, store.Ptr(t0.Add(-3*24*time.Hour)), "spec/models/order_spec.rb")
	e.otherPR(9, false, store.Ptr(t0.Add(-20*24*time.Hour)), "app/models/order.rb") // merged before the lookback
	e.otherPR(10, false, nil, "Gemfile.lock")                                       // a lockfile alone
	e.otherPR(11, false, nil, "app/unrelated.rb")
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(811, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	in := e.ownInput(KindInitial)
	in.Related = defaultRelated

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, RelatedFile)
	prompts := e.ag.submitsFor(agents.RoleJudge)
	if len(prompts) != 2 {
		t.Fatalf("judge prompts = %d, want the own pass and the candidates", len(prompts))
	}
	for i, p := range prompts {
		mustContain(t, fmt.Sprintf("judge prompt %d", i), p.Text, "\nrelated_prs: "+path+"\n")
	}
	got, raw := readRelated(t, path)
	if got.PR != "talkable/talkable#11920" || got.HeadSHA != target || got.FilesTruncated || got.More != 0 ||
		!got.MergedSince.Equal(t0.Add(-14*24*time.Hour)) {
		t.Fatalf("related.json head = %+v", got)
	}
	if len(got.Related) != 2 {
		t.Fatalf("related = %+v, want #8 and #7", got.Related)
	}
	merged, open := got.Related[0], got.Related[1]
	if merged.Number != 8 || merged.State != "merged" || merged.MergedAt == nil || !merged.MergedAt.Equal(t0.Add(-3*24*time.Hour)) ||
		merged.HeadSHA != "head8" || merged.Overlap != 1 || !slices.Equal(merged.Paths, []string{"spec/models/order_spec.rb"}) ||
		merged.Reviewed || merged.ReviewURL != "" || merged.FindingsOnPaths != 0 || merged.URL != "https://github.com/talkable/talkable/pull/8" {
		t.Errorf("#8 = %+v", merged)
	}
	if open.Number != 7 || open.State != "open" || !open.Draft || open.MergedAt != nil || open.Overlap != 1 ||
		!slices.Equal(open.Paths, []string{"app/models/order.rb"}) || !open.Reviewed ||
		open.ReviewURL != "https://github.com/talkable/talkable/pull/7#pullrequestreview-7" || open.ReviewVerdict != store.VerdictBlocking ||
		open.FindingsOnPaths != 2 {
		t.Errorf("#7 = %+v", open)
	}
	for _, leak := range []string{"Secret title", "Ignored title", "title", "body"} {
		if strings.Contains(raw, leak) {
			t.Errorf("related.json carries %q:\n%s", leak, raw)
		}
	}
}

// A delta check's judge (one prompt) names the related PRs too; a blind
// replay's never does (they would tell it of later PRs), and a PR without
// one, or without a file list for the head under review, gets no field.
func TestRelatedPRsInADeltaCheckButNeverBlindOrEmpty(t *testing.T) {
	e := newEnv(t)
	e.ownFiles(target, "app/models/order.rb")
	e.otherPR(7, false, nil, "app/models/order.rb")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "APPROVED", "APPROVE").behavior(t)}
	in := e.input(KindRereview)
	in.Round, in.Related, in.Roles = 2, defaultRelated, judgeAlone(e)
	in.Previous = &PreviousReview{ID: 601, Event: "APPROVED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	in.DeltaCheck = &DeltaCheck{Lines: 4, Files: []DeltaFile{{Path: "app/models/order.rb", Status: "modified"}}}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("delta check: RunRound = %+v, %v", res, err)
	}
	mustContain(t, "delta check prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "\ndelta_check: true\n",
		"\nrelated_prs: "+filepath.Join(res.ReportDir, RelatedFile)+"\n")

	for name, setup := range map[string]func(e *env, in *RoundInput){
		"blind": func(e *env, in *RoundInput) {
			e.ownFiles(target, "app/models/order.rb")
			e.otherPR(7, false, nil, "app/models/order.rb")
			in.Blind, in.DryRun = true, true
		},
		"nothing related": func(e *env, in *RoundInput) {
			e.ownFiles(target, "app/models/order.rb")
			e.otherPR(7, false, nil, "app/other.rb")
		},
		"another head's list": func(e *env, in *RoundInput) {
			e.ownFiles("newerhead", "app/models/order.rb")
			e.otherPR(7, false, nil, "app/models/order.rb")
		},
		"no list": func(e *env, in *RoundInput) {
			e.otherPR(7, false, nil, "app/models/order.rb")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			post := e.judgePosts(603, "COMMENTED", "COMMENT")
			in := e.input(KindInitial)
			in.Related = defaultRelated
			setup(e, &in)
			if in.DryRun {
				post.gh, post.status = nil, "dry_run"
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			for _, p := range e.ag.submitsFor(agents.RoleJudge) {
				if strings.Contains(p.Text, "related_prs") {
					t.Errorf("judge prompt names related_prs:\n%s", p.Text)
				}
			}
			if _, err := os.Stat(filepath.Join(res.ReportDir, RelatedFile)); !os.IsNotExist(err) {
				t.Errorf("%s: %v", RelatedFile, err)
			}
		})
	}
}

// An open PR without activity for 30 days (the board's UPDATED) is not
// related: reviews named PRs idle for 3 and 6 months, and a draft idle for
// a month, as open changes to coordinate with. One idle 29 days stays, with
// its activity_at, and a merged PR keeps the merge rule (merged within
// related_lookback) whatever its activity.
func TestRelatedLeavesOutOpenPRsIdleForThirtyDays(t *testing.T) {
	e := newEnv(t)
	e.ownFiles(target, "app/models/order.rb")
	e.active(e.otherPR(7, false, nil, "app/models/order.rb"), t0.Add(-31*24*time.Hour))
	e.active(e.otherPR(8, true, nil, "app/models/order.rb"), t0.Add(-29*24*time.Hour))
	e.active(e.otherPR(9, false, store.Ptr(t0.Add(-3*24*time.Hour)), "app/models/order.rb"), t0.Add(-45*24*time.Hour))
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(811, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Related, in.Roles = defaultRelated, judgeAlone(e)
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got, raw := readRelated(t, filepath.Join(res.ReportDir, RelatedFile))
	var nums []int
	for _, r := range got.Related {
		nums = append(nums, r.Number)
	}
	if !slices.Equal(nums, []int{9, 8}) {
		t.Fatalf("related = %v, want #9 and #8, not #7 idle 31 days:\n%s", nums, raw)
	}
	if a := got.Related[1].ActivityAt; a == nil || !a.Equal(t0.Add(-29*24*time.Hour)) {
		t.Errorf("#8 activity_at = %v", a)
	}
	if a := got.Related[0].ActivityAt; a == nil || !a.Equal(t0.Add(-45*24*time.Hour)) {
		t.Errorf("#9 activity_at = %v", a)
	}
	mustContain(t, RelatedFile, raw, `"activity_at": "`)
}

// related.json never names a PR Codex flagged (store.KVPRCodexFlag), open or
// merged, whatever its flag holds: in round 1 of talkable#11966 the judge
// read two flagged PRs it named with gh api, and Codex refused that
// conversation 45 minutes later. With only flagged PRs related there is no
// file.
func TestRelatedNeverNamesACodexFlaggedPR(t *testing.T) {
	e := newEnv(t)
	e.ownFiles(target, "app/models/order.rb")
	e.otherPR(7, false, nil, "app/models/order.rb")
	flagged := e.otherPR(8, false, nil, "app/models/order.rb")
	merged := e.otherPR(9, false, store.Ptr(t0.Add(-3*24*time.Hour)), "app/models/order.rb")
	for pr, v := range map[int64]string{flagged.ID: `{"kind":"codex","role":"codex-judge"}`, merged.ID: "not json"} {
		if err := e.st.SetKV(e.ctx, store.KVPRCodexFlag(pr), v); err != nil {
			t.Fatal(err)
		}
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(811, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Related, in.Roles = defaultRelated, judgeAlone(e)
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if nums := relatedNumbers(t, res.ReportDir, e.ag.submitsFor(agents.RoleJudge)); !slices.Equal(nums, []int{7}) {
		t.Fatalf("related = %v, want #7 alone, not the flagged #8 and #9", nums)
	}

	if err := e.st.SetKV(e.ctx, store.KVPRCodexFlag(e.otherPR(7, false, nil, "app/models/order.rb").ID), "{}"); err != nil {
		t.Fatal(err)
	}
	dir, prompts := e.relatedRound(2, KindInitial, "ffff000011112222333344445555666677778888", "", "app/models/order.rb")
	if nums := relatedNumbers(t, dir, prompts); nums != nil {
		t.Fatalf("related = %v, want no file with every related PR flagged", nums)
	}
}

// A re-review's related.json names only the related PRs that the previous
// round's set lacked or named with another relation (its state, or the
// paths both PRs share): one PR's rounds 5, 6 and 7 each repeated the same
// Related line. With nothing new there is no file and no related_prs, so
// the judge writes no Related line; and the next round compares with the
// whole set the previous round saw, not with what its file named.
func TestARereviewNamesOnlyTheRelatedPRsThatAreNewOrChanged(t *testing.T) {
	const h3, h4 = "3333333333333333333333333333333333333333", "4444444444444444444444444444444444444444"
	e := newEnv(t)
	e.otherPR(7, false, nil, "app/models/order.rb")
	dir, prompts := e.relatedRound(1, KindInitial, prevSHA, "", "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); !slices.Equal(got, []int{7}) {
		t.Fatalf("round 1 related = %v, want #7", got)
	}

	e.otherPR(9, false, nil, "app/models/order.rb", "app/other.rb")
	dir, prompts = e.relatedRound(2, KindRereview, target, prevSHA, "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); !slices.Equal(got, []int{9}) {
		t.Fatalf("round 2 related = %v, want the new #9 alone", got)
	}

	dir, prompts = e.relatedRound(3, KindRereview, h3, target, "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); got != nil {
		t.Fatalf("round 3 related = %v, want no file: #7 and #9 are as round 2 saw them", got)
	}

	if err := e.st.UpdatePR(e.ctx, e.otherPR(7, false, nil, "app/models/order.rb").ID, func(u *store.PRUpdate) {
		u.Set("gh_state", store.GHMerged)
		u.Set("merged_at", e.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	dir, prompts = e.relatedRound(4, KindRereview, h4, h3, "app/models/order.rb", "app/other.rb")
	if got := relatedNumbers(t, dir, prompts); !slices.Equal(got, []int{9, 7}) {
		t.Fatalf("round 4 related = %v, want #9 (one more shared path) and #7 (merged since)", got)
	}
}

// A re-review of the head the previous review covered shares its report
// directory: the own pass and the candidates prompt both name the same new
// related PR, the second not mistaking the first's set for the previous
// round's.
func TestASameHeadRereviewsPromptsNameTheSameNewRelatedPR(t *testing.T) {
	e := newEnv(t)
	e.otherPR(7, false, nil, "app/models/order.rb")
	dir, prompts := e.relatedRound(1, KindInitial, target, "", "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); !slices.Equal(got, []int{7}) {
		t.Fatalf("round 1 related = %v, want #7", got)
	}

	e.otherPR(9, false, nil, "app/models/order.rb")
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), e.judgePosts(902, "COMMENTED", "COMMENT").behavior(t)}
	before := len(e.ag.submitsFor(agents.RoleJudge))
	in := e.ownInput(KindRereview)
	in.Round, in.Related = 2, defaultRelated
	in.Previous = &PreviousReview{ID: 901, Event: "COMMENTED", SHA: target, SubmittedAt: e.clock.Now().Add(-time.Minute), Login: "talkable[bot]"}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	prompts = e.ag.submitsFor(agents.RoleJudge)[before:]
	if len(prompts) != 2 {
		t.Fatalf("judge prompts = %d, want the own pass and the candidates", len(prompts))
	}
	if got := relatedNumbers(t, res.ReportDir, prompts); !slices.Equal(got, []int{9}) {
		t.Fatalf("round 2 related = %v, want the new #9 alone", got)
	}
}

// A round from before related-all.json left only its related.json, which
// then held the whole set: the next re-review compares with it, so the line
// its review already carried is not repeated.
func TestARereviewComparesWithAnEarlierRoundsRelatedFile(t *testing.T) {
	e := newEnv(t)
	e.otherPR(7, false, nil, "app/models/order.rb")
	old := RelatedPRs{PR: "talkable/talkable#11920", HeadSHA: prevSHA, MergedSince: t0.Add(-14 * 24 * time.Hour),
		Related: []RelatedPR{{Number: 7, URL: "https://github.com/talkable/talkable/pull/7", State: "open", HeadSHA: "head7",
			Overlap: 1, Paths: []string{"app/models/order.rb"}}}}
	prevDir := e.layout.ReviewDir("talkable", "talkable", 11920, prevSHA)
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(prevDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prevDir, RelatedFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	dir, prompts := e.relatedRound(2, KindRereview, target, prevSHA, "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); got != nil {
		t.Fatalf("related = %v, want no file: #7 is as the earlier round's file named it", got)
	}
}
