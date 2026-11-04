package factory

// The ready card (spec 6.6): once the local review ended clean (or on an
// oscillation) at the task's head and the judge drafted the PR body, the
// ready stage draws the card you decide on. It draws one only when the
// review's summary is of that head, the factory's proof holds there and
// the precheck passed there (Review Focus 1: never after a review that did
// not finish or stopped at its cap). Before it draws one, it merges a fresh
// origin/<base> into the head in memory (git merge-tree, Flags.Conflicts):
// on a conflict the task goes back through its own stages, no second
// path: the writer merges the base in a coding turn (rework.json names
// origin/<base> and the paths), the proof and the precheck run again, the
// new head gets a fresh local loop, the judge drafts the body again, and
// the next card is a new revision. The card (ready-card.json) holds what
// your decision binds to: the head, the hash of report.md and pr-body.md as
// shown, and the hash of the diff. Your decision (decide) is refused when
// any of them changed since ("the branch changed after you read it"); a
// push moves the task on to its publish, a send back returns it to coding
// with your reason's key and your note in a file of the task's directory
// (never in a prompt), and a drop stops it, freeing its copy and keeping
// its branch.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// The files of the ready card in the task's directory: the card as the
// factory drew it (ready-card.json) and the coding turn a card sent the
// task back for (rework.json, kept as rework-<k>.json once the coding stage
// passed); your note of a send back is send-back-<revision>.md.
const (
	readyCardFile = "ready-card.json"
	reworkFile    = "rework.json"
)

// sendBackFile is the file of your note on card revision rev.
func sendBackFile(rev int) string { return fmt.Sprintf("send-back-%d.md", rev) }

// readyWait starts what a task at its ready card waits on you for.
const readyWait = "ready to push"

// The choices of the ready card.
const (
	ChoicePush     = "push"
	ChoiceSendBack = "send_back"
	ChoiceDrop     = "drop"
)

// reasonKeyRe is the key of a send back's reason: a word of lowercase
// letters, digits, - and _ (the screen's reason keys), never your words.
var reasonKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// DecidePayload is a ReqDecide: your decision on task TaskID's ready card.
// Choice is push, send_back or drop; a send back takes the key of its
// Reason and a Note to the writer. HeadSHA, ReportHash and DiffHash are
// what the card showed (ReadyCardFile), Shown when it was shown,
// DiffOpened when you opened the diff (zero: you did not) and DiffDwell how
// long you read it. Surface is where you decided (the screen, the CLI).
type DecidePayload struct {
	TaskID                                              int64
	Choice, Reason, Note, HeadSHA, ReportHash, DiffHash string
	DiffOpened, Shown                                   time.Time
	DiffDwell                                           time.Duration
	Surface                                             string
}

// ReadyCardFile is ready-card.json, the ready card as the factory drew it:
// its revision (a new one each time what it shows changed) and when it was
// drawn, the head and the base of the change (the PR's diff is
// base...head), the hash of report.md and pr-body.md as shown and the hash
// of the diff (the patch id of base...head), the branch, the PR's title,
// the copy a push frees (its directory's name) and its path, the FLAGS
// lines (Flags.Lines) and the local review's end.
type ReadyCardFile struct {
	Revision   int           `json:"revision"`
	Drawn      time.Time     `json:"drawn"`
	Head       string        `json:"head"`
	Base       string        `json:"base"`
	ReportHash string        `json:"report_hash"`
	DiffHash   string        `json:"diff_hash"`
	Branch     string        `json:"branch"`
	Remote     string        `json:"remote"`
	Title      string        `json:"title"`
	Copy       string        `json:"copy"`
	CopyPath   string        `json:"copy_path"`
	Flags      []string      `json:"flags"`
	Review     ReviewSummary `json:"review"`
}

// ReadReadyCard reads the ready card of task id in layout l.
func ReadReadyCard(l paths.Layout, id int64) (ReadyCardFile, error) {
	return readCard(l.FactoryTaskDir(id))
}

func readCard(dir string) (ReadyCardFile, error) {
	var c ReadyCardFile
	b, err := os.ReadFile(filepath.Join(dir, readyCardFile))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", readyCardFile, err)
	}
	return c, nil
}

