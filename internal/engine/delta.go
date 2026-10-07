package engine

// Pushes that need no re-review (trivial.go classifies them): on one PR a
// push that only changed YAML comment lines cost a 16-minute re-review by
// three agents, whose judge itself wrote that the commit changes only
// comments. Before a re-review is queued, and before a review whose head
// moved during its round asks for one, the poller compares the reviewed
// commit with the new head; a trivial delta moves reviewed_sha to the head,
// carrying the last verdict, and queues nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRTrivial holds the last push magnum skipped as trivial for a PR
// (TrivialSkip as JSON), for the PR card; a review posted afterwards
// deletes it.
func KVPRTrivial(prID int64) string { return fmt.Sprintf("pr.%d.trivial", prID) }

// TrivialSkip is a push magnum did not re-review (KVPRTrivial).
type TrivialSkip struct {
	From    string    `json:"from"`    // the reviewed commit
	To      string    `json:"to"`      // the head the review now stands for
	Classes []string  `json:"classes"` // DeltaClasses the push used
	Files   int       `json:"files"`
	At      time.Time `json:"at"`
}

// ParseTrivialSkip reads a KVPRTrivial value; ok is false for "" or a
// value it cannot read.
func ParseTrivialSkip(s string) (TrivialSkip, bool) {
	var t TrivialSkip
	if s == "" || json.Unmarshal([]byte(s), &t) != nil || t.To == "" {
		return TrivialSkip{}, false
	}
	return t, true
}

// Note is the PR card's line for the skip, e.g. "comment-only push skipped
// (a7b3f8c → 602da9d)".
func (t TrivialSkip) Note() string {
	what := "trivial"
	switch {
	case len(t.Classes) == 1 && t.Classes[0] == DeltaComments:
		what = "comment-only"
	case len(t.Classes) == 1 && t.Classes[0] == DeltaWhitespace:
		what = "whitespace-only"
	case len(t.Classes) == 1 && t.Classes[0] == DeltaDocs:
		what = "docs-only"
	case len(t.Classes) == 1 && t.Classes[0] == DeltaBase:
		what = "base-merge"
	}
	return fmt.Sprintf("%s push skipped (%s → %s)", what, textx.ShortSHA(t.From), textx.ShortSHA(t.To))
}

// KVPRDelta holds the size of a PR's unreviewed delta (DeltaRecord as
// JSON), which the re-review threshold reads ([daemon] rereview_min_lines).
func KVPRDelta(prID int64) string { return fmt.Sprintf("pr.%d.delta", prID) }

