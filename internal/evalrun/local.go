package evalrun

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// LocalCase is one round of the factory's local review: magnum's full
// review of a change in the task's copy, as a blind dry run, before any PR
// exists (docs/superpowers/specs/2026-11-03-factory-design.md, section 6.5).
// Checkout is the copy, BaseSHA and HeadSHA the change's ends, Branch its
// branch, Title (with the Jira key), Body and TicketMD what the judge reads
// from files instead of GitHub; Body is the factory's own plan, not a
// person's description, and AnswersMD the operator's answers to the
// factory's questions (the task's answers.md, "" when it has none), the only
// statement of intent a person made: they follow the ticket in the ticket
// file under one fixed heading. Watch is the repository's [[watch]], whose
// review roles round 1 runs, Roles the roles of this round and Round its
// number, from 1. TaskID is the task whose change it is and Repo its
// repository (owner/name): the round's synthetic PR number (LocalNumber) and
// its scratch directory (LocalRoot) come from them. Base is the base branch
// ("" = master), Env the round's environment (engine.LocalRound.Env): the
// factory gives the copy's, the one its agents get in it; the replay of a
// local case (RunCase) the per-PR worktree env of the synthetic number,
// never a copy's. The factory's LocalReviewer takes it.
// A replay of the round as an eval case (RunCase of a local case) sets
// ScratchRoot, where the round's scratch directory goes instead of the
// task's directory (LocalRoot), and NoNotes, which withholds the repository
// notes (Options.NoNotes); the factory sets neither.
type LocalCase struct {
	Checkout, BaseSHA, HeadSHA, Branch, Title, Body, TicketMD, AnswersMD string
	Watch                                                                config.Watch
	Roles                                                                []string
	Round                                                                int
	TaskID                                                               int64
	Repo, Base                                                           string
	Env                                                                  map[string]string
	ScratchRoot                                                          string
	NoNotes                                                              bool
}

// ConfigError is a local round that cannot run for a configuration problem
// (no gh identity to run as): Key names what to configure, Why what is
// wrong. Running it again changes nothing.
type ConfigError struct {
	Key, Why string
}

func (e *ConfigError) Error() string { return e.Key + ": " + e.Why }

// LocalResult is what a local round found: the round's Outcome (dry_run
// when it finished), its ReportDir, the judge's Verdict ("" when its result
// file is missing or does not parse) and ResultFile, the judge's result file
// it read (that of the judge among the round's roles, "" without a report
// directory), the findings' counts by priority (P0
// to P3) and, for a later round, the counts of the earlier findings still
// open, the findings themselves, and the round's number. Number, Tag and
// Scratch are the round's synthetic PR number, its agents' tag and its
// scratch directory; Refused the agent kind whose provider refused the
// round (OutcomeRefused: "codex" for a Codex warning); Missing the reviewer
// roles whose report the judge went without (a lost pane, a timeout), and
// MissingDetail the report detail of the first of them, in name order,
// that carries its pane's error line (an agent that died at its launch:
// agents.PaneErrorOf reads it), "" when none does; Error why a round that
// did not finish ended, in the round's words.
type LocalResult struct {
	Outcome, ReportDir, Verdict string
	ResultFile                  string
	Counts                      [4]int
	OpenCounts                  *[4]int
	Findings                    []LocalFinding
	Round                       int
	Number                      int
	Tag, Scratch                string
	Refused                     string
	Missing                     []string
	MissingDetail               string
	Error                       string
	// ConfigKey names what to configure when the round could not run for
	// a configuration problem (ConfigError); "" otherwise.
	ConfigKey string
}

// LocalFinding is one finding of a local round: its Severity (P0 to P3, ""
// for a simplification), Title, Path and Line (0 for one in the review's
// body), its Body, and whether it is a simplification or a question for the
// operator (the judge's provenance marks it, the skill's Local round).
type LocalFinding struct {
	Severity, Title, Path    string
	Line                     int
	Body                     string
	Simplification, Question bool
}

// localNumberBase is where the local rounds' synthetic PR numbers start:
// far above any PR number of the repositories magnum reviews (a local eval
// case's PR number is one, eval.LocalNumberBase).
const localNumberBase = eval.LocalNumberBase