// sameCard reports whether a and b show the same: everything but the
// revision and the time drawn.
func sameCard(a, b ReadyCardFile) bool {
	a.Revision, a.Drawn, b.Revision, b.Drawn = 0, time.Time{}, 0, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// The kinds of a coding turn back from the card (reworkRecord.Kind): your
// send back, or a merge of the base after a conflict.
const (
	reworkSendBack  = "send_back"
	reworkMergeBase = "merge_base"
)

// reworkRecord is rework.json: why the task is back at coding from its
// card revision Card at Head: your send back (the key of its Reason and
// the file of your Note, in the task's directory), or a merge of Base
// (origin/<base>) that conflicts in Conflicts. The coding stage's first
// prompt names it (PromptData), and its pass sets it aside.
type reworkRecord struct {
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason,omitempty"`
	Note      string    `json:"note,omitempty"`
	Base      string    `json:"base,omitempty"`
	BaseSHA   string    `json:"base_sha,omitempty"`
	Conflicts []string  `json:"conflicts,omitempty"`
	Card      int       `json:"card"`
	Head      string    `json:"head"`
	At        time.Time `json:"at"`
}

// readRework reads rework.json in dir; ok is false when there is none.
func readRework(dir string) (reworkRecord, bool) {
	var rw reworkRecord
	b, err := os.ReadFile(filepath.Join(dir, reworkFile))
	if err != nil || json.Unmarshal(b, &rw) != nil {
		return rw, false
	}
	return rw, true
}

// consumeRework sets task id's rework.json aside (rework-<k>.json) once
// the coding turn it named passed; a failure is logged: the next coding
// attempt would name it again.
func (e *Engine) consumeRework(id int64) {
	dir := e.d.Layout.FactoryTaskDir(id)
	from := filepath.Join(dir, reworkFile)
	if !fsx.Exists(from) {
		return
	}
	for k := 1; ; k++ {
		to := filepath.Join(dir, fmt.Sprintf("rework-%d.json", k))
		if fsx.Exists(to) {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			e.log.Warn("set rework.json aside", "task", id, "err", err)
		}
		return
	}
}

// sameConflict is the turn back from a card (rework.json, or one set aside
// as rework-<k>.json) that was for the same conflicts with the base at
// baseSHA, if there is one.
func sameConflict(dir, baseSHA string, conflicts []string) (reworkRecord, bool) {
	names, _ := filepath.Glob(filepath.Join(dir, "rework*.json"))
	for _, name := range names {
		var rw reworkRecord
		if b, err := os.ReadFile(name); err != nil || json.Unmarshal(b, &rw) != nil {
			continue
		}
		if rw.Kind == reworkMergeBase && rw.BaseSHA == baseSHA && slices.Equal(rw.Conflicts, conflicts) {
			return rw, true
		}
	}
	return reworkRecord{}, false
}

// mergeWhy starts the why of a merge turn that did not merge the base: the
// fix turn's prompt carries it, so the ledger tells that turn from others.
const mergeWhy = "the base is not merged"

// mergeProblem is the coding stage's end past its checks for a turn back
// from the card to merge the base (rework.json): the base's commit the card
// saw must be in head, the branch's new head; nil when it is, or when the
// turn is for no merge. Not merged, it is a change problem (a fix turn)
// naming the base.
func (e *Engine) mergeProblem(ctx context.Context, t registry.Task, c Copy, head string) (*endProblem, error) {
	rw, ok := readRework(e.d.Layout.FactoryTaskDir(t.ID))
	if !ok || rw.Kind != reworkMergeBase || rw.BaseSHA == "" || head == "" {
		return nil, nil
	}
	merged, err := e.d.Git.IsAncestor(ctx, c.Path, rw.BaseSHA, head)
	if err != nil || merged {
		return nil, err
	}
	return &endProblem{file: "the branch", change: true, merge: rw.Base, why: fmt.Sprintf("%s: %s at %s is not in %s; merge it "+
		"(`git merge %s`, never a rebase), resolve the conflicts so that both changes hold, and commit the merge", mergeWhy, rw.Base,
		short(rw.BaseSHA), t.Branch, rw.Base)}, nil
}

// prTemplates are where a repository keeps its PR template, in GitHub's
// order.
var prTemplates = []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md", "pull_request_template.md",
	"PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md", "docs/PULL_REQUEST_TEMPLATE.md"}

