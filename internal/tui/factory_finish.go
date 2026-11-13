package tui

// The task card's S: end the task's local review now, as `magnum factory
// finish-review`, behind the y/N the factory wrote for it (the card's
// FinishReview, which says what happens: the round in flight finishes, none
// starts after it, a safe stop goes to the ready card). The factory sets it
// only on a task in its local review at work.

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// finishKey ends a task's local review now.
const finishKey = "S"

// askFinish asks the y/N of task card c's FinishReview, and its y ends the
// task's local review; a card that offers none says so and asks nothing.
// The question wraps (factoryQuestion.wrap), as P's and E's do: y answers
// all it names, what an unsafe stop does among it, and the factory's plan
// runs past the card's three lines at common widths.
func (m *factoryModel) askFinish(c TaskCard) tea.Cmd {
	if c.FinishReview == "" {
		return m.note(finishKey + " ends a local review at work: " + c.Key + " is not in one")
	}
	task, act := m.task, m.act
	was := m.bar.asking
	cmd := m.ask(&factoryFollow{kind: followTask, task: task}, c.FinishReview,
		func(ctx context.Context) (ActionResult, error) { return act.FinishReview(ctx, task) })
	if was == nil && m.bar.asking != nil {
		m.bar.asking.wrap = true
	}
	return cmd
}
