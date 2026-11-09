package factory

// The gates before code (spec 6.3 and 6.8). A gate stage holds its task
// until its gate holds, and every question goes to you in one batch: the
// questions of questions.json (the judge's) and of each questions-<role>.json
// (the critic's, an investigator's, the writer's) that the task has not
// asked yet become one new revision of the task's questions, with the
// questions still open carried over, and the task waits for your answers.
// no_open_questions holds when no question is open; before it passes, an
// answer that changed the plan (another option than the recommended one,
// your own words) has one turn of the judge amend report.md, its checks and
// its questions (factory-amend.md). reproduced holds on a report whose
// Proof says, in so many words, that the bug reproduced; anything else asks
// you the reproduction question, and your answer is acted on: "drop" stops
// the task, any other has the judge amend the report with it. Everything a
// gate decides lives in the registry and the task's directory, so a restart
// at any point asks nothing twice and loses no answer.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// AmendPrompt is the prompt of the judge's turn that amends report.md when
// your answers change the plan: the report stage's roles get it in place of
// their own prompt.
const AmendPrompt = "factory-amend.md"

// factoryAsks raises the questions the factory itself asks (the
// reproduction question of a report whose judge asked none).
const factoryAsks = "factory"

// runGate runs one pass of gate stage st of task t (the engine's runner for
// config.FactoryStageGate): done reports that its gate holds. A gate of a
// later build leaves the task where it is.
func (e *Engine) runGate(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	switch st.Gate {
	case config.FactoryGateNoOpenQuestions:
		return e.gateQuestions(ctx, t, st)
	case config.FactoryGateReproduced:
		return e.gateReproduced(ctx, t, st)
	}
	return false, nil
}

// gateQuestions is the no_open_questions gate: the questions the task's
// files hold are asked (awaitAnswers), and once none is open, an amend of
// the report that the answers made due runs first; the amended questions
// are asked in turn.
func (e *Engine) gateQuestions(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if done, err := e.amendInFlight(ctx, t, st); err != nil || !done {
		return false, err
	}
	for range 2 { // once more after an amend that passed in this pass
		if waits, err := e.awaitAnswers(ctx, t); err != nil || waits {
			return false, err
		}
		if dropped, err := e.dropIfAnswered(ctx, t); err != nil || dropped {
			return false, err
		}
		due, err := e.amendDue(ctx, t)
		switch {
		case err != nil:
			return false, err
		case !due:
			return true, nil
		}
		if done, err := e.amend(ctx, t, st); err != nil || !done {
			return false, err
		}
	}
	return false, nil
}

