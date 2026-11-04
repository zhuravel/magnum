package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// The start of a text that names a Jira key somewhere but at its start (the
// operator's request of 2026-11-04, spec 6.1), against the intake rig.

// foundText names PR-12345 in the middle of its words.
const foundText = "fix the expiry date in PR-12345, only the refund path"

// noProjects leaves the rig's trackers with the Jira site alone, naming no
// project: every bare key of the text is then read from Jira.
func noProjects(r *intakeRig) { r.cfg.Board.Trackers = nil }

// foundKeyEvent reports whether task id has the info event of a task started
// from a key found in its text.
func foundKeyEvent(r *intakeRig, id int64, key string) bool {
	for _, ev := range r.events(id, "intake") {
		if ev.Phase == registry.PhaseInfo && ev.Text == "key "+key+" found in your text" {
			return true
		}
	}
	return false
}

// A key in the middle of a start's text that Jira knows starts a key task
// with the whole text as its guidance: nothing is cut out, the ticket is read
// as for a key at the start, and the words are recorded as the guidance that
// starts the task.
func TestAKeyFoundInTheTextStartsAKeyTaskWithTheWholeTextAsGuidance(t *testing.T) {
	r := newIntakeRig(t)
	task := r.mustStart(foundText)
	asked, searched := slices.Clone(r.site.asked), slices.Clone(r.site.searched)
	if task.JiraKey != "PR-12345" || task.Prompt != "" || task.Guidance != foundText || task.State != registry.TaskActive ||
		task.Title != "Credit expiry reminder email" || task.Kind != "feature" || task.Repo != "talkable/talkable" ||
		task.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" {
		t.Fatalf("task %+v, want a key task of PR-12345 with the whole text as guidance", task)
	}
	want, err := r.client.Ticket(r.ctx, "PR-12345")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.taskFile(task.ID, "ticket.md"); got != string(want.Markdown()) {
		t.Fatalf("ticket.md:\n%s\nwant Ticket.Markdown():\n%s", got, want.Markdown())
	}
	if got := r.taskFile(task.ID, "guidance.md"); got != foundText+"\n" {
		t.Fatalf("guidance.md %q, want the whole text", got)
	}
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), "prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("prompt.md for a key task: %v", err)
	}
	ins, err := r.reg.Inputs(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != registry.InputGuidance || ins[0].Role != registry.RoleIntake || ins[0].Text != foundText ||
		ins[0].Surface != "cli" {
		t.Fatalf("inputs %+v, want one guidance input of the intake role with the whole text", ins)
	}
	if !foundKeyEvent(r, task.ID, "PR-12345") {
		t.Fatalf("intake events %+v, want the info event: key PR-12345 found in your text", r.events(task.ID, "intake"))
	}
	if len(asked) != 1 || asked[0] != "PR-12345" {
		t.Fatalf("Jira was asked for %q, want PR-12345 once", asked)
	}
	if len(searched) != 0 {
		t.Fatalf("a key task searched Jira's titles: %q", searched)
	}

	// A Jira link in the middle is its key, whatever the words around it.
	linkText := "this one is wrong " + jiraTestSite + "/browse/PR-12346 only the advocates"
	link := r.mustStart(linkText)
	if link.JiraKey != "PR-12346" || link.Prompt != "" || link.Kind != "bug" || link.Guidance != linkText {
		t.Fatalf("a link in the middle made %+v, want a key task of PR-12346", link)
	}
}

