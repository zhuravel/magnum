package factory

// The factory screen's data (spec 9): ScreenData reads the factory's
// registry, its agents store and the tasks' directories into the screen's
// tui.FactoryData. Home gets one row per task, grouped by who it waits on
// (needs_you is NEEDS YOU, done today is DONE TODAY, a published task goes
// by its PR in magnum's registry, screen_prs.go, and the rest waits on
// others), and one per open PR of yours no task has (by what it waits on,
// as a task's, or LEFT ALONE: screen_own.go), each blocker naming who first
// (WaitsWho and WaitsWhat, the agents at work from the sessions), the count
// of the decisions owed apart from the tasks, the holds of the machine, what
// frees a queued task, the digest after an absence of more than an hour and
// the alerts; each task gets its cards: the task card (its stages, its
// agents by what they do, its report), and the decision card it waits on,
// its questions or its ready card (the FLAGS line, the local review's
// leftovers, every changed file unread with the flagged ones first, and
// what the push frees). It reads and never writes, and draws nothing: the
// painters are the screen's (internal/tui); the screen sets the cursors,
// the hints and the questions it asks itself.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// waitsYou is the WaitsWho of a task that waits on the operator: the word
// the screen's painter draws in yellow, one constant for both.
const waitsYou = tui.WaitsOnYou

// ScreenOptions are what ScreenData reads besides the registries: the
// config (the roles' words, the kinds' stages, the repositories) and the
// layout (the tasks' directories, the holds the daemon saved).
type ScreenOptions struct {
	Config *config.Config
	Layout paths.Layout
	// ReadyCards, when set, gets each ready card the data draws, as read
	// once for it (the pager pages the card drawn).
	ReadyCards func(id int64, c ReadyCardFile)
	// DaemonDown says no factory daemon runs (its pidfile names no live
	// process): nothing moves, so home's one blocker says so and how to
	// start it, the last daemon's holds.json is not read, and no working
	// mark turns.
	DaemonDown bool
	// Magnum reads the published tasks' PRs in magnum's registry
	// (Magnum.PRStates), once per call: a published task's row then says
	// its PR's verdict; and your own open PRs (Magnum.OwnPRs), once per
	// call, for their rows. nil keeps the words of a screen without it
	// ("magnum: review of #N"), and lists no PR of yours.
	Magnum Magnum
}

// The blocker lines of a factory daemon that is down, short and long.
const (
	daemonDownShort = "factory daemon down: nothing moves"
	daemonDownLong  = "factory daemon down: nothing moves (`magnum factory install` starts it)"
)

const (
	// awayAfter is the absence after which home digests what finished
	// meanwhile.
	awayAfter = time.Hour
	// alertsShown bounds the alerts `!` lists, newest first.
	alertsShown = 200
	// flashFor is how long the answer to a request of yours is the news of
	// the status rule.
	flashFor = 10 * time.Minute
	// ownAnswer is the question card's last option: your own words.
	ownAnswer = "Type your own answer"
)

// holdWords are the blocker lines of home for the holds of the machine, as
// spec 9.C says the memory's ("memory full: new tasks wait").
var holdWords = map[string]string{
	HoldHerdr:  "herdr down: every stage waits",
	HoldInfra:  "infra down: every stage waits",
	HoldDisk:   "disk full: new tasks wait",
	HoldMemory: "memory full: new tasks wait",
}

// flashKinds are the requests whose answer is news for the status rule:
// yours, not the screen's own focus reports and previews.
var flashKinds = []string{ReqStart, ReqAnswer, ReqDecide, ReqStop, ReqRetry, ReqLetGo}

// ScreenData is the factory screen's data at now for an operator last seen
// on home at lastSeen (zero: never): see the file's comment. agents may be
// nil (no agent ran yet): the tasks then have no agents. reg may be nil
// (the factory has no registry yet: no task was ever started): home then
// has no task, news or alert, and lists your PRs alone.
func ScreenData(ctx context.Context, reg *registry.Registry, agents *store.Store, now, lastSeen time.Time, o ScreenOptions) (tui.FactoryData, error) {
	d := tui.FactoryData{Cards: map[int64]tui.FactoryCards{}}
	var tasks []registry.Task
	var err error
	if reg != nil {
		if tasks, err = reg.Tasks(ctx); err != nil {
			return d, err
		}
	}
	s := &screen{ctx: ctx, reg: reg, agents: agents, cfg: cmp.Or(o.Config, &config.Config{}), l: o.Layout, now: now, tasks: tasks}
	for _, t := range tasks {
		v, err := s.read(t)
		switch {
		case ctx.Err() != nil:
			return d, ctx.Err()
		case err != nil: // that task's row says so; the rest of home is drawn
			row, cards := unreadable(t, err)
			cards.Task.PRURL, cards.Task.Issue, cards.Task.IssueURL = prURL(t, registry.PRLink{}), t.JiraKey, s.issueURL(t)
			d.Rows = append(d.Rows, row)
			d.Cards[t.ID] = cards
			d.Decisions++
		case v != nil:
			s.views = append(s.views, v)
		}
	}
	if err := s.readPRs(o.Magnum, lastSeen); err != nil {
		return d, err
	}
	for _, v := range s.views {
		row, cards, decisions := s.task(v)
		d.Rows = append(d.Rows, row)
		d.Cards[v.t.ID] = cards
		d.Decisions += decisions
		if v.card != nil && o.ReadyCards != nil {
			o.ReadyCards(v.t.ID, *v.card)
		}
	}
	own, ownCards, ownErr := s.ownRows(o.Magnum)
	if ctx.Err() != nil {
		return d, ctx.Err()
	}
	d.Rows = append(d.Rows, own...)
	maps.Copy(d.Cards, ownCards)
	slices.SortStableFunc(d.Rows, func(a, b tui.FactoryRow) int {
		if c := cmp.Compare(a.Group, b.Group); c != 0 || a.Group != tui.GroupNeedsYou && a.Group != tui.GroupYours {
			return c
		}
		return a.Since.Compare(b.Since) // NEEDS YOU, as `a` walks it, and your PRs magnum leaves alone: the oldest wait first
	})
	d.Repos = s.repos()
	switch {
	case o.DaemonDown: // nothing moves: no hold of the last daemon's is in force, and no agent works for it
		d.Blocking, d.BlockingLong = []string{daemonDownShort}, []string{daemonDownLong}
		for i := range d.Rows {
			if d.Rows[i].Mark == tui.MarkWorking && d.Rows[i].PR == nil { // magnum's fixer on a PR is magnum's daemon's
				d.Rows[i].Mark = tui.MarkWaits
			}
		}
	default:
		if d.Blocking, d.BlockingLong, err = blockingLines(o.Layout); err != nil {
			return d, err
		}
	}
	if short, long := s.oldBuildLines(); short != "" {
		d.Blocking = append(slices.Clone(d.Blocking), short)
		d.BlockingLong = append(slices.Clone(d.BlockingLong), long)
	}
	if ownErr != nil {
		d.Blocking = append(slices.Clone(d.Blocking), ownUnread)
		d.BlockingLong = append(slices.Clone(d.BlockingLong), ownUnreadLong(ownErr))
	}
	if d.Digest, err = s.digest(lastSeen, d.Rows); err != nil {
		return d, err
	}
	d.Next = s.next(d.Rows)
	if d.Flash, err = s.flash(); err != nil {
		return d, err
	}
	if d.Alerts, err = s.alerts(); err != nil {
		return d, err
	}
	return d, nil
}

