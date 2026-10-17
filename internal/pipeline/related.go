package pipeline

// Related PRs (DECISIONS "The judge knows the related PRs"): a round sees
// only its own PR, so two open PRs fixing the same flaky spec, or a PR
// undoing what another just fixed, went unnoticed. Every judge prompt (the
// own pass and the candidates phase alike, and a round's one prompt) names
// related.json: the repository's other open PRs, drafts included, and those
// merged within related_lookback whose changed paths (the poller's lists,
// pr_files) overlap this PR's at the head under review, with what magnum
// knows of them. Numbers, URLs, heads and paths only: PR text is data, never
// in magnum's files (the judge reads a PR itself with gh when it matters).
// A blind replay never gets one: it would tell of later PRs. A PR Codex
// flagged is never named (DECISIONS "related.json never names a
// Codex-flagged PR").
//
// The Related lines say nothing twice and name only live PRs (DECISIONS
// "The Related lines name only live PRs and say nothing twice" and "A
// related PR is named again only after a state change or a new file"): an
// open PR without activity for relatedIdle is left out, and related.json
// leaves out each related PR that the PR's earlier posted reviews named
// (related-all.json in the report directory of the newest posted review's
// head) in the same state and with every path it shares now; with none
// left there is no file.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// RelatedFile is the related PRs' file in the round's report directory.
const RelatedFile = "related.json"

// relatedAllFile is the round's whole related set and what the PR's posted
// reviews named (relatedRecord) beside related.json, which holds only part
// of the set: a later round reads what was named from it.
const relatedAllFile = "related-all.json"

// related.json's caps: the PRs sharing the most paths, and the paths of each.
const (
	maxRelatedPRs   = 10
	maxRelatedPaths = 20
)

// relatedIdle: an open PR without activity for this long is not related
// (reviews named PRs idle for months as open changes to coordinate with).
const relatedIdle = 30 * 24 * time.Hour

// RelatedPRs is related.json.
type RelatedPRs struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose paths were compared
	// FilesTruncated: this PR changes more files than its list holds (100),
	// so a PR sharing only a later one is missing.
	FilesTruncated bool        `json:"files_truncated,omitempty"`
	MergedSince    time.Time   `json:"merged_since"` // merged PRs from then on count (related_lookback)
	Related        []RelatedPR `json:"related"`
	More           int         `json:"more,omitempty"` // related PRs past the cap of ten
}

