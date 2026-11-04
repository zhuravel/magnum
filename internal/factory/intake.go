package factory

// Intake (spec section 6.1): `magnum factory start` and the new-task screen
// hand the daemon a Jira key or a Jira link with your guidance after it, a
// GitHub PR or Actions run link (later milestones), or a free prompt. The
// daemon reads the ticket itself (the token never enters a pane) and writes
// it to the task's directory as a data file (ticket.md, beside guidance.md
// with your words, or prompt.md with the prompt), picks the kind of task,
// names its branch, and starts the task at once, or queues it while memory
// is full or max_active tasks are at work. One key has one task: a second
// start returns the first. A key Jira cannot read now (it does not answer)
// makes a task that waits for Jira; an unknown key or a missing token file
// makes none.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
)

// StartInput is what a start's text holds (ParseStart): a Jira key with
// your guidance after it, a GitHub PR link (PR) or an Actions run link
// (Run), with the words after the link as Guidance, or else the whole text
// as a prompt.
type StartInput struct {
	Key, Guidance, Prompt string
	PR, Run               *Link
}

// Link is a GitHub link of a start: its URL as given, the repository
// (owner/name) and the PR's or the run's number.
type Link struct {
	URL, Repo string
	Number    int64
}

// StartPayload is a ReqStart (and the start of the new-task screen): your
// text, the kind of task you chose ("" = the daemon picks it, PickKind),
// the repository ("" = the only [[factory.repo]]), and where you asked
// (Surface, for the record of your input; "" = the CLI).
type StartPayload struct {
	Text, Kind, Repo string
	Surface          string `json:",omitempty"`
}

// PreviewPayload is a ReqPreview: the new-task screen's text, whose preview
// the daemon answers as the JSON of a tui.NewTaskForm.
type PreviewPayload struct {
	Text string
}

var (
	// ErrKeyRuns is a start for a Jira key that a task holds (it is not
	// done or stopped): the start returns that task and makes none. The
	// error's text names the task, or its PR.
	ErrKeyRuns = errors.New("factory: the key has a task")
	// ErrLater is a start of a later milestone: a PR link (adoption,
	// milestone 2) or a run link (CI tasks, milestone 3).
	ErrLater = errors.New("factory: not in this milestone")
)

// keyRuns is ErrKeyRuns naming the task that holds the key.
type keyRuns struct{ t registry.Task }

func (k keyRuns) Error() string {
	if k.t.PRNumber > 0 {
		return fmt.Sprintf("%s has PR #%d", k.t.JiraKey, k.t.PRNumber)
	}
	return fmt.Sprintf("%s already runs as task %d", k.t.JiraKey, k.t.ID)
}

func (k keyRuns) Is(target error) bool { return target == ErrKeyRuns }

// laterError is ErrLater saying what comes when.
type laterError struct{ what string }

func (l laterError) Error() string        { return l.what }
func (l laterError) Is(target error) bool { return target == ErrLater }

// The waits and the words of intake.
const (
	// waitJira is a task's wait for Jira, jiraWaitWhat what it waits for.
	waitJira     = "Jira"
	jiraWaitWhat = "start when Jira answers"
	// waitMemory is a queued task's wait for memory.
	waitMemory = "memory"
	// startSurface is where a start without one came from.
	startSurface = "cli"
	// intakeRole raises the question of which Jira issue a prompt is, and
	// marks the input that starts a task.
	intakeRole = registry.RoleIntake
	noIssue    = "no issue"
	// whichIssue is that question's text.
	whichIssue = "Which Jira issue is this?"
)

// The tries of a key while Jira does not answer: after 1, 2, 4, 8 and 16
// minutes, then every 30, and after 6 hours the task needs you.
const (
	jiraFirstTry = time.Minute
	jiraTryMax   = 30 * time.Minute
	jiraGiveUp   = 6 * time.Hour
	// jiraTryShifts caps the doubling: 1m<<5 is past jiraTryMax.
	jiraTryShifts = 5
)

// jiraBackoff is the wait after failed tries of Jira (the start's own
// read is try 0): 1, 2, 4, 8, 16, then 30 minutes.
func jiraBackoff(tries int) time.Duration {
	return min(jiraFirstTry<<min(max(tries, 0), jiraTryShifts), jiraTryMax)
}

