package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
)

// post-review refuses a judge-written footer by the marker this package
// appends the footer under.
func TestPostReviewRefusesThisFooterMarker(t *testing.T) {
	if postreview.FooterMarker != footerMarker {
		t.Fatalf("post-review refuses %q, the footer starts with %q", postreview.FooterMarker, footerMarker)
	}
}

// The footer the judge wrote itself before magnum appended one (the old
// default line).
const judgeWrittenFooter = "_Automated review by [Magnum](https://github.com/zhuravel/magnum). Reply on a thread with `fixed`, `not a bug: <why>` or `won't fix: <why>`; " +
	"simplifications are optional. New pushes are re-reviewed automatically._"

// lastUpdate is the body of the last review edit, or fails.
func (e *env) lastUpdate() string {
	e.t.Helper()
	if len(e.gh.updates) == 0 {
		e.t.Fatal("no review edit")
	}
	return e.gh.updates[len(e.gh.updates)-1].Body
}

// magnum appends the identity's footer to the review it verified: once, as
// its own paragraph after a blank line, below the judge's body and the run
// marker, starting with the footer marker. Verifying the same review again
// (a continued round) changes nothing, and the note AppendToReview adds goes
// above the footer.
func TestVerifiedReviewGetsTheFooterOnceAfterABlankLine(t *testing.T) {
	e := newEnv(t)
	post := e.judgePosts(665, "COMMENTED", "COMMENT")
	post.body = "**Verdict** findings\n\n<details><summary>Checks</summary>\n\nspecs ran\n</details>"
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 1 || e.gh.updates[0].ID != 665 {
		t.Fatalf("updates = %+v, want one edit of review 665", e.gh.updates)
	}
	got := e.lastUpdate()
	head, footer, ok := strings.Cut(got, "\n\n"+footerMarker+"\n")
	if !ok || !strings.HasPrefix(head, post.body+"\n<!-- magnum:run="+res.JudgeRunID+" head=") || !strings.HasSuffix(head, " -->") {
		t.Fatalf("edited body = %q", got)
	}
	if !strings.HasPrefix(footer, "**Reviewed commit:** `"+target[:10]+"`\n\n<details><summary>ℹ️ About Magnum</summary>\n\n") ||
		!strings.HasSuffix(footer, "New pushes are re-reviewed automatically. A thread reply gets an answer without a push, "+
			"and a review request for `talkable[bot]` starts a round.\n\n</details>") ||
		!strings.Contains(footer, "; simplifications are optional") {
		t.Fatalf("footer = %q", footer)
	}
	if strings.Count(got, footerMarker) != 1 {
		t.Fatalf("footer marker %d times: %q", strings.Count(got, footerMarker), got)
	}
	if evs := eventsOfKind(e.events(), "round.footer"); len(evs) != 1 {
		t.Errorf("round.footer events = %+v", evs)
	}

	// The continued round finds the same review by its marker: nothing to edit.
	again := judgePost{reviewID: 665, event: "COMMENT", status: "posted", findings: map[string]int{"P2": 2}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{again.behavior(t)}
	in := e.input(KindContinue)
	in.ContinueRunID = res.JudgeRunID
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted || res.ReviewID != 665 {
		t.Fatalf("continued round = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 1 {
		t.Fatalf("the re-verified review was edited again: %+v", e.gh.updates[1:])
	}

	line := "_Reviewed abc1234; 1 commit arrived during the review, re-review follows._"
	for range 2 { // the second call finds the note already there
		if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, 665, line); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.gh.updates) != 2 || e.lastUpdate() != head+"\n\n"+line+"\n\n"+footerMarker+"\n"+footer {
		t.Fatalf("after the note: %d edits, body %q", len(e.gh.updates), e.lastUpdate())
	}
}

// withFooter puts the footer last, after a blank line, and replaces a
// footer magnum appended before (everything from the footer marker on)
// instead of adding a second one; a body that has the footer already is
// left alone.
func TestWithFooterReplacesItsOwnFooterAndIsIdempotent(t *testing.T) {
	const block = footerMarker + "\n**Reviewed commit:** `d4e5f6a7b8`"
	for _, tc := range []struct {
		name, body, want string
		changed          bool
	}{
		{"glued to </details>", "</details>\n<!-- magnum:run=r-1 head=d4e5f6a -->", "</details>\n<!-- magnum:run=r-1 head=d4e5f6a -->\n\n" + block, true},
		{"trailing space", "verdict \r\n\n", "verdict\n\n" + block, true},
		{"an older footer", "verdict\n\n_note_\n\n" + footerMarker + "\nold footer\n", "verdict\n\n_note_\n\n" + block, true},
		{"an older footer glued", "verdict\n" + footerMarker + "\nold footer", "verdict\n\n" + block, true},
		{"the same footer", "verdict\n\n" + block, "verdict\n\n" + block, false},
		{"the same footer, a trailing newline", "verdict\n\n" + block + "\n", "verdict\n\n" + block + "\n", false},
		{"the same footer glued", "</details>\n" + block, "</details>\n\n" + block, true},
		{"only a footer", block, block, false},
		{"an old judge-written footer", "verdict\n<!-- m -->\n\n" + judgeWrittenFooter, "verdict\n<!-- m -->\n\n" + judgeWrittenFooter, false},
	} {
		got, changed := withFooter(tc.body, block)
		if got != tc.want || changed != tc.changed {
			t.Errorf("%s: withFooter(%q) = %q, %v; want %q, %v", tc.name, tc.body, got, changed, tc.want, tc.changed)
		}
		if again, changed := withFooter(got, block); changed || again != got {
			t.Errorf("%s: withFooter is not idempotent on %q: %q", tc.name, got, again)
		}
	}
}

// A watch that runs no role aliased "simplify" never posts simplifications,
// so its footer does not call them optional.
func TestFooterLeavesSimplificationsOutWhenTheWatchRunsNoSimplifyRole(t *testing.T) {
	e := newEnv(t)
	e.cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"*"}, Identity: "talkable-app",
		Roles: []string{"codex-judge", "claude-review", "codex-review"}}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(666, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	got := e.lastUpdate()
	if !strings.Contains(got, footerMarker+"\n**Reviewed commit:**") || strings.Contains(got, "simplifications") {
		t.Fatalf("footer without a simplify role: %q", got)
	}
}

