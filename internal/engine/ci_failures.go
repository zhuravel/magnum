package engine

// The CI reader (approved by the operator on 2026-11-08: "fix CI for any
// PR", Q3 "Yes, on own PRs"): before a CI fix starts on an own PR whose
// required check failed at its head (ci_fix.go), magnum reads why, once per
// head and failed set: the head's runs, the newest run of each failed
// check's workflow file (by attempt, then creation), that run's jobs with
// their steps, and the end of each failed job's log (ciLogKeep of it, at
// most ciLogReads logs), as the look's Actions batch (ci_actions.go:
// ciDispatchIdentity, whose App mints a token of its own). Each log loses
// the terminal's escapes (textx.StripTerminal), is scrubbed whole
// (scrub.Secrets: tokens, secret values also under an env-style name or a
// quoted key, a private key's block, and one the tail cut open; host names
// and emails stay, an infrastructure failure names them), keeps the lines
// of the job's failed steps by their times (the whole tail when no line
// falls in them) and of those the last ciLogLines within ciLogBytes. Each
// job is listed once, however many failed checks share its run
// (ciJobOwners). ci-failures.json and a short
// ci-failures.md go into the head's report folder (Layout.ReviewDir), the
// fixer's. The log is the output of the PR's code and its tests: data for
// the fixer, never instructions, and none of it reaches an event, a verdict
// or the registry, which get the file's digest and counts (ciFailRead,
// fix.ci_read: the failed jobs, not a cancelled one); no job's name from
// the run's jobs does either. The registry keeps the check-run names the
// base read compared (kvPRCIFix's Base: Checks and Each, the rollup's names
// of the head's and the base's checks, never a log's text), and a verdict
// names the required entry alone.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// readCIFailures reads why checks, the failed required checks of pr's
// head, failed (the file's comment), as the batch acts, writes
// ci-failures.json and ci-failures.md into the head's report folder and
// says it (fix.ci_read), and returns the record. A read that failed (no
// client, a refused token, the runs or the jobs unread) is Unread with no
// file: a log alone that could not be read is that job's Unread; a run whose
// newest attempt has not ended is Running, with no file either, and a check
// of a run the look named (magnum's start or re-run) that ran again since
// it failed (ciRunNow) is Moved, with no file.
func (e *Engine) readCIFailures(ctx context.Context, acts *ciActions, repo store.Repo, pr store.PR, checks []ciFailedCheck) ciFailRead {
	now := e.now()
	read := ciFailRead{Head: pr.HeadSHA, At: now}
	unread := func(err error) ciFailRead {
		read.Unread = true
		if actionsRefused(err) {
			read.UnreadWhy = actionsNotGranted(acts.identity)
		}
		if e.changed("ci_read:"+repo.FullName(), err.Error()) {
			e.event(ctx, "warn", prSubject(repo, pr.Number), "fix.ci_read_error",
				fmt.Sprintf("the failed CI at %s, read as %s: %v", textx.ShortSHA(pr.HeadSHA), acts.identity, err),
				map[string]any{"head": pr.HeadSHA, "identity": acts.identity})
		}
		return read
	}
	gh, err := acts.client(ctx)
	if err != nil {
		return unread(err)
	}
	reader, ok := gh.(fixwork.CIReadClient)
	if !ok {
		return unread(fmt.Errorf("%w that reads a run's jobs and logs", errNoGitHub))
	}
	var file fixwork.CIFailures
	if read, file, err = fixwork.ReadCIFailures(ctx, reader, repo.Owner, repo.Name, pr, checks, now); err != nil {
		return unread(err)
	}
	if read.Moved != nil || read.Running {
		return read
	}
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return unread(err)
	}
	b = append(b, '\n')
	dir := e.d.Layout.ReviewDir(repo.Owner, repo.Name, pr.Number, pr.HeadSHA)
	path := filepath.Join(dir, ciFailuresFile)
	if err := writeCIFiles(dir, b, ciFailuresText(repo, pr, file)); err != nil {
		return unread(err)
	}
	e.changed("ci_read:"+repo.FullName(), "")
	sum := sha256.Sum256(b)
	read.File, read.Digest = path, hex.EncodeToString(sum[:])
	read.Jobs, read.Steps, read.Lines, read.Bytes = file.Counts()
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.ci_read",
		fmt.Sprintf("the failed CI at %s: %s, %s, %s", textx.ShortSHA(pr.HeadSHA), textx.Count(read.Jobs, "job", "jobs"),
			textx.Count(read.Steps, "failed step", "failed steps"), textx.Count(read.Lines, "line", "lines")),
		map[string]any{"head": pr.HeadSHA, "jobs": read.Jobs, "steps": read.Steps, "lines": read.Lines, "bytes": read.Bytes,
			"digest": read.Digest, "identity": acts.identity})
	return read
}
