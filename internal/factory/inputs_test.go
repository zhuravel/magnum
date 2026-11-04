package factory

import (
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The one place the engine records an input classifies your words (the task's
// text, guidance, a send-back's note, a pane's turn, one line said, an
// answer), whatever the surface; a decision's text is the machine's choice,
// not your words, a retry has none, a dropped paste has none left, and a
// class the caller knows stays.
func TestRecordInputClassifiesTheWordsYouTyped(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12390", registry.TaskActive, "coding")
	for _, c := range []struct {
		name string
		in   registry.Input
		want string
	}{
		{"a pane's turn that starts a step", registry.Input{Kind: registry.InputPaneTyped, Text: "open PR"}, registry.ClassStepStart},
		{"one line said that approves", registry.Input{Kind: registry.InputSayOneLine, Text: "yes"}, registry.ClassApproval},
		{"a pane's turn that gives access", registry.Input{Kind: registry.InputPaneTyped, Text: "I gave you access to the sandbox"}, registry.ClassAccess},
		{"the task's text", registry.Input{Kind: registry.InputTaskText, Text: "fix the points"}, registry.ClassInfo},
		{"guidance", registry.Input{Kind: registry.InputGuidance, Text: "wrong table, use the ledger"}, registry.ClassCorrection},
		{"a send back's note", registry.Input{Kind: registry.InputSendBack, Text: "you forgot the down method"}, registry.ClassCorrection},
		{"a dropped paste", registry.Input{Kind: registry.InputPaneTyped, DroppedReason: "a long paste"}, ""},
		{"an answer that takes the recommendation", registry.Input{Kind: registry.InputAnswer, Text: "do as recommended"}, registry.ClassStepStart},
		{"an answer in your own words", registry.Input{Kind: registry.InputAnswer, Text: "Seven days before it expires"}, registry.ClassInfo},
		{"an answer that approves", registry.Input{Kind: registry.InputAnswer, Text: "Yes"}, registry.ClassApproval},
		{"a decision", registry.Input{Kind: registry.InputDecision, Text: "push"}, ""},
		{"a retry", registry.Input{Kind: registry.InputRetry}, ""},
		{"a class the caller knows", registry.Input{Kind: registry.InputPaneTyped, Text: "open PR", Class: registry.ClassInfo}, registry.ClassInfo},
	} {
		c.in.TaskID = task.ID
		id, err := h.e.recordInput(h.ctx, c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ins, err := h.reg.Inputs(h.ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, in := range ins {
			if in.ID == id {
				found = true
				if in.Class != c.want {
					t.Errorf("%s: class %q, want %q", c.name, in.Class, c.want)
				}
			}
		}
		if !found {
			t.Errorf("%s: input %d was not recorded", c.name, id)
		}
	}
}