// The footer says when the PR's watch re-reviews a push and whose review
// request starts a round: outside [daemon] quiet_hours (with the daemon's
// UTC offset), not on a draft a watch with include_drafts = false skips, and
// a request for the poll login when that is a person's (a gh identity),
// else for the posting login.
func TestFooterSaysWhenTheWatchReReviewsAndWhoseRequestStartsARound(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name  string
		quiet string
		watch config.Watch
		want  string
	}{
		{"quiet hours, drafts skipped, a gh poll login", "03:00-12:00",
			config.Watch{Owner: "talkable", Include: []string{"*"}, Identity: "talkable-app", PollIdentity: "zhuravel", IncludeDrafts: &no},
			"New pushes are re-reviewed automatically outside 03:00-12:00 UTC, drafts only on request. " +
				"A thread reply gets an answer without a push, and a review request for `zhuravel` starts a round."},
		{"an App polls", "",
			config.Watch{Owner: "talkable", Include: []string{"*"}, Identity: "talkable-app", PollIdentity: "talkable-app"},
			"New pushes are re-reviewed automatically. " +
				"A thread reply gets an answer without a push, and a review request for `talkable[bot]` starts a round."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.Daemon.QuietHours = tc.quiet
			e.cfg.Watches = []config.Watch{tc.watch}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(670, "COMMENTED", "COMMENT").behavior(t)}
			if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			if got := e.lastUpdate(); !strings.HasSuffix(got, tc.want+"\n\n</details>") {
				t.Fatalf("footer = %q, want it to end %q", got, tc.want)
			}
		})
	}
}