// jiraTry is when a task that waits for Jira tries it next, how many tries
// failed since the daemon started, and whether its kind is intake's own
// pick (from your words; Jira's answer refines it) rather than yours; the
// loop's own. After a restart a task tries at once, then backs off from 1
// minute again, and keeps the kind it has; the 6 hours count from the task's
// creation whatever the restarts.
type jiraTry struct {
	tries  int
	next   time.Time
	picked bool
}

// The bounds of the work of a start off the loop: the overlap line's three
// reads, and a prompt's search of Jira.
const (
	overlapBound = 2 * time.Minute
	searchBound  = jira.MaxTime + 10*time.Second
)

// The bounds of what intake writes and reads.
const (
	// maxSlug is the most characters of a branch's slug (the words after
	// your login and the key).
	maxSlug = 40
	// maxTitle is the most runes of a prompt task's title.
	maxTitle = 72
	// issueSearch is how many issues the search of a prompt reads, and
	// issueOptions how many of the best matches the question offers.
	issueSearch  = 20
	issueOptions = 3
	// maxExcerpt is the most runes of a ticket's description the preview
	// shows.
	maxExcerpt = 200
)

// start makes a task of p (ReqStart, the screen's start). A key a task
// holds returns that task with ErrKeyRuns and makes none; a PR or run link
// is ErrLater; an unknown kind or repository, an unknown key, a missing
// token file or another refusal of Jira's makes no task and says why.
// While Jira does not answer, the task waits for it (retryJira). Each start
// records your text as an input (task_text, or the guidance after a key)
// and writes the task's data files: ticket.md (Ticket.Markdown) and
// guidance.md for a key, prompt.md for a prompt, each an artifact.
func (e *Engine) start(ctx context.Context, p StartPayload) (registry.Task, error) {
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return registry.Task{}, errors.New("nothing to start: give a Jira key, a link or the task in your words")
	}
	repo, err := e.startRepo(p.Repo)
	if err != nil {
		return registry.Task{}, err
	}
	in := ParseStart(text, e.trackers(repo.Repo))
	switch {
	case in.PR != nil:
		return registry.Task{}, laterError{"a PR link: adoption comes in milestone 2"}
	case in.Run != nil:
		return registry.Task{}, laterError{"an Actions run link: CI tasks come in milestone 3"}
	}
	if p.Kind != "" && !hasKind(e.cfg.Factory, p.Kind) {
		return registry.Task{}, fmt.Errorf("no kind of task %q: one of %s", p.Kind, strings.Join(slices.Sorted(maps.Keys(e.cfg.Factory.Kinds)), ", "))
	}
	if in.Key != "" {
		if t, err := e.reg.TaskByKey(ctx, in.Key); err == nil {
			return t, keyRuns{t}
		} else if !errors.Is(err, registry.ErrNotFound) {
			return registry.Task{}, err
		}
	}
	login, err := e.operatorLogin(repo.Repo)
	if err != nil {
		return registry.Task{}, err
	}
	if in.Key != "" {
		return e.startKey(ctx, p, repo, login, in)
	}
	return e.startPrompt(ctx, p, repo, login, in)
}

// startRepo is the repository of a start: the [[factory.repo]] named, or
// the only one.
func (e *Engine) startRepo(name string) (config.FactoryRepo, error) {
	var names []string
	for _, r := range e.cfg.Factory.Repos {
		names = append(names, r.Repo)
	}
	switch {
	case name != "":
		if r, ok := e.cfg.FactoryRepo(name); ok {
			return r, nil
		}
		if len(names) == 0 {
			return config.FactoryRepo{}, fmt.Errorf("no [[factory.repo]] is %s: the config declares none", name)
		}
		return config.FactoryRepo{}, fmt.Errorf("no [[factory.repo]] is %s: one of %s", name, strings.Join(names, ", "))
	case len(names) == 1:
		return e.cfg.Factory.Repos[0], nil
	case len(names) == 0:
		return config.FactoryRepo{}, errors.New("no [[factory.repo]] is configured: the factory has no repository to work in")
	}
	return config.FactoryRepo{}, fmt.Errorf("which repository? name one of %s (--repo)", strings.Join(names, ", "))
}