// screen is one read of ScreenData.
type screen struct {
	ctx    context.Context
	reg    *registry.Registry
	agents *store.Store
	cfg    *config.Config
	l      paths.Layout
	now    time.Time
	tasks  []registry.Task
	views  []*taskView
	// merged is when the PRs of the done tasks it read merged (readPRs).
	merged map[int64]time.Time
	// oldBuild is the build magnum's daemon runs when its binary on disk is
	// newer, as a PR's read said it (noteBuild); nil for none.
	oldBuild *engine.Build
}

// taskView is what the screen read of a task on home.
type taskView struct {
	t     registry.Task
	group tui.FactoryGroup
	// since is when the task began to wait as it does: its last span, or
	// for a published task the later of it and its PR's wait (readPRs).
	since  time.Time
	link   registry.PRLink // its PR, Number 0 before it has one
	spans  []registry.Span
	tried  map[string]bool // the stages it had an attempt of
	done   map[string]bool // the roles verified in its current stage's newest attempt
	agents []store.Session // the newest session of each role, newest first
	letGo  map[string]string
	all    []registry.Question // the questions of its newest revision
	open   []registry.Question // the open ones
	asks   map[string]ask
	onYou  int            // open questions that wait on you
	card   *ReadyCardFile // its ready card, while it waits on you there
	// reporter is its ticket's reporter, whom its questions offer to ask
	// ("" for none, and for a task of your words).
	reporter string
	retry    bool
	// pr is what a published task waits on by its PR in magnum's registry
	// (readPRs); nil without a read of it.
	pr *prWait
}

// read is what the screen shows of task t, nil for a task home leaves out
// (stopped, or done before today; a published task stays, its group set by
// its PR once readPRs read it, which also drops a task done today whose PR
// merged before).
func (s *screen) read(t registry.Task) (*taskView, error) {
	if t.State == registry.TaskStopped || (t.State == registry.TaskDone && !sameDay(t.Updated, s.now)) {
		return nil, nil
	}
	v := &taskView{t: t, tried: map[string]bool{}, done: map[string]bool{}, letGo: map[string]string{}}
	if t.PRNumber > 0 || t.State == registry.TaskPublished {
		l, err := s.reg.LinkedPR(s.ctx, t.ID)
		if err != nil && !errors.Is(err, registry.ErrNotFound) {
			return nil, err
		}
		v.link = l
	}
	switch {
	case t.State == registry.TaskDone:
		v.group = tui.GroupDone
	case waitsOnYou(t):
		v.group = tui.GroupNeedsYou
	default:
		v.group = tui.GroupWaits
	}
	var err error
	if v.spans, err = s.reg.Spans(s.ctx, t.ID); err != nil {
		return nil, err
	}
	v.since = t.Updated
	if n := len(v.spans); n > 0 {
		v.since = v.spans[n-1].At
	}
	if err := s.readAgents(v); err != nil {
		return nil, err
	}
	if err := s.readQuestions(v); err != nil {
		return nil, err
	}
	if t.State == registry.TaskNeedsYou && strings.HasPrefix(t.WaitsWhat, readyWait) {
		if c, err := ReadReadyCard(s.l, t.ID); err == nil {
			v.card = &c
		}
	}
	v.retry = retryRefusalOf(t, v.open, v.asks) == nil // the retry's own guard, on what this view read
	if t.JiraKey != "" {
		v.reporter = readReporter(s.l.FactoryTaskDir(t.ID))
	}
	return v, nil
}

// unreadable is the row and the card of task t the screen could not read:
// its wait is on you, and says why.
func unreadable(t registry.Task, err error) (tui.FactoryRow, tui.FactoryCards) {
	what := "the screen cannot read it: " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
	w := tui.WaitsOn{Who: waitsYou, What: what, Short: "the screen cannot read it", OnYou: true}
	row := tui.FactoryRow{ID: t.ID, Key: TaskKey(t), Title: t.Title, Repo: t.Repo, Group: tui.GroupNeedsYou, Mark: tui.MarkNeedsYou,
		WaitsOn: []tui.WaitsOn{w}, Since: t.Updated, Move: "see the task card", MoveShort: "task card", Help: waitsYou + ": " + what}
	card := tui.TaskCard{Key: row.Key, Kind: t.Kind, Title: t.Title, Repo: t.Repo, Branch: cmp.Or(t.RemoteBranch, t.Branch), Copy: t.Copy,
		Started: t.Created, WaitingFor: waitsYou + ": " + what}
	return row, tui.FactoryCards{Task: card}
}

