package cli

// `magnum merge-check`: whether an open PR's specs still pass once another
// PR is merged (internal/mergecheck). An experiment the operator runs; the
// daemon never does. It holds a pool slot next to a running daemon through
// the slot's hold_reason (slots.HoldFree), which the daemon never touches.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/mergecheck"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const mergeCheckUsage = "merge-check <repo> --merged <commit|#PR> --pr N [--slot NAME] [--keep] [--command CMD] [--timeout 1h] [--json]"

type mergeCheckOpts struct {
	merged, slot, command string
	pr                    int
	keep, asJSON          bool
	timeout               time.Duration
}

func newMergeCheckCmd(c *Context) *cobra.Command {
	var o mergeCheckOpts
	cmd := newCommand(groupAct, mergeCheckUsage, "check whether a PR's specs still pass once another PR is merged (experiment)",
		"Merges PR N's head into the merged commit (--merged: a commit id, or #M for the merge commit GitHub reports "+
			"for PR M) and runs the spec files either side touches on the result, in a pool slot of <repo> "+
			"(owner/name, or a pool's repository name): the spec files the PR changes, those the merged commit "+
			"changes and those of the Ruby files it changes (app/models/a.rb → spec/models/a_spec.rb). The examples "+
			"that fail are run again at the PR head alone: one that does not fail there is a clash (the merge "+
			"breaks it), one that does was already failing. A textual conflict is a result too.\n\n"+
			"The command holds a free slot (--slot names one) with the hold reason merge-check, shown by `magnum "+
			"slots` and `magnum status`: the daemon, which may keep running, never claims, evicts or releases a "+
			"held slot. It never takes a pinned or held slot or one holding a PR. The merge is checked out with the "+
			"slot's checkout steps and readiness (its databases reloaded when they carry another schema, then the "+
			"repository's prepare and ready commands); the spec runs go through the slot's db-lock line as `zsh -lc "+
			"'magnum db-lock … -- <command> --format json …'`. The slot is then released as a round's release does, "+
			"also after a failure or ctrl+c; --keep leaves it held for a look (`magnum slots unpin <slot>` hands it "+
			"back).\n\n"+
			"It prints the failing examples with their verdicts and writes result.json, the runs' JSON reports and "+
			"their output under the state directory (merge-check/<owner>-<name>-<N>-<time>/), and a "+
			"merge_check.result event on the PR (`magnum logs <repo>#N`). It refuses while `magnum daemon-restart "+
			"--drain` drains, or while another command runs slot work in-process; with no daemon running it holds "+
			"the daemon's lock until it ends. Exit status 0 when the check reached a verdict, 1 when it did not or "+
			"the slot could not be released.",
		func(pos []string) int { return runMergeCheck(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.merged, "merged", "", "the merged commit (its id), or #M: the merge commit of merged PR M")
	fs.IntVar(&o.pr, "pr", 0, "the open PR whose head is merged and checked")
	fs.StringVar(&o.slot, "slot", "", "hold this free pool slot (default: the least recently used free one)")
	fs.BoolVar(&o.keep, "keep", false, "leave the slot held for inspection instead of releasing it")
	fs.StringVar(&o.command, "command", mergecheck.DefaultCommand, "the command that runs spec files (in the slot, under db-lock)")
	fs.DurationVar(&o.timeout, "timeout", mergecheck.DefaultTimeout, "how long each spec run may take")
	fs.BoolVar(&o.asJSON, "json", false, "print the result as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePools)
	_ = cmd.RegisterFlagCompletionFunc("slot", completeFlag(c.completeSlots))
	return cmd
}

func runMergeCheck(c *Context, o mergeCheckOpts, pos []string) int {
	const name = "merge-check"
	if len(pos) != 1 || o.pr <= 0 || strings.TrimSpace(o.merged) == "" {
		return actUsage(c, name, "needs <repo>, --merged and --pr N", mergeCheckUsage)
	}
	mergedPR, mergedSHA, err := parseMergedArg(o.merged)
	if err != nil {
		return actUsage(c, name, err.Error(), mergeCheckUsage)
	}
	ctx, stop := signalContext()
	defer stop()
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, name, err)
	}
	defer a.Close()
	pool, err := mergeCheckPool(a.Config, pos[0])
	if err != nil {
		return cmdFail(c, name, err)
	}
	if err := mergeCheckFlagged(ctx, a.Store, defaultRepo(a.Config), pool.Repo, o.pr); err != nil {
		return cmdFail(c, name, err)
	}
	if v, ok, err := a.Store.GetKV(ctx, engine.KVDaemonDraining); err != nil {
		return cmdFail(c, name, err)
	} else if ok && v != "" {
		return cmdFail(c, name, errors.New("the daemon is draining for a restart (`magnum daemon-restart --drain`); run merge-check after it"))
	}
	unlock, who, err := acquireOps(c.Layout)
	switch {
	case err != nil:
		return cmdFail(c, name, err)
	case who == opsBusy:
		return cmdFail(c, name, opsBusyErr(c.Layout))
	case who == opsMine && unlock != nil:
		defer unlock()
	case who == opsDaemon: // the daemon runs: the check works beside it, on a slot it holds in the registry
	default:
		return cmdFail(c, name, opsHolderErr(who))
	}
	if mergedPR > 0 {
		if mergedSHA, err = mergeCommitOf(ctx, a, pool.Repo, mergedPR); err != nil {
			return cmdFail(c, name, err)
		}
	}
	dir := filepath.Join(c.Layout.State(), "merge-check",
		fmt.Sprintf("%s-%d-%s", strings.ReplaceAll(pool.Repo, "/", "-"), o.pr, time.Now().UTC().Format("20060102T150405Z")))
	magnum, err := os.Executable()
	if err != nil {
		magnum = ""
	}
	res, err := mergecheck.Run(ctx, mergecheck.Deps{
		Store: a.Store, Git: a.Git, Run: a.Runner, Slots: a.Slots,
		Progress: func(line string) { fmt.Fprintln(c.Stderr, "merge-check:", actClean(line)) },
	}, mergecheck.Options{
		Pool: pool, Readiness: a.Config.ReadinessFor(pool.Repo), PR: o.pr, Merged: mergedSHA, MergedPR: mergedPR,
		Slot: o.slot, Keep: o.keep, Command: o.command, Magnum: magnum, Dir: dir, Timeout: o.timeout,
	})
	if res.Slot == "" { // nothing held, nothing recorded
		return cmdFail(c, name, err)
	}
	if o.asJSON {
		if jerr := writeJSON(c.Stdout, res); jerr != nil {
			return cmdFail(c, name, jerr)
		}
	} else {
		printMergeCheck(c.Stdout, res)
	}
	if err != nil {
		return cmdFail(c, name, err)
	}
	return 0
}

// parseMergedArg reads --merged: "#M" or digits name merged PR M, 7 to 64
// hex digits a commit. An abbreviated commit id of digits only must be
// written in full.
func parseMergedArg(s string) (pr int, sha string, err error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutPrefix(s, "#"); ok || strings.Trim(s, "0123456789") == "" {
		pr, err := strconv.Atoi(n)
		if err != nil || pr <= 0 {
			return 0, "", fmt.Errorf("--merged %q: not a PR number", s)
		}
		return pr, "", nil
	}
	sha = strings.ToLower(s)
	if len(sha) < 7 || len(sha) > 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		return 0, "", fmt.Errorf("--merged %q: neither a commit id (7 to 64 hex digits) nor #PR", s)
	}
	return 0, sha, nil
}