// trackers are what tells a Jira link of repo (ParseStart): the [jira]
// site and the repository's tracker templates on it.
func (e *Engine) trackers(repo string) string {
	j := e.cfg.Jira
	if !j.On() {
		return ""
	}
	out := []string{strings.TrimRight(j.URL, "/")}
	for _, t := range e.cfg.TrackersFor(repo) {
		if j.Names(t.URL) {
			out = append(out, t.URL)
		}
	}
	return strings.Join(out, " ")
}

// operatorLogin is your GitHub login, which starts a task's branch: the
// operator identity of the repository's watch (config.OperatorLogin).
func (e *Engine) operatorLogin(repo string) (string, error) {
	var w config.Watch
	if p := e.cfg.WatchFor(repo); p != nil {
		w = *p
	}
	if l := e.cfg.OperatorLogin(w); l != "" {
		return l, nil
	}
	return "", errors.New(`no [[identity]] of kind "gh" names your GitHub login: a task's branch starts with it`)
}

// branchFor is the branch of a task of repo: <login>-<KEY>-<slug> for a
// key, <login>-<slug> for a prompt, with the slug of the Jira summary or
// the prompt (a key alone without words). A name another task than self
// has (as its branch or the branch its PR is pushed to), or a branch the
// repository's main clone has (local or origin's: yours, made without the
// factory), gets -2, -3, ...: a claim would check that branch out, and its
// push would meet it.
func (e *Engine) branchFor(ctx context.Context, self int64, repo, login, key, words string) (string, error) {
	parts := []string{login}
	if key != "" {
		parts = append(parts, key)
	}
	if s := slug(words); s != "" {
		parts = append(parts, s)
	} else if key == "" {
		parts = append(parts, "task")
	}
	name := strings.Join(parts, "-")
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, t := range tasks {
		if t.ID != self {
			taken[t.Branch], taken[t.RemoteBranch] = true, true
		}
	}
	for n, b := 2, name; ; n++ {
		if !taken[b] && !e.branchExists(ctx, repo, b) {
			return b, nil
		}
		b = fmt.Sprintf("%s-%d", name, n)
	}
}

// branchExists reports whether the main clone of repo has branch b, its
// own or origin's; false without git, and when git cannot tell (a claim's
// switch reports a clash then).
func (e *Engine) branchExists(ctx context.Context, repo, b string) bool {
	r, ok := e.cfg.FactoryRepo(repo)
	if !ok || e.d.Git == nil || r.MainClone == "" {
		return false
	}
	for _, ref := range []string{"refs/heads/" + b, "refs/remotes/origin/" + b} {
		if _, err := e.d.Git.RevParse(ctx, r.MainClone, ref); err == nil {
			return true
		}
	}
	return false
}

// jiraDown reports whether err is Jira not answering (curl exit 6, 7 or
// 28, HTTP 5xx), which a task waits out, not a refusal.
func jiraDown(err error) bool {
	je, ok := errors.AsType[*jira.Error](err)
	if !ok {
		return false
	}
	switch je.Class {
	case jira.ClassTimeout:
		return true
	case jira.ClassUnreachable:
		return je.Code == 6 || je.Code == 7
	case jira.ClassStatus:
		return je.Code >= 500
	}
	return false
}

// jiraCause is a refusal of Jira's for key in the words of a start.
func jiraCause(key string, err error) error {
	je, ok := errors.AsType[*jira.Error](err)
	switch {
	case ok && je.Class == jira.ClassNotFound:
		return fmt.Errorf("%s is not in Jira", key)
	case ok && je.Class == jira.ClassTokenFile && errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("the Jira token file is missing: %s", je.File)
	}
	return fmt.Errorf("cannot read %s from Jira: %w", key, err)
}