// The message that starts a task is typed and never a step start, however
// the task is started: a key found in words that name a step is no different
// from a key at the start.
func TestTheTextOfAKeyFoundInTheTextIsNeverAStepStart(t *testing.T) {
	r := newIntakeRig(t)
	task := r.mustStart("fix the expiry date in PR-12345, then open PR")
	rep, err := stats.ComputeAt(r.ctx, r.reg, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, ts := range rep.Tasks {
		if ts.TaskID != task.ID {
			continue
		}
		seen = true
		if ts.Typed != 1 || ts.StepStarts != 0 {
			t.Errorf("a found key's text that names a step: typed %d, step starts %d; want 1 and 0", ts.Typed, ts.StepStarts)
		}
	}
	if !seen {
		t.Fatalf("no stats for task %d", task.ID)
	}
}

// A found key follows the one-key-one-task rule as a key at the start does: a
// start whose text names a key that a task holds returns that task, reads
// nothing from Jira and makes no task.
func TestAKeyFoundInTheTextThatHasATaskReturnsThatTask(t *testing.T) {
	r := newIntakeRig(t)
	first := r.mustStart("PR-12345")
	reads := r.site.reads()

	for _, text := range []string{foundText, "the refund of PR-12399 and the reminder of PR-12345"} {
		got, err := r.start(text)
		if !errors.Is(err, ErrKeyRuns) || err.Error() != fmt.Sprintf("PR-12345 already runs as task %d", first.ID) || got.ID != first.ID {
			t.Fatalf("a start of %q = task %d, %v; want task %d with ErrKeyRuns", text, got.ID, err, first.ID)
		}
	}
	if n := len(r.tasks()); n != 1 {
		t.Fatalf("%d tasks, want 1: no new row", n)
	}
	if n := r.site.reads(); n != reads {
		t.Fatalf("a start for a key that runs read Jira %d times more", n-reads)
	}
}

// A word of the shape of a key that Jira does not know ("UTF-8", "ISO-8601")
// stays a word of the prompt, and that is no error: the task is a prompt
// task as before. A key Jira knows counts whatever unknown ones come before
// it.
func TestAnUpperCaseWordJiraDoesNotKnowStaysInThePrompt(t *testing.T) {
	r := newIntakeRig(t)
	noProjects(r)
	text := "Read the dates as UTF-8 and ISO-8601 in the digest export."
	task, err := r.start(text)
	if err != nil {
		t.Fatalf("a start with unknown keys in its text = %v, want no error", err)
	}
	r.e.waitBackground()
	if task.JiraKey != "" || task.Prompt != text || task.Guidance != "" || task.State != registry.TaskActive {
		t.Fatalf("task %+v, want a prompt task of the whole text", task)
	}
	if got := r.site.asked; !slices.Equal(got, []string{"UTF-8", "ISO-8601"}) {
		t.Fatalf("Jira was asked for %q, want both words once", got)
	}
	if got := r.taskFile(task.ID, "prompt.md"); got != text+"\n" {
		t.Fatalf("prompt.md %q", got)
	}
	ins, _ := r.reg.Inputs(r.ctx, task.ID)
	if len(ins) != 1 || ins[0].Kind != registry.InputTaskText || ins[0].Text != text {
		t.Fatalf("inputs %+v, want the task's text", ins)
	}
	if qs, _ := r.reg.Questions(r.ctx, task.ID); len(qs) != 1 || qs[0].Text != whichIssue || len(r.site.searched) != 1 {
		t.Fatalf("questions %+v after %d searches, want the title search's question as for any prompt", qs, len(r.site.searched))
	}
	for _, ev := range r.events(task.ID, "intake") {
		if strings.Contains(ev.Text, "found in your text") || ev.Phase == registry.PhaseFail {
			t.Fatalf("intake event %+v, want none about the words Jira does not know", ev)
		}
	}

	// A known key after an unknown word counts.
	r.site.asked = nil
	mixed := r.mustStart("Read the dates as UTF-8 in the reminder of PR-12346")
	if mixed.JiraKey != "PR-12346" || !slices.Equal(r.site.asked, []string{"UTF-8", "PR-12346"}) {
		t.Fatalf("task %+v after reading %q, want a key task of PR-12346", mixed, r.site.asked)
	}
}

// Jira is asked for at most 3 of the keys a text names, the first 3.
func TestNoMoreThanThreeFoundKeysAreRead(t *testing.T) {
	r := newIntakeRig(t)
	noProjects(r)
	task := r.mustStart("Read UTF-8, ISO-8601 and SHA-256 dates in the reminder of PR-12345")
	if want := []string{"UTF-8", "ISO-8601", "SHA-256"}; !slices.Equal(r.site.asked, want) {
		t.Fatalf("Jira was asked for %q, want only the first 3 words: %q", r.site.asked, want)
	}
	if task.JiraKey != "" || task.Prompt == "" {
		t.Fatalf("task %+v, want a prompt task: the fourth key is not read", task)
	}
}

// Several keys of the text that Jira knows start a prompt task that asks
// which issue it is, with the keys as its options in the text's order, the
// first recommended; no title search is made.
func TestSeveralKeysFoundInTheTextAskWhichIssue(t *testing.T) {
	r := newIntakeRig(t)
	text := "the reminder of PR-12345 and the refund of PR-12346 both lose points"
	task := r.mustStart(text)
	if task.JiraKey != "" || task.Prompt != text || task.State != registry.TaskActive {
		t.Fatalf("task %+v, want a prompt task of the whole text", task)
	}
	qs, err := r.reg.Questions(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 {
		t.Fatalf("questions %+v, want one", qs)
	}
	q := qs[0]
	if q.Text != whichIssue || q.Revision != 1 || q.State != registry.QuestionOpen || q.RaisedBy != "intake" {
		t.Fatalf("question %+v", q)
	}
	var labels []string
	for _, o := range q.Options {
		labels = append(labels, o.Label)
	}
	if want := []string{"PR-12345 Credit expiry reminder email", "PR-12346 Points vanish after a refund", "no issue"}; !slices.Equal(labels, want) {
		t.Fatalf("options %q, want %q", labels, want)
	}
	if q.Recommended != 0 || q.Why == "" {
		t.Fatalf("recommended %d (%q), want the first key, 0", q.Recommended, q.Why)
	}
	if got := r.e.namedIssue(task, q, q.Options[1].Label); got != "PR-12346" {
		t.Fatalf("an answer taking the second option names %q, want PR-12346: Task 13's link reads it", got)
	}
	if len(r.site.searched) != 0 {
		t.Fatalf("a title search was made: %q", r.site.searched)
	}
	if got := r.reload(task.ID); got.QuestionRev != 1 {
		t.Fatalf("the task is at question revision %d, want 1", got.QuestionRev)
	}

	// The order of the text decides which is first.
	swapped := r.mustStart("the refund of PR-12346 first, then the reminder of PR-12345")
	qs, _ = r.reg.Questions(r.ctx, swapped.ID)
	if len(qs) != 1 || !strings.HasPrefix(qs[0].Options[qs[0].Recommended].Label, "PR-12346 ") {
		t.Fatalf("questions %+v, want PR-12346 recommended: it comes first in the text", qs)
	}
}

// If Jira does not answer, a key found in the text waits like a key at the
// start: "start when Jira answers", tried after 1, 2 and 4 minutes, for no
// cost, and after 6 hours it goes on from the operator's words. The wait
// holds no key (none is known yet). When Jira answers, the key it knows starts
// the key task, with the whole text as its guidance and the branch named for
// the key.
func TestAKeyFoundInTheTextWaitsForJiraLikeAKeyAtTheStart(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	task := r.mustStart(foundText)
	if task.State != registry.TaskWaiting || task.WaitsWho != "Jira" || task.WaitsWhat != "start when Jira answers" {
		t.Fatalf("task %s waits %q: %q, want waiting on Jira: start when Jira answers", task.State, task.WaitsWho, task.WaitsWhat)
	}
	if task.JiraKey != "" || task.Prompt != foundText || task.Kind != "bug" || !strings.HasPrefix(task.Branch, "zhuravel-") {
		t.Fatalf("task %+v, want the text kept, a kind from its words and a branch, no key yet", task)
	}
	ins, _ := r.reg.Inputs(r.ctx, task.ID)
	if len(ins) != 1 || ins[0].Role != registry.RoleIntake || ins[0].Text != foundText {
		t.Fatalf("inputs %+v, want the text recorded once, as the start", ins)
	}
	if p := r.taskFile(task.ID, "prompt.md"); p != foundText+"\n" { // a task you take over meanwhile has your request
		t.Fatalf("prompt.md %q, want the whole text", p)
	}
	at := func(d time.Duration) {
		t.Helper()
		r.clock.Set(t0.Add(d))
		r.tick()
	}
	base := r.site.curlCalls()
	for _, step := range []struct {
		at    time.Duration
		tries int
	}{{59 * time.Second, 0}, {time.Minute, 1}, {2*time.Minute + 59*time.Second, 1}, {3 * time.Minute, 2}, {7 * time.Minute, 3}} {
		at(step.at)
		if got := r.site.curlCalls() - base; got != step.tries {
			t.Fatalf("at +%s Jira was tried %d times, want %d", step.at, got, step.tries)
		}
		if got := r.reload(task.ID); got.State != registry.TaskWaiting || got.WaitsWho != "Jira" {
			t.Fatalf("at +%s the task is %s waiting on %q", step.at, got.State, got.WaitsWho)
		}
	}
	if evs := r.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("Jira's outage counted as the task's failure: %+v", evs)
	}
	// After 6 hours of silence a key task needs you; a text whose found keys
	// never got an answer goes on from the operator's words instead.
	at(6 * time.Hour)
	r.e.waitBackground()
	got := r.reload(task.ID)
	if got.State != registry.TaskActive || got.WaitsWho != "" || got.JiraKey != "" || got.Prompt != foundText {
		t.Fatalf("after 6h the task is %+v, want a prompt task that goes on from the words", got)
	}
	if !refusedEvent(r, task.ID, "Jira did not answer for 6h; it goes on from your words") {
		t.Fatalf("intake events %+v, want the info event: Jira did not answer for 6h; it goes on from your words", r.events(task.ID, "intake"))
	}

	// Another task of the same words waits too, and Jira answers it at once.
	r.clock.Set(t0.Add(7 * time.Hour))
	second := r.mustStart(foundText)
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	got = r.reload(second.ID)
	if got.State != registry.TaskActive || got.WaitsWho != "" || got.JiraKey != "PR-12345" || got.Guidance != foundText ||
		got.Title != "Credit expiry reminder email" || got.Kind != "feature" || got.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" {
		t.Fatalf("after Jira answered the task is %+v, want the key task of PR-12345 with the whole text as guidance", got)
	}
	if md := r.taskFile(second.ID, "ticket.md"); !strings.HasPrefix(md, "# PR-12345 ") {
		t.Fatalf("ticket.md:\n%s", md)
	}
	if g := r.taskFile(second.ID, "guidance.md"); g != foundText+"\n" {
		t.Fatalf("guidance.md %q, want the whole text", g)
	}
	if !foundKeyEvent(r, second.ID, "PR-12345") {
		t.Fatalf("intake events %+v, want the info event: key PR-12345 found in your text", r.events(second.ID, "intake"))
	}
	if ins, _ := r.reg.Inputs(r.ctx, second.ID); len(ins) != 1 {
		t.Fatalf("inputs %+v, want the one start", ins)
	}
	if _, err := r.start(foundText); !errors.Is(err, ErrKeyRuns) {
		t.Fatalf("a second start of the text Jira answered for = %v, want ErrKeyRuns", err)
	}
}

