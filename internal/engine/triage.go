package engine

// Triage of small diffs (DECISIONS "Triage of small diffs"): a one-line fix
// does not need every reviewer, so before a round starts its agents a cheap
// model reads the round's diff and says which of them the diff needs
// ([triage]). The model's answer is advice that magnum bounds: only a diff of
// at most max_lines changed lines is asked about, the judge always runs, the
// answer can only remove roles that have a summary, and anything that goes
// wrong runs every role. The diff is the PR's own text, so none of this
// depends on the model reading it faithfully.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
)

const (
	// triageMaxDiffBytes caps the diff text sent to the model: a diff of few
	// lines can still be huge (a minified bundle).
	triageMaxDiffBytes = 128 << 10
	// triageAnswerTail is the end of the command's stdout searched for the
	// answer; the answer is one short line.
	triageAnswerTail = 16 << 10
	// triageReasonRunes and triageWhyRunes clip the model's reason and a
	// failure's text in events.
	triageReasonRunes = 120
	triageWhyRunes    = 200
)

// triageData feeds the triage prompt (prompts/triage.md).
type triageData struct {
	Kind string // pipeline.KindInitial: the whole PR; pipeline.KindRereview: the commits since the last review
	// OwnDiff: the commits since the last review merged the base branch
	// in or were rebased onto it, so Diff is the PR's own diff (against
	// its base) of each file whose own change they altered (measureRange).
	OwnDiff bool
	Lines   int          // changed lines, added plus deleted
	Roles   []triageRole // the roles the model may leave out
	Diff    string
}

// triageRole is a role the model may leave out, with what it checks.
type triageRole struct {
	Name    string
	Summary string
}

// triage drops from the round the reviewers its diff does not need
// ([triage]): rs.toRun, the roles the round starts, and rs.roles, the
// candidates the pipeline plans from, lose the same roles, so the pipeline
// runs the set decided here. It applies to the first review and re-reviews
// only, not to a continued round, an eval or a round whose roles were named
// (`magnum review --role`, or a rerun of a role that earned one); only roles
// RolesToRun picked and that carry a summary can be dropped, never the
// judge. A diff above max_lines, one that cannot be read in full, and every
// failure of the command run every role; a failure records why.
func (e *Engine) triage(ctx context.Context, job *roundJob, rs *roundSetup) {
	tc := e.cfg.Triage
	if !tc.Enabled || job.evalHead != "" || len(rs.requested) > 0 ||
		(job.kind != pipeline.KindInitial && job.kind != pipeline.KindRereview) {
		return
	}
	var offered []triageRole
	for _, r := range rs.toRun {
		if r.Removable() {
			offered = append(offered, triageRole{Name: r.Name, Summary: strings.TrimSpace(r.Summary)})
		}
	}
	if len(offered) == 0 {
		return
	}
	subject := prSubject(job.repo, job.pr.Number)
	everyRole := func(level, why string) {
		if ctx.Err() != nil {
			return // the round is stopping; its setup fails on its own
		}
		e.event(ctx, level, subject, "round.triage", "triage: every role runs: "+why, map[string]any{"why": why})
	}

	files, own, err := e.triageFiles(ctx, job, rs.target)
	if err != nil {
		everyRole("warn", "the diff could not be read: "+oneLine(err.Error(), triageWhyRunes))
		return
	}
	lines, incomplete := 0, false
	for _, f := range files {
		if f.Truncated || f.Patch == "" {
			incomplete = true
			continue
		}
		lines += patchLines(f.Patch)
	}
	switch {
	case lines > tc.MaxLines:
		e.log.Debug("triage: the diff is above max_lines; every role runs", "subject", subject, "lines", lines, "max_lines", tc.MaxLines)
		return
	case incomplete:
		everyRole("info", "a file of the diff has no patch (binary, too large or renamed), so its size is unknown")
		return
	case lines == 0:
		return // nothing changed to ask about
	}
	diff := triageDiffText(files)
	if len(diff) > triageMaxDiffBytes {
		everyRole("info", fmt.Sprintf("the diff is %d KiB, too long to send", len(diff)>>10))
		return
	}

	ans, err := e.askTriage(ctx, tc, triageData{Kind: job.kind, OwnDiff: own, Lines: lines, Roles: offered, Diff: diff})
	if err != nil {
		everyRole("warn", err.Error())
		return
	}

	// The answer names the roles to run: a name matches a role by name or
	// alias; a name that is no role of the round is ignored, but an answer
	// without a single role of the round (the judge's name counts) is not an
	// answer.
	keep := map[string]bool{}
	known := false
	for _, n := range ans.Run {
		if i := slices.IndexFunc(rs.toRun, func(r config.Role) bool { return r.Matches(n) }); i >= 0 {
			known = true
			keep[rs.toRun[i].Name] = true
		}
	}
	if len(ans.Run) > 0 && !known {
		everyRole("warn", "its answer names none of the round's roles")
		return
	}
	var skipped []string
	for _, r := range offered {
		if !keep[r.Name] {
			skipped = append(skipped, r.Name)
		}
	}
	drop := func(roles []config.Role) []config.Role {
		return slices.DeleteFunc(slices.Clone(roles), func(r config.Role) bool { return slices.Contains(skipped, r.Name) })
	}
	rs.toRun, rs.roles = drop(rs.toRun), drop(rs.roles)

	runs, skips := strings.Join(roleNames(rs.toRun), ", "), "none"
	if len(skipped) > 0 {
		skips = strings.Join(skipped, ", ")
	}
	msg := fmt.Sprintf("triage (%d lines): runs %s; skips %s", lines, runs, skips)
	reason := oneLine(ans.Reason, triageReasonRunes) // the model read the PR: its words are PR content
	if reason != "" {
		msg += ": " + reason
	}
	e.event(ctx, "info", subject, "round.triage", msg,
		map[string]any{"lines": lines, "max_lines": tc.MaxLines, "own_diff": own, "runs": roleNames(rs.toRun), "skips": skipped, "reason": reason})
}