// readAgents reads task v's attempts, its current stage's verified roles,
// its sessions (the newest of each role) and the prompts you let go of.
func (s *screen) readAgents(v *taskView) error {
	as, err := s.reg.Attempts(s.ctx, v.t.ID)
	if err != nil {
		return err
	}
	var cur string
	for _, a := range as {
		v.tried[a.Stage] = true
		if a.Stage == v.t.Stage {
			cur = a.ID
		}
	}
	if cur != "" {
		runs, err := s.reg.RoleRuns(s.ctx, cur)
		if err != nil {
			return err
		}
		for _, rr := range runs {
			if rr.Outcome == "verified" || !rr.Verified.IsZero() {
				v.done[rr.Role] = true
			}
		}
	}
	if s.agents != nil {
		ss, err := runtime.TaskSessions(s.ctx, s.agents, v.t.ID)
		if err != nil {
			return err
		}
		newest := map[string]store.Session{}
		for _, x := range ss { // oldest first: the later generation wins
			newest[x.Role] = x
		}
		for _, x := range newest {
			v.agents = append(v.agents, x)
		}
		slices.SortFunc(v.agents, func(a, b store.Session) int {
			return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.ID, a.ID))
		})
	}
	return s.readLetGo(v)
}

// readLetGo finds the roles of task v whose newest prompt you let go of
// (its ledger entry abandoned, and your let-go recorded since it was sent):
// the prompt's first line, which the agent may not have received.
func (s *screen) readLetGo(v *taskView) error {
	gone, err := s.reg.LetGone(s.ctx, v.t.ID)
	if err != nil || len(gone) == 0 {
		return err
	}
	ins, err := s.reg.Inputs(s.ctx, v.t.ID)
	if err != nil {
		return err
	}
	for _, e := range gone {
		if slices.ContainsFunc(ins, func(in registry.Input) bool {
			return in.Kind == registry.InputRetry && in.Role == e.Role && !in.At.Before(e.Sent)
		}) {
			v.letGo[e.Role] = textx.FirstLine(strings.TrimSpace(e.Text))
		}
	}
	return nil
}

// readQuestions reads task v's newest revision of questions and whom the
// open ones wait on.
func (s *screen) readQuestions(v *taskView) error {
	if v.t.QuestionRev == 0 {
		return nil
	}
	qs, err := s.reg.Questions(s.ctx, v.t.ID)
	if err != nil {
		return err
	}
	for _, q := range qs {
		if q.Revision != v.t.QuestionRev {
			continue
		}
		v.all = append(v.all, q)
		if q.State == registry.QuestionOpen {
			v.open = append(v.open, q)
		}
	}
	if v.asks, err = readAsksIn(s.l.FactoryTaskDir(v.t.ID)); err != nil {
		return err
	}
	v.onYou, _, _ = questionsAsked(v.open, v.asks)
	return nil
}

// task is task v's row on home, its cards and the decisions it owes you.
func (s *screen) task(v *taskView) (tui.FactoryRow, tui.FactoryCards, int) {
	t := v.t
	row := tui.FactoryRow{ID: t.ID, Key: TaskKey(t), Title: t.Title, Repo: prRef(v).Repo, Group: v.group, Since: v.since}
	if n := cmp.Or(v.link.Number, t.PRNumber); n > 0 {
		row.GitHub = "#" + strconv.Itoa(n)
	}
	row.WaitsOn = s.blockers(v)
	decisions := 0
	switch v.group {
	case tui.GroupDone:
		row.Mark, row.Since = tui.MarkDone, time.Time{}
	case tui.GroupNeedsYou:
		row.Mark = tui.MarkNeedsYou
		row.Move, row.MoveShort = s.move(v)
		decisions = 1
		if s.onQuestions(v) {
			decisions = v.onYou
		}
	default:
		row.Mark = tui.MarkWaits
		if t.State == registry.TaskActive && len(row.WaitsOn) > 0 && isAgents(row.WaitsOn[0].Who) && working(v.agents) > 0 {
			row.Mark = tui.MarkWorking // only a working agent moves
		}
	}
	if v.pr != nil {
		row.Mark = v.pr.mark // magnum's fixer at work on the PR moves too
		v.pr.cells.fill(&row)
		row.PR = v.pr.pr
	}
	if t.State == registry.TaskQueued && t.WaitsWho == waitCopy {
		row.Frees = s.frees(v)
	}
	row.Help = s.help(v, row)
	cards := tui.FactoryCards{Task: s.taskCard(v, row)}
	if s.onQuestions(v) {
		cards.Question = s.questionCard(v)
	}
	if v.card != nil {
		cards.Ready = s.readyCard(v)
		cards.Bind = tui.ReadyBinding{HeadSHA: v.card.Head, ReportHash: v.card.ReportHash, DiffHash: v.card.DiffHash}
	}
	return row, cards, decisions
}

// onQuestions reports whether task v waits on your answers to its
// questions.
func (s *screen) onQuestions(v *taskView) bool {
	return v.group == tui.GroupNeedsYou && v.onYou > 0
}

// blockers are what task v waits on, who first, one each: you, a person,
// an agent, a hold, a copy, magnum; a task that waits on you while one of
// its agents works has both, you first.
func (s *screen) blockers(v *taskView) []tui.WaitsOn {
	t := v.t
	switch {
	case v.pr != nil:
		return []tui.WaitsOn{v.pr.on}
	case t.State == registry.TaskDone:
		return []tui.WaitsOn{{Who: waitNothing, What: "merged " + clock(s.doneAt(t))}}
	case t.State == registry.TaskPublished && waitsOnYou(t): // the tick's own wait on you (a close past the grace)
		return []tui.WaitsOn{{Who: waitsYou, What: t.WaitsWhat, Short: clauseShort(t.WaitsWhat), OnYou: true}}
	case t.State == registry.TaskPublished:
		return []tui.WaitsOn{{Who: "magnum", What: fmt.Sprintf("review of #%d", cmp.Or(v.link.Number, t.PRNumber))}}
	}
	var first tui.WaitsOn
	switch {
	case waitsOnYou(t):
		first = s.onYouBlocker(v)
	case t.WaitsWho != "":
		first = tui.WaitsOn{Who: t.WaitsWho, What: t.WaitsWhat, Short: shortWhat(t.WaitsWho, t.WaitsWhat)}
		if t.WaitsWho == waitCopy {
			if key, _ := s.freer(v); key != "" {
				first.Short = "after " + key
			}
		}
	default:
		if a, ok := s.agentBlocker(v, false); ok {
			first = a
		} else {
			first = s.stageBlocker(t)
		}
	}
	out := []tui.WaitsOn{first}
	if !isAgents(first.Who) {
		if a, ok := s.agentBlocker(v, true); ok {
			out = append(out, a)
		}
	}
	return out
}

