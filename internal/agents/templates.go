package agents

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
)

// RenderPrompt executes a resolved prompt template (config.Config.ResolvePrompt
// or RolePrompt) with data: JudgeData for the judge's prompts, RoleData for
// every other session role and ShellData for a shell role's full-line
// template (values or pointers). Prompts are Go text/template files; a field
// the template needs but data lacks is an error. JudgeData is completed
// first: its Reports (see Report) and the notes fields NotesPath implies
// (NotesDir, NotesLock and the lock commands; see NotesFiles). ShellData values are shell-quoted when needed
// (see ShellLine; use it to build a shell role's line). Trailing newlines are
// trimmed: a prompt or typed command must not end with one (a shell line
// would submit an extra empty command).
func RenderPrompt(p config.Prompt, data any) (string, error) {
	name := p.Name
	if name == "" {
		name = "prompt"
	}
	switch d := data.(type) {
	case JudgeData:
		data = d.completed()
	case *JudgeData:
		if d == nil {
			return "", fmt.Errorf("agents: render %s: nil data", name)
		}
		data = d.completed()
	case ShellData:
		q, err := d.shellSafe()
		if err != nil {
			return "", fmt.Errorf("agents: render %s: %w", name, err)
		}
		data = q
	case *ShellData:
		if d == nil {
			return "", fmt.Errorf("agents: render %s: nil data", name)
		}
		q, err := d.shellSafe()
		if err != nil {
			return "", fmt.Errorf("agents: render %s: %w", name, err)
		}
		data = q
	}
	return render(name, p.Text, data)
}