// startKey starts a task of a Jira key no task holds.
func (e *Engine) startKey(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput) (registry.Task, error) {
	if e.d.Jira == nil {
		return registry.Task{}, fmt.Errorf("[jira] is off, so the factory cannot read %s: set [jira] url, user and token_file, or start from your words", in.Key)
	}
	tk, err := e.d.Jira.Ticket(ctx, in.Key)
	switch {
	case err != nil && ctx.Err() != nil:
		return registry.Task{}, ctx.Err()
	case err != nil && jiraDown(err):
		return e.startWaiting(ctx, p, repo, login, in, err)
	case err != nil:
		return registry.Task{}, jiraCause(in.Key, err)
	}
	if tk.Key != in.Key { // the issue moved to another project: its key now
		if t, err := e.reg.TaskByKey(ctx, tk.Key); err == nil {
			return t, keyRuns{t}
		}
	}
	kind, why := p.Kind, "you chose it"
	if kind == "" {
		kind, why = PickKind(e.cfg.Factory, &tk, in.Guidance)
	}
	branch, err := e.branchFor(ctx, 0, repo.Repo, login, tk.Key, tk.Summary)
	if err != nil {
		return registry.Task{}, err
	}
	state, who, what, err := e.startState(ctx)
	if err != nil {
		return registry.Task{}, err
	}
	t, err := e.reg.CreateTask(ctx, registry.Task{JiraKey: tk.Key, Guidance: in.Guidance, Kind: kind, Repo: repo.Repo,
		Title: cmp.Or(tk.Summary, tk.Key), State: state, Branch: branch, WaitsWho: who, WaitsWhat: what})
	if err != nil {
		return e.heldBy(ctx, tk.Key, err)
	}
	e.recordStart(ctx, t, p, in)
	if err := e.writeTicket(ctx, t, tk); err != nil {
		return t, err
	}
	e.event(ctx, t.ID, "intake", registry.PhaseOK, fmt.Sprintf("%s: %s, %s (%s), on %s", startVerb(t), tk.Key, kind, why, branch))
	e.noteOverlap(ctx, t)
	return t, nil
}

// heldBy maps a CreateTask error: another start made a task of the key
// meanwhile (ErrActiveKey), which the start returns.
func (e *Engine) heldBy(ctx context.Context, key string, err error) (registry.Task, error) {
	if _, ok := errors.AsType[registry.ErrActiveKey](err); ok {
		if t, terr := e.reg.TaskByKey(ctx, key); terr == nil {
			return t, keyRuns{t}
		}
	}
	return registry.Task{}, err
}

// startWaiting makes the task of a key Jira does not answer for: waiting
// for Jira (retryJira tries it), with your guidance, a kind from the start
// (yours, else the one your words pick, PickKind without a ticket) and a
// branch named by the key (<login>-<KEY>), so a task you take over and let
// go on has both; the title and ticket.md come when Jira answers, a picked
// kind follows its type, and the summary's words join the branch's name.
func (e *Engine) startWaiting(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput,
	cause error) (registry.Task, error) {
	kind := p.Kind
	if kind == "" {
		kind, _ = PickKind(e.cfg.Factory, nil, in.Guidance)
	}
	branch, err := e.branchFor(ctx, 0, repo.Repo, login, in.Key, "")
	if err != nil {
		return registry.Task{}, err
	}
	t, err := e.reg.CreateTask(ctx, registry.Task{JiraKey: in.Key, Guidance: in.Guidance, Kind: kind, Repo: repo.Repo,
		Title: in.Key, State: registry.TaskWaiting, Branch: branch, WaitsWho: waitJira, WaitsWhat: jiraWaitWhat})
	if err != nil {
		return e.heldBy(ctx, in.Key, err)
	}
	next := e.now().Add(jiraBackoff(0))
	e.jiraTries[t.ID] = &jiraTry{next: next, picked: p.Kind == ""}
	e.recordStart(ctx, t, p, in)
	e.event(ctx, t.ID, "intake", registry.PhaseFail, fmt.Sprintf("%s waits for Jira (%v) and tries again at %s",
		in.Key, cause, next.Local().Format("15:04")))
	return t, nil
}

// jiraTryOf is task t's next try of Jira, a wait that began at since (a
// restart: at once), and whether it is due: its time came, or the time to
// give up did (since + jiraGiveUp), which takes one more try.
func (e *Engine) jiraTryOf(t registry.Task, since time.Time) (*jiraTry, bool) {
	now := e.now()
	w := e.jiraTries[t.ID]
	if w == nil {
		w = &jiraTry{next: now}
		e.jiraTries[t.ID] = w
	}
	return w, !now.Before(w.next) || !now.Before(since.Add(jiraGiveUp))
}

