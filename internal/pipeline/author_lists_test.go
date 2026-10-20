package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

// A clean review is no approval of an author the auto-approval author lists
// refuse: with auto_approve_skip_authors naming the author, or an
// auto_approve_authors that applies leaving it out (an author magnum does
// not know included), the judge's no-findings event is COMMENT, for a gh
// identity (whose default is APPROVE: a clean review as the operator's
// account is the operator's approval) and for an App set to APPROVE alike. A
// listed author keeps the configured APPROVE, the skip list wins over the
// allowlist, a repository's list replaces the watch's, and without lists
// nothing changes. The blocking event is never touched.
func TestJudgeEventsGiveNoApproveToAnAuthorTheAutoApprovalListsRefuse(t *testing.T) {
	cfg := config.Defaults()
	cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"*"},
		AutoApproveSkipAuthors: []string{"alice"}, AutoApproveAuthors: []string{"alice", "rev-ann"}}}
	gh := &config.Identity{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}
	app := &config.Identity{Name: "talkable-app", Kind: "app", Login: "talkable[bot]", NoFindingsEvent: "APPROVE"}
	check := func(what, author, want string) {
		t.Helper()
		for _, id := range []*config.Identity{gh, app} {
			if nf, be := JudgeEvents(cfg, "talkable/talkable", id, author, false); nf != want || be != "REQUEST_CHANGES" {
				t.Errorf("%s, %s identity, author %q: events %s/%s, want %s/REQUEST_CHANGES", what, id.Kind, author, nf, be, want)
			}
		}
	}
	check("skipped although allowed", "alice", "COMMENT")
	check("skipped, another case", "Alice[bot]", "COMMENT")
	check("outside the allowlist", "bob-rev", "COMMENT")
	check("unknown author", "", "COMMENT")
	check("on the allowlist", "rev-ann", "APPROVE")
	check("on the allowlist, another case", "REV-ANN", "APPROVE")

	cfg.Repos = []config.Repo{{Repo: "talkable/talkable", AutoApproveAuthors: []string{"bob-rev"}}}
	check("on the repository's list", "bob-rev", "APPROVE")
	check("on the watch's list only", "rev-ann", "COMMENT")

	cfg.Repos, cfg.Watches[0].AutoApproveSkipAuthors, cfg.Watches[0].AutoApproveAuthors = nil, nil, nil
	check("no lists", "bob-rev", "APPROVE")
	check("no lists, unknown author", "", "APPROVE")
}

// A round posted as a gh identity whose no_findings_event is APPROVE asks its
// judge for a COMMENT on a clean review (the event post-review submits comes
// from the prompt's no_findings_event) when the PR's author is on
// auto_approve_skip_authors or outside the auto_approve_authors that
// applies, and says why in the round's judge event; a listed author's round
// asks for the APPROVE configured. Before, authorRefusal stopped only the
// separate automatic approval, and the judge's own clean review approved as
// the operator's account.
func TestTheJudgeIsAskedForACommentOnAnAuthorTheAutoApprovalListsRefuse(t *testing.T) {
	watch := config.Watch{Owner: "talkable", Include: []string{"*"}}
	for name, tc := range map[string]struct {
		skip, allow []string
		want        string
	}{
		"skipped author":               {skip: []string{"alice"}, want: "COMMENT"},
		"author outside the allowlist": {allow: []string{"rev-ann"}, want: "COMMENT"},
		"skipped and listed author":    {skip: []string{"alice"}, allow: []string{"alice"}, want: "COMMENT"},
		"listed author":                {allow: []string{"Alice"}, want: "APPROVE"},
		"no lists":                     {want: "APPROVE"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			w := watch
			w.AutoApproveSkipAuthors, w.AutoApproveAuthors = tc.skip, tc.allow
			e.cfg.Watches = []config.Watch{w}
			e.r.Identity = fakeIdentity{name: "zhuravel", login: "zhuravel", kind: "gh"}
			fa := captureJudge(e)
			p := e.judgePosts(509, "COMMENTED", "COMMENT")
			p.graphLogin, p.graphType, p.restLogin, p.restType = "zhuravel", "User", "zhuravel", "User"
			e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
			in := e.input(KindInitial)
			in.PR.AuthorLogin = new("alice")
			if _, err := e.r.RunRound(e.ctx, in); err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			if jd := fa.data(t); jd.NoFindingsEvent != tc.want || jd.BlockingEvent != "REQUEST_CHANGES" {
				t.Fatalf("judge data events %s/%s, want %s/REQUEST_CHANGES", jd.NoFindingsEvent, jd.BlockingEvent, tc.want)
			}
			mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "identity: gh", "\nno_findings_event: "+tc.want+"\n")
			said := false
			for _, ev := range eventsOfKind(e.events(), "round.judge") {
				said = said || strings.Contains(ev.Message, "no APPROVE: the auto-approval author lists")
			}
			if said != (tc.want == "COMMENT") {
				t.Fatalf("the round.judge event says the author lists withhold the approval: %v, want %v: %+v", said, tc.want == "COMMENT", e.events())
			}
		})
	}
}