// The kind you chose stays for a key found in the text, at once and after a
// wait for Jira; the kind your words pick follows the ticket's type then.
func TestAKindYouChoseStaysForAKeyFoundInTheText(t *testing.T) {
	r := newIntakeRig(t)
	chosen, err := r.e.start(r.ctx, StartPayload{Text: foundText, Kind: "chore"})
	if err != nil || chosen.Kind != "chore" {
		t.Fatalf("a start with a kind = %+v, %v; want the chore kept", chosen, err)
	}
	r.e.waitBackground()

	r.site.setDown(7)
	waited, err := r.e.start(r.ctx, StartPayload{Text: "fix the reminder email in PR-12346, only the refund path", Kind: "feature"})
	if err != nil || waited.State != registry.TaskWaiting || waited.Kind != "feature" {
		t.Fatalf("a start while Jira is down = %+v, %v; want a waiting task of the kind you chose", waited, err)
	}
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	if got := r.reload(waited.ID); got.State != registry.TaskActive || got.JiraKey != "PR-12346" || got.Kind != "feature" {
		t.Fatalf("after Jira answered the task is %+v, want the kind you chose kept (the ticket's type is a Bug)", got)
	}
}

// A task that waits for Jira with a key found in its text can be taken over
// and let go on, as a key task's wait can: it starts its kind's first stage
// with the request it has, and Jira's answer then finds the task at its stage
// and leaves it alone.
func TestAWaitingTextYouTakeOverGoesOnWithYourRequest(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	task := r.mustStart(foundText)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskNeedsYou, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.reload(task.ID); got.Stage != "investigate" || got.JiraKey != "" {
		t.Fatalf("the resumed task is %+v, want it at its kind's first stage with no key", got)
	}
	if p := r.taskFile(task.ID, "prompt.md"); p != foundText+"\n" {
		t.Fatalf("prompt.md %q, want the request", p)
	}
	if evs := r.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("the resumed task failed: %+v", evs)
	}
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	if got := r.reload(task.ID); got.JiraKey != "" || got.Stage != "investigate" {
		t.Fatalf("a task at its stage was changed after Jira answered: %+v", got)
	}
}