// onYouBlocker is the blocker of a task that waits on you: its wait in the
// engine's words, the ready card's files in place of the card's title.
func (s *screen) onYouBlocker(v *taskView) tui.WaitsOn {
	t := v.t
	w := tui.WaitsOn{Who: waitsYou, What: t.WaitsWhat, Short: shortWhat(waitsYou, t.WaitsWhat), OnYou: true}
	switch {
	case v.card != nil:
		added, removed, notIn := fileCounts(v.card.Files)
		w.What = fmt.Sprintf("%s · %s +%d −%d", readyWait, plural(len(v.card.Files), "file"), added, removed)
		w.Short = readyWait
		if notIn > 0 {
			w.Long = fmt.Sprintf("%s, %d not in the plan", w.What, notIn)
		}
	case s.onQuestions(v):
		w.Long = fmt.Sprintf("%s · first: %q", w.What, firstOnYou(v).Text)
	}
	return w
}

// agentBlocker is the blocker of task v's agents: what the working ones do
// (the roles' doings), and how many of its stage's roles are done; for a
// task at an agents stage with none working yet (and not workingOnly), the
// roles still to run. false when no agent is at work.
func (s *screen) agentBlocker(v *taskView, workingOnly bool) (tui.WaitsOn, bool) {
	var doings []string
	add := func(role string) {
		if d := s.doing(role); !slices.Contains(doings, d) {
			doings = append(doings, d)
		}
	}
	for _, x := range v.agents {
		if agentState(x) == "working" {
			add(x.Role)
		}
	}
	st, stage := s.stage(v.t)
	var roles []string
	if stage && st.Type == config.FactoryStageAgents {
		roles = st.Roles
	}
	finished := 0
	for _, r := range roles {
		if v.done[r] || v.done[s.cfg.Factory.CodexFallback[r]] {
			finished++
		} else if len(doings) == 0 && !workingOnly && v.t.State == registry.TaskActive {
			add(r)
		}
	}
	if len(doings) == 0 {
		return tui.WaitsOn{}, false
	}
	w := tui.WaitsOn{Who: "agent", What: strings.Join(doings, ", ")}
	if len(doings) > 1 {
		w.Who = "agents"
	}
	if len(roles) > 1 && finished > 0 {
		w.Short = w.What
		w.What += fmt.Sprintf(", %d of %d done", finished, len(roles))
	}
	return w, true
}

// stageBlocker is what an active task without a wait of its own waits on:
// the factory's own step at its stage (magnum's local review, the push).
func (s *screen) stageBlocker(t registry.Task) tui.WaitsOn {
	st, ok := s.stage(t)
	what := cmp.Or(stageWhat(t.Stage), "starting")
	if ok {
		switch st.Type {
		case config.FactoryStageReview:
			what = "local review"
		case config.FactoryStageEffect:
			what = "pushing the branch and opening the PR"
		case config.FactoryStageDecision:
			what = "drawing the ready card"
		}
	}
	if t.State == registry.TaskQueued {
		what = "queued"
	}
	return tui.WaitsOn{Who: "magnum", What: what}
}

// move is your move on a task that needs you, in your own words and a
// short form: answer its questions, read the diff and push, retry.
func (s *screen) move(v *taskView) (long, short string) {
	switch {
	case v.pr != nil && v.pr.move != "":
		return v.pr.move, v.pr.moveShort
	case s.onQuestions(v) && v.onYou == 1:
		return "answer 1 question", "answer it"
	case s.onQuestions(v):
		return fmt.Sprintf("answer %d questions", v.onYou), "answer them"
	case v.card != nil:
		return "read the diff, then push", "diff, then push"
	case v.retry:
		return "retry", "retry"
	}
	return "see the task card", "task card"
}

// help is the line under the rule while task v's row is selected.
func (s *screen) help(v *taskView, row tui.FactoryRow) string {
	switch {
	case v.pr != nil && v.pr.help != "": // what the fixer left you, every item
		return v.pr.help
	case s.onQuestions(v):
		return fmt.Sprintf("%s · first: %q", v.t.WaitsWhat, firstOnYou(v).Text)
	case v.card != nil:
		_, _, notIn := fileCounts(v.card.Files)
		text := readyWait + ": " + plural(len(v.card.Files), "file")
		if notIn > 0 {
			text += fmt.Sprintf(", %d not in the plan", notIn)
		}
		if n := len(v.card.Flags); n > 0 {
			text += " · " + plural(n, "flag")
		}
		return text
	case v.t.State == registry.TaskQueued && v.t.WaitsWho == waitCopy:
		text := "queued: " + v.t.WaitsWhat
		if key, name := s.freer(v); key != "" && name != "" {
			text += fmt.Sprintf(" · the push of %s frees %s for it", key, name)
		}
		return text
	}
	parts := make([]string, len(row.WaitsOn))
	for i, w := range row.WaitsOn {
		parts[i] = w.Who + ": " + cmp.Or(w.Long, w.What)
	}
	return strings.Join(parts, " · ")
}

// frees is what frees a queued task that waits for a copy: the push of the
// task at its ready card that holds one, for the oldest queued task (no
// queued task jumps another); a later one starts after the oldest.
func (s *screen) frees(v *taskView) string {
	if first := s.firstQueued(); first != nil && first.ID != v.t.ID {
		return "starts after " + TaskKey(*first)
	}
	if key, _ := s.freer(v); key != "" {
		return "frees when " + key + " is pushed"
	}
	return "frees when a task at work ends"
}