// render executes template text with data as is.
func render(name, text string, data any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("agents: render %s: %w", name, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("agents: render %s: %w", name, err)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// RolePrompt resolves the role's prompt of a prompt kind (config.PromptInitial,
// PromptRereview, PromptContinue, PromptRecovery, PromptNudge, PromptStop;
// see config.Role.PromptFile) in pipeline.prompts_dir or the embedded
// defaults and renders it with data (see RenderPrompt). A role without such
// a prompt, or a name that resolves nowhere, wraps ErrUnknownTemplate
// (config.ErrPromptNotFound).
func (m *Manager) RolePrompt(role config.Role, promptKind string, data any) (string, error) {
	p, err := m.d.Config.RolePrompt(role, promptKind)
	if err != nil {
		return "", fmt.Errorf("agents: %w", err)
	}
	return RenderPrompt(p, data)
}

// Report is one candidate report listed in a judge prompt, one per non-judge
// role of the round in pipeline order. RenderPrompt completes it: Label
// defaults to Role; a report without Path is Missing; a Missing report has
// no Path; Detail and Reason fill each other, then default to Status (or
// "no report") for a missing one.
type Report struct {
	Role    string // the role's name, e.g. claude-review
	Label   string // how the prompt names it ("" = Role)
	Path    string // the report file (absolute); "" when Missing
	Status  string // the role's outcome this round (ok, failed, timeout, ...); informational
	Missing bool   // no usable report this round
	Detail  string // why it is missing, e.g. "timed out after 40m"
	// Reason is Detail under its older name, for prompt files that still
	// render `missing ({{.Reason}})`.
	Reason string
}

func (r Report) completed() Report {
	if r.Label == "" {
		r.Label = r.Role
	}
	if r.Path == "" {
		r.Missing = true
	}
	if r.Missing {
		r.Path = ""
	}
	if r.Detail == "" {
		r.Detail = r.Reason
	}
	if r.Missing && r.Detail == "" {
		r.Detail = r.Status
		if r.Detail == "" {
			r.Detail = "no report"
		}
	}
	if r.Reason == "" {
		r.Reason = r.Detail
	}
	return r
}

// PreviousReview is an earlier review by the reviewer login (recovery mode).
type PreviousReview struct {
	ID          int64
	Event       string // APPROVE, COMMENT, CHANGES_REQUESTED, ...
	SHA         string // commit the review was on (short is fine)
	SubmittedAt string // RFC3339
}

// ReviewThread is an inline thread the reviewer login started on the PR
// (JudgeData.Threads), as magnum writes it to the threads file.
type ReviewThread struct {
	ID string `json:"id"` // GraphQL node id
	// CommentID is the REST id of the thread's first comment: a reply goes
	// to POST repos/{o}/{r}/pulls/{n}/comments/{CommentID}/replies.
	CommentID int64         `json:"comment_id"`
	URL       string        `json:"url"`
	Finding   string        `json:"finding"`  // the first line of the thread's first comment
	Location  string        `json:"location"` // path:line; the original line when outdated
	Resolved  bool          `json:"resolved"`
	Outdated  bool          `json:"outdated"`
	Replies   []ThreadReply `json:"replies"` // oldest first
}

// ThreadReply is one reply of a ReviewThread.
type ThreadReply struct {
	ID     int64  `json:"id"` // REST comment id
	Author string `json:"author"`
	// Own: the reviewer login wrote it (an earlier rebuttal); it has no Class.
	Own bool `json:"own,omitempty"`
	// Class is what the reply's first words claim: fixed, not a bug, won't
	// fix or other.
	Class     string `json:"class,omitempty"`
	Body      string `json:"body"`                // an excerpt: at most 600 characters
	Truncated bool   `json:"truncated,omitempty"` // Body was cut
}

// JudgeData feeds every judge prompt (judge-*.md). Fields a template does not
// use may stay zero.
type JudgeData struct {
	// Readiness is what magnum's prepare/ready probes found before the
	// reviewers (status and reason only; command output stays in
	// readiness.json because it is PR text).
	Readiness Readiness

	RunID           string
	Owner, Repo     string
	Number          int
	URL             string
	HeadSHA         string
	BaseRef         string
	BaseSHA         string
	Checkout        string // absolute checkout path the judge works in
	IdentityKind    string // gh | app
	ReviewerLogin   string // REST form, e.g. talkable[bot]
	GhConfigDir     string // "" for the gh identity
	NoFindingsEvent string // APPROVE | COMMENT
	BlockingEvent   string // REQUEST_CHANGES | COMMENT
	SelfAuthored    bool
	Reports         []Report // one per non-judge role of the round, in pipeline order
	ResultFile      string   // <report dir>/<the judge's output>, e.g. codex-judge.json
	DryRun          bool
	// Blind: an evaluation replay (pipeline.RoundInput.Blind), rendered as
	// `blind: true`; the skill then judges the local diff of HeadSHA only.
	Blind         bool
	SkillPath     string // the role's skill, absolute
	Model, Effort string // the judge role's model and this round's effort (config.Role.EffortFor)
	// EffortInPrompt: the agent's kind sets the effort only at launch (its
	// effort args) and this round's Effort differs from the role's, so a
	// running session cannot switch to it; the prompt asks for it in words.
	EffortInPrompt bool
	// NotesPath is the repository notes file: the judge reads it first and
	// rewrites it at the end of a round that taught something durable. ""
	// = no notes, and the prompts leave the paragraph out.
	NotesPath string
	// NotesDir is the harness directory next to NotesPath and NotesLock the
	// lock the judge holds while it rewrites the notes (NotesFiles; derived
	// from NotesPath when empty). NotesLockCommand and NotesUnlockCommand
	// are the shell lines that take and release it (NotesLockLine,
	// NotesUnlockLine; always derived).
	NotesDir, NotesLock                  string
	NotesLockCommand, NotesUnlockCommand string
	// NotesHarness names the harness directory's entries (sorted; a
	// directory ends in "/"), at most NotesHarnessMax of them, so the judge
	// sees scripts the notes no longer mention; NotesHarnessMore counts the
	// rest.
	NotesHarness     []string
	NotesHarnessMore int

	// Re-review / recovery.
	PreviousReviewID int64
	PreviousEvent    string
	PreviousHeadSHA  string
	Since            string // RFC3339: read every comment since then
	ForcePushed      bool
	MovedFrom        string // previous checkout path when the PR changed slots
	PreviousReviews  []PreviousReview
	// Threads are the inline threads the reviewer login started on the PR,
	// each reply classified (re-review). Replies are PR content, so no
	// prompt prints them: magnum writes Threads to ThreadsFile and a prompt
	// names that file and ThreadSummary.
	Threads []ReviewThread
	// ThreadsFile is the JSON file holding Threads; "" = magnum did not read
	// the threads, and the judge reads the replies itself.
	ThreadsFile string
	// ThreadSummary counts Threads and their replies by class, e.g. "3
	// threads (1 resolved, 1 outdated); replies: 1 fixed, 1 not a bug; 1
	// thread without a reply".
	ThreadSummary string
	// FormerLogins are the logins (REST form) this PR's earlier reviews were
	// posted as before magnum moved it to ReviewerLogin (an identity
	// migration): their reviews and threads are the judge's own history,
	// while every new write goes as ReviewerLogin. Empty for most PRs.
	FormerLogins []string

	// Stop.
	Reason string
}

// completed is d with its Reports and notes fields completed (a copy; d is
// not modified).
func (d *JudgeData) completed() JudgeData {
	out := *d
	out.Reports = make([]Report, len(d.Reports))
	for i, r := range d.Reports {
		out.Reports[i] = r.completed()
	}
	if out.NotesPath != "" {
		dir, lock := NotesFiles(out.NotesPath)
		out.NotesDir, out.NotesLock = cmp.Or(out.NotesDir, dir), cmp.Or(out.NotesLock, lock)
		out.NotesLockCommand, out.NotesUnlockCommand = NotesLockLine(out.NotesLock), NotesUnlockLine(out.NotesLock)
	}
	return out
}

// Prompt modes of RoleData.Mode.
const (
	ModeInitial  = config.PromptInitial  // the role's first run for the PR
	ModeRereview = config.PromptRereview // a new head after the role's earlier run
	ModeRestart  = config.PromptRestart  // a push cut the role's turn short; the round restarted on the new head
)

// RoleData feeds the prompts of every non-judge session role (claude-review,
// claude-simplify, a droid or omp reviewer, ...): the union of what those
// prompts may use. Fields a template does not use may stay zero.
type RoleData struct {
	URL             string // the pull request
	Owner, Repo     string // repo is the name part, e.g. talkable
	Number          int
	HeadSHA         string // the commit under review (checked out, detached)
	PreviousHeadSHA string // the head of the role's previous run (rereview)
	BaseSHA         string // the merge base; `git diff <BaseSHA>..HEAD` is the PR's diff
	BaseRef         string // the base ref, e.g. origin/master or a stacked PR's parent branch
	ReportPath      string // where the role writes its report (its output, absolute)
	Model, Effort   string // the role's model and this round's effort (config.Role.EffortFor; claude-review passes it to /code-review)
	// EffortInPrompt: the agent's kind sets the effort only at launch and
	// this round's Effort differs from the role's (see JudgeData).
	EffortInPrompt bool
	Mode           string // ModeInitial, ModeRereview or ModeRestart
	Since          string // RFC3339: the role's previous run (rereview)
	// ForcePushed: PreviousHeadSHA is no longer in the branch (rereview).
	ForcePushed bool
	// RestartedFrom is the head the role was reviewing when a push cut its
	// turn short (ModeRestart); HeadSHA is the new head.
	RestartedFrom string
	// NotesPath is the repository notes file the role reads first (hints from
	// earlier reviews of the repository); "" = no notes.
	NotesPath string
	// Blind: an evaluation replay (pipeline.RoundInput.Blind); the role
	// must not read reviews, comments or commits after HeadSHA.
	Blind bool
}

// ShellData feeds a shell role's line (ShellLine): the role's command
// template, or a full-line .sh prompt template. Every value is shell-quoted
// when needed before it is substituted, because the result is typed into a
// shell.
type ShellData struct {
	Title      string   // the pane title, e.g. "PR #729 codex-review - talkable"; "" = no title prefix
	Command    string   // the role's command template ("" = role.Command)
	Args       []string // the role's args (nil = role.Args)
	Capture    string   // the role's capture ("" = role.Capture)
	ReportPath string   // the role's output, absolute
	Marker     string   // the done marker, DoneMarker(runID); [A-Za-z0-9._-]+
	BaseRef    string   // e.g. origin/master, or the parent branch of a stacked PR
	// BaseSHA is the merge base of HeadSHA and the base; "" when unknown.
	// Unlike BaseRef it does not move when the base branch gains commits,
	// so `codex review --base {{if .BaseSHA}}{{.BaseSHA}}{{else}}{{.BaseRef}}{{end}}`
	// reviews exactly the PR's diff.
	BaseSHA string
	HeadSHA string
	URL     string

	// RunID and ExtraArgs keep full-line templates written for the old
	// codex_review.sh data working: RunID is Marker without its
	// MAGNUM_DONE_ prefix (or, when Marker is empty, sets it), ExtraArgs
	// defaults to Args.
	RunID     string
	ExtraArgs []string

	// Template is the resolved full-line template of a role with Prompt
	// (config.Config.RolePrompt(role, config.PromptInitial)); nil = resolve
	// role.Prompt among the embedded defaults. Manager.ShellLine fills it
	// from the configuration. Not a template variable.
	Template *config.Prompt
}

const donePrefix = "MAGNUM_DONE_"

// DoneMarker is the line a shell role's line prints when its command
// finished, followed by the command's exit status ("<marker> 0").
func DoneMarker(runID string) string { return donePrefix + runID }

// doneFormat is the printf format a shell line ends with: the marker on a
// line of its own with the exit status of the command before it.
func doneFormat(marker string) string { return marker + " %d" }

// CommandAnchor is text that only the pane's echo of a shell line built
// with marker contains (the done printf's format, which the output shows
// with a number instead): pane text after it is the command's output.
func CommandAnchor(marker string) string { return doneFormat(marker) }

var markerSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// filled completes d from the role: Command, Args and Capture when unset,
// and Marker/RunID/ExtraArgs from each other.
func (d ShellData) filled(role config.Role) ShellData {
	if d.Command == "" {
		d.Command = role.Command
	}
	if d.Args == nil {
		d.Args = role.Args
	}
	if d.Capture == "" {
		d.Capture = role.Capture
	}
	if d.Marker == "" && d.RunID != "" {
		d.Marker = DoneMarker(d.RunID)
	}
	if d.RunID == "" {
		d.RunID = strings.TrimPrefix(d.Marker, donePrefix)
	}
	if d.ExtraArgs == nil {
		d.ExtraArgs = d.Args
	}
	return d
}

// shellSafe checks d and returns it with every value shell-quoted.
func (d ShellData) shellSafe() (ShellData, error) {
	if !markerSafe.MatchString(d.Marker) {
		return d, fmt.Errorf("shell line: done marker %q must match %s", d.Marker, markerSafe)
	}
	if d.RunID != "" && !markerSafe.MatchString(d.RunID) {
		return d, fmt.Errorf("shell line: run id %q must match %s", d.RunID, markerSafe)
	}
	if strings.ContainsFunc(d.Title, unicode.IsControl) {
		return d, fmt.Errorf("shell line: title %q has control characters", d.Title)
	}
	quoteAll := func(in []string) []string {
		if in == nil {
			return nil
		}
		out := make([]string, len(in))
		for i, a := range in {
			out[i] = shellQuote(a)
		}
		return out
	}
	out := ShellData{
		Command: d.Command, Capture: d.Capture, Marker: d.Marker, RunID: d.RunID,
		Args: quoteAll(d.Args), ExtraArgs: quoteAll(d.ExtraArgs),
		ReportPath: quoteNonEmpty(d.ReportPath), BaseRef: quoteNonEmpty(d.BaseRef), BaseSHA: quoteNonEmpty(d.BaseSHA),
		HeadSHA: quoteNonEmpty(d.HeadSHA), URL: quoteNonEmpty(d.URL), Title: quoteNonEmpty(d.Title),
	}
	return out, nil
}

// requireUsed refuses a template that uses an argument value d leaves empty
// (an empty value renders as nothing, which would shift the command's
// arguments, e.g. `--base` without a ref). Title, the args and Capture may
// be empty. A template that names both .BaseSHA and .BaseRef falls back
// from one to the other, so one of them is enough.
func (d ShellData) requireUsed(text string) error {
	uses := func(name string) bool { return strings.Contains(text, "."+name) }
	if uses("BaseSHA") && uses("BaseRef") {
		if d.BaseSHA == "" && d.BaseRef == "" {
			return errors.New("uses .BaseSHA and .BaseRef, which are both empty")
		}
	} else {
		for _, f := range [][2]string{{"BaseSHA", d.BaseSHA}, {"BaseRef", d.BaseRef}} {
			if f[1] == "" && uses(f[0]) {
				return fmt.Errorf("uses .%s, which is empty", f[0])
			}
		}
	}
	for _, f := range [][2]string{
		{"ReportPath", d.ReportPath}, {"HeadSHA", d.HeadSHA}, {"URL", d.URL}, {"Marker", d.Marker}, {"RunID", d.RunID},
	} {
		if f[1] == "" && uses(f[0]) {
			return fmt.Errorf("uses .%s, which is empty", f[0])
		}
	}
	return nil
}

// quoteNonEmpty is shellQuote, keeping "" empty (so `{{if .Title}}` works).
func quoteNonEmpty(s string) string {
	if s == "" {
		return ""
	}
	return shellQuote(s)
}

// ShellLine builds the one-line command typed into a shell role's pane
// (see RunShell). With role.Command set (the role's command template
// overridable by d.Command) the line is
//
//	[printf '\033]0;%s\007' <Title>; DISABLE_AUTO_TITLE=true; ]set -o pipefail; <command> <args...>[ | tee <ReportPath>]; printf '\n<Marker> %d\n' "$?"
//
// where <command> is the command template executed with d (every value
// shell-quoted, e.g. `command codex review --base {{.BaseSHA}}`), the args
// (role.Args, or d.Args) are shell-quoted and appended, and the tee is there
// only for capture "stdout" (ReportPath is then required). The optional
// prefix sets the pane's terminal title (OSC 0), which herdr's sidebar
// shows, and stops oh-my-zsh from resetting it after the command (its title
// hooks honour DISABLE_AUTO_TITLE at run time). pipefail makes the status
// the command's, not tee's; the final printf puts the marker on a line of
// its own (also after output without a trailing newline) followed by that
// status, which RunShell returns (config.Role.StatusOK judges it). With
// role.Prompt set instead, that full-line .sh template (d.Template, else the
// embedded default of that name) is rendered with d and must contain the
// marker (prompts/codex-review.sh shows the same ending). Marker must match
// [A-Za-z0-9._-]+; trailing newlines are trimmed.
func ShellLine(role config.Role, d ShellData) (string, error) {
	if !role.IsShell() {
		return "", fmt.Errorf("agents: shell line %s: not a shell role (kind %q)", role.Name, role.Kind)
	}
	d = d.filled(role)
	if d.Command == "" {
		return promptLine(role, d)
	}
	q, err := d.shellSafe()
	if err != nil {
		return "", fmt.Errorf("agents: shell line %s: %w", role.Name, err)
	}
	if d.Capture == config.CaptureStdout && d.ReportPath == "" {
		return "", fmt.Errorf("agents: shell line %s: capture stdout needs a report path", role.Name)
	}
	if err := d.requireUsed(d.Command); err != nil {
		return "", fmt.Errorf("agents: shell line %s: command: %w", role.Name, err)
	}
	cmd, err := render(role.Name+" command", d.Command, q)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("agents: shell line %s: the command renders empty", role.Name)
	}
	var b strings.Builder
	if q.Title != "" {
		fmt.Fprintf(&b, `printf '\033]0;%%s\007' %s; DISABLE_AUTO_TITLE=true; `, q.Title)
	}
	b.WriteString("set -o pipefail; ")
	b.WriteString(cmd)
	for _, a := range q.Args {
		b.WriteString(" " + a)
	}
	if d.Capture == config.CaptureStdout {
		// stdout only: `codex review` prints its verdict there and the whole
		// transcript on stderr, which stays visible in the pane.
		b.WriteString(" | tee " + q.ReportPath)
	}
	fmt.Fprintf(&b, `; printf '\n%s\n' "$?"`, doneFormat(q.Marker))
	return b.String(), nil
}

// promptLine renders a shell role's full-line .sh template.
func promptLine(role config.Role, d ShellData) (string, error) {
	p := d.Template
	if p == nil {
		if role.Prompt == "" {
			return "", fmt.Errorf("agents: shell line %s: the role has neither command nor prompt", role.Name)
		}
		embedded, err := (&config.Config{}).ResolvePrompt(role.Prompt)
		if err != nil {
			return "", fmt.Errorf("agents: shell line %s: %w", role.Name, err)
		}
		p = &embedded
	}
	if err := d.requireUsed(p.Text); err != nil {
		return "", fmt.Errorf("agents: shell line %s: %s: %w", role.Name, p.Name, err)
	}
	line, err := RenderPrompt(*p, d)
	if err != nil {
		return "", err
	}
	if !strings.Contains(line, d.Marker) {
		return "", fmt.Errorf("agents: shell line %s: %s must echo the done marker %s", role.Name, p.Name, d.Marker)
	}
	return line, nil
}

// ShellLine is ShellLine with the role's full-line template (role.Prompt)
// resolved through the configuration (pipeline.prompts_dir, then the
// embedded defaults) unless d.Template is set.
func (m *Manager) ShellLine(role config.Role, d ShellData) (string, error) {
	if d.Template == nil && role.IsShell() && role.Command == "" && d.Command == "" && role.Prompt != "" {
		p, err := m.d.Config.RolePrompt(role, config.PromptInitial)
		if err != nil {
			return "", fmt.Errorf("agents: shell line %s: %w", role.Name, err)
		}
		d.Template = &p
	}
	return ShellLine(role, d)
}

var shellPlain = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote leaves plain words alone and single-quotes everything else,
// including a word starting with "=" (zsh expands =word to a command path).
func shellQuote(s string) string {
	if shellPlain.MatchString(s) && !strings.HasPrefix(s, "=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
