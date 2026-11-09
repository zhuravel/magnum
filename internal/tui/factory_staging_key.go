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
	"time"

	tea "charm.land/bubbletea/v2"
)

// stagingKey is E on row r (home's, or the card's): the question of r's
// staging plan that answered away from it (held), unless E refuses r now;
// else, on a row whose open PR magnum's registry has and whose repository
// has staging servers, the chooser of them; else a note of why, and no
// request.
func (m *factoryModel) stagingKey(r FactoryRow) tea.Cmd {
	row, words, why := m.stagingRow(r)
	if cmd, held := m.raiseHeldPlan(r.ID, "E", why); held {
		return cmd
	}
	switch {
	case why != "":
		return m.note(why)
	case m.act == nil:
		return m.note(noActions)
	case m.bar.busy != "": // as the bar refuses a question while an action runs
		return m.bar.show("wait: "+m.bar.busy+" runs", factoryFlashWait)
	}
	m.bar.choose, m.bar.chooseRow = newStagingChoice(row), r.ID
	m.bar.choose.words = words
	return nil
}

// stagingRow is what E's chooser lists of row r: the board's row of its PR
// with its staging envs, and each env's state in the screen's words. The
// envs are the row's staging cells (stagingInfosOf, stagingWords); a row
// without cells yet (the factory daemon has not written them, or reads
// none: your own PR's) has the envs the config names for the PR's
// repository (FactoryPR.StagingEnvs), each not read yet, which the review
// daemon plans all the same. why is E's refusal of the row ("" for none):
// no PR in magnum's registry, else the board's (no staging envs in the
// config, a PR merged or closed).
func (m factoryModel) stagingRow(r FactoryRow) (row actRow, words []string, why string) {
	if r.PR == nil {
		return actRow{}, nil, "E acts on a PR in magnum's registry: " + cleanText(r.Key) + " has none there"
	}
	row = m.prActRow(r, true)
	if len(r.Stagings) > 0 {
		row.stagings, words = stagingInfosOf(r.Stagings), m.stagingWords(r.Stagings)
	} else {
		for _, env := range r.PR.StagingEnvs {
			row.stagings = append(row.stagings, StagingInfo{Env: cleanText(env), State: "unknown", Why: stagingNotRead})
			words = append(words, stagingNotRead)
		}
	}
	return row, words, actionRefusal(actStaging, row)
}

// raiseHeldPlan is key (E or P) on row with a plan of it held: the key's
// refusal of the row now (why, as a fresh press runs it) drops the held
// plan with its words, and nothing runs; while another action runs it
// waits; else the plan's question goes up. held is false when no plan of
// the row's is held by key: the key then acts as usual.
func (m *factoryModel) raiseHeldPlan(row int64, key, why string) (cmd tea.Cmd, held bool) {
	i := m.bar.heldOf(&planOf{row: row, key: key})
	switch {
	case i < 0:
		return nil, false
	case why != "":
		h := m.bar.unhold(i)
		text := planDropText(h.q, "dropped", why)
		m.bar.log.add(logEntry{at: m.now(), what: h.q.label, text: text})
		return m.note(text), true
	case m.bar.busy != "":
		return m.bar.show("wait: "+m.bar.busy+" runs", factoryFlashWait), true
	}
	m.bar.raiseHeld(row, key)
	return nil, true
}

// stagingNotRead is the state of an env no staging cell tells yet.
const stagingNotRead = "not read yet"

// stagingWords are cells' states in the factory screen's own words, as the
// row's cell and the task card's STAGINGS line say them in their long form
// (stageCell: "latest", "CI 12/41", "CI ✗", "deploying"), without the
// cell's age; none and unknown in words.
func (m factoryModel) stagingWords(cells []StagingCell) []string {
	p := newCardPainter(m.viewWidth(), m.st, m.g, m.now())
	out := make([]string, len(cells))
	for i, c := range cells {
		c.State, c.Count, c.Seen = cleanText(c.State), cleanText(c.Count), time.Time{}
		var b strings.Builder
		for _, sg := range p.stageCell(c, formLong) {
			b.WriteString(sg.text)
		}
		switch w := b.String(); {
		case c.State == "none":
			out[i] = "none"
		case w == "?":
			out[i] = "unknown"
		default:
			out[i] = w
		}
	}
	return out
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
	p := planOf{row: m.bar.chooseRow, view: m.view, key: "E", name: c.label, ref: c.ref}
	return m.bar.start("Plan the staging deploy of "+c.label, m.stagingPlan(p, envs))
}

// stagingPlan is the daemon's plan of p's PR's deploy to envs, whose
// question then goes up as the y/N, on p's row (wrapped: y answers every
// migration and warning it names), and whose y deploys, bound to the
// plan's head and to what it named (StagingAsked). A deploy the daemon has
// not answered yet is queued: the screen follows no request, so its last
// line says what follows it.
func (m *factoryModel) stagingPlan(p planOf, envs []string) factoryAction {
	act, ref, label := m.act, p.ref, p.name
	return func(ctx context.Context) (ActionResult, error) {
		res, err := act.StagingPlan(ctx, ref, envs)
		if err != nil || res.Ask == "" || res.Staging == nil {
			return res, err
		}
		asked := *res.Staging
		res.nextFactory = &factoryQuestion{sentence: res.Ask, label: "Deploy " + label + " to staging", wrap: true, plan: &p,
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
