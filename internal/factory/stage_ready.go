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
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
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

// readyWait starts what a task at its ready card waits on you for (the
// stats find the card's wait by it).
const readyWait = registry.ReadyWait

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
// base...head), the branch's commits after origin/<base> (0 in a card an
// older build drew; the PR's status file counts them), the hash of
// report.md and pr-body.md as shown and the hash of the diff (the patch id
// of base...head), the branch, the PR's title, the copy a push frees (its
// directory's name) and its path, the FLAGS lines (Flags.Lines, then a line
// for each state QA did not reach), the local review's end and the changed
// files, the flagged ones first (cardFilesOf), and the QA run that passed
// at the head (qa-summary.json, its quotes left out), nil for none.
type ReadyCardFile struct {
	Revision   int           `json:"revision"`
	Drawn      time.Time     `json:"drawn"`
	Head       string        `json:"head"`
	Base       string        `json:"base"`
	Commits    int           `json:"commits"`
	ReportHash string        `json:"report_hash"`
	DiffHash   string        `json:"diff_hash"`
	Branch     string        `json:"branch"`
	Remote     string        `json:"remote"`
	Title      string        `json:"title"`
	Copy       string        `json:"copy"`
	CopyPath   string        `json:"copy_path"`
	Flags      []string      `json:"flags"`
	Review     ReviewSummary `json:"review"`
	Files      []CardFile    `json:"files"`
	QA         *QAResult     `json:"qa,omitempty"`
}

// CardFile is one file the change of a ready card changes: its path, the
// lines its diff adds and removes (both 0 for a binary file), whether the
// plan named it, and whether a FLAGS line names it (a migration the branch
// adds, a sensitive file, an old guard, a conflict with the base).
type CardFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	InPlan  bool   `json:"in_plan"`
	Flagged bool   `json:"flagged,omitempty"`
}

// cardFiles are the files of the change now in copy dir as the card lists
// them (cardFilesOf), with their lines (git diff --numstat).
func (e *Engine) cardFiles(ctx context.Context, dir string, now cardState, f Flags) ([]CardFile, error) {
	stats, err := e.d.Git.DiffStat(ctx, dir, now.base, now.head)
	if err != nil {
		return nil, err
	}
	return cardFilesOf(stats, f), nil
}