// gateReproduced is the reproduced gate: it holds on a report whose Proof
// says "Reproduced: yes". Otherwise the reproduction question waits on you:
// the judge's (questions.json), else the factory's own, with the four ways
// on (ReproductionOptions), one recommended. A "drop" stops the task
// (dropIfAnswered); any other answer since the report was written has the
// judge amend the report with it, and the gate reads the amended Proof.
func (e *Engine) gateReproduced(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if done, err := e.amendInFlight(ctx, t, st); err != nil || !done {
		return false, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	for range 2 { // once more after an amend that passed in this pass
		if waits, err := e.awaitAnswers(ctx, t); err != nil || waits {
			return false, err
		}
		if dropped, err := e.dropIfAnswered(ctx, t); err != nil || dropped {
			return false, err
		}
		r, at, err := readReport(dir)
		if err != nil {
			return false, err
		}
		if r.reproduced() {
			return true, nil
		}
		answered, err := e.reproductionAnswered(ctx, t, at)
		if err != nil {
			return false, err
		}
		if !answered {
			return false, e.askReproduction(ctx, t, dir)
		}
		if len(e.reportRoles(t)) == 0 {
			return false, e.stageFailed(ctx, t, needsYou{"no stage of the " + t.Kind + " kind writes report.md to act on your answer: " +
				"its Proof must say Reproduced: yes"})
		}
		if done, err := e.amend(ctx, t, st); err != nil || !done {
			return false, err
		}
	}
	return false, nil
}

// readReport reads report.md in dir and its time; a missing one is an empty
// report (it names no reproduction) of the zero time.
func readReport(dir string) (report, time.Time, error) {
	path := filepath.Join(dir, reportFile)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return parseReport(""), time.Time{}, nil
	}
	if err != nil {
		return report{}, time.Time{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return report{}, time.Time{}, err
	}
	return parseReport(string(b)), fi.ModTime(), nil
}

// reproductionAnswered reports whether you answered a reproduction question
// of task t (one that offers every one of ReproductionOptions) after the
// report of time at was written: an answer the report does not act on yet.
func (e *Engine) reproductionAnswered(ctx context.Context, t registry.Task, at time.Time) (bool, error) {
	qs, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(qs, func(q registry.Question) bool {
		return q.State == registry.QuestionAnswered && q.Answered.After(at) && offersReproduction(q.Options)
	}), nil
}

// dropIfAnswered stops task t when you answered its newest reproduction
// question "drop", at whichever gate sees it first (the questions gate asks
// the judge's reproduction question in the default bug kind): a drop is
// final, and no amend of the report runs for it. dropped reports whether it
// stopped the task.
func (e *Engine) dropIfAnswered(ctx context.Context, t registry.Task) (dropped bool, err error) {
	qs, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return false, err
	}
	for _, q := range slices.Backward(qs) {
		if !offersReproduction(q.Options) {
			continue
		}
		if q.State != registry.QuestionAnswered || !isDrop(q.Answer) {
			return false, nil
		}
		return true, e.endTask(ctx, t, "dropped by you: the bug did not reproduce",
			registry.Input{TaskID: t.ID, Kind: registry.InputDrop, Surface: answerSurface, Stage: t.Stage, Role: q.RaisedBy})
	}
	return false, nil
}

// askReproduction asks the reproduction question of a report that names no
// reproduction, as a revision of its own: the judge's from questions.json,
// else the factory's own (reproductionQuestion). The task then waits on
// you.
func (e *Engine) askReproduction(ctx context.Context, t registry.Task, dir string) error {
	q := reproductionQuestion()
	if qs, p := readQuestions(dir, questionsFile, time.Time{}); p == nil {
		if i := slices.IndexFunc(qs, isReproductionQuestion); i >= 0 {
			q = toQuestion(qs[i], e.reportRole(ctx, t, qs[i].RaisedBy))
		}
	}
	if err := e.storeBatch(ctx, t, []registry.Question{q}, "the report names no reproduction"); err != nil {
		return err
	}
	_, err := e.awaitAnswers(ctx, t)
	return err
}

// reproductionQuestion is the factory's own question of a bug that did not
// reproduce, with ReproductionOptions, a hint recommended.
func reproductionQuestion() registry.Question {
	means := []string{
		"you say where or how it breaks, and the judge tries the reproduction again with it",
		"the judge plans the fix on the cause another agent found, and tries to reproduce that",
		"the reproduction is tried in an environment closer to the one where the bug shows",
		"the task stops",
	}
	q := registry.Question{Text: "The bug did not reproduce. How does the task go on?",
		Context:     "report.md's Proof does not say `Reproduced: yes`, and a bug's fix waits for a reproduction",
		Example:     "a spec that fails before the fix and passes after it",
		Recommended: 0, Why: "a hint from you is the shortest way to a reproduction",
		RaisedBy: factoryAsks, Evidence: "report.md, its Proof section"}
	for i, label := range ReproductionOptions {
		q.Options = append(q.Options, registry.Option{Label: label, Means: means[i]})
	}
	return q
}

// offersReproduction reports whether options are those of a reproduction
// question: every one of ReproductionOptions among their labels.
func offersReproduction(options []registry.Option) bool {
	labels := make([]string, 0, len(options))
	for _, o := range options {
		labels = append(labels, normLabel(o.Label))
	}
	for _, want := range ReproductionOptions {
		if !slices.Contains(labels, normLabel(want)) {
			return false
		}
	}
	return true
}