// mergeCheckPool is the [[pool]] of repo: owner/name, or the repository name
// of exactly one pool.
func mergeCheckPool(cfg *config.Config, repo string) (config.Pool, error) {
	if strings.Contains(repo, "/") {
		if p := cfg.PoolFor(repo); p != nil {
			return *p, nil
		}
		return config.Pool{}, fmt.Errorf("no [[pool]] for %s in config.toml: merge-check runs in a pool slot", repo)
	}
	var found []config.Pool
	for _, p := range cfg.Pools {
		if _, n, _ := strings.Cut(p.Repo, "/"); strings.EqualFold(n, repo) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return config.Pool{}, fmt.Errorf("no [[pool]] for a repository named %s in config.toml: merge-check runs in a pool slot", repo)
	}
	return config.Pool{}, fmt.Errorf("several pools are named %s; give owner/name", repo)
}

// mergeCheckFlagged refuses a PR Codex flagged (store.KVPRCodexFlag) with
// the reason `magnum review` gives: no agent runs here, but magnum leaves a
// flagged PR alone. A PR the registry does not know is not flagged.
func mergeCheckFlagged(ctx context.Context, st *store.Store, defRepo, repo string, number int) error {
	r, err := st.RepoByFullName(ctx, repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	pr, err := st.PRByRepoNumber(ctx, r.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if f, ok := codexFlagOf(ctx, st, pr.ID); ok {
		label := actRefLabel(defRepo, repo, number)
		return fmt.Errorf("%s: %s", label, f.Sentence(label))
	}
	return nil
}

// mergeCommitOf is the commit merged PR number put on its base branch, as
// GitHub reports it to the poll identity of the watch covering repo.
func mergeCommitOf(ctx context.Context, a *app.App, repo string, number int) (string, error) {
	w := a.Config.WatchFor(repo)
	if w == nil {
		return "", fmt.Errorf("%s is not watched, so no identity reads it on GitHub; give --merged the merge commit's id", repo)
	}
	gh := a.GitHub(w.PollIdentity)
	if gh == nil {
		return "", fmt.Errorf("no GitHub client for poll identity %q; give --merged the merge commit's id", w.PollIdentity)
	}
	owner, name, _ := strings.Cut(repo, "/")
	states, _, err := gh.ConfirmStates(ctx, owner, name, []int{number})
	if err != nil {
		return "", fmt.Errorf("read #%d on GitHub: %w", number, err)
	}
	st, ok := states[number]
	switch {
	case !ok:
		return "", fmt.Errorf("%s#%d not found on GitHub", repo, number)
	case !st.Merged:
		return "", fmt.Errorf("%s#%d is not merged (%s)", repo, number, strings.ToLower(st.State))
	}
	mc := strings.ToLower(st.MergeCommitOid)
	if (len(mc) != 40 && len(mc) != 64) || strings.Trim(mc, "0123456789abcdef") != "" {
		return "", fmt.Errorf("GitHub names no merge commit for %s#%d; give --merged its id", repo, number)
	}
	return mc, nil
}

// printMergeCheck prints a check's result for a person: the verdict, the
// failing examples (PR text, made safe for the terminal), the slot and the
// result file.
func printMergeCheck(w io.Writer, r mergecheck.Result) {
	merged := textx.ShortSHA(r.Merged)
	if r.MergedPR > 0 {
		merged += fmt.Sprintf(" (#%d)", r.MergedPR)
	}
	fmt.Fprintf(w, "%s#%d merged onto %s: %s\n", r.Repo, r.PR, merged, r.Verdict)
	fmt.Fprintf(w, "  %s\n", statusSafe(mergecheck.Summary(r), 0))
	if r.Head != "" {
		line := "  PR head " + textx.ShortSHA(r.Head)
		if r.Tree != "" {
			line += ", merged tree " + textx.ShortSHA(r.Tree)
		}
		fmt.Fprintln(w, line+", slot "+r.Slot)
	}
	for _, f := range r.Conflicts {
		fmt.Fprintf(w, "  conflict  %s\n", statusSafe(f, 160))
	}
	for _, run := range []*mergecheck.SpecRun{r.MergedRun, r.HeadRun} {
		if run == nil {
			continue
		}
		for _, chk := range run.Readiness {
			if !chk.OK {
				fmt.Fprintf(w, "  readiness on the %s tree: %s %s: %s\n", run.Tree, chk.Kind, statusSafe(chk.Command, 80), statusSafe(chk.Detail, 200))
			}
		}
	}
	if len(r.Failures) > 0 {
		rows := make([][]string, 0, len(r.Failures))
		for _, f := range r.Failures {
			at := f.File
			if f.Line > 0 {
				at += ":" + strconv.Itoa(f.Line)
			}
			rows = append(rows, []string{"  " + strings.ReplaceAll(f.Verdict, "_", " "), statusSafe(at, 80),
				"at head: " + strings.ReplaceAll(f.AtHead, "_", " "), statusSafe(f.Description, 120)})
		}
		actTable(w, nil, rows)
	}
	switch {
	case r.Kept:
		fmt.Fprintf(w, "  %s stays held (merge-check); `magnum slots unpin %s` hands it back\n", r.Slot, r.Slot)
	case r.Released:
		fmt.Fprintf(w, "  %s released\n", r.Slot)
	default:
		fmt.Fprintf(w, "  %s is still held: %s\n", r.Slot, statusSafe(r.ReleaseError, 300))
	}
	if r.File != "" {
		fmt.Fprintf(w, "  result: %s\n", inspTilde(r.File))
	}
}
