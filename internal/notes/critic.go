package notes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
)

// A critique (DECISIONS "The notes critic decides curation proposals")
// works in a run directory under the notes root (Repo.Critic()/<run>):
// magnum copies there what the critic reads, because its agent may not read
// outside its working directory, and the critic writes its decision next
// to the copies. The critic never touches the live notes; magnum acts on
// its decision (apply, reject or re-curate).
const (
	criticReview          = "review.json"     // what `magnum notes <repo> --review --json` prints
	criticRejections      = "rejections.json" // why the repository's last proposals were declined
	criticCurrent         = "current.md"      // the notes now (read only)
	criticCurrentHarness  = "current"         // the harness now (read only)
	criticProposed        = "proposed.md"     // the proposed notes
	criticProposedHarness = "proposed"        // the proposed harness
	criticChanges         = "changes.json"    // the curator's changes.json
	criticDecision        = "decision.json"   // the critic's answer
)

// CriticDirName is the directory under the notes root that holds every
// critique's run directory (<notes>/.critic/<owner>/<repo>/<run>).
const CriticDirName = ".critic"

// Critic is the directory holding r's critiques.
func (r Repo) Critic() string { return filepath.Join(r.Root, CriticDirName, r.Owner, r.Name) }

// maxDecisionBytes bounds the critic's decision file: a word, a fingerprint,
// a one-line reason and two short lists.
const maxDecisionBytes = 64 << 10

// Bounds on the decision's lists: free text kept with the decision for the
// operator, so each item is one line.
const (
	MaxDecisionItems = 60
	maxItemRunes     = 300
)

// MaxReasonRunes bounds a one-line reason: a curator's for a change, the
// critic's for its decision.
const MaxReasonRunes = maxReasonRunes

// CriticRun is one critique's run directory.
type CriticRun struct{ Dir string }

// Review is review.json, the proposal as `magnum notes <repo> --review
// --json` prints it.
func (c CriticRun) Review() string { return filepath.Join(c.Dir, criticReview) }

// Rejections is rejections.json, why the repository's last proposals were
// declined, and by whom.
func (c CriticRun) Rejections() string { return filepath.Join(c.Dir, criticRejections) }

// Current is current.md, a copy of the notes now.
func (c CriticRun) Current() string { return filepath.Join(c.Dir, criticCurrent) }

// CurrentHarness is current/, a copy of the harness now.
func (c CriticRun) CurrentHarness() string { return filepath.Join(c.Dir, criticCurrentHarness) }

// Proposed is proposed.md, a copy of the proposed notes.
func (c CriticRun) Proposed() string { return filepath.Join(c.Dir, criticProposed) }

// ProposedHarness is proposed/, a copy of the proposed harness.
func (c CriticRun) ProposedHarness() string { return filepath.Join(c.Dir, criticProposedHarness) }

// Changes is changes.json, the curator's account of every section and file.
func (c CriticRun) Changes() string { return filepath.Join(c.Dir, criticChanges) }

// Decision is decision.json, the critic's answer.
func (c CriticRun) Decision() string { return filepath.Join(c.Dir, criticDecision) }

// PrepareCritic makes a new run directory dir holding live (the notes as
// current.md, the harness as current/), proposed (proposed.md and
// proposed/), the proposal's changes.json, the review data (review.json)
// and the earlier rejections (rejections.json). The decision file is the
// critic's to write.
func PrepareCritic(dir string, live, proposed State, changes, review, rejections []byte) (CriticRun, error) {
	run := CriticRun{Dir: dir}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return run, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return run, err
	}
	for _, f := range []struct {
		dir   string
		files []Blob
	}{{run.CurrentHarness(), live.Files}, {run.ProposedHarness(), proposed.Files}} {
		if err := os.Mkdir(f.dir, 0o700); err != nil {
			return run, err
		}
		if err := writeTree(f.dir, f.files); err != nil {
			return run, err
		}
	}
	if len(changes) == 0 {
		changes = []byte("{}")
	}
	for _, f := range []struct {
		path string
		body []byte
	}{{run.Current(), live.Notes}, {run.Proposed(), proposed.Notes}, {run.Review(), review}, {run.Rejections(), rejections},
		{run.Changes(), changes}} {
		if err := fsx.WriteFileAtomic(f.path, f.body, 0o600); err != nil {
			return run, err
		}
	}
	return run, nil
}

// The critic's decisions.
const (
	DecisionApply    = "apply"    // apply the proposal as reviewed
	DecisionReject   = "reject"   // reject it, with a reason the next curation reads
	DecisionRecurate = "recurate" // supersede it: a new curation from the notes now reads it and the reason
)

// Decision is the critic's decision.json. Target is the fingerprint of the
// state it reviewed (the prompt gives it), which binds an apply to that
// state. Losses lists the removed lines the critic accepts losing and
// Checked what it examined: free text the registry keeps with the decision
// for the operator, which magnum interprets in no other way.
type Decision struct {
	Decision string   `json:"decision"`
	Target   string   `json:"target"`
	Reason   string   `json:"reason"`
	Losses   []string `json:"losses"`
	Checked  []string `json:"checked"`
}