// isDrop reports whether answer takes the reproduction question's "drop".
func isDrop(answer string) bool {
	return normLabel(answer) == normLabel(ReproductionOptions[len(ReproductionOptions)-1])
}

// normLabel is an option's label as answers are matched to it
// (registry.NormLabel, answers.md's rule).
func normLabel(s string) string { return registry.NormLabel(s) }

// awaitAnswers asks the questions the task's files hold that it has not
// asked (askQuestions) and reports whether the task waits for answers: a
// question of its newest revision is open, and the task waits on you or on
// whom you asked (waitForAnswers). answers.md is written again first when
// the registry holds answers it lacks (a restart between the two).
func (e *Engine) awaitAnswers(ctx context.Context, t registry.Task) (bool, error) {
	if err := e.writeAnswers(ctx, t); err != nil {
		return true, err
	}
	if _, err := e.askQuestions(ctx, t, true); err != nil {
		return true, e.stageFailed(ctx, t, err)
	}
	open, err := e.openQuestions(ctx, t.ID)
	if err != nil || len(open) == 0 {
		return err != nil, err
	}
	return true, e.waitForAnswers(ctx, t, open)
}

// questionsOf is a questions file of a task's directory and the role it is
// raised by.
type questionsOf struct{ name, raisedBy string }

// questionFiles are the questions files in task t's directory dir:
// questions.json (raised by the role of the kind's report stage), then each
// questions-<role>.json by name, raised by the role its name gives, but for
// the QA walker's and a second reader's (asksNoOne): they ask no one, so a
// file one of them wrote anyway never reaches you.
func (e *Engine) questionFiles(ctx context.Context, t registry.Task, dir string) ([]questionsOf, error) {
	var out []questionsOf
	if _, err := os.Stat(filepath.Join(dir, questionsFile)); err == nil {
		out = append(out, questionsOf{questionsFile, e.reportRole(ctx, t, "")})
	}
	names, err := filepath.Glob(filepath.Join(dir, roleQuestionsFile("*")))
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	for _, path := range names {
		name := filepath.Base(path)
		role := strings.TrimSuffix(strings.TrimPrefix(name, "questions-"), ".json")
		if role != "" && role != "*" && !e.asksNoOne(t, role) {
			out = append(out, questionsOf{name, role})
		}
	}
	return out, nil
}

// asksNoOne reports whether role, in task t's kind, has no questions file
// (questionsFileOf): a stage names it (a qa stage, or one it is the second
// reader of), and none gives it a questions file.
func (e *Engine) asksNoOne(t registry.Task, role string) bool {
	named := false
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if !slices.Contains(st.Roles, role) {
			continue
		}
		if questionsFileOf(e.cfg, st, role) != "" {
			return false
		}
		named = true
	}
	return named
}

// reportRole is the role that raises the questions of questions.json: the
// first role of the kind's agents stage that ends on the report, named as it
// ran the report (reportRan), else written (the file's own raised_by), else
// "judge".
func (e *Engine) reportRole(ctx context.Context, t registry.Task, written string) string {
	if roles := e.reportRoles(t); len(roles) > 0 {
		return e.reportRan(ctx, t, roles[0])
	}
	return cmp.Or(written, "judge")
}

// reportRan is role, the first of task t's report stage, as it ran the
// report: its codex_fallback when a run of the fallback in that stage is one
// the runtime verified and no run of role itself is (a Codex judge refused,
// one on a task Codex never worked on by then, or one that yielded to
// Codex's soft cap or its usage limit, which mark nothing), else role. So
// the questions it raises name the role whose run ended on the report, as
// the role runs keep it, which stats tell apart as the judge's. A read that
// fails is logged, and the name is role's.
func (e *Engine) reportRan(ctx context.Context, t registry.Task, role string) string {
	fb := e.cfg.Factory.CodexFallback[role]
	if fb == "" || fb == role {
		return role
	}
	var stage string
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageAgents && st.End == config.FactoryEndReport {
			stage = st.Name
			break
		}
	}
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		e.log.Warn("read the attempts of a task's report stage", "task", t.ID, "err", err)
		return role
	}
	ran := map[string]bool{}
	for _, a := range as {
		if a.Stage != stage {
			continue
		}
		rrs, err := e.reg.RoleRuns(ctx, a.ID)
		if err != nil {
			e.log.Warn("read the role runs of a task's report stage", "task", t.ID, "err", err)
			return role
		}
		for _, rr := range rrs {
			if rr.Outcome == "verified" {
				ran[rr.Role] = true
			}
		}
	}
	if ran[fb] && !ran[role] {
		return fb
	}
	return role
}