// prTemplate is the path of the repository's PR template in the copy at
// copyPath, "" when it has none.
func prTemplate(copyPath string) string {
	if copyPath == "" {
		return ""
	}
	for _, rel := range prTemplates {
		p := filepath.Join(copyPath, rel)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// prTitle is the PR's title: the Jira key and the task's title, the title
// alone for a task of a prompt (a title that starts with its key keeps it
// once).
func prTitle(t registry.Task) string {
	title := strings.Join(strings.Fields(t.Title), " ")
	if t.JiraKey != "" && !strings.HasPrefix(title, t.JiraKey) {
		title = strings.TrimSpace(t.JiraKey + " " + title)
	}
	return title
}

// ConfirmSentence is the y/N question of the ready card's push: the branch
// it pushes (the task's remote branch, else its own), the copy it frees and
// the queued task that then starts (each left out when "").
func ConfirmSentence(t registry.Task, frees string, starts string) string {
	s := "Push " + cmp.Or(t.RemoteBranch, t.Branch) + " and open the PR?"
	if frees != "" {
		s += " This frees " + frees
		if starts != "" {
			s += ", and " + starts + " starts"
		}
		s += "."
	}
	return s + " y/N"
}

// NextQueued is the key of the queued task that starts when a copy frees:
// the oldest queued task, when it waits for a copy ("" when none waits,
// or the oldest waits on something a copy does not free, such as memory:
// no queued task jumps another).
func NextQueued(tasks []registry.Task) string {
	queued := slices.DeleteFunc(slices.Clone(tasks), func(t registry.Task) bool { return t.State != registry.TaskQueued })
	slices.SortFunc(queued, func(a, b registry.Task) int { return cmp.Compare(a.ID, b.ID) })
	if len(queued) == 0 || queued[0].WaitsWho != waitCopy {
		return ""
	}
	return TaskKey(queued[0])
}

// runDecision runs one pass of a decision stage (the engine's runner for
// config.FactoryStageDecision).
func (e *Engine) runDecision(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if st.Decision == config.FactoryDecisionReadyPush {
		return e.runReady(ctx, t, st)
	}
	return false, nil
}

// cardState is what a card shows of task's change now: its head and base,
// the hash of report.md and pr-body.md and the hash of the diff.
type cardState struct {
	head, base, reportHash, diffHash string
}

// runReady runs one pass of the ready stage of task t: your push decision
// bound to the change as it is passes the stage (the publish comes next);
// else, the card's conditions holding (readyProblem, your gh identity, the
// branch at the task's head), a conflict with a fresh origin/<base> sends
// the task back to merge it, and otherwise the card is drawn (a new
// revision when what it shows changed) and the task waits on you.
func (e *Engine) runReady(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
	if e.d.Git == nil {
		return false, nil
	}
	sum, why := e.readyProblem(t)
	if why != "" {
		return false, e.stageFailed(ctx, t, needsYou{"no ready card: " + why})
	}
	if _, err := e.prLogin(t); err != nil {
		return false, e.stageFailed(ctx, t, err)
	}
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return false, err
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return false, err
	case bh.Problem != "":
		return false, e.stageFailed(ctx, t, needsYou{"no ready card: " + bh.Problem})
	case bh.Head != t.HeadSHA:
		return false, e.stageFailed(ctx, t, needsYou{fmt.Sprintf("the branch moved to %s since the factory's checks passed at %s: "+
			"no ready card for a head they did not check", short(bh.Head), short(t.HeadSHA))})
	}
	now, err := e.cardNow(ctx, t, c, bh)
	if err != nil {
		return false, err
	}
	if ok, err := e.pushApproved(ctx, t, now); err != nil || ok {
		return ok, err
	}
	if err := e.nameRemoteBranch(ctx, &t); err != nil {
		return false, e.stageFailed(ctx, t, err)
	}
	f, err := e.flags(ctx, t, c)
	if err != nil {
		return false, err
	}
	card, err := e.drawCard(ctx, t, c, now, f, sum)
	if err != nil {
		return false, err
	}
	if len(f.Conflicts) > 0 { // the card names them, and the writer merges the base before you read one
		base := "origin/" + baseOf(repo)
		baseSHA, err := e.d.Git.RevParse(ctx, c.Path, base)
		if err != nil {
			return false, err
		}
		return false, e.stageFailed(ctx, t, e.mergeTheBase(ctx, t, card, base, baseSHA, f.Conflicts))
	}
	e.toOperator(ctx, t, fmt.Sprintf("%s (card %d): %s", readyWait, card.Revision, card.Title), "the ready card")
	return false, nil
}

// readyProblem is why task t has no ready card ("" when it may have one)
// and the local review's end: the review ended clean or on an oscillation
// (review-summary.json), at the task's head, where the factory's proof
// holds and the precheck passed, and the report and the PR body are there.
// A review that did not finish or stopped at its cap never has a card.
func (e *Engine) readyProblem(t registry.Task) (ReviewSummary, string) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	var sum ReviewSummary
	b, err := os.ReadFile(filepath.Join(dir, reviewSummaryFile))
	if err != nil {
		return sum, reviewSummaryFile + " is missing: the local review did not end clean"
	}
	if err := json.Unmarshal(b, &sum); err != nil {
		return sum, reviewSummaryFile + " is not valid JSON: " + err.Error()
	}
	switch sum.Exit {
	case registry.ExitClean, registry.ExitOscillation:
	case registry.ExitUnavailable:
		return sum, "review unavailable: the local review did not finish"
	case registry.ExitCap:
		return sum, "the local review stopped at its cap with findings that block"
	default:
		return sum, fmt.Sprintf("the local review ended %q, neither clean nor on an oscillation", sum.Exit)
	}
	switch {
	case t.HeadSHA == "":
		return sum, "the task has no head the factory's checks passed at"
	case sum.Head != t.HeadSHA:
		return sum, fmt.Sprintf("the local review reviewed %s, another head than %s", short(sum.Head), short(t.HeadSHA))
	}
	var p Proof
	if b, err := os.ReadFile(filepath.Join(dir, factoryProofFile)); err != nil || json.Unmarshal(b, &p) != nil || !p.Holds(t.HeadSHA) {
		return sum, "the factory's proof did not pass at " + short(t.HeadSHA)
	}
	if !precheckPassedAt(dir, t.HeadSHA) {
		return sum, "the precheck did not pass at " + short(t.HeadSHA)
	}
	for _, name := range []string{reportFile, prBodyFile} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() == 0 {
			return sum, name + " is missing"
		}
	}
	return sum, ""
}

