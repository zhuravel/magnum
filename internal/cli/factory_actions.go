package cli

// The factory screen's actions (tui.FactoryActions): each key that changes
// something is a request to the factory daemon through its requests table,
// as the factory's commands send them, with the screen as its surface. The
// adapter queues the request, wakes the daemon (factoryKick) and waits for
// its answer (waitFactoryRequest); the daemon's handler records your input,
// once, so the adapter records none. Stepping into a pane, the time you saw
// home and the terminal's focus carry no words and no decision: they record
// no input. The screen asks y/N before each action; the adapter confirms
// nothing itself. f, F and x on your own PR's row are not the factory's:
// they run the board's own code (screenActions: magnum's fix, fix off and
// abort requests, in magnum's registry), never the factory daemon's.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenSurface is where the screen's requests come from, for the record of
// your input.
const screenSurface = "screen"

// factoryFocusWait bounds the wait for the daemon's answer to a focus
// report: best effort, it never holds the screen.
const factoryFocusWait = 2 * time.Second

// factoryActions are the factory screen's actions over the screen's source
// (its registries, the ready cards of its last frame). focusPane and
// revealSession are herdr's agent focus on the factory's session and the
// terminal on that session brought to the front; tests replace them.
type factoryActions struct {
	c             *Context
	src           *factoryScreenSource
	pid           int
	focusPane     func(ctx context.Context, target string) error
	revealSession func(ctx context.Context) (string, error)
	// board runs f, F and x on your own PR's row: the PR board's actions.
	board *screenActions
}

var _ tui.FactoryActions = (*factoryActions)(nil)

// newFactoryActions are the actions of the factory screen of c over src:
// herdr is the factory's own session ([factory] herdr_session, its socket),
// and the reveal brings the configured terminal on that session to the
// front; board, the PR board's actions, runs the keys on your own PRs.
func newFactoryActions(c *Context, src *factoryScreenSource, board *screenActions) *factoryActions {
	a := &factoryActions{c: c, src: src, pid: os.Getpid(), board: board}
	a.focusPane = func(ctx context.Context, target string) error {
		return (&herdr.Client{Socket: c.Config.FactoryHerdr().Socket}).AgentFocus(ctx, target)
	}
	a.revealSession = func(ctx context.Context) (string, error) {
		out, err := reveal.Reveal(ctx, daemonSys.runner(false, nil), c.Config.Terminal, os.Getenv("HERDR_BIN_PATH"),
			reveal.Options{Session: c.Config.FactoryHerdr().Name})
		return out.String(), err
	}
	return a
}

// Start starts a task from the new task's text in repo (StartPayload).
func (a *factoryActions) Start(ctx context.Context, text, kind, repo string, wordsOnly bool) (tui.ActionResult, error) {
	return a.send(ctx, factory.ReqStart, factory.StartPayload{Text: text, Kind: kind, Repo: repo, WordsOnly: wordsOnly, Surface: screenSurface})
}

// Preview is the daemon's new-task form of text in repo (PreviewPayload).
func (a *factoryActions) Preview(ctx context.Context, text, repo string) (tui.NewTaskForm, error) {
	var f tui.NewTaskForm
	q, _, err := a.request(ctx, factory.ReqPreview, factory.PreviewPayload{Text: text, Repo: repo}, factoryStartWait)
	if err != nil {
		return f, err
	}
	switch q.State {
	case registry.RequestDone:
		if err := json.Unmarshal([]byte(q.Result), &f); err != nil {
			return f, fmt.Errorf("the factory daemon's preview: %w", err)
		}
		return f, nil
	case registry.RequestFailed:
		return f, errors.New(q.Result)
	}
	return f, errors.New("the factory daemon has not read the text yet")
}

// Answer sends a batch of answers to the questions of revision rev,
// indexes from 0 as the screen sends them (AnswerPayload). A batch refused
// because the questions changed comes back as tui.QuestionsChangedError.
func (a *factoryActions) Answer(ctx context.Context, taskID int64, rev int, answers, needless map[int]string) (tui.ActionResult, error) {
	q, running, err := a.request(ctx, factory.ReqAnswer, factory.AnswerPayload{TaskID: taskID, Revision: rev, Answers: answers,
		Needless: needless, Surface: screenSurface}, factoryStartWait)
	if err != nil {
		return tui.ActionResult{}, err
	}
	if q.State == registry.RequestFailed {
		return tui.ActionResult{}, a.answerRefused(ctx, taskID, rev, q.Result)
	}
	return outcome(q, running)
}

