package factory

// The rating request (ReqRate): the task card's + and − on a published or a
// done task. A rating is no decision and changes nothing outside the
// factory, so it asks no y/N on the screen and every one is kept: rating
// again stores a new rating.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// ratingReasons are the keys of a rating's reason, in the order the task
// card offers them by digit: the cause and the fix, how much it changed, how
// it followed your words, the tests, the time it took you. One set serves +
// and −: it names what the verdict is about. The screen's list is pinned to
// this one by a test in the cli.
var ratingReasons = []string{"cause", "scope", "words", "tests", "time"}

// RatingReasons are the keys a rating takes for its reason, in order.
func RatingReasons() []string { return slices.Clone(ratingReasons) }

// RatePayload is a ReqRate: task TaskID is rated Value, 1 for + and -1 for −,
// with the key of a reason (one of RatingReasons; "" for none). DiffOpened says the screen showed you the task's diff before
// (the d key, or the ready card's diff in this run of the screen); the
// daemon adds the diff your newest push on the task's ready card recorded
// as opened. Surface is where you asked, for the record of your input.
type RatePayload struct {
	TaskID     int64
	Value      int
	Reason     string
	DiffOpened bool
	Surface    string
}

// rateTask stores your rating p (ReqRate) and records it as your input. A
// task is rated once it is published or done; the value is 1 or -1 and the
// reason one of RatingReasons. The diff counts as opened first when the screen says so or
// when the registry records that the diff of the task's newest push was
// opened when you pushed (a card sent back and drawn again for a new head
// is a diff not read yet): a rating given without it is noise (spec 11.2),
// which the answer says.
func (e *Engine) rateTask(ctx context.Context, p RatePayload) (string, error) {
	if p.Value != 1 && p.Value != -1 {
		return "", fmt.Errorf("a rating is + (1) or − (-1), not %d", p.Value)
	}
	if p.Reason != "" && !slices.Contains(ratingReasons, p.Reason) {
		return "", fmt.Errorf("a rating takes one of the keys %s, not %q", strings.Join(ratingReasons, ", "), p.Reason)
	}
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if t.State != registry.TaskPublished && t.State != registry.TaskDone {
		return "", fmt.Errorf("%s is %s: a task is rated once it is published or done", TaskKey(t), t.State)
	}
	opened := p.DiffOpened
	if !opened {
		ds, err := e.reg.Decisions(ctx, t.ID)
		if err != nil {
			return "", err
		}
		opened = pushedWithDiffOpened(ds)
	}
	if err := e.reg.RecordRating(ctx, registry.Rating{TaskID: t.ID, Value: p.Value, ReasonKey: p.Reason, DiffOpenedFirst: opened}); err != nil {
		return "", err
	}
	sign := "+"
	if p.Value < 0 {
		sign = "−"
	}
	text := fmt.Sprintf("%+d", p.Value)
	if p.Reason != "" {
		text += ": " + p.Reason
	}
	if _, err := e.recordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputRating, Surface: cmp.Or(p.Surface, "cli"), Stage: t.Stage,
		Text: text}); err != nil {
		e.log.Warn("record a rating's input", "task", t.ID, "err", err)
	}
	words := fmt.Sprintf("%s: rated %s", TaskKey(t), sign)
	if p.Reason != "" {
		words += " (" + p.Reason + ")"
	}
	if opened {
		return words + ", the diff opened first", nil
	}
	return words + ", the diff not opened first: noise", nil
}

// pushedWithDiffOpened reports whether the newest push among decisions ds
// was taken with the diff opened: the diff that went out is the one you
// read. A diff read for a card you sent back is another one; a task with no
// push decision (a PR the factory adopted) has none.
func pushedWithDiffOpened(ds []registry.Decision) bool {
	for _, d := range slices.Backward(ds) {
		if d.Choice == ChoicePush {
			return !d.DiffOpened.IsZero()
		}
	}
	return false
}
