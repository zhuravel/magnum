package evalrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// LocalInputs are what one round of the factory's local review read,
// frozen when its eval case is added (`magnum eval add --local-task`,
// FreezeLocal) in the case's own directory (LocalCaseDir), so that its
// replay never reads the factory's task directory, which can be cleaned:
// the task and the loop's round (Cycle the review cycle it ran in), the
// repository (owner/name), the change's base and head commits, its branch
// and base branch, the title (with the Jira key), the plan as the judge's
// description (Body), the ticket (TicketMD) and the operator's answers
// (AnswersMD) its ticket file joins (LocalTicket), and the round's roles;
// Frozen is when it was added. The texts are files of their own beside
// inputs.json (plan.md, ticket.md, answers.md). AnswersAsOf is set when
// the answers are not a file the round read but rebuilt from the factory's
// registry as they stood when the round began (its cycle's start), and
// AnswersLeftOut counts the answers given at that start or later, which
// the rebuild left out.
type LocalInputs struct {
	Task    int64     `json:"task"`
	Round   int       `json:"round"`
	Cycle   int       `json:"cycle"`
	Repo    string    `json:"repo"`
	BaseSHA string    `json:"base_sha"`
	HeadSHA string    `json:"head_sha"`
	Branch  string    `json:"branch"`
	Base    string    `json:"base"`
	Title   string    `json:"title"`
	Roles   []string  `json:"roles"`
	Frozen  time.Time `json:"frozen"`
	// CodexNever is the factory's mark that Codex never works on the task
	// again (factoryread.LocalTask.CodexNever) as it was when the case was
	// added: a replay whose roles run Codex is refused on it (Refused).
	CodexNever bool `json:"codex_never,omitempty"`

	AnswersAsOf    time.Time `json:"answers_as_of,omitzero"`
	AnswersLeftOut int       `json:"answers_left_out,omitempty"`

	Body      string `json:"-"`
	TicketMD  string `json:"-"`
	AnswersMD string `json:"-"`
}

// The files of a local case's frozen inputs, in its directory.
const (
	localInputsFile  = "inputs.json"
	localPlanFile    = "plan.md"
	localTicketMD    = "ticket.md"
	localAnswersFile = "answers.md"
)

// Number is the synthetic PR number the round ran under (LocalNumber),
// which its replay runs under again.
func (in LocalInputs) Number() int { return LocalNumber(in.Task, in.Round) }

// PR is the round's synthetic PR, owner/repo#Number: the local case's pr in
// the corpus (eval.Case.PR).
func (in LocalInputs) PR() string { return fmt.Sprintf("%s#%d", in.Repo, in.Number()) }

// check says what in lacks to be replayed (nil = nothing).
func (in LocalInputs) check() error {
	owner, name, ok := strings.Cut(in.Repo, "/")
	switch {
	case in.Task <= 0 || in.Round < 1 || in.Round > config.MaxFactoryCycles:
		return fmt.Errorf("task %d, round %d: a local case needs its task and a round of 1 to %d", in.Task, in.Round, config.MaxFactoryCycles)
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		return fmt.Errorf("repository %q is not owner/name", in.Repo)
	case !fullSHA.MatchString(in.BaseSHA) || !fullSHA.MatchString(in.HeadSHA):
		return errors.New("the base and the head are not full commit ids")
	case strings.TrimSpace(in.Branch) == "":
		return errors.New("no branch")
	case strings.TrimSpace(in.Title) == "":
		return errors.New("no title")
	case len(in.Roles) == 0:
		return errors.New("no roles")
	}
	return nil
}

// LocalCaseDir is where local case name keeps its frozen inputs:
// paths.Layout.LearnLocalCases()/<name>, beside the learned corpus, outside
// any checkout (a case name is a file name: eval's idPattern).
func LocalCaseDir(l paths.Layout, name string) string {
	return filepath.Join(l.LearnLocalCases(), name)
}

// LocalResultFile is the file in a task's directory where the factory keeps
// the result of review cycle n's round (an evalrun.LocalResult as JSON), as
// the factory names it (internal/factory's reviewRoundFile).
func LocalResultFile(n int) string { return fmt.Sprintf("local-review-%d.json", n) }