// answerRefused is the error of a batch for revision rev of task id that
// the daemon refused with result: the questions changed when the task's
// newest revision is another one (when it was asked, and why in the
// registry's words: the agent that asked the new questions), else the
// daemon's words. Nothing parses the daemon's prose.
func (a *factoryActions) answerRefused(ctx context.Context, id int64, rev int, result string) error {
	reg, err := a.src.registryFor(false)
	if err != nil || reg == nil {
		return errors.New(result)
	}
	t, err := reg.TaskByID(ctx, id)
	if err != nil || t.QuestionRev == rev {
		return errors.New(result)
	}
	qs, err := reg.Questions(ctx, id)
	if err != nil {
		return errors.New(result)
	}
	changed := &tui.QuestionsChangedError{At: t.Updated, Why: "Newer evidence changed the questions."}
	before := map[string]bool{}
	for _, q := range qs {
		if q.Revision < t.QuestionRev {
			before[factory.QuestionKey(q.Text)] = true
		}
	}
	asker, n := "", 0
	for _, q := range qs {
		if q.Revision != t.QuestionRev {
			continue
		}
		if !q.Asked.IsZero() {
			changed.At = q.Asked
		}
		if !before[factory.QuestionKey(q.Text)] {
			n++
			asker = cmp.Or(asker, q.RaisedBy)
		}
	}
	if n > 0 {
		changed.Why = fmt.Sprintf("The agent %s asked %s.", a.doing(asker), plural(n, "new question"))
	}
	return changed
}

// doing is what role name's agent does ("finding the cause"), or its name;
// of a question two roles raised ("fx-judge, fx-points"), the first's.
func (a *factoryActions) doing(name string) string {
	name, _, _ = strings.Cut(name, ", ")
	i := slices.IndexFunc(a.c.Config.Factory.Roles, func(r config.Role) bool { return r.Name == name })
	if i < 0 {
		return name
	}
	return cmp.Or(a.c.Config.Factory.Roles[i].Doing, name)
}

// Decide decides on the ready card, bound to what it showed (DecidePayload).
func (a *factoryActions) Decide(ctx context.Context, taskID int64, choice, reason, note string, b tui.ReadyBinding) (tui.ActionResult, error) {
	return a.send(ctx, factory.ReqDecide, factory.DecidePayload{TaskID: taskID, Choice: choice, Reason: reason, Note: note,
		HeadSHA: b.HeadSHA, ReportHash: b.ReportHash, DiffHash: b.DiffHash, Shown: b.Shown, DiffOpened: b.DiffOpened,
		DiffDwell: b.DiffDwell, Surface: screenSurface})
}

// Stop drops a task: its agents stop, its copy is freed, its branch kept.
func (a *factoryActions) Stop(ctx context.Context, taskID int64) (tui.ActionResult, error) {
	return a.send(ctx, factory.ReqStop, factory.StopPayload{TaskID: taskID, Surface: screenSurface})
}

// Retry lets a task that waits on you go on at its stage.
func (a *factoryActions) Retry(ctx context.Context, taskID int64) (tui.ActionResult, error) {
	return a.send(ctx, factory.ReqRetry, factory.RetryPayload{TaskID: taskID, Surface: screenSurface})
}

