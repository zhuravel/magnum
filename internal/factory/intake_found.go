package factory

// A start whose text names Jira keys elsewhere than at its start (the
// operator's request of 2026-11-04, spec 6.1): the keys of a free prompt are
// read from Jira, and the keys Jira knows decide what the task is (startFound):
// one key makes a key task with the whole text as its guidance, several ask
// which issue the task is, and none leaves a prompt task. A task whose reads
// Jira does not answer waits keyless (startFoundWaiting, retryFound).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/jira"
)

// foundBudget is the time all the reads of the keys of one start, or of one
// preview, share (a test shortens it).
var foundBudget = jira.MaxTime

// foundReads is the most keys of a text that a start reads from Jira (the
// first of in.Found), and foundNote the note the preview gives a key found
// in the text.
const (
	foundReads = 3
	foundNote  = "found in your text"
)

// noteFound records, as an info event of task id, that its key came from
// the middle of the text.
func (e *Engine) noteFound(ctx context.Context, id int64, key string) {
	e.event(ctx, id, "intake", registry.PhaseInfo, "key "+key+" "+foundNote)
}

// The tails of the note on a found key that Jira refused for a cause of its
// own: what then becomes of the key.
const (
	refusedAlone  = "starts from your words"       // no key of the text is known
	refusedBeside = "it stays a word of your text" // another key of the text is
)

// foundRefusal is a found key that Jira refused for a cause of its own (a
// missing token file, a 401, any refusal but "no such issue"), cause in Jira's
// words: never an error of a start, since a broken Jira setup must not block
// a prompt that holds a word of the shape of a key.
type foundRefusal struct{ key, cause string }

// note is the refusal in the words of the preview and of the task's events,
// redacted as every event is (the cause names no token and no answer).
func (r foundRefusal) note(tail string) string {
	return execx.Redact("Jira refused " + r.key + ": " + r.cause + "; " + tail)
}

// jiraRefusal is a refusal of Jira's in the words of a cause, without the
// key.
func jiraRefusal(err error) string {
	if je, ok := errors.AsType[*jira.Error](err); ok && je.Class == jira.ClassTokenFile && errors.Is(err, fs.ErrNotExist) {
		return "the Jira token file is missing: " + je.File
	}
	return err.Error()
}

// errFoundTime is the end of the time the reads of one start's keys share.
var errFoundTime = errors.New("the reads of the keys of the text took longer than Jira may take for one")

// foundSilent reports whether Jira's refusal err is silence for a found key,
// which the task waits out: jiraDown's (curl exit 6, 7 or 28, a 5xx), a 429
// (Jira asks to wait), and any other failure of curl to reach Jira. Such a
// key must not be dropped, since the operator named it.
func foundSilent(err error) bool {
	if jiraDown(err) {
		return true
	}
	je, ok := errors.AsType[*jira.Error](err)
	return ok && (je.Class == jira.ClassUnreachable || (je.Class == jira.ClassStatus && je.Code == http.StatusTooManyRequests))
}

// noSuchIssue reports whether Jira's refusal err is "no such issue, or the
// account cannot see it" (HTTP 404): the one refusal that makes a found key a
// plain word, with nothing said.
func noSuchIssue(err error) bool {
	je, ok := errors.AsType[*jira.Error](err)
	return ok && je.Class == jira.ClassNotFound
}

// readFound reads the keys of a prompt's words from Jira (at most
// foundReads, in order, within foundBudget together) and gives the tickets of
// those Jira knows, and the keys it refused for a cause of its own
// (foundRefusal: a 401, a 403, a missing token file, anything but silence and
// "no such issue"). A key with no such issue (noSuchIssue) is a word of the
// prompt, no error. The error is Jira's silence (foundSilent), or the end of
// the shared time (errFoundTime), or the end of the context; it stops the
// reading.
func (e *Engine) readFound(ctx context.Context, keys []string, read jiraReader) (known []jira.Ticket, refused []foundRefusal, err error) {
	bctx, cancel := context.WithTimeout(ctx, foundBudget)
	defer cancel()
	for _, k := range keys[:min(len(keys), foundReads)] {
		tk, err := read(bctx, k)
		switch {
		case err == nil:
			known = append(known, tk)
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		case bctx.Err() != nil, errors.Is(err, errFoundTime): // the time ran out now, or for a preview's kept read
			return nil, nil, errFoundTime
		case foundSilent(err):
			return nil, nil, err
		case !noSuchIssue(err):
			refused = append(refused, foundRefusal{k, jiraRefusal(err)})
		}
	}
	return known, refused, nil
}