// freer is the task whose push frees a copy for queued task v, and the
// copy's name: the oldest task that waits on you at its ready card, for
// the oldest queued task only.
func (s *screen) freer(v *taskView) (key, copyName string) {
	if first := s.firstQueued(); first == nil || first.ID != v.t.ID {
		return "", ""
	}
	var best *taskView
	for _, x := range s.views {
		if x.card != nil && (best == nil || x.since.Before(best.since)) {
			best = x
		}
	}
	if best == nil {
		return "", ""
	}
	return TaskKey(best.t), cmp.Or(best.card.Copy, best.t.Copy)
}

// firstQueued is the oldest queued task, nil for none.
func (s *screen) firstQueued() *registry.Task {
	for i, t := range s.tasks { // the registry lists them oldest first
		if t.State == registry.TaskQueued {
			return &s.tasks[i]
		}
	}
	return nil
}

// taskCard is task v's task card.
func (s *screen) taskCard(v *taskView, row tui.FactoryRow) tui.TaskCard {
	t := v.t
	c := tui.TaskCard{Key: TaskKey(t), Kind: t.Kind, Title: t.Title, Repo: row.Repo, Branch: cmp.Or(t.RemoteBranch, t.Branch), Copy: t.Copy,
		Started: t.Created, PR: row.GitHub, PRURL: prURL(t, v.link), Issue: t.JiraKey, IssueURL: s.issueURL(t), Retry: v.retry}
	parts := make([]string, 0, len(row.WaitsOn)+len(v.letGo))
	for _, w := range row.WaitsOn {
		parts = append(parts, w.Who+": "+w.What)
	}
	// A prompt you let go of may not have reached its agent: its first line
	// is on the WAITING FOR line, which the card always draws.
	for _, role := range slices.Sorted(maps.Keys(v.letGo)) {
		parts = append(parts, "let go: "+v.letGo[role])
	}
	c.WaitingFor = strings.Join(parts, " · ")
	if v.pr != nil {
		c.Links, c.FixNeeds, c.FixReport = v.pr.links, v.pr.needs, v.pr.report
		if v.pr.card != "" { // who left you how many items; the card lists them
			c.WaitingFor = strings.Join(append([]string{v.pr.card}, parts[1:]...), " · ")
		}
	}
	c.Stages = s.stages(v, row)
	for _, x := range v.agents {
		c.Agents = append(c.Agents, s.cardAgent(x))
	}
	c.Report = s.report(t)
	return c
}

// prURL is the GitHub page of task t's PR, which the screen's b opens: its
// link's (the PR the factory published, or one it adopted), else the
// number the task keeps; "" before it has one.
func prURL(t registry.Task, link registry.PRLink) string {
	n, repo := t.PRNumber, t.Repo
	if link.Number > 0 {
		n, repo = link.Number, cmp.Or(link.Repo, t.Repo)
	}
	if n <= 0 || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
}

// issueURL is the page of task t's Jira issue, which the screen's t opens:
// its key's link by the repository's trackers ([board] trackers, a
// [[repo]]'s or [[watch]]'s first: Config.TrackersFor), as the board links
// a PR's issue (config.Issue); "" for a task of your words, or a key no
// template names.
func (s *screen) issueURL(t registry.Task) string {
	if t.JiraKey == "" {
		return ""
	}
	if key, url := config.Issue(t.JiraKey, s.cfg.TrackersFor(t.Repo)); key == t.JiraKey {
		return url
	}
	return ""
}

// cardAgent is session x on its task's card: named by its role's doing,
// with the role's why, its state, how long it ran and when its pane last
// showed a sign of life. Nothing in 1a records what an agent does at the
// moment, so its Now stays empty.
func (s *screen) cardAgent(x store.Session) tui.CardAgent {
	role, _ := s.role(x.Role)
	a := tui.CardAgent{Name: s.doing(x.Role), Why: role.Why, State: agentState(x), Output: lastSign(x)}
	end := s.now
	if a.State != "working" {
		end = a.Output
		if x.ClosedAt != nil && x.ClosedAt.After(end) {
			end = *x.ClosedAt
		}
	}
	a.For = max(end.Sub(x.StartedAt), 0)
	return a
}

// stages are task v's stages on its card, in its kind's order: done (or
// skipped) before the one it is at, that one working, waiting or failed,
// and the rest to do; a published or done task has them all done. Each
// takes the time between its spans.
func (s *screen) stages(v *taskView, row tui.FactoryRow) []tui.CardStage {
	t := v.t
	kind := s.cfg.Factory.Kinds[t.Kind]
	at := stageIndex(kind, t.Stage)
	ended := t.State == registry.TaskPublished || t.State == registry.TaskDone
	entered, left := map[string]time.Time{}, map[string]time.Time{}
	prev := ""
	for _, sp := range v.spans {
		if sp.Stage != prev {
			if prev != "" {
				left[prev] = sp.At
			}
			entered[sp.Stage] = sp.At
			delete(left, sp.Stage)
		}
		prev = sp.Stage
	}
	out := make([]tui.CardStage, 0, len(kind.Stages))
	for i, st := range kind.Stages {
		cs := tui.CardStage{Name: stageWhat(st.Name)}
		in, out2 := entered[st.Name], left[st.Name]
		switch {
		case ended || (at >= 0 && i < at):
			cs.State, cs.Proof, cs.Finished = "done", stageProof(st), out2
			if st.Type == config.FactoryStageAgents && !v.tried[st.Name] {
				cs.State, cs.Proof, cs.Finished = "skipped", "skipped", time.Time{}
				if st.When == config.FactoryWhenWide {
					cs.Proof = "skipped: the plan is narrow"
				}
			}
			if !in.IsZero() && !out2.IsZero() {
				cs.Took = out2.Sub(in)
			}
		case i == at:
			cs.State = "working"
			switch {
			case t.State == registry.TaskNeedsYou && (st.Type == config.FactoryStageGate || st.Type == config.FactoryStageDecision):
				cs.State = "waiting"
			case t.State == registry.TaskNeedsYou:
				cs.State = "failed"
			case t.State != registry.TaskActive || (len(row.WaitsOn) > 0 && !isAgents(row.WaitsOn[0].Who) && row.WaitsOn[0].Who != "magnum"):
				cs.State = "waiting"
			}
			if len(row.WaitsOn) > 0 {
				cs.Proof = row.WaitsOn[0].Who + ": " + row.WaitsOn[0].What
			}
			if !in.IsZero() {
				cs.Took = s.now.Sub(in)
			}
		default:
			cs.State = "todo"
		}
		out = append(out, cs)
	}
	return out
}

