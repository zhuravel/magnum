package pipeline

// The changed files' history (DECISIONS "The reviewers read the changed
// files' history"): in 4 of the 5 known misses the changed file's own
// recent log named the fix the PR undid ("Fix flaky mock generation" above a
// PR that changed the same test cleanup), and no prompt asked for it. Before
// the reviewers, a round writes history.json in its report directory: each
// file the PR changes that exists on its base (at most 40, in git's order,
// the watch's related_ignore paths aside) with its last 8 commits on
// origin/<base> (short SHA, date, subject, and the PR number a squash
// merge's subject ends with), read through gitx in the checkout; a blind
// replay reads them at its merge base, never anything newer. A commit the
// base got after the PR's merge base is marked after_merge_base: a PR passed
// its own tests and failed once merged with a base commit that changed the
// tool it called, without a textual conflict, so the skill runs the
// affected specs on the merged tree. The judge prompts name the file as
// `history`, the claude reviewers in one sentence.
// Paths and subjects come from the base branch's commits: data in the file,
// never in a prompt or an event.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// HistoryFile is the changed files' history in the round's report directory.
const HistoryFile = "history.json"

// history.json's caps, and how many logs git reads at a time.
const (
	maxHistoryFiles   = 40
	maxHistoryCommits = 8
	historyWorkers    = 4
)

// HistoryTimeout bounds reading the history: it is a hint, and the
// reviewers wait for it (a big repository without a commit-graph walks its
// whole history for a file changed long ago).
var HistoryTimeout = 2 * time.Minute

// FilesHistory is history.json.
type FilesHistory struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose changed files are listed
	// Base is the revision the logs were read at: origin/<base branch>, or
	// the merge base in a blind replay.
	Base  string        `json:"base"`
	Files []FileHistory `json:"files"`
	More  int           `json:"more,omitempty"` // changed files past the cap of 40
}

// FileHistory is one changed file of FilesHistory.
type FileHistory struct {
	Path    string          `json:"path"`
	Commits []HistoryCommit `json:"commits"` // newest first, at most 8
}

// HistoryCommit is one commit of a FileHistory.
type HistoryCommit struct {
	SHA     string `json:"sha"`  // git's unique abbreviation
	Date    string `json:"date"` // the committer date, YYYY-MM-DD
	Subject string `json:"subject"`
	PR      int    `json:"pr,omitempty"` // the PR a subject ending in "(#123)" names
	// AfterMergeBase: the base has the commit after the PR's merge base,
	// so the PR was never tested with it (a log at the merge base lacks
	// it). Never set without a merge base, nor in a blind replay, which
	// reads at the merge base.
	AfterMergeBase bool `json:"after_merge_base,omitempty"`
}

// sameCommit: a and b abbreviate the same commit id (git may abbreviate
// one longer than the other when objects arrive between two logs).
func sameCommit(a, b string) bool {
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

var historyPRNumber = regexp.MustCompile(`\(#(\d+)\)$`)

// historyPR is the PR number subject ends with as "(#123)", the way GitHub
// titles a squash merge; 0 when it ends otherwise.
func historyPR(subject string) int {
	m := historyPRNumber.FindStringSubmatch(strings.TrimSpace(subject))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// historyBase is the revision the logs are read at and the base the changed
// files are diffed against: origin/<base> and the merge base (origin/<base>
// without one), or in a blind replay the merge base for both; ok is false
// for a blind replay without one, since anything newer would tell of later
// commits.
func (rd *round) historyBase() (rev, base string, ok bool) {
	if rd.in.Blind {
		return rd.in.BaseSHA, rd.in.BaseSHA, rd.in.BaseSHA != ""
	}
	return rd.baseRef(), cmp.Or(rd.in.BaseSHA, rd.baseRef()), true
}

// filesHistory reads history.json's content for the head under review, the
// logs at rev of the files it changes against base, within HistoryTimeout;
// Files is empty when it changes no file the base has, related_ignore's
// aside. When base (the merge base) is not rev, a file with commits gets a
// second log at base, and its commits that log lacks are AfterMergeBase.
func (rd *round) filesHistory(ctx context.Context, rev, base string) (FilesHistory, error) {
	ctx, cancel := context.WithTimeout(ctx, HistoryTimeout)
	defer cancel()
	in := rd.in
	out := FilesHistory{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), HeadSHA: in.TargetSHA, Base: rev}
	paths, err := rd.r.Git.ModifiedPaths(ctx, in.SlotPath, base, in.TargetSHA)
	if err != nil {
		return out, err
	}
	paths = slices.DeleteFunc(paths, func(p string) bool {
		return slices.ContainsFunc(in.Related.Ignore, func(g string) bool { return config.MatchPath(g, p) })
	})
	if len(paths) > maxHistoryFiles {
		out.More, paths = len(paths)-maxHistoryFiles, paths[:maxHistoryFiles]
	}
	out.Files = make([]FileHistory, len(paths))
	errs := make([]error, len(paths))
	sem := make(chan struct{}, historyWorkers)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			commits, err := rd.r.Git.FileLog(ctx, in.SlotPath, rev, p, maxHistoryCommits)
			var older []gitx.Commit
			if err == nil && len(commits) > 0 && base != rev {
				older, err = rd.r.Git.FileLog(ctx, in.SlotPath, base, p, maxHistoryCommits)
			}
			f := FileHistory{Path: p, Commits: make([]HistoryCommit, 0, len(commits))}
			for _, c := range commits {
				after := base != rev && !slices.ContainsFunc(older, func(o gitx.Commit) bool { return sameCommit(o.SHA, c.SHA) })
				f.Commits = append(f.Commits, HistoryCommit{SHA: c.SHA, Date: c.Date, Subject: c.Subject, PR: historyPR(c.Subject), AfterMergeBase: after})
			}
			out.Files[i], errs[i] = f, err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// writeHistory writes history.json for the head under review, before its
// stages run (a restart writes the new head's), and keeps its path for the
// prompts in rd.historyFile; "" when there is none: a continued turn, no
// Git, a blind replay without its merge base, a PR that changes no file the
// base has, or a failure, which warns: the review goes on without it.
func (rd *round) writeHistory(ctx context.Context) {
	rd.historyFile = ""
	if rd.r.Git == nil || rd.in.Kind == KindContinue {
		return
	}
	rev, base, ok := rd.historyBase()
	if !ok {
		rd.event(ctx, "info", "round.history", "a blind replay without its merge base: no "+HistoryFile, nil)
		return
	}
	h, err := rd.filesHistory(ctx, rev, base)
	if err == nil && len(h.Files) == 0 {
		return
	}
	path := filepath.Join(rd.dir, HistoryFile)
	if err == nil {
		var b []byte
		if b, err = json.MarshalIndent(h, "", "  "); err == nil {
			err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.history", fmt.Sprintf("could not read the changed files' history; the reviewers go without it: %v", err), nil)
		}
		return
	}
	rd.historyFile = path
	at := rev
	if rd.in.Blind {
		at = "the merge base " + textx.ShortSHA(rev)
	}
	msg := fmt.Sprintf("%s: the last commits of %d changed file(s) on %s", HistoryFile, len(h.Files), at)
	if h.More > 0 {
		msg += fmt.Sprintf(" (%d more left out)", h.More)
	}
	rd.event(ctx, "info", "round.history", msg, map[string]any{"files": len(h.Files), "more": h.More, "base": rev, "file": path})
}