// noteRefused records the refusals of readFound as info events of task id,
// each naming its cause; known is how many keys of the text Jira knows. It
// returns the first one's note, for the answer to a start ("" when none).
func (e *Engine) noteRefused(ctx context.Context, id int64, refused []foundRefusal, known int) string {
	tail := refusedAlone
	if known > 0 {
		tail = refusedBeside
	}
	var first string
	for _, r := range refused {
		note := r.note(tail)
		e.event(ctx, id, "intake", registry.PhaseInfo, note)
		first = cmp.Or(first, note)
	}
	return first
}

// startFound starts a task of a prompt that names Jira keys elsewhere than
// at its start (in.Found), once Jira says which of them it knows:
//   - one: a key task of that key with the whole text as its guidance;
//   - several: a prompt task whose first question asks which of them it is;
//   - none: a prompt task as any other, the words Jira does not know staying
//     words of the prompt;
//   - Jira does not answer: the task waits for Jira (startFoundWaiting) and
//     goes on by these rules when it answers.
//
// A key Jira refuses for a cause of its own (a missing token file, a 401)
// counts as unknown, never an error: the task goes on from the words, and an
// info event names the cause (noteRefused). Only a key at the start of the
// text is an error.
func (e *Engine) startFound(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput) (registry.Task, string, error) {
	known, refused, err := e.readFound(ctx, in.Found, e.d.Jira.Ticket)
	switch {
	case err != nil && ctx.Err() != nil:
		return registry.Task{}, "", ctx.Err()
	case err != nil:
		t, err := e.startFoundWaiting(ctx, p, repo, login, in, err)
		return t, "", err
	}
	var t registry.Task
	if len(known) == 1 {
		t, err = e.startTicketed(ctx, p, repo, login, StartInput{Key: known[0].Key, Guidance: in.Prompt}, known[0], true)
	} else {
		t, err = e.startPrompt(ctx, p, repo, login, in, known)
	}
	if err != nil {
		return t, "", err
	}
	return t, e.noteRefused(ctx, t.ID, refused, len(known)), nil
}

// startFoundWaiting makes the task of a prompt whose found keys Jira does not
// answer for: a prompt task waiting for Jira (retryJira tries it) that holds
// no key, since none is known yet, with a kind from its words, a branch
// named from them and prompt.md, so a task you take over and let go on has
// your request; retryFound gives it its key, or goes on from the words.
func (e *Engine) startFoundWaiting(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput,
	cause error) (registry.Task, error) {
	kind := p.Kind
	if kind == "" {
		kind, _ = PickKind(e.cfg.Factory, nil, in.Prompt)
	}
	branch, err := e.branchFor(ctx, 0, repo.Repo, login, "", in.Prompt)
	if err != nil {
		return registry.Task{}, err
	}
	t, err := e.reg.CreateTask(ctx, registry.Task{Prompt: in.Prompt, Kind: kind, Repo: repo.Repo, Title: promptTitle(in.Prompt),
		State: registry.TaskWaiting, Branch: branch, WaitsWho: waitJira, WaitsWhat: jiraWaitWhat})
	if err != nil {
		return registry.Task{}, err
	}
	next := e.now().Add(jiraBackoff(0))
	e.jiraTries[t.ID] = &jiraTry{next: next, picked: p.Kind == ""}
	e.recordStart(ctx, t, p, in)
	if err := e.writeData(ctx, t, "ticket", "prompt.md", []byte(in.Prompt+"\n")); err != nil {
		return t, err
	}
	e.event(ctx, t.ID, "intake", registry.PhaseFail, fmt.Sprintf("%s found in your text: waits for Jira (%v) and tries again at %s",
		strings.Join(in.Found[:min(len(in.Found), foundReads)], ", "), cause, next.Local().Format("15:04")))
	return t, nil
}

