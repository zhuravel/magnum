package factory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// answerLeft answers task id's question of a round's leftovers with answer:
// you put the copy back yourself (go on), or drop the task.
func (r *reviewRig) answerLeft(id int64, answer string) {
	r.t.Helper()
	got := r.reload(id)
	qs := r.newestQuestions(id)
	if got.State != registry.TaskNeedsYou || len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionPutBack, optionDrop}) {
		r.t.Fatalf("task %s waits %q with questions %+v; want the put-back-or-drop question", got.State, got.WaitsWhat, qs)
	}
	r.mustAnswer(AnswerPayload{TaskID: id, Revision: got.QuestionRev, Answers: map[int]string{0: answer}})
}

// putBackAlerts are task id's alerts that say the factory put a round's
// leftovers back by itself.
func (r *reviewRig) putBackAlerts(id int64) int {
	n := 0
	for _, ev := range r.events(id, "alert") {
		if strings.Contains(ev.Text, "put back by itself") {
			n++
		}
	}
	return n
}

// The factory puts a round's leftovers back by itself only with no doubt
// that they are the round's alone. Any doubt (an agent's turn in flight in
// the copy, you in one of its panes, you or an agent at work there since
// the round began, a runtime that cannot tell either) saves them first
// (review-1-left.diff, mode 0600) and asks you one question, never "retry":
// you put the copy back yourself and go on, or drop the task. The question
// names the saved diff, the paths and the reviewed head; nothing is put
// back, under the doubt or after it. Once you put the copy back, "go on"
// lets the round's result stand.
func TestARoundsLeftoverInDoubtAsksYouToPutItBackOrDrop(t *testing.T) {
	for _, tc := range []struct {
		name, doubt string
		set, clear  func(r *reviewRig)
	}{
		{"you typed since the round began", "you typed into a pane",
			func(r *reviewRig) {
				r.rt.setTouchedAt("you typed into a pane of PR-12420 at 10:00:00", r.clock.Now().Add(time.Second))
			},
			func(r *reviewRig) { r.rt.setTouchedAt("", time.Time{}) }},
		{"an agent at work", "the fx-writer agent works in the copy",
			func(r *reviewRig) { r.rt.setAtWork("fx-writer") }, func(r *reviewRig) { r.rt.setAtWork("") }},
		{"you in a pane", "you are in the fx-writer pane",
			func(r *reviewRig) { r.rt.setOperatorIn("fx-writer") }, func(r *reviewRig) { r.rt.setOperatorIn("") }},
		{"a runtime that cannot tell who works there", "cannot tell who works in the copy",
			func(r *reviewRig) { r.rt.setAtWorkErr(errors.New("herdr: socket gone")) }, func(r *reviewRig) { r.rt.setAtWorkErr(nil) }},
		{"a runtime that cannot tell who changed it", "cannot tell who changed it",
			func(r *reviewRig) { r.rt.setTouchedErr(errors.New("herdr: socket gone")) }, func(r *reviewRig) { r.rt.setTouchedErr(nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant, or yours\n")
				tc.set(r)
			})
			task, head := r.reviewTask("PR-12420")
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			qs := r.newestQuestions(task.ID)
			if got.State != registry.TaskNeedsYou || strings.Contains(strings.ToLower(got.WaitsWhat), "retry") || len(qs) != 1 ||
				!slices.Equal(optionLabels(qs[0]), []string{optionPutBack, optionDrop}) {
				t.Fatalf("task %s waits %q with %+v; want the put-back-or-drop question", got.State, got.WaitsWhat, qs)
			}
			for _, want := range []string{tc.doubt, "README", "review-1-left.diff", short(head)} {
				if !strings.Contains(qs[0].Text, want) {
					t.Errorf("the question %q does not name %q", qs[0].Text, want)
				}
			}
			if r.read("README") != "a reviewer's mutant, or yours\n" {
				t.Fatal("the factory put the copy back under a doubt")
			}
			fi, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), "review-1-left.diff"))
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("review-1-left.diff %v (%v); want it saved, private", fi, err)
			}
			tc.clear(r)
			r.git(r.copyDir, "checkout", "--", "README") // you put the copy back
			r.answerLeft(task.ID, optionPutBack)
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if after := r.reload(task.ID); after.Stage != "pr_body" || !isLive(after) || len(r.rv.calls()) != 1 || r.putBackAlerts(task.ID) != 0 {
				t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body on the round's result, nothing put back by the factory",
					after.State, after.Stage, after.WaitsWhat, len(r.rv.calls()))
			}
		})
	}
}

// "Go on" only makes the factory look at the copy again: a copy that still
// has what the round left asks again, saying so, and nothing is put back,
// even with the doubt gone. Your drop stops the task.
func TestAGoOnOverACopyThatStillHasTheLeftoverAsksAgain(t *testing.T) {
	r, got := leftAfterYouTyped(t, "PR-12421")
	r.rt.setTouchedAt("", time.Time{}) // the doubt is gone: the factory still puts nothing back by itself
	r.answerLeft(got.ID, optionPutBack)
	r.run(4, func() bool { return r.reload(got.ID).QuestionRev != got.QuestionRev })
	again := r.reload(got.ID)
	qs := r.newestQuestions(got.ID)
	if again.State != registry.TaskNeedsYou || again.QuestionRev == got.QuestionRev || len(qs) != 1 ||
		!strings.Contains(qs[0].Text, "still has it after your go-on (1)") || r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatalf("task %s waits %q with %+v, README %q; want the question again, nothing put back", again.State, again.WaitsWhat, qs,
			r.read("README"))
	}
	r.answerLeft(got.ID, optionDrop)
	r.run(2, nil)
	if after := r.reload(got.ID); isLive(after) || r.read("README") != "your edit, or a reviewer's\n" {
		t.Fatalf("task %s at %s after your drop, README %q; want it stopped, the copy as it was", after.State, after.Stage,
			r.read("README"))
	}
}
