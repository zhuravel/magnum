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