// cardFilesOf are the changed files stats as the card lists them: the
// files a FLAGS line of f names or the plan did not name first, then the
// rest, each part in git's order.
func cardFilesOf(stats []gitx.FileStat, f Flags) []CardFile {
	flagged := map[string]bool{}
	for _, p := range slices.Concat(f.Sensitive, f.Conflicts) {
		flagged[p] = true
	}
	for _, m := range f.Migrations {
		flagged[m.Path] = true
	}
	for _, g := range f.OldGuards {
		flagged[g.Path] = true
	}
	files := make([]CardFile, 0, len(stats))
	for _, s := range stats {
		files = append(files, CardFile{Path: s.Path, Added: s.Added, Removed: s.Removed, InPlan: !slices.Contains(f.NotInPlan, s.Path),
			Flagged: flagged[s.Path]})
	}
	slices.SortStableFunc(files, func(a, b CardFile) int {
		return cmp.Compare(boolRank(!a.Flagged && a.InPlan), boolRank(!b.Flagged && b.InPlan))
	})
	return files
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DiffOrderFile is the order file (git diff -O) of a ready card's diff in
// the task's directory: its files as the card lists them, flagged first, so
// the pager shows them in that order.
const DiffOrderFile = "diff-order.txt"

// diffOrder is the text of DiffOrderFile for files: one path a line, its
// glob characters escaped (git reads each line as a pattern).
func diffOrder(files []CardFile) string {
	var b strings.Builder
	for _, f := range files {
		p := f.Path
		for _, c := range []string{`\`, "*", "?", "["} {
			p = strings.ReplaceAll(p, c, `\`+c)
		}
		b.WriteString(p + "\n")
	}
	return b.String()
}

// DiffOrderPath is task id's DiffOrderFile in layout l.
func DiffOrderPath(l paths.Layout, id int64) string {
	return filepath.Join(l.FactoryTaskDir(id), DiffOrderFile)
}

// DiffArgs are the arguments of git that page the diff of ready card c in
// its copy, as every diff of the factory reads one (no external diff, no
// textconv: the copy's attributes are the change's), its files in the
// order of orderFile ("" for git's own order: git refuses an order file it
// cannot read): `git --paginate -C <copy> diff --no-ext-diff --no-textconv
// [-O<order>] <base>...<head> --`. It reads nothing.
func DiffArgs(c ReadyCardFile, orderFile string) []string {
	args := []string{"--paginate", "-C", c.CopyPath, "diff", "--no-ext-diff", "--no-textconv"}
	if orderFile != "" {
		args = append(args, "-O"+orderFile)
	}
	return append(args, c.Base+"..."+c.Head, "--")
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
// send back, a merge of the base after a conflict, or a branch that moved
// after the coding stage's checks passed (a commit the factory did not
// make: yours, in the copy).
const (
	reworkSendBack  = "send_back"
	reworkMergeBase = "merge_base"
	reworkHeadMoved = "head_moved"
)

// reworkRecord is rework.json: why the task is back at coding from its
// card revision Card at Head: your send back (the key of its Reason and
// the file of your Note, in the task's directory), a merge of Base
// (origin/<base>) that conflicts in Conflicts, or a branch that moved from
// Head, where the checks passed, to Moved. The coding stage's first prompt
// names it (PromptData), and its pass sets it aside.
type reworkRecord struct {
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason,omitempty"`
	Note      string    `json:"note,omitempty"`
	Base      string    `json:"base,omitempty"`
	BaseSHA   string    `json:"base_sha,omitempty"`
	Conflicts []string  `json:"conflicts,omitempty"`
	Moved     string    `json:"moved,omitempty"`
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

// prTitle is the PR's title: the task's title laid out by format (the
// repository's title_format, "" = config.DefaultFactoryTitleFormat), {key}
// the task's issue key (the Jira key, or a Linear one: Task.JiraKey holds
// either) and {title} its title with the white space of its lines folded.
// A task with no key gets its title alone, with no empty brackets, whatever
// the format's other words. The key is kept once, in the format's form:
//   - a title that starts (or, for a format that ends with the key, ends)
//     with the formatted key, in any case, keeps it, written as the format
//     writes it ("[abc-12] Title" is "[ABC-12] Title" in brackets);
//   - one that starts with the key bare or in brackets, in any case, where
//     the key ends there ("ABC-12: Title", "ABC-12 - Title", "(ABC-12) Title"),
//     is laid out from what follows it. A key that a hyphen, a dot or a
//     letter joins to the next word is part of that word ("ABC-12-beta",
//     "ABC-1.5", and ABC-12 for the key ABC-1).
func prTitle(t registry.Task, format string) string {
	title := strings.Join(strings.Fields(t.Title), " ")
	key := t.JiraKey
	if key == "" {
		return title
	}
	format = cmp.Or(format, config.DefaultFactoryTitleFormat)
	pre, post, _ := strings.Cut(format, config.TitleTitle)
	pre, post = strings.TrimSpace(strings.ReplaceAll(pre, config.TitleKey, key)), strings.TrimSpace(strings.ReplaceAll(post, config.TitleKey, key))
	if (pre != "" || post != "") && hasPrefixFold(title, pre) && hasSuffixFold(title, post) && len(title) >= len(pre)+len(post) {
		return pre + title[len(pre):len(title)-len(post)] + post
	}
	if strings.Contains(format, config.TitleKey) {
		title = withoutLeadingKey(title, key)
	}
	return strings.TrimSpace(strings.NewReplacer(config.TitleKey, key, config.TitleTitle, title).Replace(format))
}

// hasPrefixFold and hasSuffixFold are strings.HasPrefix and HasSuffix with
// letters of any case equal.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// withoutLeadingKey is title without key at its start, when the key stands
// there as a word of its own: bare or in one pair of brackets, in any case,
// then the end of the title, or a space, a colon, a comma or a semicolon, or
// a dash and a space. Any other title is returned as it is: a hyphen, a dot
// or a letter after the key joins it to the next word.
func withoutLeadingKey(title, key string) string {
	rest, wrapped := title, false
	if rest != "" && strings.ContainsRune("([{<", rune(rest[0])) {
		rest, wrapped = rest[1:], true
	}
	if !hasPrefixFold(rest, key) {
		return title
	}
	rest = rest[len(key):]
	if wrapped {
		if rest == "" || !strings.ContainsRune(")]}>", rune(rest[0])) {
			return title
		}
		rest = rest[1:]
	}
	if rest != "" && !strings.ContainsRune(" :,;", rune(rest[0])) && !startsWithDash(rest) {
		return title
	}
	rest = strings.TrimLeft(rest, " :,;")
	if startsWithDash(rest) {
		_, n := utf8.DecodeRuneInString(rest)
		rest = strings.TrimLeft(rest[n:], " ")
	}
	return rest
}

// startsWithDash reports whether s starts with a hyphen or a dash that
// stands alone: followed by a space, or by nothing.
func startsWithDash(s string) bool {
	r, n := utf8.DecodeRuneInString(s)
	return strings.ContainsRune("-\u2013\u2014", r) && r != utf8.RuneError && (n == len(s) || s[n] == ' ')
}

// titleFormat is the title_format of the factory repository repo ("" = the
// default).
func (e *Engine) titleFormat(repo string) string {
	r, _ := e.cfg.FactoryRepo(repo)
	return r.TitleFormat
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
// the hash of report.md and pr-body.md, the hash of the diff and the
// branch's commits after origin/<base>.
type cardState struct {
	head, base, reportHash, diffHash string
	commits                          int
}

// runReady runs one pass of the ready stage of task t: your push decision
// bound to the change as it is passes the stage (the publish comes next);
// else, the card's conditions holding (readyProblem, your gh identity, the
// branch at the task's head), a conflict with a fresh origin/<base> sends
// the task back to merge it, and otherwise the card is drawn (a new
// revision when what it shows changed) and the task waits on you. A task
// whose card QA keeps goes back to its qa stage (qaBack).
func (e *Engine) runReady(ctx context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
	if e.d.Git == nil {
		return false, nil
	}
	sum, why := e.readyProblem(t)
	if back, err := e.qaBack(ctx, t, why); err != nil || back {
		return false, err
	}
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
		return false, e.headMoved(ctx, t, bh.Head)
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
// holds and the precheck passed, QA passed when the task has QA
// (qaProblem), and the report and the PR body are there. A review that did
// not finish or stopped at its cap never has a card.
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
	if why := e.qaProblem(t); why != "" {
		return sum, why
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
	return cardState{head: bh.Head, base: bh.Base, reportHash: rh, diffHash: dh, commits: bh.Commits}, nil
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

// drawCard writes task t's card of its change now in copy c, with flags f,
// the review's end sum and the QA run that passed at its head: the card as
// it was when nothing it shows changed, else a new revision, with an event.
func (e *Engine) drawCard(ctx context.Context, t registry.Task, c Copy, now cardState, f Flags, sum ReviewSummary) (ReadyCardFile, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	files, err := e.cardFiles(ctx, c.Path, now, f)
	if err != nil {
		return ReadyCardFile{}, err
	}
	card := ReadyCardFile{Head: now.head, Base: now.base, Commits: now.commits, ReportHash: now.reportHash, DiffHash: now.diffHash, Branch: t.Branch,
		Remote: cmp.Or(t.RemoteBranch, t.Branch), Title: prTitle(t, e.titleFormat(t.Repo)), Copy: filepath.Base(c.Path), CopyPath: c.Path, Flags: f.Lines(), Review: sum,
		Files: files}
	if res, ok := readQASummary(dir); ok && res.Head == now.head {
		card.QA = qaForCard(res)
		card.Flags = append(card.Flags, qaBlockedFlags(res.Blocked)...)
	}
	old, err := readCard(dir)
	if err == nil && sameCard(old, card) {
		return old, nil
	}
	card.Revision, card.Drawn = old.Revision+1, e.now()
	if err := fsx.WriteFileAtomic(filepath.Join(dir, DiffOrderFile), []byte(diffOrder(files)), 0o600); err != nil {
		return card, err
	}
	if err := writeJSON(filepath.Join(dir, readyCardFile), card); err != nil {
		return card, err
	}
	lines := f.Lines() // the factory's own flags; QA's states are counted, never named: the walker's words stay on the card
	if card.QA != nil && len(card.QA.Blocked) > 0 {
		lines = append(lines, "QA did not reach "+plural(len(card.QA.Blocked), "state"))
	}
	flags := "nothing flagged"
	if len(lines) > 0 {
		flags = plural(len(lines), "flag") + ": " + textx.Clip(strings.Join(lines, "; "), 300)
	}
	e.event(ctx, t.ID, "ready", registry.PhaseInfo, fmt.Sprintf("card %d at %s: %s", card.Revision, short(card.Head), flags))
	return card, nil
}

// mergeTheBase sends task t back from its card to its coding stage to merge
// base (the fresh origin/<base>, at baseSHA), which conflicts with its head
// in conflicts; the coding stage passes only once baseSHA is in the head
// (mergeProblem). The same conflict at the same base a second time (a merge
// turn the card sent the task back for already, whose merge did not take)
// is a needsYou, never another round, and so is a base that conflicted
// maxBaseMerges times in a row (each merge costs a fresh local loop).
func (e *Engine) mergeTheBase(ctx context.Context, t registry.Task, card ReadyCardFile, base, baseSHA string, conflicts []string) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if prev, ok := sameConflict(dir, baseSHA, conflicts); ok {
		return needsYou{fmt.Sprintf("the same conflict with %s at %s in %s, which card %d sent the writer back to merge: "+
			"merge it on %s in the copy and commit it, then `magnum factory retry %d`: the factory's checks run again at the new head "+
			"before a new card; or drop the task", base, short(baseSHA), textx.Clip(strings.Join(conflicts, ", "), 200), prev.Card, t.Branch, t.ID)}
	}
	if n := mergesInARow(dir); n >= maxBaseMerges {
		return needsYou{fmt.Sprintf("%s conflicted with %s %d times in a row, now in %s, each merge a fresh local review: merge it on %s "+
			"in the copy and commit it, then `magnum factory retry %d`; or drop the task", base, t.Branch, n, textx.Clip(strings.Join(conflicts, ", "),
			200), t.Branch, t.ID)}
	}
	rw := reworkRecord{Kind: reworkMergeBase, Base: base, BaseSHA: baseSHA, Conflicts: conflicts, Card: card.Revision, Head: t.HeadSHA,
		At: e.now()}
	return e.backToCoding(ctx, t, rw, liveStates, fmt.Sprintf("%s conflicts with %s in %s: the writer merges it", base, short(t.HeadSHA),
		textx.Clip(strings.Join(conflicts, ", "), 200)))
}

// maxBaseMerges is how many merges of the base in a row the card sends the
// writer back for: the next conflict needs you.
const maxBaseMerges = 3

// mergesInARow counts the newest turns back from the card in dir (the ones
// set aside, rework-<k>.json, newest first) that merged the base, up to the
// first of another kind.
func mergesInARow(dir string) int {
	n := 0
	for k := 1; fsx.Exists(filepath.Join(dir, fmt.Sprintf("rework-%d.json", k))); k++ {
		var rw reworkRecord
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("rework-%d.json", k)))
		switch {
		case err != nil, json.Unmarshal(b, &rw) != nil, rw.Kind != reworkMergeBase:
			n = 0
		default:
			n++
		}
	}
	return n
}

// headMoved sends task t, whose branch moved to head after the coding
// stage's checks passed at t.HeadSHA (a commit the factory did not make:
// yours, in the copy), back through those checks at the new head, as a
// send back without a note does: the writer's turn names the move and asks
// for the proof again, then the proof and the precheck run at the new
// head, a fresh local loop reviews it, the judge drafts the body again and
// a new card is drawn. No ready card and no review is ever of a head the
// checks did not pass.
func (e *Engine) headMoved(ctx context.Context, t registry.Task, head string) error {
	rw := reworkRecord{Kind: reworkHeadMoved, Head: t.HeadSHA, Moved: head, At: e.now()}
	if card, err := ReadReadyCard(e.d.Layout, t.ID); err == nil {
		rw.Card = card.Revision
	}
	return e.backToCoding(ctx, t, rw, liveStates, fmt.Sprintf("the branch moved to %s since the factory's checks passed at %s: "+
		"they run again at the new head", short(head), short(t.HeadSHA)))
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
// its branch. A decision taken ends the wait after a failure of the task's
// stage (retryLater), as a retry does: the tick it kicks runs the task.
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
	delete(e.lastFail, t.ID)
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
	in := withWords(registry.Input{TaskID: t.ID, Kind: registry.InputSendBack, Surface: surface, Stage: t.Stage}, p.Note)
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
// local review left open (knownList, from review-summary.json), then the
// Visual QA section of the QA run that passed at the task's head (qaMarker,
// qaSection; none without QA), in place of those an earlier append wrote,
// and records the body as the pr_body artifact.
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
	if res, ok := readQASummary(dir); ok && res.Head == t.HeadSHA {
		if qa := qaSection(res); qa != "" {
			text += "\n" + qaMarker + "\n" + qa
		}
	}
	return e.saveArtifact(ctx, t, "pr_body", prBodyFile, []byte(text))
}

// knownList is the PR body's "Known" section: what magnum's review of the
// change before the PR left open, none of it blocking: the leftovers, the
// findings the writer declined with the risk of the suggested change, the
// doubts (findings that came back twice after their fix), and the
// reviewers a round went without. The heading stands outside and the rest
// is folded in <details> with a one-line count, as the team's PR
// description layout folds every section but Key changes, Demo and Related
// Stories.
func knownList(sum ReviewSummary) string {
	var b strings.Builder
	b.WriteString("## Known\n\n<details>\n")
	lines := knownLines(sum, true)
	if len(lines) == 0 {
		b.WriteString("<summary>nothing the local review left open</summary>\n\n")
		b.WriteString("magnum's review of this change before the PR left nothing open.\n\n</details>\n")
		return b.String()
	}
	note := "notes"
	if len(lines) == 1 {
		note = "note"
	}
	fmt.Fprintf(&b, "<summary>%d %s the local review left open</summary>\n\n", len(lines), note)
	b.WriteString("What magnum's review of this change before the PR left open; none of it blocks.\n\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	b.WriteString("\n</details>\n")
	return b.String()
}

// knownLines are the items of the "Known" list of sum, one line each: the
// leftovers, the findings the writer declined with the risk of the
// suggested change, the doubts, and the reviewers a round went without.
// code puts a finding's place in backticks (the PR body's Markdown); the
// ready card's KNOWN row has it plain.
func knownLines(sum ReviewSummary, code bool) []string {
	var out []string
	for _, i := range sum.Leftovers {
		out = append(out, knownItem(i, code))
	}
	for _, i := range sum.Declined {
		out = append(out, fmt.Sprintf("declined: %s. The risk of the suggested change: %s", knownItem(i, code), oneLine(i.Risk)))
	}
	for _, i := range sum.Doubts {
		out = append(out, fmt.Sprintf("a doubt: %s (it came back twice after its fix)", knownItem(i, code)))
	}
	for _, role := range sum.Missing {
		out = append(out, fmt.Sprintf("a round of the review went without %s's report", role))
	}
	return out
}

// knownItem is a finding in a line: its priority, its place (in backticks
// when code) and its title.
func knownItem(i ReviewItem, code bool) string {
	var parts []string
	if i.Severity != "" {
		parts = append(parts, i.Severity)
	}
	if place := i.Path; place != "" {
		if i.Line > 0 {
			place += ":" + strconv.Itoa(i.Line)
		}
		if code {
			place = "`" + place + "`"
		}
		parts = append(parts, place+":")
	}
	return strings.Join(append(parts, oneLine(i.Title)), " ")
}

// oneLine is s with its lines and spaces joined by single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
