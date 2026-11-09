package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// deployFirstWhat is the needs-you line of a PR whose staging deploy comes
// before its merge, as factory.ScreenData gives it.
const deployFirstWhat = "deploy to a staging before the merge (it changes db/migrate/1_a.rb) · nothing open at a1b2c3d · approved, merge is yours"

// deployFirst makes the row of key in d wait on a staging deploy before its
// merge (factory.ScreenData's DeployFirst): its PR is on no staging, its
// move is E, its card waits for the deploy.
func deployFirst(d *FactoryData, key string) {
	for i, r := range d.Rows {
		if r.Key != key {
			continue
		}
		d.Rows[i].WaitsOn = []WaitsOn{{Who: WaitsOnYou, What: deployFirstWhat, Short: "deploy to a staging before the merge", OnYou: true}}
		d.Rows[i].Move, d.Rows[i].MoveShort, d.Rows[i].DeployFirst = "E deploys to a staging", "E deploys it", true
		d.Rows[i].Stagings, d.Rows[i].Help = stagingCells("none", "outdated", "none"), "you: "+deployFirstWhat
		c := d.Cards[r.ID]
		c.Task.Stagings, c.Task.WaitingFor = d.Rows[i].Stagings, "you: "+deployFirstWhat
		d.Cards[r.ID] = c
	}
}

// hintLine is the last line of h's frame: its key hints.
func (h *factoryHarness) hintLine() string {
	h.t.Helper()
	lines := strings.Split(h.view(), "\n")
	return lines[len(lines)-1]
}

// wantEFirst checks that line offers E's deploy right after the hints
// before it (after; "" for none: it is the first), and only once.
func wantEFirst(t *testing.T, where, line, after string) {
	t.Helper()
	before, _, found := strings.Cut(line, "E deploy first")
	before = strings.TrimLeft(strings.TrimRight(before, " ·"), " ")
	switch {
	case !found:
		t.Errorf("%s: the hints %q do not offer E first", where, line)
	case before != after:
		t.Errorf("%s: the hints %q, want E right after %q", where, line, after)
	case strings.Contains(line, "E staging"):
		t.Errorf("%s: the hints %q offer E twice", where, line)
	}
}

// A row whose staging deploy comes before its merge (DeployFirst: its PR
// changes a migration and is on no staging) says so, its move E, and
// offers E first: on home right after enter, on its task card after j k,
// and first on your PR's card; E opens the chooser of its envs as ever,
// with the y/N after the plan. A row without it keeps E last, before ?.
// Before, the row said "merge #4534 on GitHub" and offered E last.
func TestARowWhoseDeployComesFirstOffersEFirst(t *testing.T) {
	h := testPublished(t, 200, 40)
	h.src.change(func(d *FactoryData) {
		deployFirst(d, "PR-12324")
		for i, r := range d.Rows {
			if r.Key == "PR-12320" {
				d.Rows[i].Stagings = stagingCells("latest", "ci", "none")
			}
		}
	})
	h.press("ctrl+r", "g")
	h.selectRow("PR-12324")
	mustContain(t, h.view(), "E deploys to a staging", "deploy to a staging before the merge")
	wantEFirst(t, "home", h.hintLine(), "enter task card")
	h.press("g")
	h.selectRow("PR-12320")
	if line := h.hintLine(); strings.Contains(line, "E deploy first") || !strings.Contains(line, "E staging") {
		t.Errorf("home's hints on a row whose deploy is not first: %q, want E last", line)
	}

	h.open("PR-12324")
	mustContain(t, h.view(), "you: deploy to a staging before the merge (it changes db/migrate/1_a.rb)")
	wantEFirst(t, "the task card", h.hintLine(), "j k move")
	h.press("E")
	mustContain(t, h.view(), "Deploy PR-12324's #4534 to: 1 [ ] void none · 2 [ ] bart outdated · 3 [ ] bastion none")
	h.press("1", "enter")
	mustContain(t, h.view(), stagingAsk("example/talkable#4534", []string{"void"})+" y/N")
	h.wantStaging([]string{"example/talkable#4534 void"}, nil)

	y := testYours(t, 200, 40)
	y.src.change(func(d *FactoryData) { deployFirst(d, "#4603") })
	y.press("ctrl+r")
	y.open("#4603")
	wantEFirst(t, "your PR's card", y.hintLine(), "")
}

// The help lists E among your PR's card's keys, which offers it first where
// the PR's deploy comes before its merge. Before, the list left E out.
func TestTheHelpListsEOnYourPRsCard(t *testing.T) {
	h := testFactory(t, 200, 60)
	mustContain(t, ansi.Strip(strings.Join(h.m.helpContent(200), "\n")),
		"your PR's card has the PR's keys alone: f F K r R z M U, E (offered first where the PR's deploy comes before its merge)")
}
