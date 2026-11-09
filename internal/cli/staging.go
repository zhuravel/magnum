package cli

// `magnum staging [<pr>] [--json]`: where open PRs stand on their
// repository's staging envs ([[factory.repo.staging]]), as the review
// daemon last told it (engine.KVPRStaging, which it writes from
// internal/staging's Status): the same facts as the board's STAGE column
// and the card's STAGINGS block. It reads the registry only: no git, no
// GitHub, no mirror.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

const stagingUsage = "staging [<pr>] [--json]"

type stagingOpts struct{ json bool }

func newStagingCmd(c *Context) *cobra.Command {
	var o stagingOpts
	cmd := newCommand(groupInspect, stagingUsage, "where open PRs stand on their repository's staging envs",
		"Print where a PR stands on each staging env of its repository (its [[factory.repo]]'s [[factory.repo.staging]] "+
			"entries, in their order): live (the env's newest deployment holds the PR's head), pushed (CI running, CI "+
			"failed on the PR's changes or not, waiting for Jenkins, or no deployment confirms it), an older head (how "+
			"many commits behind), not there, or unknown and why; when it was deployed or pushed, and the CI run's page. "+
			"Without a PR, every open PR of the repositories with staging envs that is on any of them. The review daemon "+
			"tells it from a treeless mirror and GitHub's deployments at most once a minute; this command reads only "+
			"what it stored, so a stopped daemon's statuses read unknown once they are stale. --json prints the PR as "+
			"an object, or the list (as [] when none). Read only: it deploys nothing.",
		func(pos []string) int { return runStaging(c, o, pos) })
	cmd.Flags().BoolVar(&o.json, "json", false, "print the PR (an object) or the PRs on a staging (a list) as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runStaging(c *Context, o stagingOpts, pos []string) int {
	if len(pos) > 1 {
		return actUsage(c, "staging", "one PR at a time", stagingUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "staging", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return stagingMain(ctx, c, d, pos, o)
}

// stagingJSON is one PR of `magnum staging --json`: its ref, the head its
// stagings are told for, and its state per env in the config's order.
type stagingJSON struct {
	Ref      string           `json:"ref"` // owner/name#N
	Head     string           `json:"head"`
	Stagings []prsJSONStaging `json:"stagings"`
}

// stagingPR is a PR as the command prints it.
type stagingPR struct {
	full     string
	number   int
	head     string
	stagings []tui.StagingInfo
}

func stagingMain(ctx context.Context, c *Context, d *actDeps, pos []string, o stagingOpts) int {
	now := d.now()
	lastTick := daemonLastTick(ctx, d.Store)
	if len(pos) == 1 {
		p, err := stagingOfRef(ctx, d, pos[0], lastTick, now)
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		if o.json {
			return stagingWriteJSON(c, stagingJSONOf(p))
		}
		stagingPrint(c.Stdout, d, p, now)
		return 0
	}
	repos := engine.StagingRepos(d.Cfg)
	var on []stagingPR
	for _, repo := range repos {
		prs, err := stagingOpenPRs(ctx, d.Store, repo)
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		envs := engine.StagingEnvs(d.Cfg, repo)
		for _, pr := range prs {
			p := stagingPR{full: repo, number: pr.Number, head: pr.HeadSHA, stagings: stagingShown(ctx, d.Store, envs, pr, lastTick, now)}
			if slices.ContainsFunc(p.stagings, onStaging) {
				on = append(on, p)
			}
		}
	}
	if o.json {
		out := make([]stagingJSON, 0, len(on))
		for _, p := range on {
			out = append(out, stagingJSONOf(p))
		}
		return stagingWriteJSON(c, out)
	}
	switch {
	case len(repos) == 0:
		fmt.Fprintln(c.Stdout, "no watched repository has staging envs ([[factory.repo.staging]] in config.toml): nothing to tell")
	case len(on) == 0:
		line := "no open PR is on a staging"
		if why := engine.StagingStale(lastTick, now); why != "" {
			line += ", as far as magnum can tell: " + strings.TrimPrefix(why, "stale: ") + " (`magnum status` says whether it runs)"
		}
		fmt.Fprintln(c.Stdout, line)
	}
	for i, p := range on {
		if i > 0 {
			fmt.Fprintln(c.Stdout)
		}
		stagingPrint(c.Stdout, d, p, now)
	}
	return 0
}

// onStaging reports whether an env holds a commit of the PR: live, pushed
// or an older head.
func onStaging(s tui.StagingInfo) bool {
	switch staging.State(s.State) {
	case staging.StateLatest, staging.StatePushed, staging.StateOutdated:
		return true
	}
	return false
}

// stagingOfRef is the PR ref names: refused when its repository has no
// staging envs or it is not open (the daemon tells open PRs only).
func stagingOfRef(ctx context.Context, d *actDeps, ref string, lastTick, now time.Time) (stagingPR, error) {
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return stagingPR{}, err
	}
	full := t.full()
	envs := engine.StagingEnvs(d.Cfg, full)
	switch {
	case len(envs) == 0:
		return stagingPR{}, fmt.Errorf("%s has no staging envs: its [[factory.repo]] in config.toml names no [[factory.repo.staging]]", full)
	case t.PR.GHState != store.GHOpen:
		return stagingPR{}, fmt.Errorf("%s is not open: magnum tells the staging status of open PRs only", d.actLabel(full, t.PR.Number))
	}
	return stagingPR{full: full, number: t.PR.Number, head: t.PR.HeadSHA, stagings: stagingShown(ctx, d.Store, envs, t.PR, lastTick, now)}, nil
}

// stagingOpenPRs are repo's open PRs in the registry, by number; none for
// a repository it does not know yet.
func stagingOpenPRs(ctx context.Context, st *store.Store, repo string) ([]store.PR, error) {
	r, err := st.RepoByFullName(ctx, repo)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return st.ListPRs(ctx, store.PRFilter{RepoID: r.ID, GHOpen: true})
}

// stagingShown is pr's staging status per env of envs, as the board reads
// it (boardStagings).
func stagingShown(ctx context.Context, st *store.Store, envs []staging.Env, pr store.PR, lastTick, now time.Time) []tui.StagingInfo {
	rec, ok := readStagingRecord(ctx, st, pr.ID)
	shown := engine.StagingShown(rec, ok, envs, pr.HeadSHA, lastTick, now)
	out := make([]tui.StagingInfo, len(shown))
	for i, s := range shown {
		out[i] = tui.StagingInfo{Env: s.Env, State: s.State, Detail: s.Detail, InTip: s.InTip, Behind: s.Behind,
			PushedAt: s.PushedAt, DeployedAt: s.DeployedAt, RunURL: s.RunURL, Why: s.Why}
	}
	return out
}

func stagingJSONOf(p stagingPR) stagingJSON {
	return stagingJSON{Ref: fmt.Sprintf("%s#%d", p.full, p.number), Head: p.head, Stagings: mapList(p.stagings, prsJSONStagingOf)}
}

func stagingWriteJSON(c *Context, v any) int {
	if err := writeJSON(c.Stdout, v); err != nil {
		return cmdFail(c, "staging", err)
	}
	return 0
}

// stagingPrint prints p: its label and head, then a line per env (its name
// and the card's words), why it cannot be told and the CI run's page
// under it.
func stagingPrint(w io.Writer, d *actDeps, p stagingPR, now time.Time) {
	fmt.Fprintf(w, "%s at %s\n", d.actLabel(p.full, p.number), textx.ShortSHA(p.head))
	nameW := 0
	for _, s := range p.stagings {
		nameW = max(nameW, len(s.Env))
	}
	indent := strings.Repeat(" ", 2+nameW+2)
	for _, s := range p.stagings {
		fmt.Fprintf(w, "  %-*s  %s\n", nameW, s.Env, tui.StagingSummary(s, now))
		if s.Why != "" {
			fmt.Fprintf(w, "%s%s\n", indent, s.Why)
		}
		if s.RunURL != "" {
			fmt.Fprintf(w, "%srun %s\n", indent, s.RunURL)
		}
	}
}
