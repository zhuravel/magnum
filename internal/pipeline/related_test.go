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
	judgeOnly := func(e *env) []config.Role {
		roles := e.r.Config.RolesFor(nil)
		return roles[slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge }):][:1]
	}

	e := newEnv(t)
	e.ownFiles(target, "app/models/order.rb")
	e.otherPR(7, false, nil, "app/models/order.rb")
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "APPROVED", "APPROVE").behavior(t)}
	in := e.input(KindRereview)
	in.Round, in.Related, in.Roles = 2, defaultRelated, judgeOnly(e)
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