// DeltaRecord is the measured delta of a PR from its reviewed commit to a
// head (KVPRDelta). It describes the PR only while From is its reviewed_sha
// and To its head.
type DeltaRecord struct {
	// Version is deltaRecordVersion for a delta measured with the PR's own
	// diff in view (base_merge.go); 0 is a record from before.
	Version int    `json:"version,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	DeltaSize
	// Since is the first push the review does not cover: kept while From
	// stays the reviewed commit.
	Since time.Time `json:"since"`
}

// deltaCheck is one comparison of the reviewed commit with a newer head:
// whether skip_trivial_deltas lets the review stand for it (trivial, with
// the classes it used and its file count) and its size for the re-review
// threshold. measured is false when nothing was compared.
type deltaCheck struct {
	measured bool
	trivial  bool
	classes  []string
	files    int
	size     DeltaSize
	// commits is the push's commit count (GitHub's total_commits); base
	// and rebased name, for the class DeltaBase, the branch the push
	// merged (or, rebased, the one GitHub says it diverged onto).
	commits int
	base    string
	rebased bool
}

// change says what a trivial push did, for events: "changes comments only
// (1 file)", "only merges master (13 commits, the PR's own changes
// unchanged)".
func (dc deltaCheck) change() string {
	if slices.Contains(dc.classes, DeltaBase) {
		verb := "merges " + dc.base
		if dc.rebased {
			verb = "rebases onto " + dc.base
		}
		return fmt.Sprintf("only %s (%d %s, the PR's own changes unchanged)", verb, dc.commits, textx.Plural(dc.commits, "commit", "commits"))
	}
	return fmt.Sprintf("changes %s (%d %s)", DeltaLabel(dc.classes), dc.files, textx.Plural(dc.files, "file", "files"))
}

// checkDelta measures from...to as the watch's poll identity, when the
// watch skips trivial deltas or has a re-review threshold, with the measure
// triage and the rerun share (measureRange): one GitHub call, and when the
// push has a merge commit or diverged from the reviewed commit (a rebase, a
// force push) two more that compare the PR's own diff against base before
// and after it. A push trivial by its own files stands as it is; otherwise
// an unchanged own diff is the class DeltaBase, a changed one gives the size
// of that change, and an incomplete comparison keeps the size of from...to.
// A failure of the first call measures nothing, and nor does a head GitHub
// finds behind the reviewed commit (a force push back to an ancestor: no
// commit and no file to measure, while the review discusses code the push
// dropped): the PR gets its re-review.
func (e *Engine) checkDelta(ctx context.Context, repo store.Repo, w config.Watch, base, from, to string) deltaCheck {
	allowed := e.cfg.TrivialDeltas(&w)
	if len(allowed) == 0 && e.cfg.ThrottleFor(&w).RereviewMinLines <= 0 {
		return deltaCheck{}
	}
	if from == "" || to == "" || from == to {
		return deltaCheck{}
	}
	gh := e.gh(w.PollIdentity)
	if gh == nil {
		return deltaCheck{}
	}
	m, err := e.measureRange(ctx, gh, repo, base, from, to)
	if err != nil {
		e.log.Info("delta: compare failed; the push is re-reviewed", "repo", repo.FullName(), "from", textx.ShortSHA(from), "to", textx.ShortSHA(to), "err", err)
		return deltaCheck{}
	}
	pc := m.push
	if pc.Status == "behind" || pc.Commits == 0 {
		e.log.Info("delta: the head is behind the reviewed commit; the push is re-reviewed", "repo", repo.FullName(),
			"from", textx.ShortSHA(from), "to", textx.ShortSHA(to), "status", pc.Status)
		return deltaCheck{}
	}
	dc := deltaCheck{measured: true, files: len(pc.Files), commits: pc.Commits}
	dc.size, dc.classes, dc.trivial = assessDelta(pc.Files, allowed)
	switch {
	case dc.trivial || !m.ownOK:
	case len(m.own.changed) > 0:
		dc.size = m.own.size
	case slices.Contains(allowed, DeltaBase):
		dc.trivial, dc.classes, dc.base, dc.rebased = true, []string{DeltaBase}, base, pc.Status == "diverged"
	}
	return dc
}

// recordDelta keeps a measured delta from...to for the re-review threshold
// (KVPRDelta). pushedAt is the push that brought it when the reviewed
// commit had no measured delta yet; a record for the same reviewed commit
// keeps its Since.
func (e *Engine) recordDelta(ctx context.Context, prID int64, from, to string, dc deltaCheck, pushedAt time.Time) {
	if !dc.measured {
		return
	}
	rec := DeltaRecord{Version: deltaRecordVersion, From: from, To: to, DeltaSize: dc.size, Since: pushedAt.UTC()}
	if prev, ok := e.deltaRecord(ctx, prID); ok && prev.From == from && !prev.Since.IsZero() {
		rec.Since = prev.Since
	}
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, KVPRDelta(prID), string(b))
	}
}

// deltaRecord reads KVPRDelta.
func (e *Engine) deltaRecord(ctx context.Context, prID int64) (DeltaRecord, bool) {
	v, ok := e.getKV(ctx, KVPRDelta(prID))
	if !ok || v == "" {
		return DeltaRecord{}, false
	}
	var rec DeltaRecord
	if json.Unmarshal([]byte(v), &rec) != nil || rec.From == "" {
		return DeltaRecord{}, false
	}
	return rec, true
}

// deltaFacts fills f's delta from KVPRDelta when it measured f's reviewed
// commit against the PR's head.
func (e *Engine) deltaFacts(ctx context.Context, pr store.PR, f *eligibility.PRFacts) {
	if f.ReviewedSHA == "" {
		return
	}
	rec, ok := e.deltaRecord(ctx, pr.ID)
	if !ok || rec.From != f.ReviewedSHA || rec.To != pr.HeadSHA {
		return
	}
	f.DeltaKnown, f.DeltaReadable = rec.Complete, rec.Readable()
	f.DeltaLines, f.DeltaAddedFiles, f.DeltaSince = rec.Lines, rec.AddedFiles, rec.Since
}

// settlePush handles a push to a reviewed PR before a re-review is queued:
// a trivial one (checkDelta) settles the PR, reviewed at the new head with
// the last verdict, nothing queued and no approval dismissed (the approval
// still describes the code); any other measured delta is kept for the
// re-review threshold. It reports whether it settled the PR.
func (e *Engine) settlePush(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR) (bool, error) {
	from := deref(pr.ReviewedSHA)
	if from == "" || from == pr.HeadSHA {
		return false, nil
	}
	dc := e.checkDelta(ctx, repo, w, prBase(repo, pr), from, pr.HeadSHA)
	if !dc.trivial {
		e.recordDelta(ctx, pr.ID, from, pr.HeadSHA, dc, e.now())
		return false, nil
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", from)
		u.Set("reviewed_sha", pr.HeadSHA)
		u.Set("skip_reason", nil)
		u.Set("next_eligible_at", nil)
		u.Set("pending_since", nil)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
	})
	if err != nil {
		return true, err
	}
	e.recordTrivial(ctx, repo, pr, TrivialSkip{From: from, To: pr.HeadSHA, Classes: dc.classes, Files: dc.files, At: e.now()},
		fmt.Sprintf("%s → reviewed: the push to %s %s since the review of %s; no re-review, the review stands",
			pr.State, textx.ShortSHA(pr.HeadSHA), dc.change(), textx.ShortSHA(from)))
	return true, nil
}

// recordTrivial keeps a trivial skip for the card (KVPRTrivial) and records
// pr.trivial_delta.
func (e *Engine) recordTrivial(ctx context.Context, repo store.Repo, pr store.PR, t TrivialSkip, msg string) {
	if b, err := json.Marshal(t); err == nil {
		e.setKV(ctx, KVPRTrivial(pr.ID), string(b))
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.trivial_delta", msg,
		map[string]any{"classes": t.Classes, "files": t.Files, "from": t.From, "to": t.To})
}