// triageFiles reads the diff the round reviews from GitHub, as the watch's
// poll identity (like rerunRoles): for a re-review the commits since the
// reviewed one, measured like the re-review gate measured them
// (measureRange): when they merged the base branch in or were rebased (own
// is true), the PR's own diff of each file whose own change they altered,
// not the base branch's files. Else (a first review, or a re-review of the
// reviewed commit itself) the whole PR, i.e. the base branch's merge base
// with target, which GitHub's comparison of the base branch with target
// is. A post-merge round compares with its merge base instead: the base
// branch holds the merged head after a merge-commit merge.
func (e *Engine) triageFiles(ctx context.Context, job *roundJob, target string) (files []github.FileDelta, own bool, err error) {
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return nil, false, errors.New("no GitHub client")
	}
	if reviewed := deref(job.pr.ReviewedSHA); job.kind == pipeline.KindRereview && reviewed != "" && reviewed != target {
		m, err := e.measureRange(ctx, gh, job.repo, job.diffBase(), reviewed, target)
		if err != nil {
			return nil, false, err
		}
		return m.files(), m.ownOK, nil
	}
	base := prBase(job.repo, job.pr)
	if job.postMerge && job.mergeBase != "" {
		base = job.mergeBase
	}
	if base == "" {
		return nil, false, errors.New("no base to compare with")
	}
	files, err = e.compareFiles(ctx, gh, job.repo, base, target)
	return files, false, err
}

// triageAnswer is what the model says: the roles that run, and why.
type triageAnswer struct {
	Run    []string
	Reason string
}

// askTriage renders the prompt and runs the command with it on stdin. The
// error says, for the event, why there is no answer.
func (e *Engine) askTriage(ctx context.Context, tc config.Triage, data triageData) (triageAnswer, error) {
	if e.d.Runner == nil || len(tc.Command) == 0 {
		return triageAnswer{}, errors.New("no command runner or no command")
	}
	p, err := e.cfg.ResolvePrompt(tc.Prompt)
	if err != nil {
		return triageAnswer{}, fmt.Errorf("the prompt cannot be read: %s", oneLine(err.Error(), triageWhyRunes))
	}
	prompt, err := agents.RenderPrompt(p, data)
	if err != nil {
		return triageAnswer{}, fmt.Errorf("the prompt does not render: %s", oneLine(err.Error(), triageWhyRunes))
	}
	res, err := e.d.Runner.Run(ctx, execx.Cmd{
		Name: tc.Command[0], Args: slices.Clone(tc.Command[1:]), Dir: e.triageDir(), Stdin: []byte(prompt),
		Timeout: tc.Timeout.Duration, NoTTY: true, Label: "triage",
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return triageAnswer{}, fmt.Errorf("the command timed out after %s", tc.Timeout.Duration)
	case err != nil:
		return triageAnswer{}, fmt.Errorf("the command failed: %s", oneLine(execx.Redact(err.Error()), triageWhyRunes))
	}
	ans, ok := parseTriage(string(res.Stdout))
	if !ok {
		return triageAnswer{}, errors.New(`the command's output has no JSON object with a "run" list`)
	}
	return ans, nil
}

// triageDir is where the command runs: a private directory under the state
// directory, never the PR's checkout. A model CLI reads the settings, hooks
// and instruction files of its working directory, and the checkout's are
// the PR's to write.
func (e *Engine) triageDir() string {
	if !e.d.Layout.Valid() {
		return ""
	}
	dir := filepath.Join(e.d.Layout.State(), "triage")
	_ = os.MkdirAll(dir, 0o700) // a failure surfaces as the command's own
	return dir
}

// parseTriage reads the last JSON object of the command's output that has a
// "run" list: the CLI may print other text around its answer.
func parseTriage(out string) (triageAnswer, bool) {
	if len(out) > triageAnswerTail {
		out = out[len(out)-triageAnswerTail:]
	}
	for i := strings.LastIndexByte(out, '{'); i >= 0; i = strings.LastIndexByte(out[:i], '{') {
		var a struct {
			Run    *[]string `json:"run"`
			Reason string    `json:"reason"`
		}
		if json.NewDecoder(strings.NewReader(out[i:])).Decode(&a) == nil && a.Run != nil {
			return triageAnswer{Run: *a.Run, Reason: a.Reason}, true
		}
	}
	return triageAnswer{}, false
}

// patchLines counts the lines a file's patch adds or deletes. GitHub's patch
// holds the hunks only, so every "+" or "-" line is a changed one.
func patchLines(patch string) int {
	n := 0
	for l := range strings.Lines(patch) {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			n++
		}
	}
	return n
}

// triageDiffText renders the files as one unified diff, without a final newline.
func triageDiffText(files []github.FileDelta) string {
	var b strings.Builder
	for _, f := range files {
		from, to := "a/"+cmp.Or(f.PreviousPath, f.Path), "b/"+f.Path
		switch f.Status {
		case "added":
			from = "/dev/null"
		case "removed":
			to = "/dev/null"
		}
		fmt.Fprintf(&b, "--- %s\n+++ %s\n%s\n", from, to, strings.TrimRight(f.Patch, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneLine is s on one line (runs of whitespace become one space), cut to n
// runes and redacted.
func oneLine(s string, n int) string {
	return clipRunes(execx.Redact(strings.Join(strings.Fields(s), " ")), n)
}