// ReadDecision reads the critic's decision.json in run: a regular file of
// the run directory (readRegular: what an agent leaves is not trusted), one
// JSON value, no unknown fields. Problems are worded for a nudge (the file's
// name and the parse error, never its content); missing reports that the
// file was not written.
func ReadDecision(run CriticRun) (d Decision, missing bool, problems []string) {
	root, err := os.OpenRoot(run.Dir)
	if err != nil {
		return d, false, []string{"the critique's directory cannot be read: " + err.Error()}
	}
	defer root.Close()
	b, err := readRegular(root, criticDecision, maxDecisionBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return d, true, []string{criticDecision + " was not written"}
	case err != nil:
		return d, false, []string{readProblem(criticDecision, err)}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Decision{}, false, []string{criticDecision + " is not valid: " + err.Error()}
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Decision{}, false, []string{criticDecision + " is not valid: more than one JSON value"}
	}
	return d, false, nil
}

// DecisionCheck is what ValidateDecision compares a decision with.
type DecisionCheck struct {
	// Target is the fingerprint the prompt gave: the state the critic
	// reviewed.
	Target string
	// Conflict: the notes changed since the proposal was made and its merge
	// with their changes conflicts. Lossy: what applying it would write loses
	// lines of the notes now (CheckPreserved). Either makes apply invalid and
	// recurate valid.
	Conflict, Lossy bool
	// Leaks names what is wrong with the reason (a pull request, a login, a
	// branch, a probe file, a repository, a secret or a home path): phrases
	// to report, nil = none. Nil checks nothing.
	Leaks func(text string) []string
}

// ValidateDecision returns what is wrong with decision d (nil = valid), in
// words fit for a nudge: the decision is one of the three; the target is
// the fingerprint the prompt gave; a proposal that cannot be applied (its
// stale merge conflicts, or it would lose lines) is rejected or re-curated,
// and only such a proposal is re-curated; reject and recurate need a
// reason; the reason is one line of at most maxReasonRunes runes and passes
// c.Leaks; losses and checked hold at most MaxDecisionItems one-line items
// with no secret and no home directory path.
func ValidateDecision(d Decision, c DecisionCheck) []string {
	var problems []string
	blocked := c.Conflict || c.Lossy
	switch d.Decision {
	case DecisionApply:
		switch {
		case c.Conflict:
			problems = append(problems, "apply is not valid: the notes changed since the proposal was made and its merge conflicts; reject or recurate")
		case c.Lossy:
			problems = append(problems, "apply is not valid: the proposal would lose lines of the notes (review.json's not_preserved); reject or recurate")
		}
	case DecisionRecurate:
		if !blocked {
			problems = append(problems, "recurate is valid only for a proposal that cannot be applied (a conflicting merge, or lines it would lose); apply or reject")
		}
	case DecisionReject:
	default:
		problems = append(problems, `"decision" must be apply, reject or recurate`)
	}
	if strings.TrimSpace(d.Target) != c.Target {
		problems = append(problems, fmt.Sprintf(`"target" must be the fingerprint of the state you reviewed, as the prompt gives it: %s`, c.Target))
	}
	reason := strings.TrimSpace(d.Reason)
	switch {
	case reason == "" && (d.Decision == DecisionReject || d.Decision == DecisionRecurate):
		problems = append(problems, d.Decision+` needs a "reason"`)
	case strings.ContainsAny(reason, "\r\n"):
		problems = append(problems, `"reason" must be one line`)
	case utf8.RuneCountInString(reason) > maxReasonRunes:
		problems = append(problems, fmt.Sprintf(`"reason" is over %d characters`, maxReasonRunes))
	}
	if c.Leaks != nil && reason != "" {
		for _, l := range c.Leaks(reason) {
			problems = append(problems, `"reason" `+l)
		}
	}
	for _, list := range []struct {
		name  string
		items []string
	}{{"losses", d.Losses}, {"checked", d.Checked}} {
		if len(list.items) > MaxDecisionItems {
			problems = append(problems, fmt.Sprintf("%q holds more than %d items", list.name, MaxDecisionItems))
		}
		for _, it := range list.items {
			switch {
			case strings.ContainsAny(it, "\r\n"), utf8.RuneCountInString(it) > maxItemRunes:
				problems = append(problems, fmt.Sprintf("each item of %q is one line of at most %d characters", list.name, maxItemRunes))
			case execx.Redact(it) != it, homePathRe.MatchString(it):
				problems = append(problems, fmt.Sprintf("an item of %q holds a secret or a home directory path", list.name))
			default:
				continue
			}
			break
		}
	}
	return problems
}

// ReasonLeaks applies the leak rules to a decider's one-line reason that
// the next curation reads (the critic's): CheckLeaks (a pull request or
// issue number, one of c's branches, a secret, a home directory path), and
// a probe file name. nil = none.
func ReasonLeaks(text string, c Check) []string {
	out := CheckLeaks(text, c)
	for _, m := range pathTokenRe.FindAllString(text, -1) {
		if tok := strings.Trim(m, "."); probeFileName(tok) {
			out = append(out, "names a probe file")
			break
		}
	}
	return out
}
