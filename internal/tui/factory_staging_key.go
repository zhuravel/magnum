package tui

// E on the factory screen, home's rows and the task card: a staging deploy
// of the row's PR, through magnum's own request path, as the board's E
// (staging_key.go): the board's chooser of the PR's staging envs on the
// footer's rule, enter the daemon's plan of the envs chosen (a dry run),
// its question as the y/N, wrapped over the footer's lines, and y the
// deploy bound to the plan's head and to what it named. The factory daemon
// never deploys: its staging client reads (factory.NewStaging).

import (
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// stagingKey is E on row r (home's, or the card's): on a row whose open PR
// magnum's registry has and whose staging servers the screen shows, the
// chooser of them; else a note of why, and no request.
func (m *factoryModel) stagingKey(r FactoryRow) tea.Cmd {
	if r.PR == nil {
		return m.note("E acts on a PR in magnum's registry: " + cleanText(r.Key) + " has none there")
	}
	row := m.prActRow(r, true)
	row.stagings = stagingInfosOf(r.Stagings)
	if len(row.stagings) == 0 && r.yourPR() { // the factory reads the staging servers of its tasks' PRs alone
		return m.note(row.label + " is your PR: this screen reads no staging servers of it (`magnum staging " +
			cleanText(r.PR.Ref) + " --deploy <envs>` deploys it)")
	}
	if why := actionRefusal(actStaging, row); why != "" {
		return m.note(why)
	}
	switch {
	case m.act == nil:
		return m.note(noActions)
	case m.bar.busy != "": // as the bar refuses a question while an action runs
		return m.bar.show("wait: "+m.bar.busy+" runs", factoryFlashWait)
	}
	m.bar.choose = newStagingChoice(row)
	return nil
}

// chooseKey is a key while E's chooser is up (stagingChoice.press): enter
// with envs chosen asks the daemon's plan of them, and a key that is no
// digit, a or enter cancels.
func (m *factoryModel) chooseKey(k string) tea.Cmd {
	c := m.bar.choose
	envs, up := c.press(k)
	switch {
	case up:
		return nil
	case envs == nil:
		m.bar.choose = nil
		return m.note("Deploy " + c.label + " to staging: cancelled, nothing was run")
	}
	m.bar.choose = nil
	return m.bar.start("Plan the staging deploy of "+c.label, m.stagingPlan(c.ref, c.label, envs))
}

// stagingPlan is the daemon's plan of ref's deploy to envs, whose question
// then goes up as the y/N (wrapped: y answers every migration and warning
// it names) and whose y deploys, bound to the plan's head and to what it
// named (StagingAsked). A deploy the daemon has not answered yet is queued:
// the screen follows no request, so its last line says what follows it.
func (m *factoryModel) stagingPlan(ref, label string, envs []string) factoryAction {
	act := m.act
	return func(ctx context.Context) (ActionResult, error) {
		res, err := act.StagingPlan(ctx, ref, envs)
		if err != nil || res.Ask == "" || res.Staging == nil {
			return res, err
		}
		asked := *res.Staging
		res.nextFactory = &factoryQuestion{sentence: res.Ask, label: "Deploy " + label + " to staging", wrap: true,
			run: func(ctx context.Context) (ActionResult, error) {
				res, err := act.StagingDeploy(ctx, ref, asked)
				if err == nil && slices.ContainsFunc(res.Requests, Request.pending) {
					res.Text = strings.TrimSpace(res.Text + "\nthe staging deploy of " + label + " is queued: `magnum staging " +
						cleanText(ref) + "` follows it")
				}
				return res, err
			}}
		return res, nil
	}
}

// stagingInfosOf are the factory's staging cells as E's chooser lists
// them (the board's StagingInfo), their words cleaned: the env's name and
// where the PR stands on it in the board's states, factory.stagingState
// the other way round: latest is latest; CI, CI ✗ and deploying (the head
// on the staging branch) are pushed; outdated and none are themselves;
// any other state is unknown, since it claims nothing the cell does not.
func stagingInfosOf(cells []StagingCell) []StagingInfo {
	var out []StagingInfo
	for _, c := range cells {
		s := StagingInfo{Env: cleanText(c.Name), State: "unknown"}
		switch st := cleanText(c.State); st {
		case "ci", "ci-fail", "deploying":
			s.State = "pushed"
		case "latest", "outdated", "none":
			s.State = st
		}
		out = append(out, s)
	}
	return out
}
