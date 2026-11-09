// Package prstatus is the contract of the factory part of a PR's status
// comment: one JSON file per PR a factory task published,
// <factory dir>/pr-status/<owner>/<repo>/<number>.json (Path, which takes
// GitHub names only, in lower case, and a positive number).
//
//   - Who writes: the factory daemon (Write), from the publish on, and only
//     when the content changes: no file exists before the PR is published.
//   - Who reads: magnum's review daemon, read-only (Read), to render it in
//     the status comment it already keeps on the PR. A missing file is no
//     factory part; a file Read refuses counts as missing.
//   - Fixed words only: counts, enums, times, the task id, the Jira key and
//     the staging environment's slug, never free text from a PR, a ticket, a
//     question or an agent. Every enum is a string type whose Valid lists
//     its words, and Read refuses any other word.
//   - Version rule: v is Version, and Read refuses any other. A field that
//     changes its meaning or its type, or goes away, bumps it. A new field or
//     a new word of an enum keeps it: a reader ignores the fields it does not
//     know, and refuses only a file that holds a word it does not know.
//   - Digest rule: digest is the hex sha256 of the canonical JSON of every
//     field but updated_at and digest, the fields a reader does not know
//     included: encoding/json's encoding of the object decoded as a map
//     (keys sorted at every level, no spaces, numbers as written). Neither
//     the order of the keys nor the update time changes it. Read refuses a
//     file whose digest does not match; Write writes nothing when the file
//     already has the digest, so an unchanged status costs no write and no
//     edit of the comment.
//
// It imports only the standard library, internal/paths and internal/fsx
// (the atomic write and the capped read; a leaf internal/paths already
// imports), so either daemon imports it without the other.
package prstatus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// Version is the schema version Write writes and Read accepts.
const Version = 1

// MaxSize is the most bytes Read reads: a bigger file is refused.
const MaxSize = 64 << 10

// Read's refusals, each wrapped with the file's path (and, for ErrInvalid,
// the field's name, never its value).
var (
	ErrTooBig   = errors.New("over 64 KiB")
	ErrCutShort = errors.New("cut short")
	ErrNotJSON  = errors.New("not a JSON object")
	ErrVersion  = errors.New("unknown schema version")
	ErrInvalid  = errors.New("invalid field")
	ErrDigest   = errors.New("digest does not match the content")
)

// Status is one PR's factory part. Every count is 0 or more, every enum one
// of its words and both times set; Write refuses, and Read refuses, any
// other.
type Status struct {
	// V is the schema version, Version.
	V int `json:"v"`
	// Digest is the hex sha256 of the content (the package doc's digest
	// rule); Write sets it.
	Digest string `json:"digest"`
	// UpdatedAt is when the content last changed, in UTC to the second
	// (Write keeps the file, and its time, while the content stays the same);
	// never zero. It is outside the digest, so Read checks it on its own.
	UpdatedAt time.Time `json:"updated_at"`
	// TaskID is the factory task that published the PR, positive.
	TaskID int64 `json:"task_id"`
	// Jira is the task's issue; the zero Jira says it has none.
	Jira Jira `json:"jira"`
	// Plan counts the task's questions.
	Plan Plan `json:"plan"`
	// Code is the change's commits, proof and specs.
	Code Code `json:"code"`
	// LocalReview is the factory's review of the change before the publish.
	LocalReview LocalReview `json:"local_review"`
	// PublishedAt is when the PR was published, in UTC to the second; never
	// zero, since the file begins at the publish.
	PublishedAt time.Time `json:"published_at"`
	// FixLoop is magnum's fix loop on the PR.
	FixLoop FixLoop `json:"fix_loop"`
	// Staging is the change's deploy to a staging environment: the first of
	// Stagings, for the readers from before them, or {state: none} for a
	// repository without one.
	Staging Staging `json:"staging"`
	// Stagings are the change's deploys to each staging environment of its
	// repository, in the config's order; none (no key) for a repository
	// without one. A field added under v=1: a reader before it ignores it.
	Stagings []Staging `json:"stagings,omitempty"`
	// Now is who the PR waits on, and the task's stage.
	Now Now `json:"now"`
}

// Jira is the task's Jira issue: its key (ABC-123) and status. Both are ""
// for a task without an issue.
type Jira struct {
	Key    string     `json:"key"`
	Status JiraStatus `json:"status"`
}

// JiraOf is the Jira of a task's issue key with status: the zero Jira (no
// issue) for a key that is not one the file takes (ABC-123), so the
// writer's key never makes Write refuse the status.
func JiraOf(key string, status JiraStatus) Jira {
	if !jiraKeyRe.MatchString(key) {
		return Jira{}
	}
	return Jira{Key: key, Status: status}
}

// Plan counts the questions the task asked you and the ones you answered.
type Plan struct {
	QuestionsAsked    int `json:"questions_asked"`
	QuestionsAnswered int `json:"questions_answered"`
}