// When Jira, once it answers, does not know the keys of a waiting text, the
// task goes on from the operator's words as a prompt task, with no error; and
// when it knows several, it asks which issue.
func TestAWaitingTextGoesOnFromTheWordsWhenJiraKnowsNoKey(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	words := r.mustStart("Read the dates of PR-12399 in the digest export")
	several := r.mustStart("the refund of PR-12346 and the reminder of PR-12345 both lose points")
	for _, task := range []registry.Task{words, several} {
		if task.State != registry.TaskWaiting || task.WaitsWho != "Jira" {
			t.Fatalf("task %+v, want waiting on Jira", task)
		}
	}
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()

	got := r.reload(words.ID)
	if got.State != registry.TaskActive || got.WaitsWho != "" || got.JiraKey != "" || got.Prompt != words.Prompt || got.Kind != words.Kind {
		t.Fatalf("after Jira answered the task is %+v, want a prompt task that goes on", got)
	}
	if p := r.taskFile(words.ID, "prompt.md"); p != words.Prompt+"\n" {
		t.Fatalf("prompt.md %q", p)
	}
	if qs, _ := r.reg.Questions(r.ctx, words.ID); len(qs) != 1 || qs[0].Text != whichIssue || len(r.site.searched) != 1 {
		t.Fatalf("questions %+v after %d searches, want the title search's question", qs, len(r.site.searched))
	}
	if foundKeyEvent(r, words.ID, "PR-12399") {
		t.Fatal("an unknown key was reported as a found key")
	}

	got = r.reload(several.ID)
	qs, _ := r.reg.Questions(r.ctx, several.ID)
	if got.State != registry.TaskActive || got.JiraKey != "" || len(qs) != 1 || qs[0].Text != whichIssue || len(qs[0].Options) != 3 ||
		!strings.HasPrefix(qs[0].Options[0].Label, "PR-12346 ") || qs[0].Recommended != 0 {
		t.Fatalf("after Jira answered the task is %+v with questions %+v, want a prompt task asking which issue, PR-12346 first", got, qs)
	}
}

// A task that waited for Jira cannot take a key that a task holds since: it
// needs you, naming the holder, and no second task holds the key.
func TestAWaitingTextWhoseKeyGotATaskNeedsYou(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	waiting := r.mustStart(foundText)
	holder := r.mustStart("PR-12345")
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()

	got := r.reload(waiting.ID)
	if got.State != registry.TaskNeedsYou || got.JiraKey != "" || !strings.Contains(got.WaitsWhat, fmt.Sprintf("PR-12345 already runs as task %d", holder.ID)) {
		t.Fatalf("the waiting task is %s with key %q: %q, want needs_you naming the holder", got.State, got.JiraKey, got.WaitsWhat)
	}
	if h := r.reload(holder.ID); h.State != registry.TaskActive || h.JiraKey != "PR-12345" {
		t.Fatalf("the holder is %+v, want it started", h)
	}
}

// Without [jira] a key in the text is a word: nothing can read it, and the
// start is a prompt task with no error.
func TestFoundKeysAreWordsWhenJiraIsOff(t *testing.T) {
	r := newIntakeRig(t)
	r.e.d.Jira = nil
	task, err := r.start(foundText)
	if err != nil || task.JiraKey != "" || task.Prompt != foundText || task.State != registry.TaskActive {
		t.Fatalf("a found key with [jira] off = %+v, %v; want a prompt task and no error", task, err)
	}
}