// prLogin is your GitHub login that opens the PR: the login of the gh
// identity the local review runs as (evalrun.LocalIdentity). A config
// without one is a needsYou naming the key.
func (e *Engine) prLogin(t registry.Task) (string, error) {
	var w config.Watch
	if p := e.cfg.WatchFor(t.Repo); p != nil {
		w = *p
	}
	name, err := evalrun.LocalIdentity(e.cfg, &w)
	if err != nil {
		key := `[[identity]] kind = "gh"`
		if ce, ok := errors.AsType[*evalrun.ConfigError](err); ok {
			key = ce.Key
		}
		return "", needsYou{"the PR opens as your gh identity, the one the local review runs as, and the config has none: set " + key}
	}
	id := e.cfg.IdentityByName(name)
	if id == nil || id.Login == "" {
		return "", needsYou{"the PR opens as your gh identity " + name + ", which names no login: set its login in [[identity]]"}
	}
	return id.Login, nil
}

// nameRemoteBranch names and records the branch task t's PR is pushed to
// (Task.RemoteBranch; *t has it): its own branch when that holds its Jira
// key or it has none, else <login>-<KEY>-<slug of its title> as intake
// names a key's branch (branchFor: no other task's and no branch of the
// main clone), for a task whose key was linked after its branch was made;
// the local branch keeps its name. A recorded name stays, unless it lacks
// the key the task got since (the ready card names it again before
// anything is pushed).
func (e *Engine) nameRemoteBranch(ctx context.Context, t *registry.Task) error {
	if t.RemoteBranch != "" && (t.JiraKey == "" || holdsKey(t.RemoteBranch, t.JiraKey)) {
		return nil
	}
	name := t.Branch
	if t.JiraKey != "" && !holdsKey(t.Branch, t.JiraKey) {
		login, err := e.operatorLogin(t.Repo)
		if err != nil {
			return needsYou{err.Error()}
		}
		if name, err = e.branchFor(ctx, t.ID, t.Repo, login, t.JiraKey, t.Title); err != nil {
			return err
		}
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) { u.Set("remote_branch", name) }); err != nil {
		return err
	}
	if name != t.Branch {
		e.event(ctx, t.ID, "ready", registry.PhaseInfo, fmt.Sprintf("the PR's branch is %s: %s was linked after %s was made, which keeps its name",
			name, t.JiraKey, t.Branch))
	}
	t.RemoteBranch = name
	return nil
}