// Code is the change: its commits, its proof and its specs.
type Code struct {
	Commits int   `json:"commits"`
	Proof   Proof `json:"proof"`
	Specs   int   `json:"specs"`
}

// LocalReview is the local review's rounds, its last verdict and the notes
// it left open.
type LocalReview struct {
	Rounds    int     `json:"rounds"`
	Verdict   Verdict `json:"verdict"`
	NotesOpen int     `json:"notes_open"`
}

// FixLoop is magnum's fix loop on the PR: its state, its cycle, its pushes
// and the review threads it answered.
type FixLoop struct {
	State           FixState `json:"state"`
	Cycle           int      `json:"cycle"`
	Pushes          int      `json:"pushes"`
	ThreadsAnswered int      `json:"threads_answered"`
}

// Staging is the deploy of the change to one staging environment: Env is
// the configured staging environment's name when it is a short slug
// ([a-z0-9-]{1,32}), else "".
// The config takes any name that is not blank, and Write refuses a whole
// status whose Env is not a slug, so the writer sets Env with StagingEnv.
type Staging struct {
	Env   string       `json:"env"`
	State StagingState `json:"state"`
}

// StagingEnv is the Env of the configured staging environment name: the
// name when it is a slug, else "" (the row then has the state without a
// name), so a name such as "Alpha" or "qa env" never makes Write refuse the
// status.
func StagingEnv(name string) string {
	if envRe.MatchString(name) {
		return name
	}
	return ""
}

// Now is who the PR waits on and the task's stage.
type Now struct {
	Who   Who   `json:"who"`
	Stage Stage `json:"stage"`
}

// JiraStatus is the issue's status, in the factory's words.
type JiraStatus string

// The Jira statuses.
const (
	JiraToDo          JiraStatus = "to_do"
	JiraInDevelopment JiraStatus = "in_development"
	JiraInCodeReview  JiraStatus = "in_code_review"
	JiraDone          JiraStatus = "done"
	JiraOther         JiraStatus = "other"
	JiraUnknown       JiraStatus = "unknown"
)

// Valid reports whether s is one of the Jira statuses.
func (s JiraStatus) Valid() bool {
	return oneOf(s, JiraToDo, JiraInDevelopment, JiraInCodeReview, JiraDone, JiraOther, JiraUnknown)
}

// JiraStatusOf is the file's word for an issue's status, by Jira's category
// of it (its statusCategory key) and its name, case aside: new is to_do and
// done is done; indeterminate is in_code_review when the name holds
// "review", in_development when it holds one of devWords, else other (a QA,
// a deploy, a block: "in development" would tell the reviewer something
// false); any other category, "" included, is other. A task whose issue
// was never read is unknown, which the writer says without this.
func JiraStatusOf(category, name string) JiraStatus {
	name = strings.ToLower(name)
	switch category {
	case "new":
		return JiraToDo
	case "done":
		return JiraDone
	case "indeterminate":
		switch {
		case strings.Contains(name, "review"):
			return JiraInCodeReview
		case slices.ContainsFunc(devWords, func(w string) bool { return strings.Contains(name, w) }):
			return JiraInDevelopment
		}
	}
	return JiraOther
}

// devWords are the words of an in-progress status's name that say the
// work is being written: "In Progress", "In Development", "In Dev",
// "Doing", "Implementing", "Work in progress".
var devWords = []string{"progress", "develop", "in dev", "doing", "coding", "implement", "work"}

// Proof is what the coding stage proved: nothing yet, the spec red on the
// base, red on the base and green at the head, or (a kind whose proof is
// green) the specs green at the head.
type Proof string

// The proofs.
const (
	ProofNone       Proof = "none"
	ProofRed        Proof = "red"
	ProofRedToGreen Proof = "red_to_green"
	ProofGreen      Proof = "green"
)

// Valid reports whether p is one of the proofs.
func (p Proof) Valid() bool { return oneOf(p, ProofNone, ProofRed, ProofRedToGreen, ProofGreen) }

// Verdict is the local review's last verdict.
type Verdict string

// The verdicts.
const (
	VerdictNone     Verdict = "none"
	VerdictClean    Verdict = "clean"
	VerdictFindings Verdict = "findings"
	VerdictFailed   Verdict = "failed"
)

// Valid reports whether v is one of the verdicts.
func (v Verdict) Valid() bool {
	return oneOf(v, VerdictNone, VerdictClean, VerdictFindings, VerdictFailed)
}

// FixState is the fix loop's state: none yet, running, needing you, done,
// or off for the PR.
type FixState string

// The fix loop's states.
const (
	FixNone     FixState = "none"
	FixRunning  FixState = "running"
	FixNeedsYou FixState = "needs_you"
	FixDone     FixState = "done"
	FixOff      FixState = "off"
)