// refusedEvent reports whether task id has the info event text among its
// intake events.
func refusedEvent(r *intakeRig, id int64, text string) bool {
	for _, ev := range r.events(id, "intake") {
		if ev.Phase == registry.PhaseInfo && ev.Text == text {
			return true
		}
	}
	return false
}

// A found key is never an error: whatever Jira refuses it for (a missing
// token file, a 401, any other refusal) but "no such issue" and silence, it
// stays a word and the task goes on from the operator's words as a prompt
// task, with an info event naming the cause. A broken Jira setup never blocks
// a prompt that holds a key-shaped word. Only a key at the start of the text
// keeps the error.
func TestAFoundKeyJiraRefusesStaysAWordAndNamesTheCause(t *testing.T) {
	r := newIntakeRig(t)
	missing := filepath.Join(t.TempDir(), "jira.token")
	r.client.TokenFile = missing
	task, err := r.start(foundText)
	if err != nil {
		t.Fatalf("a found key with no token file = %v, want no error", err)
	}
	r.e.waitBackground()
	if task.JiraKey != "" || task.Prompt != foundText || task.State != registry.TaskActive {
		t.Fatalf("task %+v, want a prompt task of the whole text", task)
	}
	if p := r.taskFile(task.ID, "prompt.md"); p != foundText+"\n" {
		t.Fatalf("prompt.md %q", p)
	}
	want := "Jira refused PR-12345: the Jira token file is missing: " + missing + "; starts from your words"
	if !refusedEvent(r, task.ID, want) {
		t.Fatalf("intake events %+v, want the info event %q", r.events(task.ID, "intake"), want)
	}
	if foundKeyEvent(r, task.ID, "PR-12345") {
		t.Fatal("a refused key was reported as found")
	}

	// A key at the start keeps today's error.
	if _, err := r.start("PR-12345"); err == nil || err.Error() != "the Jira token file is missing: "+missing {
		t.Fatalf("a key at the start with no token file = %v, want the Jira token file is missing: %s", err, missing)
	}
	if n := len(r.tasks()); n != 1 {
		t.Fatalf("%d tasks, want only the prompt task", n)
	}

	// Any other refusal is the same: a 401 here, with the cause in Jira's words.
	other := newIntakeRig(t)
	other.site.refuse("PR-12345", 401)
	got := other.mustStart(foundText)
	cause := "Jira refused the credentials (HTTP 401): check jira.user and jira.token_file"
	if got.JiraKey != "" || got.Prompt != foundText || !refusedEvent(other, got.ID, "Jira refused PR-12345: "+cause+"; starts from your words") {
		t.Fatalf("task %+v with events %+v, want a prompt task and the cause named", got, other.events(got.ID, "intake"))
	}

	// A 403 is not "no such issue": the account is refused, and that is said.
	forbidden := newIntakeRig(t)
	forbidden.site.refuse("PR-12345", 403)
	denied := forbidden.mustStart(foundText)
	if denied.JiraKey != "" || denied.Prompt != foundText ||
		!refusedEvent(forbidden, denied.ID, "Jira refused PR-12345: Jira refused the account (HTTP 403); starts from your words") {
		t.Fatalf("task %+v with events %+v, want a prompt task and the 403 named", denied, forbidden.events(denied.ID, "intake"))
	}

	// "No such issue" (a 404) is the one refusal that is only a word, and says nothing.
	unknown := other.mustStart("the refund of PR-12398 is wrong")
	for _, ev := range other.events(unknown.ID, "intake") {
		if strings.Contains(ev.Text, "refused") {
			t.Fatalf("intake event %+v about a key Jira has no issue for", ev)
		}
	}

	// A key Jira refuses beside one it knows stays a word of the key task.
	other.site.refuse("PR-12399", 400)
	mixed := other.mustStart("the refund of PR-12399 and the reminder of PR-12346")
	if mixed.JiraKey != "PR-12346" || !refusedEvent(other, mixed.ID, "Jira refused PR-12399: Jira answered HTTP 400; it stays a word of your text") {
		t.Fatalf("task %+v with events %+v, want a key task of PR-12346 and the refusal named", mixed, other.events(mixed.ID, "intake"))
	}
}

// A task that waited for Jira and finds its key refused does not need you: it
// goes on from the operator's words, with the cause in its events.
func TestAWaitingTextGoesOnFromTheWordsWhenJiraRefusesItsKey(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	task := r.mustStart(foundText)
	missing := filepath.Join(t.TempDir(), "jira.token")
	r.client.TokenFile = missing
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()

	got := r.reload(task.ID)
	if got.State != registry.TaskActive || got.WaitsWho != "" || got.JiraKey != "" || got.Prompt != foundText {
		t.Fatalf("after Jira refused the key the task is %+v, want a prompt task that goes on", got)
	}
	want := "Jira refused PR-12345: the Jira token file is missing: " + missing + "; starts from your words"
	if !refusedEvent(r, task.ID, want) {
		t.Fatalf("intake events %+v, want the info event %q", r.events(task.ID, "intake"), want)
	}
}