// holdsKey reports whether branch holds Jira key key as a whole
// dash-delimited segment (PR-1236 is not in a branch named for PR-12360).
func holdsKey(branch, key string) bool {
	return key != "" && (branch == key || strings.HasPrefix(branch, key+"-") || strings.HasSuffix(branch, "-"+key) ||
		strings.Contains(branch, "-"+key+"-"))
}

// cardNow is what task t's card shows of its change in copy c now (bh).
func (e *Engine) cardNow(ctx context.Context, t registry.Task, c Copy, bh branchHead) (cardState, error) {
	rh, err := reportHash(e.d.Layout.FactoryTaskDir(t.ID))
	if err != nil {
		return cardState{}, err
	}
	dh, err := e.d.Git.OwnDiffID(ctx, c.Path, bh.Base, bh.Head)
	if err != nil {
		return cardState{}, err
	}
	return cardState{head: bh.Head, base: bh.Base, reportHash: rh, diffHash: dh}, nil
}

// reportHash is the hash of report.md and pr-body.md in dir, as the card
// shows them: what the PR's description is drawn from, and the
// description itself.
func reportHash(dir string) (string, error) {
	h := sha256.New()
	for _, name := range []string{reportFile, prBodyFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", name, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lastDecision is task id's newest decision on its ready card.
func (e *Engine) lastDecision(ctx context.Context, id int64) (registry.Decision, bool, error) {
	ds, err := e.reg.Decisions(ctx, id)
	if err != nil {
		return registry.Decision{}, false, err
	}
	for _, d := range slices.Backward(ds) {
		if d.Kind == config.FactoryDecisionReadyPush {
			return d, true, nil
		}
	}
	return registry.Decision{}, false, nil
}

// pushApproved reports whether your newest decision on task t's card is a
// push bound to the change as it is now.
func (e *Engine) pushApproved(ctx context.Context, t registry.Task, now cardState) (bool, error) {
	d, ok, err := e.lastDecision(ctx, t.ID)
	if err != nil || !ok || d.Choice != ChoicePush {
		return false, err
	}
	return d.HeadSHA == now.head && d.ReportHash == now.reportHash && d.DiffHash == now.diffHash, nil
}

// drawCard writes task t's card of its change now in copy c, with flags f
// and the review's end sum: the card as it was when nothing it shows
// changed, else a new revision, with an event.
func (e *Engine) drawCard(ctx context.Context, t registry.Task, c Copy, now cardState, f Flags, sum ReviewSummary) (ReadyCardFile, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	card := ReadyCardFile{Head: now.head, Base: now.base, ReportHash: now.reportHash, DiffHash: now.diffHash, Branch: t.Branch,
		Remote: cmp.Or(t.RemoteBranch, t.Branch), Title: prTitle(t), Copy: filepath.Base(c.Path), CopyPath: c.Path, Flags: f.Lines(), Review: sum}
	old, err := readCard(dir)
	if err == nil && sameCard(old, card) {
		return old, nil
	}
	card.Revision, card.Drawn = old.Revision+1, e.now()
	if err := writeJSON(filepath.Join(dir, readyCardFile), card); err != nil {
		return card, err
	}
	flags := "nothing flagged"
	if len(card.Flags) > 0 {
		flags = plural(len(card.Flags), "flag") + ": " + textx.Clip(strings.Join(card.Flags, "; "), 300)
	}
	e.event(ctx, t.ID, "ready", registry.PhaseInfo, fmt.Sprintf("card %d at %s: %s", card.Revision, short(card.Head), flags))
	return card, nil
}

// mergeTheBase sends task t back from its card to its coding stage to merge
// base (the fresh origin/<base>, at baseSHA), which conflicts with its head
// in conflicts; the coding stage passes only once baseSHA is in the head
// (mergeProblem). The same conflict at the same base a second time (a merge
// turn the card sent the task back for already, whose merge did not take)
// is a needsYou, never another round.
func (e *Engine) mergeTheBase(ctx context.Context, t registry.Task, card ReadyCardFile, base, baseSHA string, conflicts []string) error {
	if prev, ok := sameConflict(e.d.Layout.FactoryTaskDir(t.ID), baseSHA, conflicts); ok {
		return needsYou{fmt.Sprintf("the same conflict with %s at %s in %s, which card %d sent the writer back to merge: "+
			"merge it in the copy, then let the task go on", base, short(baseSHA), textx.Clip(strings.Join(conflicts, ", "), 200), prev.Card)}
	}
	rw := reworkRecord{Kind: reworkMergeBase, Base: base, BaseSHA: baseSHA, Conflicts: conflicts, Card: card.Revision, Head: t.HeadSHA,
		At: e.now()}
	return e.backToCoding(ctx, t, rw, liveStates, fmt.Sprintf("%s conflicts with %s in %s: the writer merges it", base, short(t.HeadSHA),
		textx.Clip(strings.Join(conflicts, ", "), 200)))
}

// writerStage is the first agents stage of task t's kind that ends on the
// proof: the coding stage.
func (e *Engine) writerStage(t registry.Task) (config.FactoryStage, bool) {
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageAgents && st.End == config.FactoryEndProof {
			return st, true
		}
	}
	return config.FactoryStage{}, false
}

// backToCoding writes rw and moves task t (in one of from) to its coding
// stage, at work and counting a new attempt, because.
func (e *Engine) backToCoding(ctx context.Context, t registry.Task, rw reworkRecord, from []string, because string) error {
	st, ok := e.writerStage(t)
	if !ok {
		return e.stageFailed(ctx, t, needsYou{"no stage of the " + t.Kind + " kind writes the change: nothing can take it back from the card"})
	}
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reworkFile), rw); err != nil {
		return err
	}
	if err := e.reg.TransitionTask(ctx, t.ID, from, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", st.Name)
		u.Set("attempt", 0)
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because(because)
	}); err != nil {
		return err
	}
	e.event(ctx, t.ID, "ready", registry.PhaseInfo, "back to "+st.Name+": "+because)
	return nil
}