// SaveLocalInputs writes in to the new directory dir (0700, its files 0600):
// inputs.json and the three texts, all in a directory beside dir that is
// renamed into place at the end, so a case is either whole or not there. A
// dir that exists is refused: a frozen case is never overwritten.
func SaveLocalInputs(dir string, in LocalInputs) error {
	if err := in.check(); err != nil {
		return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("evalrun: local case %s exists already (%s): remove it to freeze the round again, or give another name",
			filepath.Base(dir), tilde(dir))
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".")
	if err != nil {
		return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	defer os.RemoveAll(tmp) // nothing once renamed
	for name, text := range map[string]string{localInputsFile: string(data) + "\n", localPlanFile: in.Body, localTicketMD: in.TicketMD, localAnswersFile: in.AnswersMD} {
		if err := fsx.WriteFileAtomic(filepath.Join(tmp, name), []byte(text), 0o600); err != nil {
			return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	return nil
}

// LoadLocalInputs reads the frozen inputs in dir (SaveLocalInputs). A file
// missing, or inputs that cannot be replayed, are an error (its round
// frozen again, `magnum eval add`, makes them whole).
func LoadLocalInputs(dir string) (LocalInputs, error) {
	var in LocalInputs
	data, err := os.ReadFile(filepath.Join(dir, localInputsFile))
	if err != nil {
		return in, fmt.Errorf("evalrun: local case %s has no frozen inputs in %s (eval add freezes them): %w", filepath.Base(dir), tilde(dir), err)
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("evalrun: local case %s: %s: %w", filepath.Base(dir), localInputsFile, err)
	}
	for name, text := range map[string]*string{localPlanFile: &in.Body, localTicketMD: &in.TicketMD, localAnswersFile: &in.AnswersMD} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return in, fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
		}
		*text = string(b)
	}
	if err := in.check(); err != nil {
		return in, fmt.Errorf("evalrun: local case %s: %w", filepath.Base(dir), err)
	}
	return in, nil
}

// FreezeLocal reads what round `round` of task taskID's local review read,
// for its eval case (`magnum eval add --local-task`), from the round's own
// records wherever they are kept, and from nothing that changed since it
// began; cycle picks the review cycle when the task's loops ran that round
// in several (0 = the only one). It reads, read only:
//
//   - the factory's registry (factoryread.LocalReview): the task's
//     repository and branch, whether Codex never works on it again, the
//     cycle's head and start, and the round's roles from its begin event;
//   - the round's result in the task's directory (LocalResultFile), which
//     must be this round's and names the round's scratch directory, inside
//     the task's local review (LocalRoot);
//   - in that scratch directory, the description.md the judge read (the
//     title and the plan), its ticket.md (the ticket; "" when the round had
//     none) and the report directory's prepass.json, whose base is the
//     commit the round diffed from and whose head must be the cycle's;
//   - the operator's answers: those under their heading in the round's
//     ticket file when it has them, else the task's answers.md ("" when
//     there is none) when no answer was given at the round's start or
//     later and the file was not written after it began; else the answers
//     rebuilt from the factory's registry as they stood when the round
//     began (answersAt).
//
// The base branch is the [[factory.repo]]'s (default master). What cannot
// be frozen whole is an error that says what is missing; it returns the
// round's result too, whose findings `eval add` prints.
func FreezeLocal(ctx context.Context, d Deps, taskID int64, round, cycle int) (LocalInputs, LocalResult, error) {
	var res LocalResult
	lt, err := factoryread.LocalReview(ctx, d.Layout.FactoryDB(), taskID)
	if err != nil {
		return LocalInputs{}, res, fmt.Errorf("evalrun: task %d: %w", taskID, err)
	}
	r, err := pickRound(lt, round, cycle)
	if err != nil {
		return LocalInputs{}, res, err
	}
	name := fmt.Sprintf("round %d (cycle %d) of task %d", r.Round, r.Cycle, taskID)
	fail := func(format string, args ...any) (LocalInputs, LocalResult, error) {
		return LocalInputs{}, res, fmt.Errorf("evalrun: %s cannot be frozen whole: %s", name, fmt.Sprintf(format, args...))
	}
	taskDir := d.Layout.FactoryTaskDir(taskID)
	resultFile := filepath.Join(taskDir, LocalResultFile(r.Cycle))
	b, err := os.ReadFile(resultFile)
	if err != nil {
		return fail("no result in %s (it never ended, or ran again and was set aside): %v", tilde(resultFile), err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fail("its result %s: %v", tilde(resultFile), err)
	}
	if res.Round != r.Round {
		return fail("its result %s is round %d's", tilde(resultFile), res.Round)
	}
	root := LocalRoot(d.Layout, taskID)
	scratch := res.Scratch
	if scratch == "" && res.ReportDir != "" { // a result from before it named its scratch directory: the report's is in it
		if rel, err := filepath.Rel(root, res.ReportDir); err == nil {
			scratch = filepath.Join(root, strings.Split(rel, string(filepath.Separator))[0])
		}
	}
	if !within(root, scratch) {
		return fail("its scratch directory %q is not in the task's local review (%s)", scratch, tilde(root))
	}
	desc, err := os.ReadFile(filepath.Join(scratch, LocalDescriptionFile))
	if err != nil {
		return fail("the %s its judge read is gone: %v", LocalDescriptionFile, err)
	}
	title, body, _ := strings.Cut(string(desc), "\n")
	title, ok := strings.CutPrefix(title, "# ")
	if !ok || strings.TrimSpace(title) == "" {
		return fail("its %s does not start with the title", LocalDescriptionFile)
	}
	ticket, answers, held := "", "", false
	switch t, err := os.ReadFile(filepath.Join(scratch, LocalTicketFile)); {
	case err == nil:
		ticket, answers, held = splitLocalTicket(string(t))
	case !errors.Is(err, fs.ErrNotExist):
		return fail("its %s: %v", LocalTicketFile, err)
	}
	var asOf time.Time
	left := 0
	if !held {
		if answers, asOf, left, err = answersAt(lt, taskDir, r); err != nil {
			return fail("%v", err)
		}
	}
	owner, repoName, _ := strings.Cut(lt.Repo, "/")
	report := cmp.Or(res.ReportDir, paths.Layout{Scratch: scratch}.ReviewDir(owner, repoName, LocalNumber(taskID, r.Round), r.Head))
	if !within(scratch, report) {
		return fail("its report directory %q is not in its scratch directory", report)
	}
	var pp pipeline.Prepass
	if b, err := os.ReadFile(filepath.Join(report, pipeline.PrepassFile)); err != nil {
		return fail("its report directory has no %s, which names the base it diffed from: %v", pipeline.PrepassFile, err)
	} else if err := json.Unmarshal(b, &pp); err != nil {
		return fail("its %s: %v", pipeline.PrepassFile, err)
	}
	if pp.HeadSHA != r.Head {
		return fail("its %s is of head %s, not of %s, the head its cycle reviewed", pipeline.PrepassFile, short(pp.HeadSHA), short(r.Head))
	}
	if len(r.Roles) == 0 {
		return fail("no begin event of the round names its roles")
	}
	base := config.DefaultFactoryBase
	if d.Config != nil {
		if fr, ok := d.Config.FactoryRepo(lt.Repo); ok && fr.Base != "" {
			base = fr.Base
		}
	}
	in := LocalInputs{Task: taskID, Round: r.Round, Cycle: r.Cycle, Repo: lt.Repo, BaseSHA: pp.BaseSHA, HeadSHA: r.Head, Branch: lt.Branch, Base: base,
		Title: strings.TrimSpace(title), Roles: r.Roles, Frozen: d.now(), CodexNever: lt.CodexNever, Body: strings.TrimSpace(body), TicketMD: ticket, AnswersMD: answers,
		AnswersAsOf: asOf, AnswersLeftOut: left}
	if err := in.check(); err != nil {
		return fail("%v", err)
	}
	return in, res, nil
}

// pickRound is round `round` of lt's local review, in cycle cycle when it
// is not 0; one that is not there, or that several cycles ran without a
// cycle to pick, is an error naming the rounds there are.
func pickRound(lt factoryread.LocalTask, round, cycle int) (factoryread.LocalRound, error) {
	var picked []factoryread.LocalRound
	var have []string
	for _, r := range lt.Rounds {
		if r.Round == 0 {
			have = append(have, fmt.Sprintf("cycle %d, whose begin event naming its round and roles is gone", r.Cycle))
			continue
		}
		have = append(have, fmt.Sprintf("round %d (cycle %d)", r.Round, r.Cycle))
		if r.Round == round && (cycle == 0 || r.Cycle == cycle) {
			picked = append(picked, r)
		}
	}
	switch {
	case len(picked) == 1:
		return picked[0], nil
	case len(picked) > 1:
		cycles := make([]string, len(picked))
		for i, r := range picked {
			cycles[i] = fmt.Sprint(r.Cycle)
		}
		return factoryread.LocalRound{}, fmt.Errorf("evalrun: task %d ran round %d in cycles %s: give --cycle", lt.ID, round,
			strings.Join(cycles[:len(cycles)-1], ", ")+" and "+cycles[len(cycles)-1])
	}
	where := ""
	if cycle > 0 {
		where = fmt.Sprintf(" in cycle %d", cycle)
	}
	if len(have) == 0 {
		have = []string{"none"}
	}
	return factoryread.LocalRound{}, fmt.Errorf("evalrun: task %d ran no round %d%s that the factory's registry names (its rounds: %s)",
		lt.ID, round, where, strings.Join(have, "; "))
}

// answersAt are the operator's answers as round r of lt could read them,
// when its ticket file did not hold them: the task's answers.md in taskDir
// ("" when there is none) when no answer was given at the round's start or
// later and the file was not written after it began. Else they are rebuilt
// from the factory's registry as they stood when the round began, asOf
// (factoryread.LocalTask.AnswersAt: each question answered before
// the start, with its newest answer before it), rendered as the factory
// renders answers.md (registry.AnswersMarkdown), and left counts the
// answers given at the start or later that the rebuild left out: a later
// answer (to the very question the round missed, perhaps) would make the
// replay prove nothing. A registry that holds no answer of the task cannot
// rebuild what answers.md held then: that is an error.
func answersAt(lt factoryread.LocalTask, taskDir string, r factoryread.LocalRound) (answers string, asOf time.Time, left int, err error) {
	qs, left := lt.AnswersAt(r.Started)
	path := filepath.Join(taskDir, localAnswersFile)
	st, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, 0, err
	}
	written := st != nil && st.ModTime().After(r.Started)
	switch {
	case left == 0 && !written && st == nil:
		return "", time.Time{}, 0, nil
	case left == 0 && !written:
		b, err := os.ReadFile(path)
		return string(b), time.Time{}, 0, err
	case len(lt.Questions) == 0: // written since, and nothing to rebuild from
		return "", time.Time{}, 0, fmt.Errorf("%s was written after round %d began (%s), its ticket file did not hold the answers it read, and the "+
			"factory's registry holds no answer of the task to rebuild them from", localAnswersFile, r.Round, st.ModTime().UTC().Format(time.RFC3339))
	}
	return registry.AnswersMarkdown(qs), r.Started, left, nil
}

// splitLocalTicket is LocalTicket undone: the ticket and the answers of a
// round's ticket file, held when it has the answers' heading (the answers
// as the judge read them, which LocalTicket gives back unchanged); the
// whole text as the ticket otherwise.
func splitLocalTicket(text string) (ticket, answers string, held bool) {
	if rest, ok := strings.CutPrefix(text, localAnswersHeading+"\n\n"); ok {
		return "", strings.TrimSpace(rest), true
	}
	if t, a, ok := strings.Cut(text, "\n\n"+localAnswersHeading+"\n\n"); ok {
		return t, strings.TrimSpace(a), true
	}
	return text, "", false
}

// within reports whether path is below root (an absolute path, never root
// itself).
func within(root, path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// short is a commit's first 12 digits.
func short(sha string) string { return sha[:min(12, len(sha))] }

// LocalCaseName is the name `magnum eval add` gives the case of round
// `round` of task taskID unless told another: task-<id>-round-<n>.
func LocalCaseName(taskID int64, round int) string {
	return fmt.Sprintf("task-%d-round-%d", taskID, round)
}