// LocalReviewPrompt is the factory's local reviewer prompt, which reviews
// the change in the copy (`git diff <base>..<head>`) and reads the title
// and the description from a file; LiveReviewPrompt is the posted
// reviewer prompt it stands in for in a local round (`/code-review <PR
// URL>`, whose URL no GitHub serves for a synthetic number).
const (
	LocalReviewPrompt = "claude-review-local.md"
	LiveReviewPrompt  = "claude-review.md"
)

// LocalReviewConfig is the config a local round reviews with: a copy of
// cfg (its prompt snapshot, if any, kept) whose every role that names
// LiveReviewPrompt, for any prompt kind, names LocalReviewPrompt in its
// place, resolved as cfg resolves prompts (a candidate's prompts_dir, else
// the embedded one). RunLocal applies it, so the factory's rounds and the
// replays of a local case review alike; cfg is never changed.
func LocalReviewConfig(cfg *config.Config) *config.Config {
	out := *cfg
	out.Roles = cfg.AllRoles()
	for i := range out.Roles {
		r := &out.Roles[i]
		for _, name := range []*string{&r.Prompt, &r.Rereview, &r.Restart, &r.ContinuePrompt, &r.Recovery, &r.Nudge, &r.OwnPass} {
			if *name == LiveReviewPrompt {
				*name = LocalReviewPrompt
			}
		}
	}
	return &out
}

// The bounds of the local rounds' synthetic numbers (LocalNumber).
const (
	// MaxLocalRound is the last round a synthetic number is kept for: two
	// digits of it, past the loop's config.MaxFactoryCycles (a second
	// opinion's round runs outside the loop's count).
	MaxLocalRound = 99
	// MaxLocalTask is the last task a synthetic number is kept for: past
	// it, task T's rounds 1 to 10 would take a round of task T+1 or one of
	// the numbers of rounds 11 and up.
	MaxLocalTask = 99_999
	// localLowRounds is the rounds numbered as they always were, 1 to 10
	// (the loop's config.MaxFactoryCycles); rounds past it start at
	// localHighBase.
	localLowRounds = 10
	localHighBase  = 9_000_000
)

// LocalNumber is the synthetic PR number of round n of task taskID's local
// review: 900000 + task id × 10 + round for rounds 1 to 10, the numbers
// every round ran under before a round could pass the loop's ten
// (900011 to 1900000), and 9000000 + task id × 100 + round for rounds 11 to
// MaxLocalRound (9000111 to 18999999), so no two tasks' rounds share one
// (round 11 of task T would be round 1 of task T+1 by the first rule). It
// is 0 for a task of 1 to MaxLocalTask or a round of 1 to MaxLocalRound it
// is not: no number is kept apart for it.
func LocalNumber(taskID int64, round int) int {
	switch {
	case taskID < 1 || taskID > MaxLocalTask || round < 1 || round > MaxLocalRound:
		return 0
	case round <= localLowRounds:
		return localNumberBase + int(taskID)*10 + round
	}
	return localHighBase + int(taskID)*100 + round
}

// LocalNumberFormula is the rule LocalNumber numbers round `round` by, as
// text: what `eval add` prints beside a local case's pr.
func LocalNumberFormula(round int) string {
	if round > localLowRounds {
		return fmt.Sprintf("%d + task × 100 + round", localHighBase)
	}
	return fmt.Sprintf("%d + task × 10 + round", localNumberBase)
}

// LocalRoundOf is LocalNumber undone: the task and the round whose
// synthetic PR number n is. Below localHighBase, round 10 is where the next
// task's tens start (task 7's is 900080), so the round is counted from 1,
// not taken as n's last digit; from it, the round is n's last two digits
// and must be past 10. ok is false for a number no round of a task has.
func LocalRoundOf(n int) (taskID int64, round int, ok bool) {
	if k := n - localHighBase; k >= 0 {
		task, r := k/100, k%100
		if task < 1 || task > MaxLocalTask || r <= localLowRounds {
			return 0, 0, false
		}
		return int64(task), r, true
	}
	k := n - localNumberBase - 1
	if k < 10 || k/10 > MaxLocalTask { // below task 1's round 1, or past the last task's round 10
		return 0, 0, false
	}
	return int64(k / 10), k%10 + 1, true
}