// reportRoles are the roles of the first agents stage of task t's kind that
// ends on the judge's report.
func (e *Engine) reportRoles(t registry.Task) []string {
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageAgents && st.End == config.FactoryEndReport {
			return st.Roles
		}
	}
	return nil
}

// QuestionKey is how a question is told from the questions the task asked,
// here and in the CLI: registry.QuestionKey (its text, lowercased, without
// its marks, spaces and end punctuation), the rule by which answers.md
// keeps only the newest answer of a question asked again.
func QuestionKey(text string) string { return registry.QuestionKey(text) }

// withRole is raisedBy with role added, when it is not one of its roles
// already: a question two roles raised is raised by "fx-judge, fx-points".
func withRole(raisedBy, role string) string {
	if raisedBy == "" {
		return role
	}
	if slices.Contains(strings.Split(raisedBy, ", "), role) {
		return raisedBy
	}
	return raisedBy + ", " + role
}

// askedStates are the state of each question the task asked, by its
// QuestionKey, the newest revision's winning: a question carried over is
// open there. A question voided and never carried over (it is open nowhere)
// counts as not asked.
func askedStates(have []registry.Question) map[string]string {
	states := map[string]string{}
	for _, q := range have { // by revision, oldest first
		k := QuestionKey(q.Text)
		if q.State == registry.QuestionVoided {
			if states[k] == registry.QuestionOpen {
				continue // voided by the revision that carries it over
			}
			delete(states, k)
			continue
		}
		states[k] = q.State
	}
	return states
}

// askQuestions asks the questions of task t's questions files that it has
// not asked (askedStates: no question of the same text, whichever role
// raised it, is open or answered) as one new revision (storeBatch). A
// question two files of the batch hold is asked once, the first file's
// (questions.json comes first), raised by the roles of both (withRole).
// A file that does not parse is, with strict, what the task waits on you
// for (needsYou); without, it is left to its role's end fact (another role
// may still write it). asked reports whether a revision was stored.
func (e *Engine) askQuestions(ctx context.Context, t registry.Task, strict bool) (asked bool, err error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	files, err := e.questionFiles(ctx, t, dir)
	if err != nil || len(files) == 0 {
		return false, err
	}
	have, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return false, err
	}
	seen := map[string]bool{}
	for k := range askedStates(have) {
		seen[k] = true
	}
	var fresh []registry.Question
	batch := map[string]int{} // the questions of fresh, by their key
	var from []string
	repeats := 0
	for _, f := range files {
		qs, p := readQuestions(dir, f.name, time.Time{})
		if p != nil {
			if strict {
				return false, needsYou{"the factory cannot read the questions of " + p.String()}
			}
			continue
		}
		n := len(fresh)
		for _, q := range qs {
			k := QuestionKey(q.Text)
			if i, ok := batch[k]; ok {
				if raised := withRole(fresh[i].RaisedBy, f.raisedBy); raised != fresh[i].RaisedBy {
					fresh[i].RaisedBy = raised
					repeats++
				}
				continue
			}
			if !seen[k] {
				seen[k] = true
				batch[k] = len(fresh)
				fresh = append(fresh, toQuestion(q, f.raisedBy))
			}
		}
		if len(fresh) > n {
			from = append(from, f.name)
		}
	}
	if len(fresh) == 0 {
		return false, nil
	}
	why := "from " + strings.Join(from, ", ")
	if repeats > 0 {
		why += fmt.Sprintf("; %s asked once", plural(repeats, "repeat"))
	}
	return true, e.storeBatch(ctx, t, fresh, why)
}