// jiraDidNotAnswer records try w of Jira for task t that Jira did not answer
// (err), in a wait that began at since: the next try comes after the backoff
// (jiraBackoff), never after the time to give up (since + jiraGiveUp); once
// that time came, the task needs you, "Jira did not answer for 6h".
func (e *Engine) jiraDidNotAnswer(ctx context.Context, t registry.Task, w *jiraTry, since time.Time, err error) {
	now, giveUp := e.now(), since.Add(jiraGiveUp)
	w.tries++
	if !now.Before(giveUp) {
		delete(e.jiraTries, t.ID)
		e.toOperator(ctx, t, "Jira did not answer for "+shortAge(jiraGiveUp), "Jira did not answer")
		return
	}
	w.next = now.Add(jiraBackoff(w.tries))
	if w.next.After(giveUp) {
		w.next = giveUp
	}
	e.event(ctx, t.ID, "intake", registry.PhaseInfo, fmt.Sprintf("Jira: try %d failed (%v); tries again at %s",
		w.tries, err, w.next.Local().Format("15:04")))
}

// retryJira tries Jira again for a task that waits for it, when its next
// try is due (jiraBackoff after each failed try). Jira's answer gives the
// task its ticket and starts it (startTicket); one more outage moves the
// next try; 6 hours after the start the task needs you; a refusal (an
// unknown key, a token file gone) sends the task to you with the cause. A
// wait for Jira costs the task nothing.
func (e *Engine) retryJira(ctx context.Context, t registry.Task) error {
	w, due := e.jiraTryOf(t, t.Created)
	if !due {
		return nil
	}
	if e.d.Jira == nil {
		delete(e.jiraTries, t.ID)
		e.toOperator(ctx, t, "[jira] is off, so the factory cannot read "+t.JiraKey, "Jira is off")
		return nil
	}
	tk, err := e.d.Jira.Ticket(ctx, t.JiraKey)
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil && jiraDown(err):
		e.jiraDidNotAnswer(ctx, t, w, t.Created, err)
		return nil
	case err != nil:
		delete(e.jiraTries, t.ID)
		e.toOperator(ctx, t, jiraCause(t.JiraKey, err).Error(), "Jira refused the key")
		return nil
	}
	delete(e.jiraTries, t.ID)
	return e.startTicket(ctx, t, tk, w.picked)
}

// startTicket starts a task that waited for Jira with the ticket Jira gave:
// its title, its kind (picked again with the ticket when intake picked it
// and no stage has started), its branch (named again with the summary's
// words while no stage has started and the branch does not exist yet: a
// claim never loses the branch it made) and ticket.md, then queued or
// active as a start's gate says.
func (e *Engine) startTicket(ctx context.Context, t registry.Task, tk jira.Ticket, picked bool) error {
	login, err := e.operatorLogin(t.Repo)
	if err != nil {
		e.toOperator(ctx, t, err.Error(), "no login for the branch")
		return nil
	}
	kind, why := t.Kind, "kept from the start"
	if kind == "" || (picked && t.Stage == "") {
		kind, why = PickKind(e.cfg.Factory, &tk, t.Guidance)
	}
	branch := t.Branch
	if branch == "" || (t.Stage == "" && !e.branchExists(ctx, t.Repo, branch)) {
		if branch, err = e.branchFor(ctx, t.ID, t.Repo, login, t.JiraKey, tk.Summary); err != nil {
			return err
		}
	}
	if wrote := e.writeTicket(ctx, t, tk) == nil; !wrote {
		return nil // writeData sent the task to you with the cause
	}
	state, who, what, err := e.startState(ctx)
	if err != nil {
		return err
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskWaiting}, state, func(u *registry.TaskUpdate) {
		u.Set("title", cmp.Or(tk.Summary, t.JiraKey))
		u.Set("kind", kind)
		u.Set("branch", branch)
		u.Set("waits_who", who)
		u.Set("waits_what", what)
		u.Because("Jira answered")
	}); err != nil {
		return err
	}
	t.State, t.Kind, t.Branch = state, kind, branch
	e.event(ctx, t.ID, "intake", registry.PhaseOK, fmt.Sprintf("Jira answered: %s %s, %s (%s), on %s", startVerb(t), t.JiraKey, kind, why, branch))
	e.noteOverlap(ctx, t)
	return nil
}

// startVerb says whether a new task started or was queued.
func startVerb(t registry.Task) string {
	if t.State == registry.TaskQueued {
		return "queued"
	}
	return "started"
}

