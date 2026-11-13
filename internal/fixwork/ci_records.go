package fixwork

import (
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// CIFixRecord is a PR's CI fixes: each start (written before startFix),
// the CI reader's last read (ci_failures.go) and the base's last read.
type CIFixRecord struct {
	Starts []CIFixStart `json:"starts,omitempty"`
	Read   CIFailRead   `json:"read,omitzero"`
	Base   CIBaseRead   `json:"base,omitzero"`
}

// CIFixStart is one CI fix: the head it started at, the failed required
// entries it fixed, when and by whom (autofix, magnum fix), and what it
// ended with (Outcome, "" while it runs or when its end was lost), the
// class word of a cannot (Why) and the head it pushed (Pushed).
type CIFixStart struct {
	Head    string    `json:"head"`
	Entries []string  `json:"entries,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Why     string    `json:"why,omitempty"`
	Pushed  string    `json:"pushed,omitempty"`
}

// Count is the CI fixes that count against ciFixMax: every start but an
// unstarted one (a start whose end was lost counts).
func (r CIFixRecord) Count() int {
	n := 0
	for _, s := range r.Starts {
		if s.Outcome != CIOutcomeUnstarted {
			n++
		}
	}
	return n
}

// AtHead is the newest CI fix at head that counts (zero: none), or the one
// whose fixer said it cannot on the daemon's merge of the parent that head
// is (merged with a class word): the merge is no new chance for the same
// cause.
func (r CIFixRecord) AtHead(head string) CIFixStart {
	for _, s := range slices.Backward(r.Starts) {
		switch {
		case s.Outcome == CIOutcomeUnstarted:
		case s.Head == head, s.Outcome == CIOutcomeMerged && s.Why != "" && s.Pushed == head:
			return s
		}
	}
	return CIFixStart{}
}

// LastPushed is the newest CI fix that pushed or whose parent merge moved
// the head, and its number among the ones that count (zero: none).
func (r CIFixRecord) LastPushed() (CIFixStart, int) {
	n, last, at := 0, CIFixStart{}, 0
	for _, s := range r.Starts {
		if s.Outcome == CIOutcomeUnstarted {
			continue
		}
		n++
		if s.Pushed != "" && (s.Outcome == CIOutcomePushed || s.Outcome == CIOutcomeMerged) {
			last, at = s, n
		}
	}
	return last, at
}

// The outcomes of a CI fix (CIFixStart.Outcome): it never ran a fixer (the
// fix job refused it at once: it does not count), it pushed, the daemon
// merged the parent and the fixer pushed nothing (CI runs on the merge;
// Why: the class word of a fixer that said it cannot there), the fixer
// said it cannot (Why: its class word), the run failed, was stopped or
// lost in a restart.
const (
	CIOutcomeUnstarted = "unstarted"
	CIOutcomePushed    = "pushed"
	CIOutcomeMerged    = "merged"
	CIOutcomeCannot    = "cannot"
	CIOutcomeFailed    = "failed"
	CIOutcomeStopped   = "stopped"
	CIOutcomeLost      = "lost"
)

// CIFailRead is the reader's record of a read at Head (kvPRCIFix's Read):
// the failed set it read for (Key, ciReadKey), when, the files (File, the
// JSON's path), the JSON's digest and its counts (jobs listed, failed
// steps, log lines and bytes kept), and a read that failed (Unread, with
// actionsNotGranted's words in UnreadWhy when GitHub would not mint the
// batch's token) or a run still running (Running), which the look tries
// again settle_max after At.
type CIFailRead struct {
	Head      string    `json:"head"`
	Key       string    `json:"key"`
	At        time.Time `json:"at"`
	File      string    `json:"file,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	Jobs      int       `json:"jobs,omitempty"`
	Steps     int       `json:"steps,omitempty"`
	Lines     int       `json:"lines,omitempty"`
	Bytes     int       `json:"bytes,omitempty"`
	Unread    bool      `json:"unread,omitempty"`
	UnreadWhy string    `json:"unread_why,omitempty"`
	// Running: the newest attempt of a failed check's run has not ended (a
	// re-run someone started by hand of a run the rollup shows; one of a
	// run the look named is Moved): nothing was read, and the look reads
	// again settle_max after At.
	Running bool `json:"running,omitempty"`
	// Runs are the runs the read read, at their attempts; Checked is when
	// the look last found the runs it named (magnum's own, ciShape3) still
	// at those attempts before a start (ciRunMoved).
	Runs    []CIReadRun `json:"runs,omitempty"`
	Checked time.Time   `json:"checked,omitzero"`
	// Moved, never recorded: the run that now speaks for a run the look
	// named (magnum's start or re-run) moved past its failure (ciRunNow):
	// nothing was read, and the look records the run in the start or the
	// re-run instead (noteRunMoved).
	Moved *github.WorkflowRun `json:"-"`
}

// CIReadRun is a run the CI reader read, and the attempt it read.
type CIReadRun struct {
	ID      int64 `json:"id"`
	Attempt int   `json:"attempt"`
}

// CIBaseRead is the base's read for the failed set Key at Head: the base
// tip it read from (Tip), each failed check's word (Checks, in the failed
// set's order), when, and whether the read failed (tried again settle_max
// later).
type CIBaseRead struct {
	Head   string        `json:"head"`
	Key    string        `json:"key"`
	Tip    string        `json:"tip"`
	Checks []CIBaseCheck `json:"checks,omitempty"`
	At     time.Time     `json:"at"`
	Failed bool          `json:"failed,omitempty"`
}

// Word is the base's word on the whole failed set and the commit that
// decided: failed where one failed, running where one still runs, never
// where one never ran, passed where every one passed.
func (b CIBaseRead) Word() (string, string) {
	for _, w := range []string{CIBaseFailed, CIBaseRunning, CIBaseNever} {
		if i := slices.IndexFunc(b.Checks, func(c CIBaseCheck) bool { return c.State == w }); i >= 0 {
			return w, b.Checks[i].SHA
		}
	}
	if len(b.Checks) == 0 {
		return "", ""
	}
	return CIBasePassed, b.Checks[0].SHA
}

// The base's word on a failed check (CIBaseCheck.State): it passed on the
// newest first-parent commit of the base that finished it, failed there,
// runs on a commit while no commit read finished it, or no commit of the
// ones read ran it.
const (
	CIBasePassed  = "passed"
	CIBaseFailed  = "failed"
	CIBaseRunning = "running"
	CIBaseNever   = "never"
)

// CIBaseCheck is what the base said of a failed check: its name, the
// commit that decided (SHA; "" for never) and the word (State), the worst
// of the names it compared, whose own words are in Each when it compared
// more than one (talkable's Completion skipped beside two failed jobs).
type CIBaseCheck struct {
	Name  string        `json:"name"`
	SHA   string        `json:"sha,omitempty"`
	State string        `json:"state"`
	Each  []CIBaseCheck `json:"each,omitempty"`
}

// Checks is the base's word on each name b compared: Each, or b itself.
func (b CIBaseCheck) Checks() []CIBaseCheck {
	if len(b.Each) > 0 {
		return b.Each
	}
	return []CIBaseCheck{{Name: b.Name, SHA: b.SHA, State: b.State}}
}

// PassedOnBase reports whether the check of name, one b compared, passed
// on the base.
func (b CIBaseCheck) PassedOnBase(name string) bool {
	return slices.ContainsFunc(b.Checks(), func(e CIBaseCheck) bool { return e.Name == name && e.State == CIBasePassed })
}

// CIStart is the look's start of a PR's CI workflow at Head: the workflow
// file, when it was asked (At: written before the call, so a start whose
// end was lost still counts), why it failed (Failed: GitHub's answer as a
// status, "GitHub answered HTTP 422", never its message; a 403 adds
// cannotStartWorkflows' words) or why magnum did not start it (Refused, At
// zero), and its run as the poll last read it (Run: nil, none seen). Moved
// is when the look found that a run that failed was run again (a re-run by
// hand, someone's later start at the head: noteRunMoved), which Run is
// since; the poll waits for it as for the start's (awaitCIRuns).
type CIStart struct {
	Head     string    `json:"head"`
	Workflow string    `json:"workflow"`
	At       time.Time `json:"at,omitzero"`
	Failed   string    `json:"failed,omitempty"`
	Refused  string    `json:"refused,omitempty"`
	Run      *CIRun    `json:"run,omitempty"`
	Moved    time.Time `json:"moved,omitzero"`
}

// CIRun is the newest run of a start's workflow that a dispatch started at
// the head since the start (dispatchedRun): its id (0 in a record of an
// earlier build), GitHub's status of it (queued, in_progress, completed,
// ...) and, once it ended, its conclusion.
type CIRun struct {
	ID         int64  `json:"id,omitempty"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// Running reports whether r is a run that has not ended (queued, waiting
// or in progress); false for nil, no run.
func (r *CIRun) Running() bool { return r != nil && r.Status != "completed" }

// Ended reports whether r is a run that ended; false for nil, no run.
func (r *CIRun) Ended() bool { return r != nil && r.Status == "completed" }

// CIRerun is what the look read of the runs at Head (at ReadAt) and did
// about them: Run is the run that started no job (nil: none); Queued a run
// of the head that had not ended at the read (its id, QueuedStatus its
// status then), and Unread a read that failed (UnreadWhy: GitHub would not
// mint the token of the read, actionsNotGranted's words), either of which
// the look reads again settle_max after ReadAt; At when the look asked GitHub to
// re-run Run (written before the call, so a re-run whose answer was lost
// still counts), Failed why GitHub refused (its status, never its message;
// a 403 adds cannotStartWorkflows' words) and Refused why magnum did not
// (At zero); Seen is the re-run's new attempt as the poll last read it
// (nil: none seen), and Moved when the look found that attempt, after it
// failed, run again by hand (noteRunMoved: Seen is that attempt since),
// which the poll waits for as for the re-run's (rerunWaits).
type CIRerun struct {
	Head         string     `json:"head"`
	ReadAt       time.Time  `json:"read_at"`
	Queued       int64      `json:"queued,omitempty"`
	QueuedStatus string     `json:"queued_status,omitempty"`
	Unread       bool       `json:"unread,omitempty"`
	UnreadWhy    string     `json:"unread_why,omitempty"`
	Run          *CINoJob   `json:"run,omitempty"`
	At           time.Time  `json:"at,omitzero"`
	Failed       string     `json:"failed,omitempty"`
	Refused      string     `json:"refused,omitempty"`
	Seen         *CIAttempt `json:"seen,omitempty"`
	Moved        time.Time  `json:"moved,omitzero"`
}

// CINoJob is a run that started no job: its id, its workflow file, the
// attempt that started none, and that attempt's conclusion.
type CINoJob struct {
	ID         int64  `json:"id"`
	Workflow   string `json:"workflow"`
	Attempt    int    `json:"attempt"`
	Conclusion string `json:"conclusion"`
}

// CIAttempt is a newer attempt of a re-run run: its number, GitHub's status
// and, once it ended, its conclusion, and NoJob: it ended with no job
// started.
type CIAttempt struct {
	Attempt    int    `json:"attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	NoJob      bool   `json:"no_job,omitempty"`
}

// Running reports whether a is an attempt that has not ended; false for
// nil, none seen.
func (a *CIAttempt) Running() bool { return a != nil && a.Status != "completed" }

// Ended reports whether a is an attempt that ended; false for nil, none
// seen.
func (a *CIAttempt) Ended() bool { return a != nil && a.Status == "completed" }

// Asked reports whether the look asked GitHub to re-run the head's run and
// GitHub took it.
func (r CIRerun) Asked() bool { return !r.At.IsZero() && r.Failed == "" && r.Run != nil }

// Decided reports whether the look re-ran the head's run or refused to:
// the record that a look and a restart never redo.
func (r CIRerun) Decided() bool { return !r.At.IsZero() || r.Refused != "" }

// ReadDue reports whether the look reads the runs of head (again) at now:
// none read at the head, or settleMax passed since a read that found a run
// that had not ended or that failed.
func (r CIRerun) ReadDue(head string, now time.Time, settleMax time.Duration) bool {
	return r.Head != head || (r.Queued != 0 || r.Unread) && !now.Before(r.ReadAt.Add(settleMax))
}

// RerunWaits reports whether the poll reads the head's runs for r, the
// look's re-run (awaitCIRuns) at now: GitHub took it, and it is less than
// settleMax old (or its attempt by hand that the look found, Moved) or its
// new attempt had not ended at the last read.
func (r CIRerun) RerunWaits(now time.Time, settleMax time.Duration) bool {
	return r.Asked() && (now.Before(later(r.At, r.Moved).Add(settleMax)) || r.Seen.Running())
}

// Named is the run in the verdict's words: "run 42673098082 of ci.yml".
func (r *CINoJob) Named() string { return fmt.Sprintf("run %d of %s", r.ID, r.Workflow) }

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