// toQuestion is a question of a file as the registry keeps it, raised by
// raisedBy (the file's role, whatever the file says).
func toQuestion(q QuestionFile, raisedBy string) registry.Question {
	rec := -1
	if q.Recommended != nil {
		rec = *q.Recommended
	}
	return registry.Question{Text: q.Text, Context: q.Context, Example: q.Example, Options: q.Options, Recommended: rec, Why: q.Why,
		RaisedBy: raisedBy, Evidence: q.Evidence}
}

// storeBatch stores qs as the task's next revision of questions, after the
// questions of its newest revision that are still open (a revision voids
// the open questions of the ones before it, so they come along), and
// records why.
func (e *Engine) storeBatch(ctx context.Context, t registry.Task, qs []registry.Question, why string) error {
	cur, err := e.reg.TaskByID(ctx, t.ID)
	if err != nil {
		return err
	}
	open, err := e.openQuestions(ctx, t.ID)
	if err != nil {
		return err
	}
	var batch []registry.Question
	for _, q := range open {
		batch = append(batch, registry.Question{Text: q.Text, Context: q.Context, Example: q.Example, Options: q.Options,
			Recommended: q.Recommended, Why: q.Why, RaisedBy: q.RaisedBy, Evidence: q.Evidence})
	}
	batch = append(batch, qs...)
	rev := cur.QuestionRev + 1
	if err := e.reg.ReplaceQuestions(ctx, t.ID, rev, batch); err != nil {
		return err
	}
	e.event(ctx, t.ID, "questions", registry.PhaseInfo, fmt.Sprintf("revision %d: %s, %d carried over (%s)", rev,
		plural(len(qs), "new question"), len(open), why))
	return nil
}

// openQuestions are the open questions of the task's newest revision.
func (e *Engine) openQuestions(ctx context.Context, taskID int64) ([]registry.Question, error) {
	t, err := e.reg.TaskByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return openQuestionsOf(ctx, e.reg, t)
}

// openQuestionsOf are the open questions of task t's newest revision, as t
// names it (QuestionRev).
func openQuestionsOf(ctx context.Context, reg *registry.Registry, t registry.Task) ([]registry.Question, error) {
	qs, err := reg.Questions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(qs, func(q registry.Question) bool { return q.Revision != t.QuestionRev || q.State != registry.QuestionOpen }), nil
}

// waitForAnswers has task t wait for the answers to open, the open
// questions of its newest revision (questionsWait): on you, NEEDS YOU with
// an alert, or on whom you asked, waiting.
func (e *Engine) waitForAnswers(ctx context.Context, t registry.Task, open []registry.Question) error {
	asks, err := e.readAsks(t.ID)
	if err != nil {
		return err
	}
	state, who, what := e.questionsWait(t, open, asks)
	switch {
	case t.State == state && t.WaitsWho == who && t.WaitsWhat == what:
		return nil
	case state == registry.TaskNeedsYou && t.State != registry.TaskNeedsYou:
		e.toOperator(ctx, t, what, "questions wait for your answers")
		return nil
	}
	return e.reg.TransitionTask(ctx, t.ID, []string{t.State}, state, func(u *registry.TaskUpdate) {
		u.Set("waits_who", who)
		u.Set("waits_what", what)
		u.Because("questions wait for answers")
	})
}

// questionsWait is how task t waits for the answers to its open questions:
// on you while one of them is not asked of someone ("you: 3 questions
// before coding"; a stage that a role's questions suspended: "1 question
// before coding goes on"), else on the first one you asked ("rev-ann: a
// question in Jira").
func (e *Engine) questionsWait(t registry.Task, open []registry.Question, asks map[string]ask) (state, who, what string) {
	onYou, first, byLogin := questionsAsked(open, asks)
	if onYou > 0 || first == "" {
		return registry.TaskNeedsYou, waitsYou, e.questionsBefore(t, max(onYou, 1))
	}
	if n := byLogin[first]; n > 1 {
		return registry.TaskWaiting, first, fmt.Sprintf("%d questions in Jira", n)
	}
	return registry.TaskWaiting, first, "a question in Jira"
}