// startPrompt starts a task of a free prompt: prompt.md, and the question
// which Jira issue it is (askWhichIssue).
func (e *Engine) startPrompt(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput) (registry.Task, error) {
	kind, why := p.Kind, "you chose it"
	if kind == "" {
		kind, why = PickKind(e.cfg.Factory, nil, in.Prompt)
	}
	branch, err := e.branchFor(ctx, 0, repo.Repo, login, "", in.Prompt)
	if err != nil {
		return registry.Task{}, err
	}
	state, who, what, err := e.startState(ctx)
	if err != nil {
		return registry.Task{}, err
	}
	t, err := e.reg.CreateTask(ctx, registry.Task{Prompt: in.Prompt, Kind: kind, Repo: repo.Repo, Title: promptTitle(in.Prompt),
		State: state, Branch: branch, WaitsWho: who, WaitsWhat: what})
	if err != nil {
		return registry.Task{}, err
	}
	e.recordStart(ctx, t, p, in)
	if err := e.writeData(ctx, t, "ticket", "prompt.md", []byte(in.Prompt+"\n")); err != nil {
		return t, err
	}
	e.askWhichIssue(ctx, t, in.Prompt)
	e.event(ctx, t.ID, "intake", registry.PhaseOK, fmt.Sprintf("%s from your words: %s (%s), on %s", startVerb(t), kind, why, branch))
	return t, nil
}

// recordStart records your text of a start as an input: the guidance after
// a key, or the start's text (a key alone, a prompt), scrubbed (a long
// paste is dropped, with the reason), and writes guidance.md. The input
// carries the intake role, the mark of the message that starts the task: the
// stats count it as typed and never as a step start.
func (e *Engine) recordStart(ctx context.Context, t registry.Task, p StartPayload, in StartInput) {
	kind, text := registry.InputTaskText, strings.TrimSpace(p.Text)
	if in.Key != "" && in.Guidance != "" {
		kind, text = registry.InputGuidance, in.Guidance
	}
	clean, kept := runtime.Scrub(text)
	input := registry.Input{TaskID: t.ID, Kind: kind, Surface: cmp.Or(p.Surface, startSurface), Role: intakeRole, Text: clean}
	if !kept {
		input.Text, input.DroppedReason = "", "a long paste: more than 40 lines or 4000 bytes"
	}
	if _, err := e.recordInput(ctx, input); err != nil {
		e.log.Warn("record a start's input", "task", t.ID, "err", err)
	}
	if in.Guidance != "" {
		if err := e.writeData(ctx, t, "guidance", "guidance.md", []byte(in.Guidance+"\n")); err != nil {
			e.log.Warn("write guidance.md", "task", t.ID, "err", err)
		}
	}
}

// writeTicket writes ticket.md, the ticket as the agents read it.
func (e *Engine) writeTicket(ctx context.Context, t registry.Task, tk jira.Ticket) error {
	return e.writeData(ctx, t, "ticket", ticketFile, tk.Markdown())
}

// writeData writes a data file of the task's directory (0600, the directory
// 0700) and records it as an artifact of kind. A write that fails sends
// the task to you, queued or at work: its agents would work without it.
func (e *Engine) writeData(ctx context.Context, t registry.Task, kind, name string, data []byte) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = fsx.WriteFileAtomic(filepath.Join(dir, name), data, 0o600)
	}
	if err != nil {
		err = fmt.Errorf("task %d: write %s: %w", t.ID, name, err)
		e.event(ctx, t.ID, "intake", registry.PhaseFail, err.Error())
		what := "the factory could not write " + name
		if terr := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskQueued, registry.TaskActive, registry.TaskWaiting},
			registry.TaskNeedsYou, func(u *registry.TaskUpdate) {
				u.Set("waits_who", waitsYou)
				u.Set("waits_what", what)
				u.Because("a data file failed")
			}); terr != nil {
			e.log.Warn("move a task whose data file failed to NEEDS YOU", "task", t.ID, "err", terr)
		} else {
			e.alert(ctx, t.ID, TaskKey(t)+": you: "+what)
		}
		return err
	}
	sum := sha256.Sum256(data)
	if err := e.reg.RecordArtifact(ctx, registry.Artifact{TaskID: t.ID, Kind: kind, Path: name, SHA256: hex.EncodeToString(sum[:])}); err != nil {
		e.log.Warn("record an artifact", "task", t.ID, "file", name, "err", err)
	}
	return nil
}

// scrubbed is text as the registry keeps your words (runtime.Scrub).
func scrubbed(text string) string {
	clean, _ := runtime.Scrub(text)
	return clean
}

