package tui

// Visual QA's key on the factory screen: the task card's A accepts what QA
// left you at the task's head, as `magnum factory retry --accept-qa`,
// behind y/N: the QA defects the writer declined, which QA's fix loop
// stopped on, the fixes a QA run did not walk again, or the states a walk
// could not reach. The card's AcceptQA names them; the factory sets it only
// where the accept goes on. The ready card's e, the QA run's report, is the
// report key's (factory_report.go).

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// acceptQAKey accepts what QA left you: the writer's declines, its fixes QA
// did not walk again, or the states QA could not reach.
const acceptQAKey = "A"

// askAcceptQA asks y/N for the accept of what task card c's AcceptQA names;
// a card with nothing to accept asks nothing.
func (m *factoryModel) askAcceptQA(c TaskCard) tea.Cmd {
	if c.AcceptQA == "" {
		return nil
	}
	task, act := m.task, m.act
	return m.ask(&factoryFollow{kind: followTask, task: task},
		"Accept for "+c.Key+": "+c.AcceptQA+", which the PR body then names as accepted by you?",
		func(ctx context.Context) (ActionResult, error) { return act.AcceptQA(ctx, task) })
}