// Valid reports whether s is one of the fix loop's states.
func (s FixState) Valid() bool {
	return oneOf(s, FixNone, FixRunning, FixNeedsYou, FixDone, FixOff)
}

// StagingState is the staging deploy's state.
type StagingState string

// The staging deploy's states.
const (
	StagingNone      StagingState = "none"
	StagingDeploying StagingState = "deploying"
	StagingDeployed  StagingState = "deployed"
	StagingFailed    StagingState = "failed"
)

// Valid reports whether s is one of the staging deploy's states.
func (s StagingState) Valid() bool {
	return oneOf(s, StagingNone, StagingDeploying, StagingDeployed, StagingFailed)
}

// Who is who the PR waits on.
type Who string

// Who the PR can wait on.
const (
	WhoYou       Who = "you"
	WhoMagnum    Who = "magnum"
	WhoReviewers Who = "reviewers"
	WhoCI        Who = "ci"
	WhoFactory   Who = "factory"
	WhoNone      Who = "none"
)

// Valid reports whether w is one of who a PR waits on.
func (w Who) Valid() bool {
	return oneOf(w, WhoYou, WhoMagnum, WhoReviewers, WhoCI, WhoFactory, WhoNone)
}

// Stage is the task's stage, in fixed words: the phase its kind's
// configured stage falls in (a kind names its own stages; these words do
// not change with it), then the task's state once it is published.
type Stage string

// The stages.
const (
	StagePlan      Stage = "plan"      // investigate, judge, critic and the questions and reproduction gates
	StageCode      Stage = "code"      // the writer's stage, up to its proof
	StageReview    Stage = "review"    // the local review loop
	StageReady     Stage = "ready"     // the PR body and the ready card
	StagePublished Stage = "published" // the PR is open
	StageDone      Stage = "done"      // the PR merged
	StageStopped   Stage = "stopped"   // you stopped the task
	StageFailed    Stage = "failed"    // the task failed
)

// Valid reports whether s is one of the stages.
func (s Stage) Valid() bool {
	return oneOf(s, StagePlan, StageCode, StageReview, StageReady, StagePublished, StageDone, StageStopped, StageFailed)
}

func oneOf[T ~string](w T, words ...T) bool { return slices.Contains(words, w) }

// Path is the status file of PR number of owner/repo in l's factory
// directory: paths.Layout.FactoryPRStatus, then owner, repo and
// <number>.json. owner and repo must be GitHub names (no "." or "..", no
// slash, not empty) and number positive, or Path refuses them. The names
// are lower-cased: GitHub ignores their case, so the writer's spelling and
// the reader's name one file.
func Path(l paths.Layout, owner, repo string, number int) (string, error) {
	switch {
	case len(owner) > 39 || !ownerRe.MatchString(owner):
		return "", errors.New("prstatus: the owner is not a GitHub name")
	case len(repo) > 100 || !repoRe.MatchString(repo) || repo == "." || repo == "..":
		return "", errors.New("prstatus: the repository is not a GitHub name")
	case number <= 0:
		return "", errors.New("prstatus: the PR number is not positive")
	}
	return filepath.Join(l.FactoryPRStatus(), strings.ToLower(owner), strings.ToLower(repo), strconv.Itoa(number)+".json"), nil
}

// The GitHub names Path takes, as internal/github checks them.
var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Read reads the status file at path, a regular file of at most MaxSize
// bytes (a symbolic link is refused, not followed). A missing file is no
// status: (zero, false, nil). A file Read refuses is the zero Status and an
// error that says why (errors.Is ErrTooBig, ErrCutShort, ErrNotJSON,
// ErrVersion, ErrInvalid or ErrDigest), in fixed words and the field's
// name, never the file's content; the caller treats it as missing. Fields
// Read does not know are ignored, and the digest covers them.
func Read(path string) (Status, bool, error) {
	b, err := fsx.ReadRegular(path, MaxSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Status{}, false, nil
	case errors.Is(err, fsx.ErrTooLarge):
		return Status{}, false, fmt.Errorf("prstatus: %s: %w", path, ErrTooBig)
	case err != nil:
		return Status{}, false, fmt.Errorf("prstatus: %w", err)
	}
	s, err := parse(b)
	if err != nil {
		return Status{}, false, fmt.Errorf("prstatus: %s: %w", path, err)
	}
	return s, true, nil
}