// retryFound tries Jira again for a prompt task that waits for it, w being
// its try (retryJira): the keys of its words are read again, as a start
// reads them (readFound), and Jira's answer decides as a start's does:
// one known key links to the task and starts it as a key task with the whole
// text as its guidance (linkFound), none or several send it on from its words
// (goOnFromWords). One more outage moves the next try; 6 hours after the
// start, if Jira still does not answer, the task goes on from its words (a key
// task needs you then, but a text's keys were only a possibility). A key Jira
// refuses for a cause of its own stays a word, as at a start, with an info
// event naming the cause.
func (e *Engine) retryFound(ctx context.Context, t registry.Task, w *jiraTry) error {
	known, refused, err := e.readFound(ctx, ParseStart(t.Prompt, e.trackers(t.Repo)).Found, e.d.Jira.Ticket)
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil && !e.now().Before(t.Created.Add(jiraGiveUp)):
		delete(e.jiraTries, t.ID)
		e.event(ctx, t.ID, "intake", registry.PhaseInfo, "Jira did not answer for "+shortAge(jiraGiveUp)+"; "+goesOnNote)
		return e.goOnFromWords(ctx, t, nil, "Jira did not answer")
	case err != nil:
		e.jiraDidNotAnswer(ctx, t, w, t.Created, err)
		return nil
	}
	delete(e.jiraTries, t.ID)
	e.noteRefused(ctx, t.ID, refused, len(known))
	if len(known) == 1 {
		return e.linkFound(ctx, t, known[0], w.picked)
	}
	return e.goOnFromWords(ctx, t, known, "Jira answered")
}

// linkFound gives the prompt task t, which waited for Jira, the one key of
// its words that Jira knows (ticket tk): its whole text becomes its
// guidance (guidance.md and the task's guidance), the key is linked, and the
// task starts as a key task that waited for Jira (startTicket). A key that a
// task holds since sends the task to you, naming that task: the key has one
// task.
func (e *Engine) linkFound(ctx context.Context, t registry.Task, tk jira.Ticket, picked bool) error {
	if h, err := e.reg.TaskByKey(ctx, tk.Key); err == nil {
		e.toOperator(ctx, t, keyRuns{h}.Error()+": stop this task", "its key has a task")
		return nil
	} else if !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskWaiting}, "", func(u *registry.TaskUpdate) {
		u.Set("guidance", t.Prompt)
	}); err != nil {
		return err
	}
	t.Guidance = t.Prompt
	if wrote := e.writeData(ctx, t, "guidance", "guidance.md", []byte(t.Guidance+"\n")) == nil; !wrote {
		return nil // writeData sent the task to you with the cause
	}
	if err := e.reg.LinkKey(ctx, t.ID, tk.Key); err != nil {
		if _, held := errors.AsType[registry.ErrActiveKey](err); held { // another start took it meanwhile
			return nil // the next try finds the holder
		}
		return err
	}
	t.JiraKey = tk.Key
	e.noteFound(ctx, t.ID, tk.Key)
	return e.startTicket(ctx, t, tk, picked)
}

// goesOnNote ends the info event of a text that goes on from its words
// because no key of it could be read.
const goesOnNote = "it goes on from your words"

// goOnFromWords starts the prompt task t, which waited for Jira, from its
// words (its prompt.md is there since its start): queued or active as a
// start's gate says, with its question which Jira issue it is: the keys of
// its words that Jira knows when there are several, else the title search of
// any prompt. because is why the wait ended: "Jira answered", "Jira did not
// answer" (after 6 hours) or "[jira] is off".
func (e *Engine) goOnFromWords(ctx context.Context, t registry.Task, known []jira.Ticket, because string) error {
	state, who, what, err := e.startState(ctx)
	if err != nil {
		return err
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskWaiting}, state, func(u *registry.TaskUpdate) {
		u.Set("waits_who", who)
		u.Set("waits_what", what)
		u.Because(because)
	}); err != nil {
		return err
	}
	t.State = state
	e.event(ctx, t.ID, "intake", registry.PhaseOK, fmt.Sprintf("%s: %s from your words: %s, on %s", because, startVerb(t), t.Kind, t.Branch))
	if len(known) > 1 {
		e.storeFoundIssues(ctx, t, known)
	} else {
		e.askWhichIssue(ctx, t, t.Prompt)
	}
	return nil
}