// questionsAsked splits the open questions open by whom they wait on: onYou
// on you (asked of no one in asks), the rest by the login asked, first the
// login of the first of them ("" when none is asked).
func questionsAsked(open []registry.Question, asks map[string]ask) (onYou int, first string, byLogin map[string]int) {
	byLogin = map[string]int{}
	for _, q := range open {
		a, ok := asks[QuestionKey(q.Text)]
		if !ok {
			onYou++
			continue
		}
		if first == "" {
			first = a.Login
		}
		byLogin[a.Login]++
	}
	return onYou, first, byLogin
}

// questionsOnYou reports whether the open questions open wait on you, as
// questionsWait has them (NEEDS YOU): one of them is asked of no one in
// asks, or none is asked at all.
func questionsOnYou(open []registry.Question, asks map[string]ask) bool {
	onYou, first, _ := questionsAsked(open, asks)
	return onYou > 0 || first == ""
}

// questionsBefore says what n questions of task t hold up: at a gate, the
// next stage that is no gate ("2 questions before coding"); in a stage a
// role's questions suspended, the stage ("1 question before coding goes
// on").
func (e *Engine) questionsBefore(t registry.Task, n int) string {
	kind := e.cfg.Factory.Kinds[t.Kind]
	if i := stageIndex(kind, t.Stage); i >= 0 && kind.Stages[i].Type == config.FactoryStageGate {
		for _, st := range kind.Stages[i+1:] {
			if st.Type != config.FactoryStageGate {
				return plural(n, "question") + " before " + stageWhat(st.Name)
			}
		}
		return plural(n, "question") + " before the task goes on"
	}
	return plural(n, "question") + " before " + stageWhat(t.Stage) + " goes on"
}

// questionsWaitRe are the waits of questionsWait.
var questionsWaitRe = regexp.MustCompile(`^(\d+ questions? before .+|a question in Jira|\d+ questions in Jira)$`)

// waitsOnQuestions reports whether task t waits for answers to its
// questions (questionsWait), on you or on someone you asked.
func waitsOnQuestions(t registry.Task) bool {
	switch t.State {
	case registry.TaskNeedsYou:
		return t.WaitsWho == waitsYou && questionsWaitRe.MatchString(t.WaitsWhat)
	case registry.TaskWaiting:
		return t.WaitsWho != waitsYou && strings.HasSuffix(t.WaitsWhat, " in Jira") && questionsWaitRe.MatchString(t.WaitsWhat)
	}
	return false
}

// plural is n and noun, with an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// amendDue reports whether your answers changed the plan since report.md
// was written: a question you answered after it took another option than
// the recommended one, or your own words, or the ticket of the Jira issue
// your answer linked the task to was read after it (linkedTicket). Intake's
// question which Jira issue the task is counts only by that ticket: "no
// issue" changes no plan, whatever was recommended. A kind with no stage
// that writes the report has nothing to amend it with.
func (e *Engine) amendDue(ctx context.Context, t registry.Task) (bool, error) {
	if len(e.reportRoles(t)) == 0 {
		return false, nil
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	_, at, err := readReport(dir)
	if err != nil {
		return false, err
	}
	if fi, err := os.Stat(filepath.Join(dir, ticketFile)); err == nil && !at.IsZero() && fi.ModTime().After(at) {
		return true, nil
	}
	qs, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(qs, func(q registry.Question) bool {
		intake := q.RaisedBy == intakeRole && q.Text == whichIssue
		return q.State == registry.QuestionAnswered && !q.TookRecommendation && !intake && q.Answered.After(at)
	}), nil
}

// amendInFlight goes on with an amend turn whose attempt is still open in
// gate stage st (a restart, a turn still at work): done once there is none
// or it passed. Only such a turn makes attempts of a gate.
func (e *Engine) amendInFlight(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return false, err
	}
	for _, a := range slices.Backward(as) {
		if a.Stage != st.Name {
			continue
		}
		switch a.State {
		case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
			return e.amend(ctx, t, st)
		}
		break
	}
	return true, nil
}