// decideRequest answers a ReqDecide: decide, then what it did.
func (e *Engine) decideRequest(ctx context.Context, p DecidePayload) (string, error) {
	if err := e.decide(ctx, p); err != nil {
		return "", err
	}
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	switch p.Choice {
	case ChoicePush:
		return fmt.Sprintf("%s: pushes %s at %s and opens its PR, ready for review", TaskKey(t), cmp.Or(t.RemoteBranch, t.Branch),
			short(p.HeadSHA)), nil
	case ChoiceSendBack:
		return fmt.Sprintf("%s: sent back to %s (%s): the writer reads your note", TaskKey(t), t.Stage, p.Reason), nil
	}
	return fmt.Sprintf("%s: dropped, its copy freed and its branch %s kept", TaskKey(t), t.Branch), nil
}

// decide takes your decision p on a task's ready card. A push or a send
// back is refused for a task not waiting at its card, and when what the
// card showed is not what is there now ("the branch changed after you read
// it": the head, the branch in the copy, report.md and pr-body.md, the
// diff): the card is drawn again then; a push is refused, too, when the
// card's conditions no longer hold (readyProblem). A drop is bound to no
// card: it takes a task at its ready stage whatever its card says, or
// when none could be drawn. A decision taken is recorded with what the
// card showed, and your input with it; then a push moves the task on to
// its publish, a send back writes your note to send-back-<revision>.md and
// returns the task to its coding stage (rework.json names the note and the
// reason's key), and a drop stops the task, freeing its copy and keeping
// its branch.
func (e *Engine) decide(ctx context.Context, p DecidePayload) error {
	switch p.Choice {
	case ChoicePush, ChoiceDrop:
	case ChoiceSendBack:
		if !reasonKeyRe.MatchString(p.Reason) {
			return fmt.Errorf("a send back takes the key of its reason (lowercase letters, digits, - and _), not %q", p.Reason)
		}
		if strings.TrimSpace(p.Note) == "" {
			return errors.New("a send back takes a note to the writer")
		}
	default:
		return fmt.Errorf("unknown choice %q: push, send_back or drop", p.Choice)
	}
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return err
	}
	st, ok := e.stageOf(t, t.Stage)
	if !ok || st.Type != config.FactoryStageDecision || st.Decision != config.FactoryDecisionReadyPush ||
		(!isLive(t) && t.State != registry.TaskNeedsYou) {
		return fmt.Errorf("%s is %s at %s, not at its ready stage", TaskKey(t), t.State, cmp.Or(t.Stage, "no stage"))
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	card, err := readCard(dir)
	if p.Choice != ChoiceDrop {
		switch {
		case t.State != registry.TaskNeedsYou || !strings.HasPrefix(t.WaitsWhat, readyWait):
			return fmt.Errorf("%s is %s at %s, not at its ready card", TaskKey(t), t.State, t.Stage)
		case err != nil:
			return fmt.Errorf("%s has no ready card to decide on: %w", TaskKey(t), err)
		}
		if why, err := e.cardChanged(ctx, t, card, p); err != nil {
			return err
		} else if why != "" {
			e.redraw(ctx, t, why)
			return fmt.Errorf("the branch changed after you read it: %s; the card is drawn again", why)
		}
	}
	if p.Choice == ChoicePush {
		if _, why := e.readyProblem(t); why != "" {
			e.redraw(ctx, t, why)
			return errors.New("no ready card: " + why)
		}
	}
	proof := ""
	if b, err := os.ReadFile(filepath.Join(dir, factoryProofFile)); err == nil {
		proof = sha256Hex(b)
	}
	d := registry.Decision{TaskID: t.ID, Kind: config.FactoryDecisionReadyPush, Choice: p.Choice, Note: p.Reason, HeadSHA: p.HeadSHA,
		ReportHash: p.ReportHash, DiffHash: p.DiffHash, Shown: p.Shown, DiffOpened: p.DiffOpened, DiffDwell: p.DiffDwell, Proof: proof}
	if err := e.reg.RecordDecision(ctx, d); err != nil {
		return err
	}
	surface := cmp.Or(p.Surface, "cli")
	choice := p.Choice
	if p.Reason != "" {
		choice += ": " + p.Reason
	}
	if _, err := e.recordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputDecision, Surface: surface, Stage: t.Stage,
		Text: choice}); err != nil {
		e.log.Warn("record a decision's input", "task", t.ID, "err", err)
	}
	switch p.Choice {
	case ChoicePush:
		return e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
			u.Set("waits_who", "")
			u.Set("waits_what", "")
			u.Because(fmt.Sprintf("you pushed card %d", card.Revision))
		})
	case ChoiceSendBack:
		return e.sendBack(ctx, t, card, p, surface)
	}
	return e.endTask(ctx, t, "dropped by you at its ready card", registry.Input{TaskID: t.ID, Kind: registry.InputDrop, Surface: surface,
		Stage: t.Stage})
}

