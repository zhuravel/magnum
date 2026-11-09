package cli

// `magnum staging <pr> --deploy void,bastion|all [--dry-run] [--yes]
// [--json]` and `magnum staging --cancel <effect>`: a staging deploy of the
// operator's own PR on their word (engine.ReqStagingDeploy, which the
// review daemon answers off its tick). A terminal first asks the daemon's
// plan (a dry run: a merge in its mirror, nothing pushed) and puts it as
// one y/N (StagingQuestion): the envs that take the merge, the head, the
// fixer App's bot that pushes, the envs whose merge conflicts (the
// resolver merges it first, at most 2 tries), the migrations Jenkins then
// runs on each env's shared database, the envs left out and why, and a
// warning while the default branch's CI runs. y sends the deploy bound to that head and
// to what the question named; the daemon refuses a head that moved since.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// stagingWait bounds the wait for the daemon's answer to a plan, a deploy
// or a cancel: a plan fetches the mirror and merges in it, and a cancel
// waits for a step in flight (a push). The daemon refuses a deploy asked
// longer ago, which nobody waits for any more.
const stagingWait = engine.StagingDeployWait

// stagingEnvsOf reads --deploy's value: env names separated by commas.
func stagingEnvsOf(v string) []string {
	var out []string
	for e := range strings.SplitSeq(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// stagingSend queues kind with p and waits for the daemon's answer, which
// needs a running daemon.
func stagingSend(ctx context.Context, d *actDeps, kind string, p any) (reqOutcome, error) {
	q := reqSend{Wait: stagingWait, Poll: d.quickPoll(), NeedDaemon: true}
	return d.reqs().send(ctx, kind, p, q)
}

// stagingPlan asks the daemon for p's plan (a dry run); a refusal, or an
// answer still pending, is the error.
func stagingPlan(ctx context.Context, d *actDeps, p engine.StagingDeployPayload) (engine.StagingDeployAnswer, error) {
	p.DryRun = true
	res, err := stagingSend(ctx, d, engine.ReqStagingDeploy, p)
	switch {
	case err != nil:
		return engine.StagingDeployAnswer{}, err
	case res.Failed():
		return engine.StagingDeployAnswer{}, errors.New(res.Result())
	case res.Pending():
		return engine.StagingDeployAnswer{}, fmt.Errorf("the daemon has not planned the deploy within %s (request %d): ask again later", stagingWait, res.ID())
	}
	ans, ok := engine.ParseStagingDeployAnswer(res.Result())
	if !ok {
		return engine.StagingDeployAnswer{}, fmt.Errorf("the daemon's plan cannot be read: %s", lastLineOf(res.Result()))
	}
	return ans, nil
}

// lastLineOf is s's last non-empty line.
func lastLineOf(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndexByte(s, '\n')+1:]
}

// StagingQuestion is the y/N before a staging deploy of label as the
// daemon planned it (ans): the envs that take the merge and the head, who
// merges and pushes, each env whose merge conflicts and that the resolver
// merges it first, the migrations Jenkins then runs on each env's shared
// database, a merge that changes .github/workflows, the envs left out and
// why, and the default branch's CI while it runs. It also gives the
// deploy's binding: those envs and what the question named of each; false
// when no env takes the merge, the question then saying why for each.
func StagingQuestion(label string, ans engine.StagingDeployAnswer) (question string, envs []string, seen []engine.StagingSeen, ok bool) {
	var branches, parts, left, why, resolved []string
	type migs struct {
		list string
		envs []string
	}
	var groups []migs
	for _, ep := range ans.Envs {
		if !ep.Deployable() {
			says := actClean(engine.StagingPlanSays(ep))
			left, why = append(left, actClean(ep.Env)+" is left out ("+says+")"), append(why, actClean(ep.Env)+": "+says)
			continue
		}
		envs, branches = append(envs, ep.Env), append(branches, actClean(ep.Branch))
		if len(ep.Conflicts) > 0 { // the resolver merges it: its words without the migrations, named below
			c := ep
			c.Migrations, c.Workflows = nil, false
			resolved = append(resolved, actClean(ep.Env)+" "+actClean(engine.StagingPlanSays(c)))
		}
		seen = append(seen, engine.StagingSeen{Env: ep.Env, Migrations: slices.Clone(ep.Migrations), Workflows: ep.Workflows})
		if len(ep.Migrations) > 0 {
			list := actClean(strings.Join(ep.Migrations, ", "))
			if i := slices.IndexFunc(groups, func(g migs) bool { return g.list == list }); i >= 0 {
				groups[i].envs = append(groups[i].envs, ep.Env)
			} else {
				groups = append(groups, migs{list, []string{ep.Env}})
			}
		}
	}
	if len(envs) == 0 {
		return strings.Join(why, "; "), nil, nil, false
	}
	parts = append(parts, fmt.Sprintf("Deploy %s's head %s to %s: %s merges it into %s and pushes", label, textx.ShortSHA(ans.Head),
		actJoinAnd(envs), actClean(ans.Pusher), actJoinAnd(branches)))
	parts = append(parts, resolved...)
	for _, g := range groups {
		dbs := "database"
		if len(g.envs) > 1 {
			dbs = "databases"
		}
		owners := make([]string, len(g.envs))
		for i, e := range g.envs {
			owners[i] = e + "'s"
		}
		parts = append(parts, "Jenkins then runs "+g.list+" on "+actJoinAnd(owners)+" shared "+dbs)
	}
	for _, s := range seen {
		if s.Workflows {
			parts = append(parts, s.Env+"'s merge changes .github/workflows")
		}
	}
	parts = append(parts, left...)
	if ans.MasterCI != "" {
		parts = append(parts, "warning: "+actClean(ans.MasterCI))
	}
	return strings.Join(parts, "; ") + "?", envs, seen, true
}

// actJoinAnd joins words as "a, b and c".
func actJoinAnd(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// stagingDeployMain deploys ref to o's envs: the plan and its y/N first on
// a terminal (or with --dry-run, which stops there), the deploy alone with
// --yes. Without a terminal to ask on (an agent's shell, a pipe, a
// script), a deploy needs --yes: Jenkins runs its migrations on shared
// databases, so it never starts on nobody's word.
func stagingDeployMain(ctx context.Context, c *Context, d *actDeps, ref string, o stagingOpts) int {
	deploys := !o.dryRun && !o.yes
	switch {
	case deploys && (!d.StdinTTY || !d.StdoutTTY):
		return actUsage(c, "staging", "a deploy asks y/N on a terminal, and there is none: pass --yes to deploy without asking, "+
			"or --dry-run to see the plan; nothing queued", stagingUsage)
	case deploys && o.json:
		return actUsage(c, "staging", "--json needs --yes to deploy (a terminal asks y/N first); nothing queued", stagingUsage)
	}
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "staging", err)
	}
	label := d.actLabel(t.full(), t.PR.Number)
	p := engine.StagingDeployPayload{PRTarget: t.prTarget(), Envs: stagingEnvsOf(o.deploy)}
	switch {
	case o.dryRun || deploys: // a dry run, or a terminal's y/N
		ans, err := stagingPlan(ctx, d, p)
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		if o.dryRun {
			if o.json {
				return stagingWriteJSON(c, ans)
			}
			stagingPrintPlan(c.Stdout, label, ans)
			return 0
		}
		q, envs, seen, ok := StagingQuestion(label, ans)
		if !ok {
			stagingPrintPlan(c.Stdout, label, ans)
			return cmdFail(c, "staging", fmt.Errorf("nothing to deploy: %s", q))
		}
		if !d.confirm(ctx, c.Stdout, q) {
			fmt.Fprintln(c.Stderr, "nothing queued")
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
		p.Envs, p.Head, p.Seen = envs, ans.Head, seen
	default: // --yes: nothing was asked, so nothing was seen
		p.Head = t.PR.HeadSHA
	}
	return stagingSendDeploy(ctx, c, d, label, p, stagingWait, o.json)
}

// stagingSendDeploy queues the deploy p of label, waits for the daemon's
// answer at most wait and prints it (asJSON: as JSON); a request still
// pending says how to follow it.
func stagingSendDeploy(ctx context.Context, c *Context, d *actDeps, label string, p engine.StagingDeployPayload, wait time.Duration, asJSON bool) int {
	res, err := d.reqs().send(ctx, engine.ReqStagingDeploy, p, reqSend{Wait: wait, Poll: d.quickPoll(), NeedDaemon: true})
	if err != nil {
		if errors.Is(err, errNoDaemon) {
			err = fmt.Errorf("%s: nothing was queued: %w", label, err)
		}
		return cmdFail(c, "staging", err)
	}
	ans, ok := engine.ParseStagingDeployAnswer(res.Result())
	switch {
	case res.Failed():
		res.printNotes(c.Stderr)
		return cmdFail(c, "staging", errors.New(res.Result()))
	case ok && asJSON:
		return stagingWriteJSON(c, ans)
	case ok:
		res.printNotes(c.Stderr)
		fmt.Fprintln(c.Stdout, actClean(ans.Text))
		return 0
	case asJSON:
		_ = writeJSON(c.Stdout, res.view())
		return res.jsonCode()
	}
	return res.print(c.Stdout, c.Stderr)
}

// stagingPrintPlan prints the daemon's plan of label: its head and pusher,
// then a line per env saying what it would get, and the default branch's
// CI while it runs.
func stagingPrintPlan(w io.Writer, label string, ans engine.StagingDeployAnswer) {
	fmt.Fprintf(w, "%s at %s: a dry run, merged and pushed as %s\n", label, textx.ShortSHA(ans.Head), actClean(ans.Pusher))
	nameW := 0
	for _, ep := range ans.Envs {
		nameW = max(nameW, len(ep.Env))
	}
	for _, ep := range ans.Envs {
		fmt.Fprintf(w, "  %-*s  %s\n", nameW, actClean(ep.Env), actClean(engine.StagingPlanSays(ep)))
	}
	if ans.MasterCI != "" {
		fmt.Fprintln(w, "warning: "+actClean(ans.MasterCI))
	}
}

// stagingCancelMain stops the deploy's effect id.
func stagingCancelMain(ctx context.Context, c *Context, d *actDeps, id string, o stagingOpts) int {
	res, err := stagingSend(ctx, d, engine.ReqStagingCancel, engine.StagingCancelPayload{Effect: id})
	if err != nil {
		return cmdFail(c, "staging", err)
	}
	if o.json {
		_ = writeJSON(c.Stdout, res.view())
		return res.jsonCode()
	}
	return res.print(c.Stdout, c.Stderr)
}

// StagingPlan asks the daemon's plan of a staging deploy of ref to envs
// (magnum staging --deploy --dry-run) for the screens' E: the plan printed,
// its y/N as the result's Ask and the deploy's binding as Staging (the envs
// that take the merge, the head, what the question names); a plan with
// nothing to deploy fails, saying why per env.
func (s *screenActions) StagingPlan(ctx context.Context, ref string, envs []string) (tui.ActionResult, error) {
	var ask string
	var asked *tui.StagingAsked
	res, err := s.do(ctx, "staging", func(ctx context.Context, c *Context, d *actDeps) int {
		t, err := d.resolveRef(ctx, ref)
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		label := d.actLabel(t.full(), t.PR.Number)
		ans, err := stagingPlan(ctx, d, engine.StagingDeployPayload{PRTarget: t.prTarget(), Envs: envs})
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		stagingPrintPlan(c.Stdout, label, ans)
		q, deploy, seen, ok := StagingQuestion(label, ans)
		if !ok {
			return cmdFail(c, "staging", fmt.Errorf("nothing to deploy: %s", q))
		}
		ask, asked = q, &tui.StagingAsked{Envs: deploy, Head: ans.Head}
		for _, sn := range seen {
			asked.Seen = append(asked.Seen, tui.StagingSeen{Env: sn.Env, Migrations: sn.Migrations, Workflows: sn.Workflows})
		}
		return 0
	})
	res.Ask, res.Staging = ask, asked
	return res, err
}

// StagingDeploy queues the deploy the screen's y/N bound (StagingPlan's
// Staging), waiting for the daemon's answer as quick requests do: one still
// pending is followed on the screen.
func (s *screenActions) StagingDeploy(ctx context.Context, ref string, asked tui.StagingAsked) (tui.ActionResult, error) {
	return s.do(ctx, "staging", func(ctx context.Context, c *Context, d *actDeps) int {
		t, err := d.resolveRef(ctx, ref)
		if err != nil {
			return cmdFail(c, "staging", err)
		}
		p := engine.StagingDeployPayload{PRTarget: t.prTarget(), Envs: asked.Envs, Head: asked.Head, Seen: []engine.StagingSeen{}}
		for _, sn := range asked.Seen {
			p.Seen = append(p.Seen, engine.StagingSeen{Env: sn.Env, Migrations: sn.Migrations, Workflows: sn.Workflows})
		}
		return stagingSendDeploy(ctx, c, d, d.actLabel(t.full(), t.PR.Number), p, d.quick().Wait, false)
	})
}