// Open steps into the task's pane: herdr focuses its agent on the
// factory's session (the one that waits on you, else the one at work, else
// the newest live one), and the terminal on that session comes to the
// front.
func (a *factoryActions) Open(ctx context.Context, taskID int64) (tui.ActionResult, error) {
	st, err := a.src.agentsStore()
	switch {
	case err != nil:
		return tui.ActionResult{}, fmt.Errorf("the factory's agents cannot be read: %w", err)
	case st == nil:
		return tui.ActionResult{}, errors.New("the task has no pane open: no agent of the factory ran yet")
	}
	ss, err := runtime.TaskSessions(ctx, st, taskID)
	if err != nil {
		return tui.ActionResult{}, err
	}
	s, ok := paneToOpen(ss)
	target := cmp.Or(store.Deref(s.AgentName), store.Deref(s.HerdrPaneID))
	if !ok || target == "" {
		return tui.ActionResult{}, errors.New("the task has no pane open")
	}
	doing := a.doing(s.Role)
	if err := a.focusPane(ctx, target); err != nil {
		return tui.ActionResult{}, fmt.Errorf("focus the pane of the agent %s in the %s herdr session: %w", doing,
			a.c.Config.FactoryHerdr().Name, err)
	}
	text := "you're in the pane of the agent " + doing
	out, err := a.revealSession(ctx)
	if err != nil {
		return tui.ActionResult{Text: text}, fmt.Errorf("focused the pane of the agent %s, but bringing the terminal to the front failed: %w "+
			"(fix [terminal] in config.toml)", doing, err)
	}
	if out != "" {
		text += "; " + out
	}
	return tui.ActionResult{Text: text}, nil
}

// OpenBrowser opens url in the browser, the screen's b (the task's PR) and
// t (its Jira issue), with the board's opener on the factory commands'
// runner: no registry of the board's is opened for it, and nothing is
// recorded (it carries no words and no decision).
func (a *factoryActions) OpenBrowser(ctx context.Context, url string) error {
	return actOpenURL(ctx, daemonSys.runner(false, nil), url)
}

// paneToOpen is the session of a task Open steps into: of its live ones,
// the one that waits on you, else the one at work, else the newest.
func paneToOpen(ss []store.Session) (store.Session, bool) {
	var best store.Session
	rank := -1
	for _, s := range ss {
		if s.State != store.SessionLive && s.State != store.SessionStarting {
			continue
		}
		r := 0
		switch herdr.Status(store.Deref(s.AgentStatus)) {
		case herdr.StatusBlocked:
			r = 2
		case herdr.StatusWorking:
			r = 1
		}
		if r >= rank {
			best, rank = s, r
		}
	}
	return best, rank >= 0
}

// Fix has magnum's fixer take what is open on your PR ref, as the board's f
// does (magnum fix: magnum's fix request); the screen asked first.
func (a *factoryActions) Fix(ctx context.Context, ref string) (tui.ActionResult, error) {
	if a.board == nil {
		return tui.ActionResult{}, errNoBoard
	}
	return a.board.Fix(ctx, ref)
}

// FixOff switches your PR ref out of autofix (off) or back in, as the
// board's F does; the screen asked first.
func (a *factoryActions) FixOff(ctx context.Context, ref string, off bool) (tui.ActionResult, error) {
	if a.board == nil {
		return tui.ActionResult{}, errNoBoard
	}
	return a.board.FixOff(ctx, ref, off)
}

// Abort stops what magnum runs on your PR ref (its fix loop, the re-review
// the loop forced), as the board's K does (magnum abort); the screen asked
// first.
func (a *factoryActions) Abort(ctx context.Context, ref string) (tui.ActionResult, error) {
	if a.board == nil {
		return tui.ActionResult{}, errNoBoard
	}
	return a.board.Abort(ctx, ref)
}

// errNoBoard is the refusal of a key on your PR's row by a screen built
// without the board's actions.
var errNoBoard = errors.New("this screen cannot act on magnum's PRs")

// Seen keeps that you saw home at at (factory.SaveSeen): it survives a
// restart of the screen and of the daemon, and only moves forward.
func (a *factoryActions) Seen(_ context.Context, at time.Time) error {
	return factory.SaveSeen(a.c.Layout, at)
}

// Focus reports that the screen's terminal gained or lost focus at at, the
// event's time: a focus request with the screen's pid (the daemon holds its
// toasts while that screen runs focused, and keeps the newer report by
// at). A blur also keeps at as the time you last saw home. It is best
// effort: it waits factoryFocusWait at most, and with no factory registry
// yet it sends and keeps nothing.
func (a *factoryActions) Focus(ctx context.Context, focused bool, at time.Time) error {
	reg, err := a.src.registryFor(false)
	if err != nil || reg == nil {
		return err
	}
	if !focused {
		err = factory.SaveSeen(a.c.Layout, at)
	}
	_, _, ferr := a.request(ctx, factory.ReqFocus, factory.FocusPayload{Focused: focused, PID: a.pid, At: at}, factoryFocusWait)
	return errors.Join(err, ferr)
}