// review_footer = "" turns the footer off: the verified review is not
// edited.
func TestEmptyReviewFooterAppendsNothing(t *testing.T) {
	e := newEnv(t)
	off := ""
	e.cfg.Identities[0].ReviewFooter = &off
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(667, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 0 {
		t.Fatalf("updates = %+v, want none", e.gh.updates)
	}
}

// The template sees the reviewed commit, the PR, the posting login, the
// posted event and whether the review is clean: no findings of any priority,
// no still-open earlier finding and no simplification, by the judge's
// result.
func TestFooterTemplateSeesTheReviewAndWhetherItIsClean(t *testing.T) {
	const tmpl = "{{.SHA}} {{.Short}} {{.Repo}}#{{.Number}} {{.Login}} {{.Event}}{{if .Clean}} clean{{end}}{{if .Simplify}} simplify{{end}}{{if .PostMerge}} post-merge{{end}}{{if .DeltaCheck}} delta{{end}}"
	common := target + " " + target[:10] + " talkable/talkable#11920 talkable[bot] "
	for _, tc := range []struct {
		name, state, event string
		findings           map[string]int
		extra              map[string]any
		want               string
	}{
		{"clean", "APPROVED", "APPROVE", map[string]int{"P0": 0, "P1": 0, "P2": 0, "P3": 0}, nil, common + "APPROVE clean simplify"},
		{"a P3", "COMMENTED", "COMMENT", map[string]int{"P3": 1}, nil, common + "COMMENT simplify"},
		{"a simplification", "APPROVED", "APPROVE", map[string]int{}, map[string]any{"candidates": map[string]any{"claude-simplify": map[string]int{"suggested": 1}}}, common + "APPROVE simplify"},
		{"an earlier finding still open", "COMMENTED", "COMMENT", map[string]int{}, map[string]any{"previous_findings": map[string]int{"open": 1}}, common + "COMMENT simplify"},
		{"blocking", "CHANGES_REQUESTED", "REQUEST_CHANGES", map[string]int{"P1": 1}, nil, common + "REQUEST_CHANGES simplify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			footer := tmpl
			e.cfg.Identities[0].ReviewFooter = &footer
			post := e.judgePosts(668, tc.state, tc.event)
			post.findings, post.extra = tc.findings, tc.extra
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
			if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			if got := e.lastUpdate(); !strings.HasSuffix(got, "\n\n"+footerMarker+"\n"+tc.want) {
				t.Fatalf("body = %q, want the footer %q", got, tc.want)
			}
		})
	}
}

// A dry run posts nothing, so there is no review to append a footer to.
func TestDryRunGetsNoFooter(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(0, "", "COMMENT")
	p.gh, p.status = nil, "dry_run"
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindInitial)
	in.DryRun = true
	if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 0 {
		t.Fatalf("updates = %+v, want none", e.gh.updates)
	}
}

// A review whose judge wrote the footer itself, before magnum appended one,
// is left as it is: no second footer, no rewrite. A note still goes at its
// end.
func TestReviewWithAnOldJudgeWrittenFooterIsLeftAsIs(t *testing.T) {
	e := newEnv(t)
	post := e.judgePosts(669, "COMMENTED", "COMMENT")
	post.after = "\n" + judgeWrittenFooter
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 0 {
		t.Fatalf("updates = %+v, want none", e.gh.updates)
	}

	body := "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n" + judgeWrittenFooter + "\n"
	e.gh.add(github.Review{DatabaseID: 670, Body: body}, github.RESTReview{ID: 670, UserLogin: "talkable[bot]", UserType: "Bot", Body: body})
	line := "_Reviewed abc1234; 1 commit arrived during the review, re-review follows._"
	if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, 670, line); err != nil {
		t.Fatal(err)
	}
	if got := e.lastUpdate(); got != strings.TrimSpace(body)+"\n\n"+line {
		t.Fatalf("note on an old review: %q", got)
	}
}

// A footer template that does not render (the daemon's config was never
// validated) costs the footer only: a warning, the review stays posted and
// unedited.
func TestBrokenFooterTemplateWarnsAndLeavesTheReview(t *testing.T) {
	e := newEnv(t)
	broken := "{{.Commit}}"
	e.cfg.Identities[0].ReviewFooter = &broken
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(671, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(e.gh.updates) != 0 {
		t.Fatalf("updates = %+v, want none", e.gh.updates)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "review 671: the footer does not render") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}