// amend runs one turn of the roles of the kind's report stage, with
// AmendPrompt, in gate stage st of task t: they amend report.md,
// acceptance.json and questions.json with your answers, and the turn
// passes on the report end fact with all three newer than answers.md.
func (e *Engine) amend(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	roles := e.reportRoles(t)
	if len(roles) == 0 {
		return true, nil
	}
	fi, err := os.Stat(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), answersFile))
	if err != nil {
		return false, fmt.Errorf("amend the report of %s: %w", TaskKey(t), err)
	}
	turn := config.FactoryStage{Name: st.Name, Type: config.FactoryStageAgents, Roles: roles, End: config.FactoryEndReport, Prompt: AmendPrompt}
	return e.runTurn(ctx, t, turn, agentsTurn{gate: true, since: fi.ModTime()})
}

// The verdicts of roleAsks on a run that ended.
const (
	askNone     = iota // no question of the role's to act on: its end fact decides
	askWaits           // its questions wait for your answers: the stage is suspended
	askAnswered        // you answered them, and the role is still to get the answers
)

// roleAsks is what role r's questions file says of its run that ended in
// attempt a of stage st, whose end fact did not hold for end (nil: it
// held): nothing to act on (no question, a role with no questions file (a
// qa stage's walker), a gate's turn, whose questions the gate asks, a role
// whose end fact held and whose questions a later
// no_open_questions gate asks, a questions file its end fact refuses, which
// the role's nudge is about, or questions whose answers the role got
// already); questions that wait for your answers (those not asked yet are
// asked now, with every other question the task's files hold, and the
// stage is suspended); or questions you answered whose answers the role is
// still to get (a restart came between: answersDue). Your answers go out
// once per batch, so a later turn ends on its end fact alone.
func (e *Engine) roleAsks(ctx context.Context, t registry.Task, st config.FactoryStage, a registry.Attempt, run *agentsRun, r *stageRole,
	dir string, end *endProblem) (int, error) {
	name := questionsFileOf(e.cfg, st, r.role.Name)
	if name == "" || run.turn.gate || (end == nil && e.laterQuestionsGate(t, st)) || (end != nil && end.file == name) {
		return askNone, nil
	}
	qs, p := readQuestions(dir, name, run.since(a))
	if p != nil || len(qs) == 0 {
		return askNone, nil
	}
	have, err := e.reg.Questions(ctx, t.ID)
	if err != nil {
		return askNone, err
	}
	states := askedStates(have)
	unasked, open := 0, 0
	for _, q := range qs {
		switch states[QuestionKey(q.Text)] {
		case "":
			unasked++
		case registry.QuestionOpen:
			open++
		}
	}
	if unasked > 0 || open > 0 {
		if unasked > 0 {
			if _, err := e.askQuestions(ctx, t, false); err != nil {
				return askNone, err
			}
		}
		return askWaits, e.suspend(ctx, t, a, r.role.Name, e.openIDs(ctx, t.ID))
	}
	switch due, err := e.answersDue(ctx, t, a, r, qs, have); {
	case err != nil:
		return askNone, err
	case due:
		return askAnswered, nil
	}
	return askNone, nil
}

// answersDue reports whether role r is still to get your answers to its
// questions qs (all answered; have are the task's questions): no prompt of
// your answers (answersLead) went to it in attempt a since the last of them
// was answered. The ledger and the registry both keep their times, so a
// restart tells it as well.
func (e *Engine) answersDue(ctx context.Context, t registry.Task, a registry.Attempt, r *stageRole, qs []QuestionFile,
	have []registry.Question) (bool, error) {
	mine := map[string]bool{}
	for _, q := range qs {
		mine[QuestionKey(q.Text)] = true
	}
	var answered time.Time
	for _, q := range have {
		if q.State == registry.QuestionAnswered && mine[QuestionKey(q.Text)] && q.Answered.After(answered) {
			answered = q.Answered
		}
	}
	entries, err := e.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return false, err
	}
	for _, en := range entries {
		if en.AttemptID == a.ID && en.Role == r.runAs.Name && en.Delivered != registry.DeliveredNo &&
			strings.HasPrefix(en.Text, answersLead) && !en.Sent.Before(answered) {
			return false, nil
		}
	}
	return true, nil
}