// stageProof says what made a stage done, in words.
func stageProof(st config.FactoryStage) string {
	switch st.Type {
	case config.FactoryStageGate:
		if st.Gate == config.FactoryGateReproduced {
			return "the bug reproduced"
		}
		return "no question open"
	case config.FactoryStageDecision:
		return "you pushed"
	}
	switch st.End {
	case config.FactoryEndFindings:
		return "the findings are written"
	case config.FactoryEndReport:
		return "the report and its questions are written"
	case config.FactoryEndCritic:
		return "the critic read the plan"
	case config.FactoryEndProof:
		return "the change and the factory's proof hold"
	case config.FactoryEndClean:
		return "the local review ended clean"
	case config.FactoryEndPRBody:
		return "the PR body is drafted"
	case config.FactoryEndPR:
		return "the PR is open"
	}
	return ""
}

// report are the REPORT lines of task t's card: report.md's cause, proof
// and plan, each its first line behind its label.
func (s *screen) report(t registry.Task) []string {
	r, _, err := readReport(s.l.FactoryTaskDir(t.ID))
	if err != nil {
		return nil
	}
	var out []string
	for _, sec := range []string{"cause", sectionProof, "plan"} {
		if line := firstItem(r.sections[sec]); line != "" {
			out = append(out, sec+" "+line)
		}
	}
	return out
}

// questionCard is the question card of task v: its newest revision's
// questions, each with its options, the factory's recommendation and why,
// and your own words last; the answers the factory has, and a question
// asked of someone with whom and the comment drafted for them.
func (s *screen) questionCard(v *taskView) *tui.QuestionCard {
	t := v.t
	c := &tui.QuestionCard{TaskKey: TaskKey(t), Title: t.Title, Kind: t.Kind, Revision: t.QuestionRev, Needless: true}
	for _, q := range v.all {
		if c.Asked.IsZero() || q.Asked.Before(c.Asked) {
			c.Asked = q.Asked
		}
		cq := tui.CardQuestion{Text: q.Text, Context: q.Context, Example: q.Example, Recommended: q.Recommended, Why: q.Why}
		for _, o := range q.Options {
			cq.Options = append(cq.Options, tui.CardOption{Label: o.Label, Means: o.Means})
		}
		// The ask: of whom it was asked already, else of the ticket's
		// reporter (a task of your words, or a ticket with none, has none).
		a, asked := v.asks[QuestionKey(q.Text)]
		switch who := cleanReporter(a.Login); {
		case asked && q.State == registry.QuestionOpen && who != "":
			cq.Options = append(cq.Options, askOption(who))
			cq.Answer, cq.Asks, cq.Comment = "ask "+who+" in Jira", who, s.comment(t, q, a)
		case v.reporter != "":
			cq.Options = append(cq.Options, askOption(v.reporter))
		}
		cq.Options = append(cq.Options, tui.CardOption{Label: ownAnswer, Own: true})
		if q.State == registry.QuestionAnswered {
			cq.Answer, cq.AnsweredAt, cq.TookRecommendation = q.Answer, q.Answered, q.TookRecommendation
		}
		c.Questions = append(c.Questions, cq)
	}
	c.AsOf = c.Asked
	return c
}

// askOption is the question card's option that asks who in Jira instead
// of answering (the daemon drafts the comment).
func askOption(who string) tui.CardOption {
	return tui.CardOption{Label: "Ask " + who + " in Jira (the task waits)", Send: askPrefix + who}
}

// comment is the comment drafted for whom question q of task t was asked
// of: its file in the task's directory, else the template's draft.
func (s *screen) comment(t registry.Task, q registry.Question, a ask) string {
	if a.Comment != "" && filepath.Base(a.Comment) == a.Comment {
		if b, err := os.ReadFile(filepath.Join(s.l.FactoryTaskDir(t.ID), a.Comment)); err == nil {
			return strings.TrimRight(string(b), "\n")
		}
	}
	return strings.TrimRight(draftComment(t, q, a.Login), "\n")
}

// readyCard is the ready card of task v as the factory drew it.
func (s *screen) readyCard(v *taskView) *tui.ReadyCard {
	t, card := v.t, v.card
	c := &tui.ReadyCard{TaskKey: TaskKey(t), Title: t.Title, Kind: t.Kind, Branch: cmp.Or(card.Remote, card.Branch, t.Branch),
		Waited: max(s.now.Sub(v.since), 0), Flags: card.Flags, Leftovers: knownLines(card.Review, false), Copy: card.Copy}
	for _, f := range card.Files {
		c.Files = append(c.Files, tui.CardFile{Path: f.Path, Added: f.Added, Removed: f.Removed, InPlan: f.InPlan, Flagged: f.Flagged})
		c.Unread = append(c.Unread, f.Path) // in the card's order: the flagged ones first
	}
	dir := s.l.FactoryTaskDir(t.ID)
	if r, _, err := readReport(dir); err == nil {
		c.Cause, c.Fix, c.Proof = firstItem(r.sections["cause"]), firstItem(r.sections["plan"]), firstItem(r.sections[sectionProof])
		for _, l := range items(r.sections["doubts"]) {
			if !strings.EqualFold(l, "none") {
				c.Doubts = append(c.Doubts, l)
			}
		}
	}
	if p, ok := readProof(dir); ok && p.Holds(card.Head) {
		c.Proof = proofWords(p)
	}
	if c.Frees = card.Copy; c.Frees != "" {
		if next := NextQueued(s.tasks); next != "" {
			c.Frees += "; " + next + " starts in it"
		}
	}
	c.Failed = s.failedChecks(t, card.Head)
	for _, f := range card.Flags {
		if strings.HasPrefix(f, conflictsFlag) {
			c.NoPush = "the base conflicts: the writer merges it first"
		}
	}
	rev := card.Review
	switch {
	case c.NoPush != "":
		c.Recommended, c.Why = 1, "the change conflicts with the base: send it back, and the writer merges the base first"
	case len(c.Failed) > 0:
		c.Recommended, c.Why = 1, "a check failed at this head: send it back to fix it"
	case rev.Exit == registry.ExitOscillation:
		c.Recommended, c.Why = 0, fmt.Sprintf("the local review went back and forth after %s: read KNOWN, then push", plural(rev.Rounds, "round"))
	default:
		c.Recommended, c.Why = 0, fmt.Sprintf("the local review ended clean after %s and the factory's proof holds at %s",
			plural(max(rev.Rounds, 1), "round"), short(card.Head))
	}
	return c
}