// The preview names the cause before the y: a found key Jira refuses is
// "Jira refused PR-12345: ...; starts from your words" on the JIRA line, with
// a prompt's kind, title and branch; a key at the start keeps its error.
func TestThePreviewNamesTheCauseWhenJiraRefusesAFoundKey(t *testing.T) {
	r := newIntakeRig(t)
	missing := filepath.Join(t.TempDir(), "jira.token")
	r.client.TokenFile = missing
	f, err := r.e.preview(r.ctx, foundText)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Jira refused PR-12345: the Jira token file is missing: " + missing + "; starts from your words"; f.Err != want {
		t.Fatalf("preview Err %q, want %q", f.Err, want)
	}
	if f.Jira != nil || f.Title != foundText || f.Kind == "" || f.Branch == "" || f.Starts == "" {
		t.Fatalf("preview %+v, want a prompt's preview", f)
	}
	if f, _ = r.e.preview(r.ctx, "PR-12345 only the refund path"); f.Err != "the Jira token file is missing: "+missing || f.Jira != nil {
		t.Fatalf("preview of a key at the start %+v, want today's error", f)
	}
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("a preview made %d tasks", n)
	}

	// Beside a key Jira knows, the note joins the key's line.
	other := newIntakeRig(t)
	other.site.refuse("PR-12399", 400)
	f, _ = other.e.preview(other.ctx, "the refund of PR-12399 and the reminder of PR-12346")
	if f.Jira == nil || f.Jira.Key != "PR-12346" || !strings.Contains(f.Jira.Found, "Jira refused PR-12399: Jira answered HTTP 400") || f.Err != "" {
		t.Fatalf("preview with a refused key beside a known one %+v (Jira %+v)", f, f.Jira)
	}

	// A 403 is named too, and a 404 is not.
	forbidden := newIntakeRig(t)
	forbidden.site.refuse("PR-12345", 403)
	f, _ = forbidden.e.preview(forbidden.ctx, foundText)
	if want := "Jira refused PR-12345: Jira refused the account (HTTP 403); starts from your words"; f.Err != want || f.Jira != nil {
		t.Fatalf("preview of a key Jira refuses with a 403 %+v, want Err %q", f, want)
	}
	if f, _ = forbidden.e.preview(forbidden.ctx, "the refund of PR-12398 is wrong"); f.Err != "" || f.Jira != nil {
		t.Fatalf("preview of a key Jira has no issue for %+v, want a prompt's", f)
	}
}

// When the trackers name Jira projects, a bare key of another project is a
// word, read from no Jira: "UTF-8" never waits on a Jira that is down, and the
// preview does not mention Jira. A Jira link on the tracker's site counts
// whatever its project.
func TestABareKeyOfAnotherProjectIsAWordReadFromNoJira(t *testing.T) {
	r := newIntakeRig(t) // its tracker names the project PR
	r.site.setDown(7)
	text := "Read the dates as UTF-8 and ISO-8601 in the digest export, and the report of OPS-8."
	task := r.mustStart(text)
	if task.State != registry.TaskActive || task.JiraKey != "" || task.Prompt != text {
		t.Fatalf("task %+v, want a prompt task that started at once: no word of the text waits on Jira", task)
	}
	if len(r.site.asked) != 0 {
		t.Fatalf("Jira was asked for %q, want no key", r.site.asked)
	}
	f, err := r.e.preview(r.ctx, text)
	if err != nil || f.Err != "" || f.Jira != nil || f.Title != promptTitle(text) {
		t.Fatalf("preview %+v (%v), want a prompt's, with nothing about Jira", f, err)
	}

	r.site.setDown(0)
	r.site.issues["OPS-7"] = issueJSON("OPS-7", "The ops report is slow", "Bug", "It takes a minute.")
	linked := r.mustStart("the page " + jiraTestSite + "/browse/OPS-7 is slow, and UTF-8 is not the cause")
	if linked.JiraKey != "OPS-7" || !slices.Equal(r.site.asked, []string{"OPS-7"}) {
		t.Fatalf("task %+v after reading %q, want a key task of OPS-7 from its link", linked, r.site.asked)
	}
}

// For a found key, silence is more than curl's exits 6, 7 and 28 and a 5xx: a
// 429 and every curl failure to reach Jira count, so the task waits for Jira
// instead of dropping a key the operator named.
func TestAFoundKeyThatJiraAnswersTooOftenOrCannotBeReachedWaits(t *testing.T) {
	r := newIntakeRig(t)
	r.site.refuse("PR-12345", 429)
	busy := r.mustStart(foundText)
	if busy.State != registry.TaskWaiting || busy.WaitsWho != "Jira" || busy.JiraKey != "" {
		t.Fatalf("a 429: task %+v, want waiting on Jira", busy)
	}
	for _, exit := range []int{35, 52, 56} { // curl: TLS handshake, empty reply, receive failure
		other := newIntakeRig(t)
		other.site.setDown(exit)
		got := other.mustStart(foundText)
		if got.State != registry.TaskWaiting || got.WaitsWho != "Jira" {
			t.Fatalf("curl exit %d: task %+v, want waiting on Jira", exit, got)
		}
	}
	// A key at the start keeps today's meaning: only exits 6, 7 and 28 and a 5xx are silence.
	if _, err := newIntakeRigDown(t, 35).start("PR-12345"); err == nil {
		t.Fatal("a key at the start with curl exit 35 made a task, want today's error")
	}
}