// LocalRoot is where task taskID's local rounds keep their scratch
// directories: local-review/ in the task's directory.
func LocalRoot(l paths.Layout, taskID int64) string {
	return filepath.Join(l.FactoryTaskDir(taskID), "local-review")
}

// LocalDescriptionFile and LocalTicketFile are the files a local round
// writes in its scratch directory for the judge: the change's title and
// body, and its ticket with the operator's answers after it.
const (
	LocalDescriptionFile = "description.md"
	LocalTicketFile      = pipeline.TicketFile
)

// localAnswersHeading is the one heading over the operator's answers in the
// ticket file. It is fixed (the README and the design log name it): the
// engine reads the ticket file as data and needs no field for the answers.
const localAnswersHeading = "## The operator's answers to the factory's questions"

// answeredQuestion is the heading of one answered question in answers.md as
// the factory writes it (registry.AnswersMarkdown, internal/factory/registry):
// "## <question>", a blank line, then "Asked by <role> (revision N, question
// M). ...". A line of an answer that starts with "## " is not one.
var answeredQuestion = regexp.MustCompile(`(?m)^## (.+)\n\nAsked by `)

// operatorAnswers is answers.md as the judge reads it under
// localAnswersHeading: only the questions and the answers. The file's own
// title ("# Your answers") and the line that tells the factory's agents to
// weigh the answers against their prompt come before the first question and
// are dropped, and each question's heading goes one level down, below the
// fixed heading. A text without that shape (no title, or no question
// section) is kept whole.
func operatorAnswers(answersMD string) string {
	text := strings.TrimSpace(answersMD)
	first := answeredQuestion.FindStringIndex(text)
	if first == nil || !strings.HasPrefix(text, "# ") {
		return text
	}
	return answeredQuestion.ReplaceAllString(text[first[0]:], "### ${1}\n\nAsked by ")
}

// LocalTicket is the text of a local round's ticket file: the ticket, then,
// when the operator answered anything, localAnswersHeading and the answers
// (operatorAnswers); the answers alone under their heading when the change
// has no ticket, the ticket and a newline when it has no answers, "" when it
// has neither. RunLocal writes it as ticket.md; it is exported so that the
// factory's tests read what a task's ticket.md and answers.md become.
func LocalTicket(ticketMD, answersMD string) string {
	ticket, answers := strings.TrimRight(ticketMD, "\n"), operatorAnswers(answersMD)
	if strings.TrimSpace(ticket) == "" {
		ticket = ""
	}
	switch {
	case answers == "" && ticket == "":
		return ""
	case answers == "":
		return ticket + "\n"
	case ticket == "":
		return localAnswersHeading + "\n\n" + answers + "\n"
	}
	return ticket + "\n\n" + localAnswersHeading + "\n\n" + answers + "\n"
}

// localRoundFunc runs a local round's engine: an app of cfg on layout (the
// round's scratch layout) whose agents carry tag, its engine's RunLocal of
// lr, and the release of its agents. Deps.localRound replaces it in tests.
type localRoundFunc func(ctx context.Context, cfg *config.Config, layout paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error)