// RelatedPR is one related PR of RelatedPRs.
type RelatedPR struct {
	Number   int        `json:"number"`
	URL      string     `json:"url"`
	State    string     `json:"state"` // open | merged
	Draft    bool       `json:"draft,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	// ActivityAt is its last activity as the board's UPDATED shows it
	// (store.PR.Activity); an open PR idle for relatedIdle is not related.
	ActivityAt *time.Time `json:"activity_at,omitempty"`
	HeadSHA    string     `json:"head_sha"` // the head its paths were read at
	// Overlap counts the changed paths both PRs share (related_ignore's
	// left out); Paths are the first twenty of them, sorted.
	Overlap int      `json:"overlap"`
	Paths   []string `json:"paths"`
	// FilesTruncated: it changes more files than its list holds (100), so
	// the overlap may be larger.
	FilesTruncated bool `json:"files_truncated,omitempty"`
	// Reviewed: magnum reviewed it; ReviewURL and ReviewVerdict are its last
	// posted review's (store.ReviewSummary), when magnum has its result.
	Reviewed      bool   `json:"reviewed"`
	ReviewURL     string `json:"review_url,omitempty"`
	ReviewVerdict string `json:"review_verdict,omitempty"`
	// FindingsOnPaths counts the findings magnum posted on it, over all its
	// rounds, on the shared paths (all of them, not only Paths).
	FindingsOnPaths int `json:"findings_on_paths"`
	id              int64
	shared          []string
}

// relatedPRs is the candidates whose paths overlap own's, paths matching an
// ignore glob left out: most shared paths first, then the newest PR; at
// most maxRelatedPRs of them, each with at most maxRelatedPaths paths, and
// more counts the rest.
func relatedPRs(own []string, cands []store.FilesPR, ignore []string) (out []RelatedPR, more int) {
	ignored := func(p string) bool {
		return slices.ContainsFunc(ignore, func(g string) bool { return config.MatchPath(g, p) })
	}
	mine := map[string]bool{}
	for _, p := range own {
		if !ignored(p) {
			mine[p] = true
		}
	}
	for _, c := range cands {
		var shared []string
		for _, p := range c.Files.Paths {
			if mine[p] {
				shared = append(shared, p)
			}
		}
		if len(shared) == 0 {
			continue
		}
		slices.Sort(shared)
		shared = slices.Compact(shared)
		r := RelatedPR{Number: c.Number, URL: c.URL, State: "open", Draft: c.IsDraft, ActivityAt: c.ActivityAt, HeadSHA: c.Files.HeadSHA,
			Overlap: len(shared), Paths: shared[:min(len(shared), maxRelatedPaths)], FilesTruncated: c.Files.Truncated,
			Reviewed: store.Deref(c.ReviewedSHA) != "", id: c.ID, shared: shared}
		if c.GHState == store.GHMerged {
			r.State, r.MergedAt, r.Draft = "merged", c.MergedAt, false
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RelatedPR) int {
		return cmp.Or(cmp.Compare(b.Overlap, a.Overlap), cmp.Compare(b.Number, a.Number))
	})
	if len(out) > maxRelatedPRs {
		more = len(out) - maxRelatedPRs
		out = out[:maxRelatedPRs]
	}
	return out, more
}

// related computes the round's whole related set; an empty Related when
// the PR has no file list for the head under review. A PR Codex flagged
// (store.KVPRCodexFlag) is never in it: its content must not reach another
// PR's agents, and a flag that cannot be read leaves the round without
// related PRs.
func (rd *round) related(ctx context.Context) (RelatedPRs, error) {
	in := rd.in
	now := rd.r.now()
	since := now.Add(-in.Related.Lookback).UTC()
	out := RelatedPRs{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), HeadSHA: in.TargetSHA, MergedSince: since}
	own, ok, err := rd.r.Store.PRFilesOf(ctx, in.PR.ID)
	if err != nil || !ok || own.HeadSHA != in.TargetSHA {
		return out, err
	}
	out.FilesTruncated = own.Truncated
	cands, err := rd.r.Store.PRsWithFiles(ctx, in.PR.RepoID, in.PR.ID, since, now.Add(-relatedIdle))
	if err != nil {
		return out, err
	}
	flagged, err := rd.r.Store.CodexFlaggedPRs(ctx)
	if err != nil {
		return out, err
	}
	cands = slices.DeleteFunc(cands, func(c store.FilesPR) bool { return flagged[c.ID] })
	out.Related, out.More = relatedPRs(own.Paths, cands, in.Related.Ignore)
	if len(out.Related) == 0 {
		return out, nil
	}
	ids := make([]int64, len(out.Related))
	for i, r := range out.Related {
		ids[i] = r.id
	}
	reviews, err := rd.r.Store.LastReviewSummaries(ctx, ids)
	if err != nil {
		return out, err
	}
	findings, err := rd.r.Store.PostedFindingPaths(ctx, ids)
	if err != nil {
		return out, err
	}
	for i := range out.Related {
		r := &out.Related[i]
		if s, ok := reviews[r.id]; ok {
			r.Reviewed, r.ReviewURL, r.ReviewVerdict = true, s.URL, s.Verdict
		}
		for _, p := range r.shared {
			r.FindingsOnPaths += findings[r.id][p]
		}
	}
	return out, nil
}

// toldPR is one related PR as the PR's posted reviews named it: its state
// when last named (open with its draft flag, or merged; closed once a
// round found it closed) and every path it shared in any of them. Its JSON
// is a part of RelatedPR's, so the RelatedPR entries of a record from
// before toldPR read as toldPRs.
type toldPR struct {
	Number int      `json:"number"`
	State  string   `json:"state"` // open | merged | closed
	Draft  bool     `json:"draft,omitempty"`
	Paths  []string `json:"paths"`
}

// stateClosed is a toldPR's state once a round found it closed: a closed
// PR is never related, and one reopened is named again.
const stateClosed = "closed"

// relatedRecord is related-all.json: the whole related set of the round's
// latest judge prompt, whatever its related.json named of it, and what the
// PR's posted reviews named before and after it.
type relatedRecord struct {
	Round   int    `json:"round"`
	HeadSHA string `json:"head_sha"`
	// Base is what the PR's posted reviews before the round named, which
	// related.json leaves out when unchanged: a later round that finds the
	// record replaced on its posted review's head (a later prompt of the
	// round, a round that posted nothing) takes it again. A record from
	// before toldPR held the round before's whole set here.
	Base []toldPR `json:"base"`
	// Told is Base with the round's whole set (each named now or before):
	// what a later round counts as named once the round posted. Absent from
	// a record from before it.
	Told    []toldPR    `json:"told"`
	Related []RelatedPR `json:"related"`
}

// readRelatedSet reads the related set recorded in dir: related-all.json,
// else related.json, which held the whole set before the record existed
// (its round 0 is never the current one); ok is false with neither.
func readRelatedSet(dir string) (rec relatedRecord, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, relatedAllFile))
	if err == nil {
		if err := json.Unmarshal(b, &rec); err != nil {
			return relatedRecord{}, false, fmt.Errorf("%s: %w", relatedAllFile, err)
		}
		return rec, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return relatedRecord{}, false, err
	}
	b, err = os.ReadFile(filepath.Join(dir, RelatedFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return relatedRecord{}, false, nil
	case err != nil:
		return relatedRecord{}, false, err
	}
	var old RelatedPRs
	if err := json.Unmarshal(b, &old); err != nil {
		return relatedRecord{}, false, fmt.Errorf("%s: %w", RelatedFile, err)
	}
	return relatedRecord{HeadSHA: old.HeadSHA, Related: old.Related}, true, nil
}

// relatedBase is what the PR's posted reviews before the round named: the
// record on the head of the newest of them that has one, its Told, or its
// Base when a later round replaced the record there (the round's own
// earlier prompt, a round that posted nothing: that Base came from the
// same posted reviews). The registry's runs find the posted reviews, not
// the previous review's head, which a merge of the base moves without a
// round. nil, every related PR new, before the PR's first posted review.
func (rd *round) relatedBase(ctx context.Context) ([]toldPR, error) {
	runs, err := rd.r.Store.RunsByPR(ctx, rd.in.PR.ID)
	if err != nil {
		return nil, err
	}
	read := map[string]bool{}
	for _, run := range slices.Backward(runs) {
		if run.Round >= rd.in.Round || run.State != store.RunVerified || store.Deref(run.Outcome) != OutcomePosted || read[run.TargetSHA] {
			continue
		}
		read[run.TargetSHA] = true
		rec, ok, err := readRelatedSet(rd.r.Layout.ReviewDir(rd.owner, rd.name, rd.in.PR.Number, run.TargetSHA))
		switch {
		case err != nil:
			return nil, err
		case !ok:
			continue
		case rec.Round > run.Round:
			return rec.Base, nil
		case rec.Told == nil: // a record from before Told, or one with nothing related
			return toldAfter(rec.Base, rec.Related), nil
		}
		return rec.Told, nil
	}
	return nil, nil
}

// markClosed records as closed each open PR of told that rel lacks and the
// registry knows closed, so that it is named again once reopened.
func (rd *round) markClosed(ctx context.Context, told []toldPR, rel []RelatedPR) ([]toldPR, error) {
	out := slices.Clone(told)
	for i, t := range out {
		if t.State != "open" || slices.ContainsFunc(rel, func(r RelatedPR) bool { return r.Number == t.Number }) {
			continue
		}
		pr, err := rd.r.Store.PRByRepoNumber(ctx, rd.in.PR.RepoID, t.Number)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, err
		case pr.GHState == store.GHClosed:
			out[i].State, out[i].Draft = stateClosed, false
		}
	}
	return out, nil
}

// sharedPaths are the paths r shares: all of them for a PR the round
// related, the first maxRelatedPaths for one read back from a file.
func (r RelatedPR) sharedPaths() []string {
	if r.shared != nil {
		return r.shared
	}
	return r.Paths
}

// toldAfter is told with each PR of rel named: its state now, and its
// paths added to those named before; sorted by number.
func toldAfter(told []toldPR, rel []RelatedPR) []toldPR {
	out := slices.Clone(told)
	for _, r := range rel {
		i := slices.IndexFunc(out, func(t toldPR) bool { return t.Number == r.Number })
		if i < 0 {
			out = append(out, toldPR{Number: r.Number})
			i = len(out) - 1
		}
		paths := slices.Concat(out[i].Paths, r.sharedPaths())
		slices.Sort(paths)
		out[i].State, out[i].Draft, out[i].Paths = r.State, r.Draft, slices.Compact(paths)
	}
	slices.SortFunc(out, func(a, b toldPR) int { return cmp.Compare(a.Number, b.Number) })
	return out
}

// newRelated keeps the related PRs of rel that told does not name the same
// way (namedAlike); n counts the others.
func newRelated(rel []RelatedPR, told []toldPR) (out []RelatedPR, n int) {
	for _, r := range rel {
		if slices.ContainsFunc(told, func(t toldPR) bool { return namedAlike(t, r) }) {
			n++
			continue
		}
		out = append(out, r)
	}
	return out, n
}

// namedAlike: t names r's PR in its state now (open or merged, the draft
// flag) with every path it shares now. A PR merged, closed and reopened,
// made ready or turned back into a draft, or sharing a path no earlier
// review named, is named again; a new head, fewer shared paths, activity,
// reviews and findings are not news.
func namedAlike(t toldPR, r RelatedPR) bool {
	return t.Number == r.Number && t.State == r.State && t.Draft == r.Draft &&
		!slices.ContainsFunc(r.sharedPaths(), func(p string) bool { return !slices.Contains(t.Paths, p) })
}

// writeRelatedJSON writes v indented to path.
func writeRelatedJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// addRelated records the round's related set (related-all.json) for the
// judge prompt about to go out, and writes related.json and names it in jd
// when a related PR is left that the PR's posted reviews did not name the
// same way (relatedBase, namedAlike); with none left, no related.json is
// there. A blind replay gets none. A failure leaves the prompt without it
// (a warning): the review goes on.
func (rd *round) addRelated(ctx context.Context, jd *agents.JudgeData) {
	if rd.in.Blind || rd.r.Store == nil {
		return
	}
	rel, err := rd.related(ctx)
	var base []toldPR
	if err == nil {
		base, err = rd.relatedBase(ctx)
	}
	if err == nil {
		base, err = rd.markClosed(ctx, base, rel.Related)
	}
	if err == nil {
		err = writeRelatedJSON(filepath.Join(rd.dir, relatedAllFile), relatedRecord{Round: rd.in.Round, HeadSHA: rd.in.TargetSHA,
			Base: base, Told: toldAfter(base, rel.Related), Related: rel.Related})
	}
	path := filepath.Join(rd.dir, RelatedFile)
	told := 0
	if err == nil {
		if rel.Related, told = newRelated(rel.Related, base); len(rel.Related) > 0 {
			err = writeRelatedJSON(path, rel)
		} else if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) { // an earlier round's on this head
			err = nil
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.related", fmt.Sprintf("could not list the related PRs; the judge goes without: %v", err), nil)
		}
		return
	}
	if len(rel.Related) == 0 {
		if told > 0 {
			rd.event(ctx, "info", "round.related", fmt.Sprintf("%d related PR(s) on the same paths, each named before in its state and paths: no %s", told, RelatedFile),
				map[string]any{"related": 0, "told": told})
		}
		return
	}
	jd.RelatedPRs = path
	nums := make([]string, len(rel.Related))
	for i, r := range rel.Related {
		nums[i] = fmt.Sprintf("#%d", r.Number)
	}
	msg := fmt.Sprintf("%s: %d related PR(s) on the same paths: %s", RelatedFile, len(rel.Related)+rel.More, strings.Join(nums, ", "))
	if told > 0 {
		msg += fmt.Sprintf("; %d named before in their state and paths left out", told)
	}
	rd.event(ctx, "info", "round.related", msg, map[string]any{"related": len(rel.Related), "more": rel.More, "told": told, "file": path})
}
