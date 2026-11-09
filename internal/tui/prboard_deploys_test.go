package tui

import (
	"strings"
	"testing"
	"time"
)

// deployRow is #31 of stagingRows with a deploy to bart running at its
// push and one to void that failed in its CI an hour ago.
func deployRow() PRBoardRow {
	r := stagingRows()[0]
	r.StagingDeploys = []StagingDeploy{
		{Effect: "bart-31-20261005t100000-abcd", Env: "bart", Step: "push", Phase: "begin", State: "running", Live: true,
			Text: "push of 6b6b6b6 to staging-bart", At: ago(3 * time.Minute)},
		{Effect: "void-31-20261005t090000-abcd", Env: "void", Step: "watch", Phase: "fail", State: "failed",
			Why: "ci.yml failed on the merge", RunURL: "https://github.com/talkable/talkable/actions/runs/70", At: ago(time.Hour)},
	}
	return r
}

// The card's STAGINGS block says, under each env, the deploys magnum runs
// or ran there in the last day: the effect, its state, the step and phase
// of its last event and how long ago, that event's words or why it ended,
// and the CI run's page.
func TestTheCardShowsEachStagingDeploysStepAndLastEvent(t *testing.T) {
	m := iconBoard(t, IconsUnicode, 160, 90, deployRow())
	m, _ = send(t, m, keyMsg("enter"))
	v := viewOf(m)
	mustContain(t, v,
		"deploy bart-31-20261005t100000-abcd: running · push begin · 3m ago",
		"push of 6b6b6b6 to staging-bart",
		"deploy void-31-20261005t090000-abcd: failed · watch fail · 1h ago",
		"ci.yml failed on the merge",
		"https://github.com/talkable/talkable/actions/runs/70")
	if i, j := strings.Index(v, "◐ bart"), strings.Index(v, "deploy bart-31"); i < 0 || j < i {
		t.Errorf("bart's deploy is not under bart:\n%s", v)
	}
}

// StagingDeploySummary is the card's words of a deploy, for `magnum
// staging`; a deploy's words reach the screen cleaned.
func TestStagingDeploySummaryIsTheCardsWords(t *testing.T) {
	d := deployRow().StagingDeploys[0]
	if got := StagingDeploySummary(d, boardNow); got != "running · push begin · 3m ago" {
		t.Errorf("summary %q", got)
	}
	r := deployRow()
	r.StagingDeploys[0].Text, r.StagingDeploys[1].RunURL = "push \x1b[31mof", "javascript:alert(1)"
	got := sanitizeRow(r).StagingDeploys
	if got[0].Text != "push of" || got[1].RunURL != "" {
		t.Errorf("sanitized %+v", got)
	}
}

// A deploy the library ended done without a deployment that holds its
// merge (its Why starts "pushed, not confirmed", the poll App reading no
// deployments) is not drawn as a success: yellow, in its own words, never
// "done" in green, which only a confirmed deploy is.
func TestAnUnconfirmedStagingDeployIsNotDrawnAsASuccess(t *testing.T) {
	pal := newPRBPalette(defaultStyles)
	p := p0(pal)
	unconfirmed := StagingDeploy{Effect: "bart-31-20261005t100000-abcd", Env: "bart", Step: "confirm", Phase: "ok", State: "done", Unconfirmed: true,
		Why: "pushed, not confirmed: deployments not readable (Deployments: read on the poll App)", At: ago(2 * time.Minute)}
	if got := p.stagingDeployStyle(unconfirmed).GetForeground(); got != pal.yellow.GetForeground() {
		t.Errorf("an unconfirmed done is drawn %v, want yellow", got)
	}
	if got := StagingDeploySummary(unconfirmed, boardNow); got != "not confirmed · confirm ok · 2m ago" {
		t.Errorf("summary %q", got)
	}
	confirmed := unconfirmed
	confirmed.Unconfirmed, confirmed.Why = false, ""
	if got := p.stagingDeployStyle(confirmed).GetForeground(); got != pal.green.GetForeground() {
		t.Errorf("a confirmed done is drawn %v, want green", got)
	}
	r := stagingRows()[0]
	r.StagingDeploys = []StagingDeploy{unconfirmed}
	m := iconBoard(t, IconsUnicode, 160, 90, r)
	m, _ = send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(m), "deploy bart-31-20261005t100000-abcd: not confirmed · confirm ok · 2m ago", "deployments not readable")
}

// An ended deploy's last event says why it ended, as its Why does: the
// card and `magnum staging` say it once. A deploy whose event came less
// than a minute ago says "just now", never "now ago".
func TestAStagingDeploysWordsAreSaidOnce(t *testing.T) {
	d := StagingDeploy{Effect: "void-31-20261005t090000-abcd", Env: "void", Step: "watch", Phase: "fail", State: "failed",
		Why: "ci.yml failed on the merge", Text: "ci.yml failed on the merge", At: ago(20 * time.Second)}
	if got := StagingDeployWords(d); len(got) != 1 || got[0] != "ci.yml failed on the merge" {
		t.Errorf("words %q", got)
	}
	if got := StagingDeploySummary(d, boardNow); got != "failed · watch fail · just now" {
		t.Errorf("summary %q", got)
	}
	r := stagingRows()[0]
	r.StagingDeploys = []StagingDeploy{d}
	m := iconBoard(t, IconsUnicode, 160, 90, r)
	m, _ = send(t, m, keyMsg("enter"))
	if v := viewOf(m); strings.Count(v, "ci.yml failed on the merge") != 1 {
		t.Errorf("the card says it %d times:\n%s", strings.Count(v, "ci.yml failed on the merge"), v)
	}
	d.Text = "ci.yml failed"
	if got := StagingDeployWords(d); len(got) != 2 {
		t.Errorf("words %q, want the event's and why", got)
	}
}

// While the resolver merges a deploy's conflict, the card says under the
// deploy what it does now, naming the agent's pane the operator can open;
// its words reach the screen cleaned.
func TestTheCardSaysWhatTheResolverDoesForAConflictedDeploy(t *testing.T) {
	r := deployRow()
	r.StagingDeploys[0].Step, r.StagingDeploys[0].Text = "merge", "conflict in 1 paths: the resolver works on it (try 1)"
	r.StagingDeploys[0].Resolver = `try 1: the agent resolves 1 conflicted path in slot review1: pane w3:p1 of the herdr workspace "staging merge"`
	m := iconBoard(t, IconsUnicode, 160, 90, r)
	m, _ = send(t, m, keyMsg("enter"))
	mustContain(t, viewOf(m), "resolver try 1: the agent resolves 1 conflicted path in slot review1: pane w3:p1")
	if words := StagingDeployWords(r.StagingDeploys[0]); len(words) != 2 || !strings.HasPrefix(words[1], "resolver try 1: ") {
		t.Errorf("the deploy's words %q", words)
	}
	r.StagingDeploys[0].Resolver = "try 1: pane \x1b[31mw3:p1"
	if got := sanitizeRow(r).StagingDeploys[0].Resolver; got != "try 1: pane w3:p1" {
		t.Errorf("sanitized resolver words %q", got)
	}
}