// fullSHA is a full commit id.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RunLocal runs local round c and reads its judge's result (ParseLocalResult).
// Each round is its own: a fresh scratch directory under the task's
// (LocalRoot/<round>-<tag>; c.ScratchRoot/<round>-<tag> when set) with its
// registry, reports and a copy of the repository notes (none with
// c.NoNotes), a new agent tag, and the synthetic PR number
// LocalNumber, so two rounds never share an agent, a herdr workspace label
// or an eval case lock (it holds the case lock "local-<number>" for the
// round). It writes the title and body (description.md) and the ticket with
// the operator's answers after it (ticket.md, when c has either:
// LocalTicket) there, and runs engine.RunLocal in the task's copy at
// c.HeadSHA, always with c.BaseSHA (post-review's --local-base), as a gh
// identity (the watch's, else the first configured: an App's token would be
// minted from GitHub), with the branch as the head ref and the title with the
// Jira key. The round's config is d.Config (the factory's review candidate,
// config.WithCandidate; a replay's candidate) with the local reviewer
// prompt in the posted one's place (LocalReviewConfig). A case it cannot run is an error and nothing
// starts; a round that ran is a result, whatever its outcome, and only the
// end of ctx (a drain) is an error after it started.
func RunLocal(ctx context.Context, d Deps, c LocalCase) (LocalResult, error) {
	owner, name, ok := strings.Cut(c.Repo, "/")
	switch {
	case d.Config == nil:
		return LocalResult{}, errors.New("evalrun: a local round needs a config")
	case !filepath.IsAbs(c.Checkout):
		return LocalResult{}, fmt.Errorf("evalrun: a local round needs the copy's absolute path, not %q", c.Checkout)
	case !fullSHA.MatchString(c.BaseSHA) || !fullSHA.MatchString(c.HeadSHA):
		return LocalResult{}, errors.New("evalrun: a local round needs the change's base and head as full commit ids")
	case LocalNumber(c.TaskID, c.Round) == 0:
		return LocalResult{}, fmt.Errorf("evalrun: a local round needs its task (1 to %d) and a round of 1 to %d (task %d, round %d)", MaxLocalTask, MaxLocalRound, c.TaskID, c.Round)
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		return LocalResult{}, fmt.Errorf("evalrun: a local round needs its repository as owner/name, not %q", c.Repo)
	}
	id, err := LocalIdentity(d.Config, &c.Watch)
	if err != nil {
		return LocalResult{}, err
	}
	number := LocalNumber(c.TaskID, c.Round)
	tag, err := localTag()
	if err != nil {
		return LocalResult{}, err
	}
	root := cmp.Or(c.ScratchRoot, LocalRoot(d.Layout, c.TaskID))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return LocalResult{}, fmt.Errorf("evalrun: local round %d: %w", c.Round, err)
	}
	scratch := filepath.Join(root, fmt.Sprintf("%d-%s", c.Round, tag))
	if err := os.Mkdir(scratch, 0o700); err != nil { // a fresh one, never an earlier round's
		return LocalResult{}, fmt.Errorf("evalrun: local round %d: %w", c.Round, err)
	}
	lock, err := lockCase(ctx, d, Options{RunDir: scratch}, fmt.Sprintf("local-%d", number))
	if err != nil {
		return LocalResult{}, err
	}
	defer lock.Release()
	layout := d.Layout // logs, locks and the gh config dirs stay the install's
	layout.Scratch = scratch
	if err := layout.EnsureDirs(); err != nil {
		return LocalResult{}, err
	}
	desc := filepath.Join(scratch, LocalDescriptionFile)
	if err := fsx.WriteFileAtomic(desc, []byte("# "+strings.TrimSpace(c.Title)+"\n\n"+strings.TrimSpace(c.Body)+"\n"), 0o600); err != nil {
		return LocalResult{}, fmt.Errorf("evalrun: local round %d: %w", c.Round, err)
	}
	ticket := ""
	if text := LocalTicket(c.TicketMD, c.AnswersMD); text != "" {
		ticket = filepath.Join(scratch, LocalTicketFile)
		if err := fsx.WriteFileAtomic(ticket, []byte(text), 0o600); err != nil {
			return LocalResult{}, fmt.Errorf("evalrun: local round %d: %w", c.Round, err)
		}
	}
	notes := !c.NoNotes
	if notes {
		if err := copyNotes(d.Layout, layout, owner, name); err != nil {
			d.warn("local round %d of task %d: the repository notes stay out of it: %v", c.Round, c.TaskID, err)
			notes = false
		}
	}
	w := c.Watch
	if len(c.Roles) > 0 {
		w.Roles = slices.Clone(c.Roles)
	}
	base := cmp.Or(c.Base, config.DefaultFactoryBase)
	lr := engine.LocalRound{
		EvalCase: engine.EvalCase{Owner: owner, Repo: name, Number: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", c.Repo, number),
			Head: c.HeadSHA, BaseRef: base, DefaultBranch: base, Title: c.Title, HeadRef: c.Branch,
			Checkout: c.Checkout, MainClone: c.Checkout, Watch: w, Identity: id, Notes: notes},
		BaseSHA: c.BaseSHA, DescriptionFile: desc, TicketFile: ticket, Env: maps.Clone(c.Env),
	}
	run := d.localRound
	if run == nil {
		run = d.runLocalRound
	}
	d.progress("local round %d of task %d in herdr workspace \"eval %s#%d\", agents tagged %s (blind, nothing is posted)", c.Round, c.TaskID, name, number, tag)
	res, runErr := run(ctx, LocalReviewConfig(d.Config), layout, tag, lr)
	out := LocalResult{Outcome: cmp.Or(res.Outcome, pipeline.OutcomeError), ReportDir: res.ReportDir, Round: c.Round, Number: number, Tag: tag,
		Scratch: scratch, Error: res.Error}
	if runErr != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.Outcome, out.Error = pipeline.OutcomeError, runErr.Error()
	}
	if res.Refusal != nil {
		out.Refused = res.Refusal.Kind
	}
	for role, rep := range res.Reports {
		if rep.Status != pipeline.ReportOK {
			out.Missing = append(out.Missing, string(role))
		}
	}
	slices.Sort(out.Missing)
	for _, role := range out.Missing {
		if d := res.Reports[agents.Role(role)].Detail; agents.PaneErrorOf(d) != "" {
			out.MissingDetail = d
			break
		}
	}
	if res.ReportDir == "" {
		return out, nil
	}
	out.ResultFile = filepath.Join(res.ReportDir, judgeReport(d.Config, &w)) // the round's judge's, of c.Roles
	data, err := os.ReadFile(out.ResultFile)
	if err != nil {
		out.Error = cmp.Or(out.Error, "no judge result: "+err.Error())
		return out, nil
	}
	parsed := ParseLocalResult(data)
	out.Verdict, out.Counts, out.OpenCounts, out.Findings = parsed.Verdict, parsed.Counts, parsed.OpenCounts, parsed.Findings
	return out, nil
}

