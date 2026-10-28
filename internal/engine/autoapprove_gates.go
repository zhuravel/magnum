package engine

// What auto-approval checks before it approves as the operator, past the
// registry's facts of the PR and its review (autoApproveRefusal), which A
// as the operator (OperatorApprovalRefusal) does not: the operator decides
// by hand there. A review whose judge went without a reviewer's report
// (DECISIONS "Auto-approval hears every reviewer"), a PR its watch would not
// review on its own (a forced round on it), one that changes what steers
// the review agents or its own workflows or actions (what its CI gate would
// then trust), and a head whose checks fail (other than the way the
// base branch's tip fails them, autoapprove_ci_base.go) or still run (only
// the checks required_checks names, where it is set:
// autoapprove_required.go) are not approved; the card says why (KVPRAutoApproveRefused) and a
// review.auto_approve_refused event records it once per head, round and
// reason, while the PR's status comment says only the refusal's class in
// fixed words (publicRefusal, approvalLine). A later round of magnum's on
// the head of a PR it approved as the operator is held to them too: one
// they refuse withdraws the approval with the class's words
// (standingGateRefusal, standingWithdrawal). Before them, a PR whose author
// auto_approve_skip_authors lists, or the auto_approve_authors that applies
// does not, is refused (authorRefusal), recorded once per head whatever
// round reviewed it; a standing approval is not held to that one, so an
// approval posted before the lists left the author out, and the operator's
// own A, stand.

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
// for the head (Head) and magnum's latest posted round (RunID) it decided on;
// RunID is "" for a refusal of the PR whatever round reviewed it
// (authorRefusal).
type AutoApproveRefused struct {
	Head   string    `json:"head"`
	RunID  string    `json:"run_id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Public is the refusal's class (publicRefusal), which the PR's status
	// comment says in its fixed words; "" for one GitHub hears nothing of,
	// and in a record an older build wrote.
	Public string `json:"public,omitempty"`
}

// publicRefusal is the class of a refusal of auto-approval that GitHub may
// hear of: the status comment's line (approvalLine) and the dismissal
// message of a withdrawal (standingWithdrawal) say its fixed words
// (publicText, publicReason), never the refusal's own, which can name a
// path, a reviewer role and its status, a filter or an author list. ""
// says nothing in the status comment, and in a withdrawal only that magnum
// no longer approves the PR automatically: the author lists, the watch's
// filters, a reason that carries an error's text or heals at the next
// poll, and a gate added later without a class.
type publicRefusal string

const (
	publicWorkflows    publicRefusal = "workflows"      // changesWorkflow
	publicAgentConfig  publicRefusal = "agent_config"   // agentConfigRefusal
	publicTooManyFiles publicRefusal = "too_many_files" // more files than GitHub lists
	publicUnheard      publicRefusal = "unheard"        // missingReports: a reviewer's report missing
	publicCIRuns       publicRefusal = "ci_runs"        // ciRunsRefusal
	publicCIFails      publicRefusal = "ci_fails"       // ciFailsRefusal
	publicStopped      publicRefusal = "stopped"        // a held stop (store.AutoApproveHold), never recorded here
	publicPostFailed   publicRefusal = "post_failed"    // the last failed post of the approval (postFailedRefusal)
)

// autoRefusal is why auto-approval holds a PR back: why, the card's and the
// event's words ("" = it does not); public, its class GitHub may hear of
// (publicRefusal); waits, that it is only what a standing approval waits
// out (checks that still run, a held review's full pass).
type autoRefusal struct {
	why    string
	public publicRefusal
	waits  bool
}

// fingerprint is what noteAutoRefused compares a refusal recorded for the
// card by: its head, round, reason and class.
func (r AutoApproveRefused) fingerprint() string {
	return r.Head + "|" + r.RunID + "|" + r.Reason + "|" + r.Public
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

// skipAuthorRefusal is why auto-approval refuses a PR whose author
// auto_approve_skip_authors lists, and notAllowedAuthorRefusal one whose
// author the auto_approve_authors that applies does not list
// (authorRefusal; the card's words, which name the key the board reads
// them by). They name nobody: the event and the card read them, never
// GitHub (their refusal has no class).
const (
	skipAuthorRefusal       = "the author is in auto_approve_skip_authors"
	notAllowedAuthorRefusal = "the author is not in auto_approve_authors"
)

// authorRefusal says why auto-approval refuses c for its author, "" when it
// does not: the watch's auto_approve_skip_authors, or its repository's,
// lists the PR's author (config.AutoApproveSkipsAuthor), whose PRs need a
// person's review however clean magnum's; else an auto_approve_authors
// applies (the repository's, or the watch's) and does not list it
// (config.AutoApproveAllowsAuthor): only the authors it names are trusted
// that far, and an author magnum does not know is none of them. The skip
// list is asked first, so a login on both lists is skipped. It holds back
// a new approval only: a standing one is not held to it
// (standingGateRefusal runs autoGateRefusal alone), nor is the operator's
// A (OperatorApprovalRefusal).
func (e *Engine) authorRefusal(c store.RepoPR) autoRefusal {
	login := deref(c.PR.AuthorLogin)
	switch {
	case e.cfg.AutoApproveSkipsAuthor(c.Repo, login):
		return autoRefusal{why: skipAuthorRefusal}
	case !e.cfg.AutoApproveAllowsAuthor(c.Repo, login):
		return autoRefusal{why: notAllowedAuthorRefusal}
	}
	return autoRefusal{}
}

// autoGateRefusal says why auto-approval holds c back after magnum's round
// sum although the registry lets it approve (autoApproveRefusal); "" when
// it does not. In order: the review held its approval for a full pass of
// the whole PR (heldReviewOf, which waits: the full pass decides), the
// review did not hear every reviewer
// (missingReports), the PR's watch would not review it on its own
// (watchRefusal: classify, a manual repository's, a bot's, one skip_paths
// leaves out, one Codex flagged; a forced round's included), it changes
// what steers the review agents or its own workflows or actions, or magnum
// cannot tell (changedFilesRefusal), and its head's check rollup fails or
// runs (wait for green: the judge's result says nothing of the failures it
// found unrelated), unless every failing check fails the same way on the
// base branch's tip (ciGateOf, whose going ahead on those it records:
// noteCIBase); where the repository sets required_checks, the checks it
// names decide in place of the rollup (requiredCIRefusal: one outside the
// list neither fails nor runs). waits says the refusal is only checks that
// still run, which a standing approval waits out (standingGateRefusal). A
// reason never carries an error's text, and its class (publicRefusal) is
// all GitHub hears of it: the status comment and a withdrawal say the
// class's words; a held review's and the watch's refusals have none. The
// judge's own approval is held to the same gates, but for the review's
// (approval_gates.go).
func (e *Engine) autoGateRefusal(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) autoRefusal {
	pr := c.PR
	if _, held := e.heldReviewOf(ctx, pr.ID, sum.ReviewID); held {
		return autoRefusal{why: approvalHoldRefusal, waits: true} // approval_hold.go: the full pass after it decides
	}
	if r := e.missingReports(ctx, pr.ID, sum); r.why != "" {
		return r
	}
	if why, _ := e.watchRefusal(ctx, c); why != "" {
		return autoRefusal{why: why} // a filter's: nothing on GitHub names it
	}
	if r, _ := e.changedFilesRefusal(ctx, pr); r.why != "" {
		return r
	}
	ci := e.ciGateOf(ctx, c) // autoapprove_ci_base.go
	if ci.state == pipeline.CIRedLikeBase && ci.why == "" {
		e.noteCIBase(ctx, c, sum, ci.failing, ci.base) // the approval goes ahead on checks the base fails too
	}
	return ci.refusal()
}

// watchRefusal says why no approval goes to c, a PR its watch would not
// review on its own (classify: a manual repository's, a bot's, one
// skip_paths leaves out, one Codex flagged; a forced round's included),
// and short, the judge prompt's fixed words for it (approvalGatesDue); ""
// both when its watch would, or none watches its repository.
func (e *Engine) watchRefusal(ctx context.Context, c store.RepoPR) (why, short string) {
	if w := e.cfg.WatchFor(c.Repo); w != nil {
		if dec := e.classify(ctx, *w, c.PR, e.now()); !dec.Eligible {
			return "its watch would not review it on its own: " + dec.Reason, "the PR's watch would not review it on its own"
		}
	}
	return "", ""
}

// prStateRefusal says why no approval goes to pr for what it is now, a
// draft or muted, "" for neither, and short, the judge prompt's fixed words
// for it (approvalGatesDue), as watchRefusal and changedFilesRefusal do:
// the card's words and the prompt's come from one place, so a rewording of
// either skips no gate.
func prStateRefusal(pr store.PR) (why, short string) {
	switch {
	case pr.IsDraft:
		return "it is a draft", "the PR is a draft"
	case pr.Muted:
		return "it is muted", "the PR is muted"
	}
	return "", ""
}

// kvAutoApprovalGated records "<approval id>|<run id>" once the gates let
// the PR's standing automatic approval stand after that later round of
// magnum's (standingGateRefusal): they decide once on each round, as they
// do before a new approval, also across a restart.
func kvAutoApprovalGated(prID int64) string { return fmt.Sprintf("pr.%d.auto_approval_gated", prID) }

// standingGateRefusal says why the gates a new approval runs
// (autoGateRefusal) refuse sum, magnum's latest posted round of standing
// approval a's PR, which then withdraws a: a new head that adds an
// AGENTS.md, whose review went without codex-review or whose checks fail
// is not covered by the operator's approval of an earlier head. "" when a
// stands: sum is a's own round or none, the PR's head is not the one sum
// reviewed (a head magnum has not reviewed changes nothing: its round
// decides), sum leaves something to fix (that withdraws a anyway), the
// head's checks still run (magnum waits for them, asked again every tick),
// or the gates let sum's round through already (kvAutoApprovalGated).
func (e *Engine) standingGateRefusal(ctx context.Context, a store.AutoApprovalPR, sum *store.ReviewSummary) autoRefusal {
	if sum == nil || sum.RunID == a.RunID || sum.SHA != a.PR.HeadSHA {
		return autoRefusal{}
	}
	if blocks, _ := roundBlocks(*sum, ""); blocks {
		return autoRefusal{}
	}
	key, mark := kvAutoApprovalGated(a.PRID), fmt.Sprintf("%d|%s", a.ID, sum.RunID)
	if v, _ := e.getKV(ctx, key); v == mark {
		return autoRefusal{}
	}
	r := e.autoGateRefusal(ctx, store.RepoPR{PR: a.PR, Repo: a.Repo}, *sum)
	switch {
	case r.waits:
		return autoRefusal{}
	case r.why == "":
		e.setKV(ctx, key, mark)
	}
	return r
}

// missingReports says why auto-approval refuses sum's review for the
// reports it went without, by the pipeline's record of the review's round
// (pipeline.ReadRoundMissingReports): the reports its judge went without,
// and those a round of the judge alone carried from the round its review
// builds on ("Magnum's review did not hear every reviewer: codex-review
// (login_required)"). A round magnum has no record of (one before the
// records, or a run that cannot be read) is refused too: nothing says it
// heard every reviewer. None when it heard every one. A report missing is
// of class publicUnheard, whose words never name the role; a run or a
// record magnum cannot read has no class.
func (e *Engine) missingReports(ctx context.Context, prID int64, sum store.ReviewSummary) autoRefusal {
	const unknown = "Magnum cannot tell whether its review heard every reviewer: "
	run, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "auto approval: the review's run", "pr", prID, "run", sum.RunID)
		return autoRefusal{why: unknown + "it cannot read the review's run"}
	}
	rec, ok := pipeline.ReadRoundMissingReports(ctx, e.st, prID, run.Round)
	switch {
	case !ok:
		return autoRefusal{why: fmt.Sprintf("%sit has no record of round %d's reports", unknown, run.Round)}
	case len(rec.Unheard()) > 0:
		return autoRefusal{why: "Magnum's review did not hear every reviewer: " + rec.String(), public: publicUnheard}
	}
	return autoRefusal{}
}

// agentConfigWhat is what agentConfigRefusal guards, as its refusals name
// it.
const agentConfigWhat = "the review agents' instructions or hooks"

// workflowsRefusal is why auto-approval leaves a PR that changes its own
// workflows or actions (changesWorkflow) to the operator.
const workflowsRefusal = "the PR changes its own workflows or actions: approve by hand"

// workflowDirs are what a repository's GitHub Actions run as the PR's head
// has them: its workflows and its local actions, at the checkout's root.
var workflowDirs = []string{".github/workflows/", ".github/actions/"}

// changesWorkflow reports whether p, a path the PR changes, is under one of
// workflowDirs, in any case (GitHub reads them in one; a stricter match
// costs nothing).
func changesWorkflow(p string) bool {
	lower := strings.ToLower(p)
	return slices.ContainsFunc(workflowDirs, func(dir string) bool { return strings.HasPrefix(lower, dir) })
}

// changedFilesRefusal says why auto-approval leaves pr to the operator for
// the files its head changes, by the head's file list (pr_files, which the
// poller reads with the Details; a round keeps no list of its own): what
// steers the review agents (agentConfigRefusal), else its own workflows or
// actions (changesWorkflow), which decide the checks the CI gate trusts
// (required_checks names workflows by a name any workflow file may declare,
// and a PR that edits the CI workflow controls it outright). A head magnum
// has no whole list of files for is refused too: what it does not list may
// be one of them. short is the judge prompt's fixed words for why, which
// never name a path the PR chose (approvalGatesDue). The refusal's class
// is that of the files it found, or of a list GitHub cut short; a list
// magnum cannot read or has none of has none (the next poll mends it).
func (e *Engine) changedFilesRefusal(ctx context.Context, pr store.PR) (r autoRefusal, short string) {
	const unknown = "magnum cannot tell which files the PR changes"
	f, ok, err := e.st.PRFilesOf(ctx, pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "auto approval: the PR's files", "pr", pr.ID)
		return autoRefusal{why: "Magnum cannot read the files it changes"}, unknown
	}
	listed := ok && f.HeadSHA == pr.HeadSHA
	var paths []string
	if listed {
		paths = f.Paths
	}
	if why := e.agentConfigRefusal(ctx, pr, paths); why != "" {
		return autoRefusal{why: why, public: publicAgentConfig}, "the PR changes " + agentConfigWhat
	}
	if slices.ContainsFunc(paths, changesWorkflow) {
		return autoRefusal{why: workflowsRefusal, public: publicWorkflows}, "the PR changes its own workflows or actions"
	}
	const what = agentConfigWhat + ", or its own workflows or actions"
	switch {
	case !listed:
		return autoRefusal{why: "Magnum cannot tell whether it changes " + what + ": it has no list of the head's files"}, unknown
	case f.Truncated:
		why := fmt.Sprintf("Magnum cannot tell whether it changes %s: it changes more than the %d files GitHub listed", what, len(f.Paths))
		return autoRefusal{why: why, public: publicTooManyFiles}, unknown
	}
	return autoRefusal{}, ""
}

// agentConfigRefusal says why auto-approval leaves pr to the operator for
// what it changes: an instruction file of the review agents at any depth
// (agentInstructionFiles), or a project config an agent CLI loads from the
// checkout (agents.ProjectTouched: .claude/, .codex/, .mcp.json, in any
// case), by paths, the head's file list (none when magnum has none), or a
// session of the head that ran without the PR's project config
// (agents.ProjectDeclined: git found it changed). Such files steer the
// review agents, and their hooks run on every teammate's machine once
// merged.
func (e *Engine) agentConfigRefusal(ctx context.Context, pr store.PR, paths []string) string {
	kinds := e.projectKinds()
	for _, p := range paths {
		if name, hit := agentConfigPath(kinds, p); hit {
			return "it changes " + agentConfigWhat + " (" + name + ")"
		}
	}
	for _, kind := range kinds {
		if n, ok := agents.ProjectDeclined(ctx, e.st, pr.ID, kind); ok && n.Head == pr.HeadSHA {
			names := strings.Join(n.Paths, ", ") // the kind's labels, never a path the PR named
			if names == "" {
				names, _, _, _, _ = agents.ProjectRule(kind)
			}
			return "it changes " + agentConfigWhat + " (" + names + ")"
		}
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

// noteAutoRefused records why auto-approval holds c back after magnum's
// posted round run (r; none clears it) for the card and, by its class, the
// status comment (approvalLine), and writes a review.auto_approve_refused
// event for a reason new to the PR's head and round. A run of "" is a
// refusal of the PR whatever round reviewed it (authorRefusal): recorded
// once per head. A record that differs in its class alone (one an older
// build wrote) is rewritten as it stood, with no second event, and a
// failed post (publicPostFailed) writes none: review.auto_approve_failed
// records it. e.autoRefused keeps what it recorded, so an unchanged answer
// costs no registry write (after a restart, one read).
func (e *Engine) noteAutoRefused(ctx context.Context, c store.RepoPR, run string, r autoRefusal) {
	pr := c.PR
	rec := AutoApproveRefused{Head: pr.HeadSHA, RunID: run, Reason: r.why, Public: string(r.public), At: e.now()}
	fp := ""
	if r.why != "" {
		fp = rec.fingerprint()
	}
	if prev, known := e.autoRefused[pr.ID]; known && prev == fp {
		return
	}
	e.autoRefused[pr.ID] = fp
	key := KVPRAutoApproveRefused(pr.ID)
	cur, had := e.getKV(ctx, key)
	old, ok := ParseAutoApproveRefused(cur)
	if ok && old.fingerprint() == fp {
		return
	}
	if r.why == "" {
		if had {
			e.delKV(ctx, key)
		}
		return
	}
	reclassed := ok && old.Head == rec.Head && old.RunID == rec.RunID && old.Reason == rec.Reason
	if reclassed {
		rec.At = old.At
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	e.setKV(ctx, key, string(b))
	if reclassed || r.public == publicPostFailed {
		return
	}
	repo := repoOf(c.Repo)
	e.event(ctx, "info", prSubject(repo, pr.Number), "review.auto_approve_refused",
		fmt.Sprintf("magnum does not approve %s#%d as you: %s", repo.FullName(), pr.Number, r.why),
		map[string]any{"reason": r.why, "head_sha": pr.HeadSHA, "run_id": run})
}

// postFailedRefusal is the card's words for an approval whose last post
// failed (notePostFailed).
const postFailedRefusal = "its approval did not post, and magnum does not try it again for this review"

// notePostFailed records, for the card and the status comment, that the
// last attempt to post the approval of c's head after round run failed
// (publicPostFailed), with no refusal event: the post's
// review.auto_approve_failed says it. A later round of the head, or a new
// head, decides again.
func (e *Engine) notePostFailed(ctx context.Context, c store.RepoPR, run string) {
	e.noteAutoRefused(ctx, c, run, autoRefusal{why: postFailedRefusal, public: publicPostFailed})
}

// forgetAutoRefused clears the card's reason (noteAutoRefused) when it
// names a round other than sum, magnum's latest posted round of c, which
// autoApproveRefusal holds back for its own reason (it found something to
// fix): the reason was the review before it's, or the PR's (no round:
// authorRefusal), which speaks of a review that left nothing to fix. The
// reason recorded is read once (e.autoRefused keeps it).
func (e *Engine) forgetAutoRefused(ctx context.Context, c store.RepoPR, sum store.ReviewSummary) {
	fp, known := e.autoRefused[c.PR.ID]
	if !known {
		v, _ := e.getKV(ctx, KVPRAutoApproveRefused(c.PR.ID))
		if r, ok := ParseAutoApproveRefused(v); ok {
			fp = r.fingerprint()
		}
		e.autoRefused[c.PR.ID] = fp
	}
	if _, rest, ok := strings.Cut(fp, "|"); !ok || strings.HasPrefix(rest, sum.RunID+"|") {
		return // none recorded, or it names sum's round
	}
	e.noteAutoRefused(ctx, c, sum.RunID, autoRefusal{})
}