// failedChecks are the checks the factory ran itself at head that failed
// in their newest run, one each ("lint: failed, exit 1").
func (s *screen) failedChecks(t registry.Task, head string) []string {
	cs, err := s.reg.Checks(s.ctx, t.ID)
	if err != nil || head == "" {
		return nil
	}
	newest := map[string]registry.Check{}
	var kinds []string
	for _, c := range cs {
		if c.HeadSHA != head || c.Source != "factory" {
			continue
		}
		if _, ok := newest[c.Kind]; !ok {
			kinds = append(kinds, c.Kind)
		}
		newest[c.Kind] = c
	}
	var out []string
	for _, k := range kinds {
		if c := newest[k]; c.Result == registry.CheckFail {
			out = append(out, fmt.Sprintf("%s: failed, exit %d", k, c.Exit))
		}
	}
	return out
}

// digest is what finished while you were away (more than awayAfter since
// lastSeen; none with no time seen): the tasks done or published since,
// newest first, and the chip with how long you were away and what came to
// need you meanwhile (rows). nil when nothing finished.
func (s *screen) digest(lastSeen time.Time, rows []tui.FactoryRow) (*tui.Digest, error) {
	if !s.away(lastSeen) {
		return nil, nil
	}
	type finished struct {
		at   time.Time
		line string
	}
	var fs []finished
	for _, t := range s.tasks {
		name := strings.TrimSpace(TaskKey(t) + " " + t.Title)
		switch t.State {
		case registry.TaskDone:
			if at := s.doneAt(t); at.After(lastSeen) {
				fs = append(fs, finished{at, name + " · merged " + clock(at)})
			}
		case registry.TaskPublished:
			l, err := s.reg.LinkedPR(s.ctx, t.ID)
			if err != nil && !errors.Is(err, registry.ErrNotFound) {
				return nil, err
			}
			if at := cmp.Or(l.Opened, t.Updated); at.After(lastSeen) {
				fs = append(fs, finished{at, fmt.Sprintf("%s · you pushed %s, PR #%d", name, clock(at), cmp.Or(l.Number, t.PRNumber))})
			}
		}
	}
	if len(fs) == 0 {
		return nil, nil
	}
	slices.SortStableFunc(fs, func(a, b finished) int { return b.at.Compare(a.at) })
	d := &tui.Digest{Since: lastSeen}
	for _, f := range fs {
		d.Done = append(d.Done, f.line)
	}
	fresh := 0
	for _, r := range rows {
		if r.Group == tui.GroupNeedsYou && r.Since.After(lastSeen) {
			fresh++
		}
	}
	d.Chip = fmt.Sprintf("away %s: %d done", awayWords(s.now.Sub(lastSeen)), len(fs))
	if fresh > 0 {
		d.Chip += fmt.Sprintf(", %d new for you", fresh)
	}
	return d, nil
}

// away reports whether you were away long enough for a digest: more than
// awayAfter since lastSeen (zero: never seen, no digest).
func (s *screen) away(lastSeen time.Time) bool {
	return !lastSeen.IsZero() && s.now.Sub(lastSeen) > awayAfter
}

// next is what you do next when nothing waits on you: the next decision of
// the oldest task at work, once its stage ends.
func (s *screen) next(rows []tui.FactoryRow) string {
	if slices.ContainsFunc(rows, func(r tui.FactoryRow) bool { return r.Group == tui.GroupNeedsYou }) {
		return ""
	}
	for _, v := range s.views {
		t := v.t
		kind := s.cfg.Factory.Kinds[t.Kind]
		at := stageIndex(kind, t.Stage)
		if t.State != registry.TaskActive || at < 0 {
			continue
		}
		for _, st := range kind.Stages[at+1:] {
			switch {
			case st.Type == config.FactoryStageGate && st.Gate == config.FactoryGateNoOpenQuestions:
				return fmt.Sprintf("%s's questions or its diff, once its %s ends.", TaskKey(t), stageWhat(t.Stage))
			case st.Type == config.FactoryStageDecision:
				return fmt.Sprintf("%s's diff, once its %s ends.", TaskKey(t), stageWhat(t.Stage))
			}
		}
	}
	return ""
}

// flash is the news of the status rule: the daemon's answer to the newest
// request of yours (flashKinds) it carried out within flashFor, read typed
// from the requests ("14:02 started PR-12345 as task 3: ...").
func (s *screen) flash() (string, error) {
	if s.reg == nil {
		return "", nil
	}
	q, ok, err := s.reg.LastDone(s.ctx, s.now.Add(-flashFor), flashKinds...)
	if err != nil || !ok {
		return "", err
	}
	return clock(q.Handled) + " " + textx.FirstLine(q.Result), nil
}

// alerts are the alert events and their delivery's (each toast, hold and
// drop, alertKinds), newest first.
func (s *screen) alerts() ([]tui.FactoryAlert, error) {
	if s.reg == nil {
		return nil, nil
	}
	evs, err := s.reg.RecentEvents(s.ctx, alertsShown, alertKinds...)
	if err != nil {
		return nil, err
	}
	out := make([]tui.FactoryAlert, 0, len(evs))
	for _, ev := range evs {
		out = append(out, tui.FactoryAlert{At: ev.At, Text: ev.Text})
	}
	return out, nil
}