// newIntakeRigDown is a rig whose Jira is down with curl exit code.
func newIntakeRigDown(t *testing.T, code int) *intakeRig {
	t.Helper()
	r := newIntakeRig(t)
	r.site.setDown(code)
	return r
}

// The CLI has no preview, so the answer to a start names the refusal of a
// found key: the task started from the words, and why.
func TestAStartRequestNamesTheRefusalOfAFoundKey(t *testing.T) {
	r := newIntakeRig(t)
	missing := filepath.Join(t.TempDir(), "jira.token")
	r.client.TokenFile = missing
	id, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: foundText})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.e.waitBackground()
	req, _ := r.reg.RequestByID(r.ctx, id)
	note := "Jira refused PR-12345: the Jira token file is missing: " + missing + "; starts from your words"
	if req.State != registry.RequestDone || !strings.HasPrefix(req.Result, `started "`+foundText+`" as task 1: `) || !strings.HasSuffix(req.Result, ". "+note) {
		t.Fatalf("start request %s %q, want the answer of a start with %q after it", req.State, req.Result, note)
	}
}

// slowRunner answers after d, or when its context ends first.
type slowRunner struct {
	d     time.Duration
	next  execx.Runner
	mu    sync.Mutex
	calls int
}

func (s *slowRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-time.After(s.d):
		return s.next.Run(ctx, c)
	case <-ctx.Done():
		return execx.Result{}, ctx.Err()
	}
}

// The reads of the keys of one start, and of one preview, share one deadline:
// a Jira that answers slowly cannot hold the engine's loop for a read per
// key. When the time is out, that is silence: the task waits for Jira, and the
// preview says Jira does not answer. It is never a failed start.
func TestTheFoundReadsOfAStartShareOneDeadline(t *testing.T) {
	storetest.Serial(t)
	old := foundBudget
	foundBudget = 150 * time.Millisecond
	t.Cleanup(func() { foundBudget = old })
	r := newIntakeRig(t)
	slow := &slowRunner{d: 100 * time.Millisecond, next: r.client.Runner}
	r.client.Runner = slow
	text := "the refund of PR-12346 and the reminder of PR-12345 and the third of PR-12347"
	r.site.issues["PR-12347"] = issueJSON("PR-12347", "Upgrade the gems", "Task", "Bundler 3.")

	task, err := r.start(text)
	if err != nil {
		t.Fatalf("a start whose reads ran out of time = %v, want a waiting task", err)
	}
	slow.mu.Lock()
	calls := slow.calls
	slow.mu.Unlock()
	if task.State != registry.TaskWaiting || task.WaitsWho != "Jira" || task.JiraKey != "" || calls > 2 {
		t.Fatalf("task %+v after %d reads, want it waiting on Jira: the third read is not started once the time is out", task, calls)
	}

	f, err := r.e.preview(r.ctx, text)
	if err != nil || f.Err != "Jira does not answer: the task waits for it, then starts" {
		t.Fatalf("preview %+v (%v), want Jira does not answer", f, err)
	}
}

// A task that waits for Jira with the keys of its text and finds [jira]
// switched off goes on from the words, as a start with [jira] off does.
func TestAWaitingTextGoesOnFromTheWordsWhenJiraIsSwitchedOff(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	task := r.mustStart(foundText)
	r.e.d.Jira = nil
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	got := r.reload(task.ID)
	if got.State != registry.TaskActive || got.WaitsWho != "" || got.JiraKey != "" || got.Prompt != foundText {
		t.Fatalf("with [jira] off the task is %+v, want a prompt task that goes on", got)
	}
	if !refusedEvent(r, task.ID, "[jira] is off; it goes on from your words") {
		t.Fatalf("intake events %+v, want the info event: [jira] is off; it goes on from your words", r.events(task.ID, "intake"))
	}
}