// runLocalRound is the local round's engine (localRoundFunc): an app of cfg
// on the round's scratch layout with its agents tagged tag, its engine's
// RunLocal, and the close of the round's agents and workspace after it,
// whatever it answered (a judge still at work is interrupted first).
func (d Deps) runLocalRound(ctx context.Context, cfg *config.Config, layout paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
	logger := d.Logger
	if logger == nil {
		logger = app.NewLogger(nil, d.Stderr, slog.LevelWarn)
	}
	a, err := app.New(cfg, layout, app.Options{AgentTag: tag, Stderr: d.Stderr, Logger: logger, Runner: d.Runner})
	if err != nil {
		return pipeline.RoundResult{}, err
	}
	defer a.Close()
	e := engine.FromApp(a)
	res, pr, err := e.RunLocal(ctx, lr)
	if pr.ID != 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
		if rerr := e.ReleaseEval(rctx, pr); rerr != nil {
			d.warn("local round of %s#%d: closing its agents: %v (close the herdr workspace by hand)", lr.Repo, lr.Number, rerr)
		}
		cancel()
	}
	return res, err
}

// LocalIdentity is the name of the identity a local round runs as: the
// watch's when it is a gh identity, else the first gh identity configured.
// An App's would have its token minted from GitHub, and the round reads
// nothing there. A config without one is a ConfigError. The factory opens
// its PRs as the same identity.
func LocalIdentity(cfg *config.Config, w *config.Watch) (string, error) {
	if id := cfg.IdentityByName(w.Identity); id != nil && id.Kind == "gh" {
		return id.Name, nil
	}
	for _, id := range cfg.Identities {
		if id.Kind == "gh" {
			return id.Name, nil
		}
	}
	return "", &ConfigError{Key: `[[identity]] kind = "gh"`, Why: "a local round runs as a gh identity, and the config has none"}
}

