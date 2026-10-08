package engine

// What auto-approval checks before it approves as the operator, past the
// registry's facts of the PR and its review (autoApproveRefusal), which A
// as the operator (OperatorApprovalRefusal) does not: the operator decides
// by hand there. A review whose judge went without a reviewer's report
// (DECISIONS "Auto-approval hears every reviewer"), a PR its watch would not
// review on its own (a forced round on it), one that changes what steers
// the review agents, and a head whose checks fail or still run are not
// approved; the card says why (KVPRAutoApproveRefused) and a
// review.auto_approve_refused event records it once per head, round and
// reason.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// KVPRAutoApproveRefused holds why auto-approval does not approve the PR
// as the operator although its review left nothing to fix
// (AutoApproveRefused as JSON); the board's card says it.
func KVPRAutoApproveRefused(prID int64) string {
	return fmt.Sprintf("pr.%d.auto_approve_refused", prID)
}

// AutoApproveRefused is why auto-approval holds a PR back (autoGateRefusal),
// for the head (Head) and magnum's latest posted round (RunID) it decided on.
type AutoApproveRefused struct {
	Head   string    `json:"head"`
	RunID  string    `json:"run_id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// ParseAutoApproveRefused reads a KVPRAutoApproveRefused value.
func ParseAutoApproveRefused(v string) (AutoApproveRefused, bool) {
	var r AutoApproveRefused
	if json.Unmarshal([]byte(v), &r) != nil || r.Reason == "" {
		return AutoApproveRefused{}, false
	}
	return r, true
}

// agentInstructionFiles are the files the review agents read as their
// instructions wherever they are in a checkout (Codex's AGENTS.md and its
// override, Claude Code's CLAUDE.md and its local file), matched by name in
// any case.
var agentInstructionFiles = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md"}

// autoGateRefusal says why auto-approval holds c back after magnum's round
// sum although the registry lets it approve (autoApproveRefusal); "" when
// it does not. In order: the review did not hear every reviewer
// (missingReports), the PR's watch would not review it on its own
// (classify: a manual repository's, a bot's, one skip_paths leaves out, one
// Codex flagged; a forced round's included), it changes what steers the
// review agents (agentConfigRefusal), and its head's check rollup fails or
// runs (wait for green: the judge's result says nothing of the failures it
// found unrelated).
func (e *Engine) autoGateRefusal(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) string {
	pr := c.PR
	if why := e.missingReports(ctx, pr.ID, sum); why != "" {
		return why
	}
	if w := e.cfg.WatchFor(c.Repo); w != nil {
		if dec := e.classify(ctx, *w, pr, e.now()); !dec.Eligible {
			return "its watch would not review it on its own: " + dec.Reason
		}
	}
	if why := e.agentConfigRefusal(ctx, pr); why != "" {
		return why
	}
	switch strings.ToUpper(deref(pr.CIState)) {
	case "FAILURE", "ERROR":
		return "its head's checks fail (magnum approves it once they pass)"
	case "PENDING", "EXPECTED":
		return "its head's checks have not finished (magnum approves it once they pass)"
	}
	return ""
}

// missingReports says why auto-approval refuses sum's review for the
// reports it went without, by the pipeline's record of the review's round
// (pipeline.ReadRoundMissingReports): the reports its judge went without,
// and those a round of the judge alone carried from the round its review
// builds on ("magnum's review did not hear every reviewer: codex-review
// (login_required)"). A round magnum has no record of (one before the
// records, or a run that cannot be read) is refused too: nothing says it
// heard every reviewer. "" when it heard every one.
func (e *Engine) missingReports(ctx context.Context, prID int64, sum store.ReviewSummary) string {
	const unknown = "magnum cannot tell whether its review heard every reviewer: "
	run, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		e.log.Warn("auto approval: the review's run", "pr", prID, "run", sum.RunID, "err", err)
		return unknown + "it cannot read the review's run"
	}
	rec, ok := pipeline.ReadRoundMissingReports(ctx, e.st, prID, run.Round)
	switch {
	case !ok:
		return fmt.Sprintf("%sit has no record of round %d's reports", unknown, run.Round)
	case len(rec.Unheard()) > 0:
		return "magnum's review did not hear every reviewer: " + rec.String()
	}
	return ""
}

// agentConfigRefusal says why auto-approval leaves pr to the operator for
// what it changes: an instruction file of the review agents at any depth
// (agentInstructionFiles), or a project config an agent CLI loads from the
// checkout (agents.ProjectTouched: .claude/, .codex/, .mcp.json, in any
// case), by the head's file list (pr_files), or a session of the head that
// ran without the PR's project config (agents.ProjectDeclined: git found it
// changed). Such files steer the review agents, and their hooks run on
// every teammate's machine once merged. A head magnum has no whole list of
// files for is refused too: what it does not list may be one of them.
func (e *Engine) agentConfigRefusal(ctx context.Context, pr store.PR) string {
	const what = "the review agents' instructions or hooks"
	f, ok, err := e.st.PRFilesOf(ctx, pr.ID)
	if err != nil {
		return "magnum cannot read the files it changes: " + err.Error()
	}
	kinds := e.projectKinds()
	listed := ok && f.HeadSHA == pr.HeadSHA
	if listed {
		for _, p := range f.Paths {
			if name, hit := agentConfigPath(kinds, p); hit {
				return "it changes " + what + " (" + name + ")"
			}
		}
	}
	for _, kind := range kinds {
		if n, ok := agents.ProjectDeclined(ctx, e.st, pr.ID, kind); ok && n.Head == pr.HeadSHA {
			names := strings.Join(n.Paths, ", ") // the kind's labels, never a path the PR named
			if names == "" {
				names, _, _, _, _ = agents.ProjectRule(kind)
			}
			return "it changes " + what + " (" + names + ")"
		}
	}
	switch {
	case !listed:
		return "magnum cannot tell whether it changes " + what + ": it has no list of the head's files"
	case f.Truncated:
		return fmt.Sprintf("magnum cannot tell whether it changes %s: it changes more than the %d files GitHub listed", what, len(f.Paths))
	}
	return ""
}

// agentConfigPath reports whether p, a path the PR changes, is one of the
// review agents' instruction files or under the project config of one of
// kinds (projectKinds), and how the refusal names it: p itself when it is a
// plain path, else the name it matched (the PR chose the path; its odd
// characters stay out of events).
func agentConfigPath(kinds []string, p string) (string, bool) {
	base := path.Base(p)
	for _, f := range agentInstructionFiles {
		if strings.EqualFold(base, f) {
			return shownPath(p, f), true
		}
	}
	for _, kind := range kinds {
		if agents.ProjectTouched(kind, []string{p}) {
			return shownPath(p, projectLabel(kind, p)), true
		}
	}
	return "", false
}

// projectLabel names the path of kind's project config p is or lies under
// as the kind spells it (".claude/", ".mcp.json"), for a p not shown as is.
func projectLabel(kind, p string) string {
	top, _, nested := strings.Cut(p, "/")
	base := path.Base(p)
	for _, name := range agents.ProjectPaths(kind) {
		switch {
		case strings.ContainsAny(name, "/*"): // an any-directory glob of a name listed too
		case strings.EqualFold(top, name) && nested:
			return name + "/"
		case strings.EqualFold(top, name), strings.EqualFold(base, name):
			return name
		}
	}
	return "its project config"
}

// shownPath is p when it is a plain path (ASCII letters, digits and
// "._-/+@", at most 200 bytes), else fallback.
func shownPath(p, fallback string) string {
	if len(p) == 0 || len(p) > 200 {
		return fallback
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._-/+@", r):
		default:
			return fallback
		}
	}
	return p
}

// defaultKinds are the built-in agent kinds: a PR's .claude/ steers Claude
// Code on a teammate's machine whether or not this configuration runs it.
var defaultKinds = sync.OnceValue(func() []string { return config.Defaults().KindNames() })

// projectKinds are the agent kinds whose project config auto-approval
// looks for: the configuration's and the built-in ones.
func (e *Engine) projectKinds() []string {
	kinds := slices.Clone(defaultKinds())
	for _, k := range e.cfg.KindNames() {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// noteAutoRefused records why auto-approval holds c back after round sum
// (reason; "" clears it) for the card, and writes a
// review.auto_approve_refused event for a reason new to the PR's head and
// round. e.autoRefused keeps what it recorded, so an unchanged answer costs
// no registry write (after a restart, one read).
func (e *Engine) noteAutoRefused(ctx context.Context, c store.RepoPR, sum store.ReviewSummary, reason string) {
	pr := c.PR
	fp := ""
	if reason != "" {
		fp = pr.HeadSHA + "|" + sum.RunID + "|" + reason
	}
	if prev, known := e.autoRefused[pr.ID]; known && prev == fp {
		return
	}
	e.autoRefused[pr.ID] = fp
	key := KVPRAutoApproveRefused(pr.ID)
	cur, had := e.getKV(ctx, key)
	if r, ok := ParseAutoApproveRefused(cur); ok && r.Head+"|"+r.RunID+"|"+r.Reason == fp {
		return
	}
	if reason == "" {
		if had {
			e.delKV(ctx, key)
		}
		return
	}
	b, err := json.Marshal(AutoApproveRefused{Head: pr.HeadSHA, RunID: sum.RunID, Reason: reason, At: e.now()})
	if err != nil {
		return
	}
	e.setKV(ctx, key, string(b))
	repo := repoOf(c.Repo)
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approve_refused",
		fmt.Sprintf("magnum does not approve %s#%d as you: %s", repo.FullName(), pr.Number, reason),
		map[string]any{"reason": reason, "head_sha": pr.HeadSHA, "run_id": sum.RunID})
}

// forgetAutoRefused clears the card's reason (noteAutoRefused) when it
// names a round other than sum, magnum's latest posted round of c, which
// autoApproveRefusal holds back for its own reason (it found something to
// fix): the reason was the review before it's. The reason recorded is read
// once (e.autoRefused keeps it).
func (e *Engine) forgetAutoRefused(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) {
	fp, known := e.autoRefused[c.PR.ID]
	if !known {
		v, _ := e.getKV(ctx, KVPRAutoApproveRefused(c.PR.ID))
		if r, ok := ParseAutoApproveRefused(v); ok {
			fp = r.Head + "|" + r.RunID + "|" + r.Reason
		}
		e.autoRefused[c.PR.ID] = fp
	}
	if _, rest, ok := strings.Cut(fp, "|"); !ok || strings.HasPrefix(rest, sum.RunID+"|") {
		return // none recorded, or it names sum's round
	}
	e.noteAutoRefused(ctx, c, sum, "")
}
