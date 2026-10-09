package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// maxFileFetchesPerRepo caps the ListFiles fetches one repository's poll
// makes; PRs over the cap (or after a failed fetch) are asked again on a
// later poll.
const maxFileFetchesPerRepo = 20

// mutedReason is what eligibility.Classify says about a muted PR.
const mutedReason = "muted"

// fileLister is the optional GitHub capability skip_paths needs: the files a
// PR changes (*github.Client has it; a client without it never skips).
type fileLister interface {
	ListFiles(ctx context.Context, owner, repo string, number int) ([]string, bool, error)
}

var _ fileLister = (*github.Client)(nil)

// filesWatchStates are the states in which a PR is (re)checked for a missing
// file list, so a fetch that failed earlier heals on a later poll.
var filesWatchStates = []string{store.PRQueued, store.PRRereviewPending, store.PRIneligible}

// kvPRFiles is the kv key holding changedFiles for a PR (same naming as
// store.KVPR*).
func kvPRFiles(id int64) string { return fmt.Sprintf("pr.%d.files", id) }

// changedFiles is the cached file list of one head commit.
type changedFiles struct {
	Head     string   `json:"head"`
	Complete bool     `json:"complete"`
	Files    []string `json:"files"`
}

// cachedFiles reads the PR's cached file list; ok is false when there is none
// (or it is unreadable).
func (e *Engine) cachedFiles(ctx context.Context, prID int64) (c changedFiles, ok bool) {
	v, found := e.getKV(ctx, kvPRFiles(prID))
	if !found {
		return changedFiles{}, false
	}
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		return changedFiles{}, false
	}
	return c, true
}

// fetchChangedFiles caches the files pr changes at its current head, so a
// poll never fetches the same head twice. It does nothing when the watch has
// no skip_paths, the cache already holds pr.HeadSHA, gh cannot list files or
// the repository's budget is spent. A failure is not cached: it is reported
// once per distinct error, ends the repository's fetches for this poll and is
// retried by the next one. It reports whether a fresh list was stored (the
// caller then classifies the PR again).
func (e *Engine) fetchChangedFiles(ctx context.Context, gh GitHub, w config.Watch, repo store.Repo, pr store.PR, budget *int) bool {
	if len(w.SkipPaths) == 0 {
		return false
	}
	lister, ok := gh.(fileLister)
	if !ok {
		return false
	}
	if c, ok := e.cachedFiles(ctx, pr.ID); ok && c.Head == pr.HeadSHA {
		return false
	}
	if *budget <= 0 {
		return false
	}
	*budget--
	full := repo.FullName()
	files, complete, err := lister.ListFiles(ctx, repo.Owner, repo.Name, pr.Number)
	if err != nil {
		*budget = 0
		if e.changed("files:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.files_error", fmt.Sprintf("changed files of #%d: %v", pr.Number, err), nil)
		}
		return false
	}
	e.changed("files:"+full, "")
	b, err := json.Marshal(changedFiles{Head: pr.HeadSHA, Complete: complete, Files: files})
	if err != nil {
		e.log.Warn("encode changed files", "pr", pr.ID, "err", err)
		return false
	}
	e.setKV(ctx, kvPRFiles(pr.ID), string(b))
	return true
}

// pathSkipReason is why skip_paths rejects pr ("" = it does not): the cached
// file list is for the PR's current head, complete, and every file matches a
// glob of the watch. It only reads the cache; the poller fills it.
func (e *Engine) pathSkipReason(ctx context.Context, w config.Watch, pr store.PR) string {
	if len(w.SkipPaths) == 0 {
		return ""
	}
	c, ok := e.cachedFiles(ctx, pr.ID)
	if !ok || c.Head != pr.HeadSHA || !c.Complete || !w.PathsSkipped(c.Files) {
		return ""
	}
	return fmt.Sprintf("skip_paths: all %d changed files match", len(c.Files))
}

// wantsFiles reports whether the poller should make sure the PR's file list
// is cached (fetchChangedFiles skips a head already cached): the PR is new,
// its head moved, or it waits (or is ineligible and may be queued again),
// and the watch's other filters let it through. A PR they reject anyway
// costs no fetch.
func (e *Engine) wantsFiles(ctx context.Context, w config.Watch, res store.PRUpsert, now time.Time) bool {
	if len(w.SkipPaths) == 0 || res.PR.Forced {
		return false
	}
	if !res.New && !res.HeadChanged && !slices.Contains(filesWatchStates, res.PR.State) {
		return false
	}
	return eligibility.Classify(w, e.factsFor(ctx, res.PR, w, now)).Eligible && safeBase(deref(res.PR.BaseRef))
}

// classify is eligibility.Classify plus what only the engine knows: a PR
// muted by `magnum ignore` keeps the reason "ignored", an eligible PR Codex
// flagged is rejected (codex_flag.go), so is one whose base branch name a
// shell would read (safeBase, poll.go), and one whose changed files all
// match the watch's skip_paths. Callers
// keep forced PRs out of it, so a manual review still runs (its prompts
// refuse such a base).
func (e *Engine) classify(ctx context.Context, w config.Watch, pr store.PR, now time.Time) eligibility.Decision {
	dec := eligibility.Classify(w, e.factsFor(ctx, pr, w, now))
	if !dec.Eligible {
		if dec.Reason == mutedReason && deref(pr.SkipReason) == SkipIgnored {
			dec.Reason = SkipIgnored
		}
		return dec
	}
	if f, ok := e.codexFlag(ctx, pr.ID); ok { // codex_flag.go
		return eligibility.Decision{Reason: f.SkipReason()}
	}
	if !safeBase(deref(pr.BaseRef)) {
		return eligibility.Decision{Reason: reasonUnsafeBase}
	}
	if reason := e.pathSkipReason(ctx, w, pr); reason != "" {
		return eligibility.Decision{Reason: reason}
	}
	return dec
}