// A text whose only key-shaped words meet a Jira that is down previews as the
// prompt it will be, with Jira's silence on the JIRA line.
func TestThePreviewOfKeyShapedWordsMeetsADownJira(t *testing.T) {
	r := newIntakeRig(t)
	noProjects(r)
	r.site.setDown(7)
	text := "Read the dates as UTF-8 in the digest export"
	f, err := r.e.preview(r.ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if f.Err != "Jira does not answer: the task waits for it, then starts" || f.Jira != nil || f.Title != text || f.Kind == "" || f.Branch == "" || f.Starts == "" {
		t.Fatalf("preview %+v, want a prompt's preview with Jira's silence", f)
	}
}

// A GitHub link in the middle of the text stays words of the prompt:
// adoption and CI tasks come from a link at the start.
func TestAGitHubLinkInTheMiddleOfTheTextStaysWords(t *testing.T) {
	r := newIntakeRig(t)
	text := "the title of https://github.com/talkable/talkable/pull/4527 is wrong, and the run https://github.com/talkable/talkable/actions/runs/1234567890 too"
	task, err := r.start(text)
	if err != nil {
		t.Fatalf("a start with GitHub links in the middle = %v, want a prompt task", err)
	}
	r.e.waitBackground()
	if task.JiraKey != "" || task.Prompt != text {
		t.Fatalf("task %+v, want a prompt task of the whole text", task)
	}
	if got := r.site.asked; len(got) != 0 {
		t.Fatalf("Jira was asked for %q, want no key", got)
	}
}

// The branch and the PR's title use a key found in the text as they do a key
// at the start, for a task that started at once and for one that waited for
// Jira.
func TestThePRTitleUsesAKeyFoundInTheText(t *testing.T) {
	r := newIntakeRig(t)
	now := r.mustStart(foundText)
	r.site.setDown(7)
	waited := r.mustStart("fix the reminder email in PR-12346, only the refund path")
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	waited = r.reload(waited.ID)

	for _, c := range []struct {
		task          registry.Task
		title, branch string
	}{
		{now, "PR-12345 Credit expiry reminder email", "zhuravel-PR-12345-credit-expiry-reminder-email"},
		{waited, "PR-12346 Points vanish after a refund", "zhuravel-PR-12346-points-vanish-after-refund"},
	} {
		if got := prTitle(c.task); got != c.title {
			t.Errorf("the PR title of %s = %q, want %q", c.task.JiraKey, got, c.title)
		}
		if err := r.e.nameRemoteBranch(r.ctx, &c.task); err != nil || c.task.RemoteBranch != c.branch {
			t.Errorf("the PR's branch of %s = %q (%v), want %q", c.task.JiraKey, c.task.RemoteBranch, err, c.branch)
		}
	}
}

// The new-task preview shows a key found in the text with the note "found
// in your text" for the JIRA line, the ticket the daemon read, and what a
// key task would be; it makes nothing. Words Jira does not know are a
// prompt's preview.
func TestThePreviewSaysTheKeyWasFoundInTheText(t *testing.T) {
	r := newIntakeRig(t)
	f, err := r.e.preview(r.ctx, foundText)
	if err != nil {
		t.Fatal(err)
	}
	if f.Jira == nil || f.Jira.Key != "PR-12345" || f.Jira.Found != "found in your text" || f.Jira.Type != "Story" ||
		f.Jira.Summary != "Credit expiry reminder email" || f.Jira.Read.IsZero() {
		t.Fatalf("preview Jira %+v, want PR-12345 with the note found in your text", f.Jira)
	}
	if f.Kind != "feature" || f.KindWhy != "from the Jira type Story" || f.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" ||
		f.Err != "" || f.Repo != "talkable/talkable" {
		t.Fatalf("preview %+v", f)
	}
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("a preview made %d tasks", n)
	}
	if f, _ = r.e.preview(r.ctx, "PR-12345 only the refund path"); f.Jira == nil || f.Jira.Found != "" {
		t.Fatalf("preview of a key at the start %+v, want no note", f.Jira)
	}

	// The daemon answers the screen with JSON, which carries the note.
	id, err := r.reg.EnqueueRequest(r.ctx, ReqPreview, PreviewPayload{Text: foundText})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	req, _ := r.reg.RequestByID(r.ctx, id)
	var form tui.NewTaskForm
	if err := json.Unmarshal([]byte(req.Result), &form); err != nil || form.Jira == nil || form.Jira.Found != "found in your text" {
		t.Fatalf("preview request %s %q (%v), want the form with the note", req.State, req.Result, err)
	}

	f, _ = r.e.preview(r.ctx, "Read the dates of PR-12399 in the digest export")
	if f.Jira != nil || f.Err != "" || f.Title != "Read the dates of PR-12399 in the digest export" || f.Branch == "" {
		t.Fatalf("preview of words Jira does not know %+v, want a prompt's", f)
	}

	// Several keys Jira knows are the prompt task they are, which asks which
	// issue it is: the preview names the keys, in the text's order, and
	// hands no ticket as the task's (the screen would name a key for it).
	const two = "the refund of PR-12346 and the reminder of PR-12345"
	f, _ = r.e.preview(r.ctx, two)
	if f.Jira != nil || !slices.Equal(f.Keys, []string{"PR-12346", "PR-12345"}) || f.Title != two || f.Err != "" || f.Branch == "" {
		t.Fatalf("preview of two keys %+v (Jira %+v), want the prompt task naming both keys", f, f.Jira)
	}
	beside := newIntakeRig(t)
	beside.site.refuse("PR-12399", 400)
	f, _ = beside.e.preview(beside.ctx, "the refund of PR-12399, PR-12346 and PR-12345")
	if f.Jira != nil || len(f.Keys) != 2 || f.Err != "Jira refused PR-12399: Jira answered HTTP 400; it stays a word of your text" {
		t.Fatalf("preview of two keys beside a refused one %+v, want both keys and the refusal's note", f)
	}

	first := r.mustStart("PR-12345")
	if f, _ = r.e.preview(r.ctx, foundText); f.Err != fmt.Sprintf("PR-12345 already runs as task %d", first.ID) {
		t.Fatalf("preview of a key that runs %+v", f)
	}
	r.site.setDown(7)
	if f, _ = r.e.preview(r.ctx, "fix the refund path of PR-12346"); f.Err != "Jira does not answer: the task waits for it, then starts" {
		t.Fatalf("preview while Jira is down %+v", f)
	}
}