// parse checks b in the order of Read's refusals: one JSON object, of
// Version, whose fields decode, whose digest matches and whose fields hold
// their words.
func parse(b []byte) (Status, error) {
	obj, err := object(b)
	if err != nil {
		return Status{}, err
	}
	if v, ok := obj["v"].(json.Number); !ok || v.String() != strconv.Itoa(Version) {
		return Status{}, ErrVersion
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && te.Field != "" {
			return Status{}, fmt.Errorf("%w: %s", ErrInvalid, te.Field)
		}
		return Status{}, fmt.Errorf("%w: a field of the wrong type", ErrInvalid)
	}
	d, err := digest(obj)
	if err != nil {
		return Status{}, err
	}
	if s.Digest != d {
		return Status{}, ErrDigest
	}
	if err := s.validate(); err != nil {
		return Status{}, err
	}
	return s, nil
}

// Write writes s to path when its content differs from the file's, and
// reports whether it wrote. It sets V and Digest, keeps the times in UTC to
// the second (a zero UpdatedAt is the time of the write) and refuses a
// status Read would refuse (ErrInvalid), writing nothing. When the file on
// disk has the same digest it writes nothing and returns false, so the file
// keeps the UpdatedAt of the last change. The write is atomic and durable:
// a temporary file in the same folder, synced, renamed over the file, then
// the folder synced; the file is 0600, the folders it makes 0700.
func Write(path string, s Status) (bool, error) {
	s.V = Version
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now()
	}
	s.UpdatedAt = s.UpdatedAt.UTC().Truncate(time.Second)
	s.PublishedAt = s.PublishedAt.UTC().Truncate(time.Second)
	if err := s.validate(); err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	s.Digest = ""
	b, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	obj, err := object(b)
	if err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	if s.Digest, err = digest(obj); err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	if old, ok, err := Read(path); err == nil && ok && old.Digest == s.Digest {
		return false, nil
	}
	if b, err = json.MarshalIndent(s, "", "  "); err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	if err := fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		return false, fmt.Errorf("prstatus: %w", err)
	}
	return true, nil
}

// object decodes b, which must hold one JSON object and nothing more, with
// its numbers as written (json.Number).
func object(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrCutShort
		}
		return nil, ErrNotJSON
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, ErrNotJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrNotJSON
	}
	return obj, nil
}

// digest is the digest rule over a decoded object: the hex sha256 of its
// JSON as encoding/json writes a map (keys sorted at every level, no
// spaces), without updated_at and digest. Fields the reader does not know
// count, so a writer's new field within the same v still checks.
func digest(obj map[string]any) (string, error) {
	content := make(map[string]any, len(obj))
	for k, v := range obj {
		if k != "updated_at" && k != "digest" {
			content[k] = v
		}
	}
	b, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// validate checks every field against its words, in the order of the
// fields; the error names the first wrong one, never its value.
func (s Status) validate() error {
	jiraNone := s.Jira.Key == ""
	for _, c := range []struct {
		ok    bool
		field string
	}{
		{!s.UpdatedAt.IsZero(), "updated_at"},
		{s.TaskID > 0, "task_id"},
		{jiraNone || jiraKeyRe.MatchString(s.Jira.Key), "jira.key"},
		{jiraNone && s.Jira.Status == "" || !jiraNone && s.Jira.Status.Valid(), "jira.status"},
		{s.Plan.QuestionsAsked >= 0, "plan.questions_asked"},
		{s.Plan.QuestionsAnswered >= 0, "plan.questions_answered"},
		{s.Code.Commits >= 0, "code.commits"},
		{s.Code.Proof.Valid(), "code.proof"},
		{s.Code.Specs >= 0, "code.specs"},
		{s.LocalReview.Rounds >= 0, "local_review.rounds"},
		{s.LocalReview.Verdict.Valid(), "local_review.verdict"},
		{s.LocalReview.NotesOpen >= 0, "local_review.notes_open"},
		{!s.PublishedAt.IsZero(), "published_at"},
		{s.FixLoop.State.Valid(), "fix_loop.state"},
		{s.FixLoop.Cycle >= 0, "fix_loop.cycle"},
		{s.FixLoop.Pushes >= 0, "fix_loop.pushes"},
		{s.FixLoop.ThreadsAnswered >= 0, "fix_loop.threads_answered"},
		{s.Staging.Env == "" || envRe.MatchString(s.Staging.Env), "staging.env"},
		{s.Staging.State.Valid(), "staging.state"},
		{s.Now.Who.Valid(), "now.who"},
		{s.Now.Stage.Valid(), "now.stage"},
	} {
		if !c.ok {
			return fmt.Errorf("%w: %s", ErrInvalid, c.field)
		}
	}
	for i, st := range s.Stagings {
		switch {
		case st.Env != "" && !envRe.MatchString(st.Env):
			return fmt.Errorf("%w: stagings[%d].env", ErrInvalid, i)
		case !st.State.Valid():
			return fmt.Errorf("%w: stagings[%d].state", ErrInvalid, i)
		}
	}
	return nil
}

// The words validate checks by pattern: a Jira key as the factory takes one
// (ABC-123) and a staging environment's slug.
var (
	jiraKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}-[0-9]{1,12}$`)
	envRe     = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)