// sendBack writes your note on card to send-back-<revision>.md (data the
// writer reads, never a prompt's text), records it as your input
// (scrubbed), and returns task t to its coding stage with the reason's key.
func (e *Engine) sendBack(ctx context.Context, t registry.Task, card ReadyCardFile, p DecidePayload, surface string) error {
	note := sendBackFile(card.Revision)
	text := "# Sent back from the ready card " + fmt.Sprint(card.Revision) + " (" + p.Reason + ")\n\n" + strings.TrimSpace(p.Note) + "\n"
	if err := fsx.WriteFileAtomic(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), note), []byte(text), 0o600); err != nil {
		return err
	}
	in := registry.Input{TaskID: t.ID, Kind: registry.InputSendBack, Surface: surface, Stage: t.Stage}
	if clean, kept := runtime.Scrub(p.Note); kept {
		in.Text = clean
	} else {
		in.DroppedReason = "a long paste: more than 40 lines or 4000 bytes"
	}
	if _, err := e.recordInput(ctx, in); err != nil {
		e.log.Warn("record a send back", "task", t.ID, "err", err)
	}
	rw := reworkRecord{Kind: reworkSendBack, Reason: p.Reason, Note: note, Card: card.Revision, Head: card.Head, At: e.now()}
	return e.backToCoding(ctx, t, rw, []string{registry.TaskNeedsYou}, "you sent card "+fmt.Sprint(card.Revision)+" back: "+p.Reason)
}