// localTag is a new agent tag for a local round: "local-" and 6 random hex
// digits.
func localTag() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("evalrun: agent tag: %w", err)
	}
	return "local-" + hex.EncodeToString(b[:]), nil
}

// localProvenance is the part of a provenance entry of the judge's result a
// local round reads.
type localProvenance struct {
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     *int   `json:"line"`
	Verdict  string `json:"verdict"`
	Question bool   `json:"question"`
}

// localBodyTitle is the title of the finding a local result makes of the
// review's body (ParseLocalResult), when its counts hold a P0 to P2 that no
// finding carries.
const localBodyTitle = "the review's body"

// titleLead is a comment title's priority, bold and brackets.
var titleLead = regexp.MustCompile(`^\[?P[0-3]\]?:?\s*`)

// ParseLocalResult reads a judge's result file (the skill's section 8 JSON)
// as a local round has it: the verdict, the counts and the earlier
// findings still open (store.ParseReviewResult; Verdict "" when data is no
// result), and the findings: each posted entry of the provenance with its
// severity, title, path and line, the body of the planned comment on that
// line, and whether the judge marked it a question; each simplification of
// the planned review; a planned comment no entry names, by its own
// severity and first line; and, when the counts hold a P0 to P2 that no
// finding carries, the review's body as one finding of that priority, so
// what blocks always reaches the writer. A planned review this code does
// not read leaves the provenance's findings without their bodies.
func ParseLocalResult(data []byte) LocalResult {
	var out LocalResult
	sum := store.ReviewSummary{}
	if !store.ParseReviewResult(data, &sum) {
		return out
	}
	out.Verdict, out.Counts, out.OpenCounts = sum.Verdict, sum.Counts, sum.OpenCounts
	var f struct {
		Provenance []localProvenance `json:"provenance"`
	}
	_ = json.Unmarshal(data, &f) // a provenance this code does not know is none
	parsed, err := eval.ParseResult(data)
	if err != nil { // a planned review this code does not read: the provenance still names the findings, without bodies
		parsed = eval.Result{}
	}
	var body string
	used := map[int]bool{}
	for _, p := range f.Provenance {
		if p.Verdict != "" && p.Verdict != "posted" {
			continue
		}
		lf := LocalFinding{Severity: strings.ToUpper(strings.TrimSpace(p.Severity)), Title: strings.TrimSpace(p.Title), Path: p.Path, Question: p.Question}
		if p.Line != nil {
			lf.Line = *p.Line
		}
		for i, c := range parsed.Findings {
			if !used[i] && !c.InBody && c.Path == lf.Path && c.Line == lf.Line && lf.Line > 0 {
				lf.Body, used[i] = c.Body, true
				break
			}
		}
		out.Findings = append(out.Findings, lf)
	}
	for i, c := range parsed.Findings {
		switch {
		case c.InBody:
			body = c.Body
		case used[i]:
		case c.Simplification:
			out.Findings = append(out.Findings, LocalFinding{Title: commentTitle(c.Body, true), Path: c.Path, Line: c.Line, Body: c.Body, Simplification: true})
		default:
			out.Findings = append(out.Findings, LocalFinding{Severity: c.Severity, Title: commentTitle(c.Body, false), Path: c.Path, Line: c.Line, Body: c.Body})
		}
	}
	for p := range 3 { // a P0 to P2 the counts hold and no finding carries: the body says it
		sev := fmt.Sprintf("P%d", p)
		if out.Counts[p] > 0 && !slices.ContainsFunc(out.Findings, func(lf LocalFinding) bool { return lf.Severity == sev }) {
			out.Findings = append(out.Findings, LocalFinding{Severity: sev, Title: localBodyTitle, Body: body})
			break
		}
	}
	return out
}

// commentTitle is a planned comment's title: its first line without its
// bold and its priority; a simplification's is its first sentence after
// the heading line.
func commentTitle(body string, simplification bool) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if simplification {
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "```") {
				return "Simplification: " + l
			}
		}
		return "Simplification"
	}
	return titleLead.ReplaceAllString(strings.TrimSpace(strings.ReplaceAll(lines[0], "*", "")), "")
}