// repos are the repositories home is of, which its title names: the
// configured ones, in the config's order; without any, the tasks'.
func (s *screen) repos() []string {
	var out []string
	add := func(full string) {
		if full != "" && !slices.ContainsFunc(out, func(r string) bool { return strings.EqualFold(r, full) }) {
			out = append(out, full)
		}
	}
	for _, r := range s.cfg.Factory.Repos {
		add(r.Repo)
	}
	if len(out) == 0 {
		for _, t := range s.tasks {
			add(t.Repo)
		}
	}
	return out
}

// blockingLines are the blocker lines of the holds of the machine in force,
// as the daemon last saved them (ReadHolds): short, and with the reason
// for a wide screen.
func blockingLines(l paths.Layout) (short, long []string, err error) {
	hs, err := ReadHolds(l)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range hs {
		line := cmp.Or(holdWords[h.Kind], h.Kind+": stages wait")
		short = append(short, line)
		long = append(long, line+" ("+h.Reason+")")
	}
	return short, long, nil
}

// stage is task t's current stage of its kind.
func (s *screen) stage(t registry.Task) (config.FactoryStage, bool) {
	kind := s.cfg.Factory.Kinds[t.Kind]
	if i := stageIndex(kind, t.Stage); i >= 0 {
		return kind.Stages[i], true
	}
	return config.FactoryStage{}, false
}

// role is the [[factory.role]] named name.
func (s *screen) role(name string) (config.Role, bool) {
	i := slices.IndexFunc(s.cfg.Factory.Roles, func(r config.Role) bool { return r.Name == name })
	if i < 0 {
		return config.Role{}, false
	}
	return s.cfg.Factory.Roles[i], true
}

// doing is what role name's agent does, as the screen names it ("finding
// the cause"); a role the config no longer has is its name.
func (s *screen) doing(name string) string {
	r, _ := s.role(name)
	return cmp.Or(r.Doing, name)
}

// agentState is how session x is doing, as the agents view says it:
// working, waits on you (its pane asks something), done (idle or closed),
// parked or lost.
func agentState(x store.Session) string {
	switch x.State {
	case store.SessionParked:
		return "parked"
	case store.SessionLost:
		return "lost"
	case store.SessionStarting, store.SessionLive:
		switch store.Deref(x.AgentStatus) {
		case "working":
			return "working"
		case "blocked":
			return tui.AgentWaitsOnYou
		}
	}
	return "done"
}

// working counts the sessions at work.
func working(ss []store.Session) int {
	n := 0
	for _, x := range ss {
		if agentState(x) == "working" {
			n++
		}
	}
	return n
}

// lastSign is the newest sign of life of session x: its start, its agent's
// last change of state, its last prompt.
func lastSign(x store.Session) time.Time {
	at := x.StartedAt
	for _, t := range []*time.Time{x.AgentStatusAt, x.LastPromptAt} {
		if t != nil && t.After(at) {
			at = *t
		}
	}
	return at
}

// isAgents reports whether a blocker is the task's agents.
func isAgents(who string) bool { return who == "agent" || who == "agents" }

// shortWhat is a wait's short form for a narrow screen ("" when it is
// short already): a count of questions, "full" for the disk or the memory,
// else the wait up to its reason.
func shortWhat(who, what string) string {
	switch {
	case who == waitMemory || who == HoldDisk:
		return "full"
	case questionsWaitRe.MatchString(what) && !strings.HasSuffix(what, " in Jira"):
		f := strings.Fields(what)
		return f[0] + " " + f[1]
	}
	if i := strings.IndexAny(what, "(:"); i > 0 {
		return strings.TrimRight(what[:i], " ")
	}
	return ""
}

// firstOnYou is the first open question of task v that waits on you.
func firstOnYou(v *taskView) registry.Question {
	for _, q := range v.open {
		if _, asked := v.asks[QuestionKey(q.Text)]; !asked {
			return q
		}
	}
	return registry.Question{}
}

// fileCounts are the lines a card's files add and remove, and how many the
// plan did not name.
func fileCounts(fs []CardFile) (added, removed, notInPlan int) {
	for _, f := range fs {
		added, removed = added+f.Added, removed+f.Removed
		if !f.InPlan {
			notInPlan++
		}
	}
	return added, removed, notInPlan
}

// readProof reads the factory's proof in the task directory dir.
func readProof(dir string) (Proof, bool) {
	var p Proof
	b, err := os.ReadFile(filepath.Join(dir, factoryProofFile))
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, true
}

// proofWords is the PROOF row of a proof that holds: its spec files, red
// on the base and green at the head (a kind whose proof is green: green at
// the head).
func proofWords(p Proof) string {
	specs := plural(len(p.Specs), "spec file")
	if len(p.Specs) > 0 {
		specs += " (" + strings.Join(p.Specs, ", ") + ")"
	}
	if p.Base.Command != "" {
		return specs + ": red before, green after"
	}
	return specs + ": green at the head"
}

// items are a report section's lines, each without its list mark.
func items(section string) []string {
	var out []string
	for l := range strings.SplitSeq(section, "\n") {
		l = strings.TrimSpace(l)
		for _, mark := range []string{"- ", "* ", "+ "} {
			l = strings.TrimPrefix(l, mark)
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// firstItem is a report section's first line ("" for none).
func firstItem(section string) string {
	if l := items(section); len(l) > 0 {
		return l[0]
	}
	return ""
}

// sameDay reports whether a and b are on the same local day.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

// clock is a time as the screen says it ("09:40").
func clock(t time.Time) string { return t.Local().Format("15:04") }

// awayWords is an absence in the digest's chip: whole hours from two hours
// ("14h"), else hours and minutes ("1h20m").
func awayWords(d time.Duration) string {
	if d >= 2*time.Hour {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return shortAge(d)
}