// cardChanged is what changed since task t's card showed what p says ("" for
// nothing): the card drawn since, the task's head, the branch in the copy,
// report.md and pr-body.md, the diff.
func (e *Engine) cardChanged(ctx context.Context, t registry.Task, card ReadyCardFile, p DecidePayload) (string, error) {
	switch {
	case p.HeadSHA != card.Head || p.ReportHash != card.ReportHash || p.DiffHash != card.DiffHash:
		return fmt.Sprintf("card %d shows another change than the one you read", card.Revision), nil
	case t.HeadSHA != card.Head:
		return fmt.Sprintf("the task's head is %s, not %s", short(t.HeadSHA), short(card.Head)), nil
	case e.d.Git == nil:
		return "", errors.New("this factory daemon has no git to check the branch with")
	}
	head, err := e.d.Git.RevParse(ctx, card.CopyPath, "refs/heads/"+card.Branch)
	if err != nil {
		return "", err
	}
	if head != card.Head {
		return fmt.Sprintf("%s is at %s, not at %s", card.Branch, short(head), short(card.Head)), nil
	}
	rh, err := reportHash(e.d.Layout.FactoryTaskDir(t.ID))
	if err != nil {
		return "", err
	}
	if rh != card.ReportHash {
		return reportFile + " or " + prBodyFile + " changed", nil
	}
	dh, err := e.d.Git.OwnDiffID(ctx, card.CopyPath, card.Base, card.Head)
	if err != nil {
		return "", err
	}
	if dh != card.DiffHash {
		return "the diff changed", nil
	}
	return "", nil
}

// redraw sends task t back to its ready stage's work, because what its card
// showed changed (why): the next pass draws the card again, or says what
// keeps it from one.
func (e *Engine) redraw(ctx context.Context, t registry.Task, why string) {
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because("the card changed after you read it: " + why)
	}); err != nil {
		e.log.Warn("draw a card again", "task", t.ID, "err", err)
	}
}

// knownMarker starts the "Known" list the factory appends to pr-body.md:
// what comes after it is the factory's, written again on each append.
const knownMarker = "<!-- magnum factory: the Known list -->"

// appendKnown appends to task t's pr-body.md the "Known" list of what the
// local review left open (knownList, from review-summary.json), in place of
// the one an earlier append wrote, and records the body as the pr_body
// artifact.
func (e *Engine) appendKnown(ctx context.Context, t registry.Task) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	b, err := os.ReadFile(filepath.Join(dir, prBodyFile))
	if err != nil {
		return err
	}
	body, _, _ := strings.Cut(string(b), knownMarker)
	var sum ReviewSummary
	if b, err := os.ReadFile(filepath.Join(dir, reviewSummaryFile)); err == nil {
		_ = json.Unmarshal(b, &sum) // none: the list says the review left nothing open
	}
	text := strings.TrimRight(body, "\n") + "\n\n" + knownMarker + "\n" + knownList(sum)
	return e.saveArtifact(ctx, t, "pr_body", prBodyFile, []byte(text))
}

// knownList is the PR body's "Known" section: what magnum's review of the
// change before the PR left open, none of it blocking: the leftovers, the
// findings the writer declined with the risk of the suggested change, the
// doubts (findings that came back twice after their fix), and the
// reviewers a round went without.
func knownList(sum ReviewSummary) string {
	var b strings.Builder
	b.WriteString("## Known\n\n")
	if len(sum.Leftovers)+len(sum.Declined)+len(sum.Doubts)+len(sum.Missing) == 0 {
		b.WriteString("magnum's review of this change before the PR left nothing open.\n")
		return b.String()
	}
	b.WriteString("What magnum's review of this change before the PR left open; none of it blocks.\n\n")
	for _, i := range sum.Leftovers {
		fmt.Fprintf(&b, "- %s\n", knownItem(i))
	}
	for _, i := range sum.Declined {
		fmt.Fprintf(&b, "- declined: %s. The risk of the suggested change: %s\n", knownItem(i), oneLine(i.Risk))
	}
	for _, i := range sum.Doubts {
		fmt.Fprintf(&b, "- a doubt: %s (it came back twice after its fix)\n", knownItem(i))
	}
	for _, role := range sum.Missing {
		fmt.Fprintf(&b, "- a round of the review went without %s's report\n", role)
	}
	return b.String()
}

// knownItem is a finding in a line: its priority, its place and its title.
func knownItem(i ReviewItem) string {
	var parts []string
	if i.Severity != "" {
		parts = append(parts, i.Severity)
	}
	if place := i.Path; place != "" {
		if i.Line > 0 {
			place += ":" + strconv.Itoa(i.Line)
		}
		parts = append(parts, "`"+place+"`:")
	}
	return strings.Join(append(parts, oneLine(i.Title)), " ")
}

// oneLine is s with its lines and spaces joined by single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