// Diff is the pager on the diff of the task's ready card in its copy, the
// card's files in its order when its order file is there (factory.DiffArgs;
// one stat, no read), as the screen's last frame drew the card; nil for a
// task with no ready card. It builds the command only.
func (a *factoryActions) Diff(taskID int64) *exec.Cmd {
	card, ok := a.src.readyCard(taskID)
	if !ok || card.CopyPath == "" || card.Head == "" || card.Base == "" {
		return nil
	}
	order := factory.DiffOrderPath(a.c.Layout, taskID)
	if _, err := os.Stat(order); err != nil {
		order = "" // a card drawn before its order file: git's order
	}
	// The factory's own copy and revisions from its card; the screen runs it
	// with tea.ExecProcess, which owns its life.
	cmd := exec.Command("git", factory.DiffArgs(card, order)...) //nolint:gosec,noctx // see above
	cmd.Env = append(os.Environ(), "LESS="+pagerLess(os.Getenv("LESS")))
	if p := strings.TrimSpace(a.c.Config.Factory.DiffPager); p != "" {
		cmd.Env = append(cmd.Env, "GIT_PAGER="+p) // [factory] diff_pager: your viewer, over git's diff in the card's order
	}
	return cmd
}

// pagerLess is LESS for the diff's pager: yours (git's own RX without one)
// with -+F, so less never quits at once on a diff that fits the screen,
// which would end the pager before you read it and count the diff as read.
func pagerLess(yours string) string {
	return cmp.Or(strings.TrimSpace(yours), "RX") + " -+F"
}

// send queues a request and answers with its outcome.
func (a *factoryActions) send(ctx context.Context, kind string, payload any) (tui.ActionResult, error) {
	q, running, err := a.request(ctx, kind, payload, factoryStartWait)
	if err != nil {
		return tui.ActionResult{}, err
	}
	return outcome(q, running)
}

// outcome is what the screen says of request q as last read: the daemon's
// answer, its refusal as an error, or that it waits for the daemon (with
// none running, for one to start).
func outcome(q registry.Request, running bool) (tui.ActionResult, error) {
	switch q.State {
	case registry.RequestDone:
		return tui.ActionResult{Text: q.Result}, nil
	case registry.RequestFailed:
		return tui.ActionResult{}, errors.New(q.Result)
	}
	if !running {
		return tui.ActionResult{Text: fmt.Sprintf("queued as request %d, but no factory daemon runs: `magnum factory install` loads it, "+
			"and it acts on the request if it runs within 30 minutes", q.ID)}, nil
	}
	return tui.ActionResult{Text: fmt.Sprintf("the factory daemon has not answered request %d yet: the screen shows its effect when it does", q.ID)}, nil
}

// request queues a request of kind with payload, wakes the factory daemon
// and waits up to wait for its answer: the request as last read, and
// whether a factory daemon runs. With none it does not wait: the request
// stays pending for one to start.
func (a *factoryActions) request(ctx context.Context, kind string, payload any, wait time.Duration) (registry.Request, bool, error) {
	reg, err := a.src.registryFor(true)
	if err != nil {
		return registry.Request{}, false, err
	}
	id, err := reg.EnqueueRequest(ctx, kind, payload)
	if err != nil {
		return registry.Request{}, false, err
	}
	pid, kerr := daemonSys.factoryKick(a.c.Layout)
	if pid == 0 {
		q := registry.Request{ID: id, Kind: kind, State: registry.RequestPending}
		if kerr != nil {
			return q, false, fmt.Errorf("queued as request %d, but the factory daemon could not be woken: %w", id, kerr)
		}
		return q, false, nil
	}
	q, err := waitFactoryRequestFor(ctx, reg, id, wait)
	return q, true, err
}
