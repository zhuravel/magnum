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
// "The Related lines name only live PRs and say nothing twice"): an open PR
// without activity for relatedIdle is left out, and a later round's
// related.json names only the related PRs that the set of the round before
// (related-all.json in the report directory of the head the previous review
// covered) lacked or named with another relation; with none left there is
// no file.

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

// relatedAllFile is the round's whole related set (relatedRecord) beside
// related.json, which a later round may hold only part of: the next round
// compares its set with it.
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

// relatedRecord is related-all.json: the whole related set of the round's
// latest judge prompt, whatever its related.json named of it.
type relatedRecord struct {
	Round   int    `json:"round"`
	HeadSHA string `json:"head_sha"`
	// Base is the set the round compared its own with (the round before's):
	// a later prompt of the same round, or the turn a continue finishes,
	// compares with it again, not with the round's own set.
	Base    []RelatedPR `json:"base"`
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

// relatedBase is the related set the round compares its own with: the Base
// of the round's own record when an earlier prompt of the round (or the
// paused turn a continue finishes) wrote one, in its report directory or in
// the previous head's (before a restart); else, in any round but an initial
// one, the set recorded for the head the previous review covered. nil
// (every related PR is new) for an initial round and for a head without a
// record.
func (rd *round) relatedBase() ([]RelatedPR, error) {
	rec, ok, err := readRelatedSet(rd.dir)
	if err != nil || ok && rec.Round == rd.in.Round {
		return rec.Base, err
	}
	head := rd.previousHead()
	if rd.in.Kind == KindInitial || head == "" {
		return nil, nil
	}
	if dir := rd.r.Layout.ReviewDir(rd.owner, rd.name, rd.in.PR.Number, head); dir != rd.dir {
		if rec, ok, err = readRelatedSet(dir); err != nil {
			return nil, err
		}
	}
	switch {
	case !ok:
		return nil, nil
	case rec.Round == rd.in.Round:
		return rec.Base, nil
	}
	return rec.Related, nil
}

// newRelated keeps the related PRs of rel that base lacks or names with
// another relation (sameRelation); told counts the others.
func newRelated(rel, base []RelatedPR) (out []RelatedPR, told int) {
	for _, r := range rel {
		if slices.ContainsFunc(base, func(b RelatedPR) bool { return sameRelation(b, r) }) {
			told++
			continue
		}
		out = append(out, r)
	}
	return out, told
}

// sameRelation: a and b name one PR related the same way, in the same
// state (open or merged) through the same shared paths. Its new head,
// activity, draft flag, reviews and findings change no relation.
func sameRelation(a, b RelatedPR) bool {
	return a.Number == b.Number && a.State == b.State && a.Overlap == b.Overlap && slices.Equal(a.Paths, b.Paths)
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
// when a related PR is left that the round before did not name the same
// way (relatedBase); with none left, no related.json is there. A blind
// replay gets none. A failure leaves the prompt without it (a warning): the
// review goes on.
func (rd *round) addRelated(ctx context.Context, jd *agents.JudgeData) {
	if rd.in.Blind || rd.r.Store == nil {
		return
	}
	rel, err := rd.related(ctx)
	var base []RelatedPR
	if err == nil {
		base, err = rd.relatedBase()
	}
	if err == nil {
		err = writeRelatedJSON(filepath.Join(rd.dir, relatedAllFile),
			relatedRecord{Round: rd.in.Round, HeadSHA: rd.in.TargetSHA, Base: base, Related: rel.Related})
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
			rd.event(ctx, "info", "round.related", fmt.Sprintf("%d related PR(s) on the same paths, each named before the same way: no %s", told, RelatedFile),
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
		msg += fmt.Sprintf("; %d named before the same way left out", told)
	}
	rd.event(ctx, "info", "round.related", msg, map[string]any{"related": len(rel.Related), "more": rel.More, "told": told, "file": path})
}