// noteOverlap reads the overlap line of a key's start (Overlap, without
// files: the report names them later) off the loop, after the task exists,
// within overlapBound, and records it as an advisory event: it never delays
// the start's answer. A read that failed is logged and recorded, and stops
// nothing.
func (e *Engine) noteOverlap(_ context.Context, t registry.Task) {
	e.goBackground("the overlap line", func(ctx context.Context) {
		rctx, cancel := context.WithTimeout(ctx, overlapBound)
		defer cancel()
		line, err := e.Overlap(rctx, t, nil)
		if line != "" {
			e.event(ctx, t.ID, "overlap", registry.PhaseInfo, line)
		}
		if err != nil {
			e.log.Info("the overlap line read only part", "task", t.ID, "err", err)
			e.event(ctx, t.ID, "overlap", registry.PhaseFail, "read only part: "+err.Error())
		}
	})
}

// askWhichIssue asks, as the first question of a free prompt's task, which
// Jira issue it is: the issueOptions best title matches of a search on the
// prompt's first words, and "no issue". The best match (the most of the
// prompt's search words, in their own forms, in its title) is recommended;
// with no match, or a search that failed, "no issue" is. The question is
// revision 1 of the task's questions; the gate's first batch keeps it.
// The search runs off the loop, within searchBound: the start answers
// first. Without Jira, nothing is asked.
func (e *Engine) askWhichIssue(_ context.Context, t registry.Task, prompt string) {
	if e.d.Jira == nil {
		return
	}
	e.goBackground("the question which Jira issue a prompt is", func(ctx context.Context) {
		sctx, cancel := context.WithTimeout(ctx, searchBound)
		defer cancel()
		hits, err := e.d.Jira.Search(sctx, prompt, issueSearch)
		e.storeWhichIssue(ctx, t, prompt, hits, err)
	})
}

// storeWhichIssue stores the question which Jira issue task t's prompt is,
// of the search's hits (or its error), as the task's next revision.
func (e *Engine) storeWhichIssue(ctx context.Context, t registry.Task, prompt string, hits []jira.Ticket, err error) {
	words := jira.SearchWords(prompt)
	type match struct {
		tk     jira.Ticket
		shared []string
	}
	var ms []match
	for _, tk := range hits {
		ms = append(ms, match{tk, sharedWords(words, tk.Summary)})
	}
	slices.SortStableFunc(ms, func(a, b match) int { return len(b.shared) - len(a.shared) })
	ms = ms[:min(len(ms), issueOptions)]
	q := registry.Question{
		Text:     whichIssue,
		Context:  "You started this task from your words. A Jira issue gives its agents the ticket and its PR the key.",
		Example:  textx.Clip(scrubbed(textx.FirstLine(prompt)), maxTitle),
		RaisedBy: intakeRole,
		Evidence: "a Jira search of the titles for: " + strings.Join(words, " "),
	}
	for _, m := range ms {
		means := "the task follows " + m.tk.Key
		if m.tk.Type != "" || m.tk.Status != "" {
			means += " (" + strings.Trim(m.tk.Type+", "+m.tk.Status, ", ") + ")"
		}
		q.Options = append(q.Options, registry.Option{Label: strings.TrimSpace(m.tk.Key + " " + m.tk.Summary), Means: means})
	}
	q.Options = append(q.Options, registry.Option{Label: noIssue, Means: "the agents work from your words only"})
	q.Recommended = len(q.Options) - 1
	switch {
	case err != nil:
		q.Why = "the search of Jira failed (" + err.Error() + "); name the issue in your answer if there is one"
	case len(ms) > 0 && len(ms[0].shared) > 0:
		q.Recommended = 0
		q.Why = fmt.Sprintf("its title shares %s with your words", quoteAll(ms[0].shared))
	default:
		q.Why = "no title in Jira shares a word with yours"
	}
	cur, rerr := e.reg.TaskByID(ctx, t.ID)
	if rerr != nil {
		e.log.Warn("ask which Jira issue a prompt is", "task", t.ID, "err", rerr)
		return
	}
	if err := e.reg.ReplaceQuestions(ctx, t.ID, cur.QuestionRev+1, []registry.Question{q}); err != nil {
		e.log.Warn("ask which Jira issue a prompt is", "task", t.ID, "err", err)
	}
}