// openIDs are the ids of the open questions of the task's newest revision,
// as a suspension keeps them.
func (e *Engine) openIDs(ctx context.Context, taskID int64) []string {
	open, err := e.openQuestions(ctx, taskID)
	if err != nil {
		e.log.Warn("read the open questions", "task", taskID, "err", err)
	}
	var ids []string
	for _, q := range open {
		ids = append(ids, fmt.Sprint(q.ID))
	}
	return ids
}

// laterQuestionsGate reports whether a no_open_questions gate comes after
// stage st in task t's kind: it asks the questions of a role that passed.
func (e *Engine) laterQuestionsGate(t registry.Task, st config.FactoryStage) bool {
	kind := e.cfg.Factory.Kinds[t.Kind]
	i := stageIndex(kind, st.Name)
	return i >= 0 && slices.ContainsFunc(kind.Stages[i+1:], func(s config.FactoryStage) bool {
		return s.Type == config.FactoryStageGate && s.Gate == config.FactoryGateNoOpenQuestions
	})
}

// suspend records that role's questions suspended attempt a of task t's
// stage, with the open questions (open, their ids): one suspension of a
// task is open at a time, and a second role's questions join it.
func (e *Engine) suspend(ctx context.Context, t registry.Task, a registry.Attempt, role string, open []string) error {
	err := e.reg.Suspend(ctx, registry.Suspension{TaskID: t.ID, Stage: a.Stage, AttemptID: a.ID, Role: role, Open: open})
	switch {
	case errors.Is(err, registry.ErrConflict):
		return nil // suspended already: these questions joined its batch
	case err != nil:
		return err
	}
	e.event(ctx, t.ID, "suspend", registry.PhaseInfo, fmt.Sprintf("%s suspended in %s: %s asked %s", a.Stage, a.ID, role,
		plural(len(open), "open question")))
	return nil
}

// suspended reports whether a role of run waits for your answers: while a
// question of the task's newest revision is open the task waits for them
// (waitForAnswers) and no role is prompted; once none is, each such role is
// due your answers as its next prompt (the prompt resumes the stage), and a
// task still waiting (its wait on the person you asked was replaced while
// your answer came) is back at work, on *t too.
func (e *Engine) suspended(ctx context.Context, t *registry.Task, run *agentsRun) (bool, error) {
	if !slices.ContainsFunc(run.roles, func(r *stageRole) bool { return r.asking }) {
		return false, nil
	}
	open, err := e.openQuestions(ctx, t.ID)
	if err != nil {
		return true, err
	}
	if len(open) > 0 {
		return true, e.waitForAnswers(ctx, *t, open)
	}
	if t.State == registry.TaskWaiting {
		if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskWaiting}, registry.TaskActive, func(u *registry.TaskUpdate) {
			u.Because("your answers")
		}); err != nil {
			return true, err
		}
		t.State = registry.TaskActive
	}
	for _, r := range run.roles {
		if r.asking {
			r.asking, r.next = false, nextAnswers
		}
	}
	return false, nil
}

// resume closes task t's open suspension: a suspended role got your
// answers. A task with none (another role's answers closed it) is fine.
func (e *Engine) resume(ctx context.Context, t registry.Task) error {
	s, err := e.reg.OpenSuspension(ctx, t.ID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := e.reg.Resume(ctx, t.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	e.event(ctx, t.ID, "resume", registry.PhaseInfo, fmt.Sprintf("%s resumed in %s: %s got your answers", s.Stage, s.AttemptID, s.Role))
	return nil
}
