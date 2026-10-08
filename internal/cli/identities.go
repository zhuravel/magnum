package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const identitiesUsage = "[list] | check [--name <identity>] [--json]"

// identitiesCheckTimeout bounds one identity's Check.
const identitiesCheckTimeout = 90 * time.Second

func newIdentitiesCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupInspect, "identities", "list GitHub identities; `check` verifies logins, App permissions and tokens",
		"List the GitHub identities of config.toml ([[identity]]) with the watches that post or poll as each one. "+
			"`check` verifies each identity's login, GitHub App permissions and installation token. An App whose "+
			"key is a file (private_key_file) needs nothing more; one that reads it from an environment variable "+
			"(private_key_env) needs that variable set: run the command under `mise exec` when mise provides it.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "identities", fmt.Sprintf("unknown identities subcommand %q", pos[0]), identitiesUsage)
			}
			return runIdentities(c, "list", "", false)
		})
	list := newCommand("", "list", "list the identities (the default)",
		"List the configured GitHub identities with their kind, login, the repositories they post for, the "+
			"owners they poll and the result of the last check.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "identities", "list takes no arguments", identitiesUsage)
			}
			return runIdentities(c, "list", "", false)
		})
	var name string
	var asJSON bool
	check := newCommand("", "check [<identity>] [--name <identity>] [--json]", "verify logins, App permissions and tokens",
		"Verify every identity, or only the named one: a gh identity's token must belong to its login; a GitHub "+
			"App needs its private key, pull_requests write permission, an installation token and every watched "+
			"repository in the installation; an App without read access to Actions, Checks or Commit statuses "+
			"passes with a WARN line, since the judge cannot read the PR's CI with it. "+
			"The verdict is recorded like the daemon's own check, so a pass "+
			"unblocks the identity's PRs on the next tick.",
		func(pos []string) int {
			n := name
			if len(pos) == 1 && n == "" {
				n = pos[0] // `identities check talkable-app`
			} else if len(pos) > 0 {
				return inspUsage(c, "identities", "check takes --name <identity>", identitiesUsage)
			}
			return runIdentities(c, "check", n, asJSON)
		})
	check.Flags().StringVar(&name, "name", "", "check only this identity")
	check.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	_ = check.RegisterFlagCompletionFunc("name", completeFlag(c.completeIdentities))
	check.ValidArgsFunction = completeFirst(c.completeIdentities)
	cmd.AddCommand(list, check)
	return cmd
}

// runIdentities runs `identities list` or `identities check` (name "" checks
// every identity).
func runIdentities(c *Context, sub, name string, asJSON bool) int {
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "identities", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()
	if sub == "list" {
		return identitiesList(ctx, c, a.Store, a.Config)
	}
	var srcs []identity.Source
	for _, n := range a.IdentityNames() {
		if s := a.Identities[n]; s != nil {
			srcs = append(srcs, s)
		}
	}
	return identitiesCheck(ctx, c, a.Store, srcs, name, asJSON)
}

// identitiesKick wakes the daemon after a check (engine.KickDaemon; tests
// replace it).
var identitiesKick = engine.KickDaemon

// identitiesDaemonPID names the daemon that should record the verdicts
// (engine.DaemonPID; tests replace it).
var identitiesDaemonPID = engine.DaemonPID

// identitiesVerdictWait bounds the wait for the daemon to record the
// verdicts handed to it.
const identitiesVerdictWait = 15 * time.Second

// identitiesVerdict is one identity's verdict to record.
type identitiesVerdict struct {
	name, reason string
	pass         bool
}

// identitiesResult is one identity's check (the --json form).
type identitiesResult struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	Login string   `json:"login"`
	Pass  bool     `json:"pass"`
	Lines []string `json:"lines"`
	Error string   `json:"error,omitempty"`
}

// identitiesCheck runs Check for every identity (or the named one), prints
// the PASS/FAIL lines and records the verdicts as the daemon's own check
// does (identitiesRecordAll), so a passing check unblocks the identity's PRs
// on the daemon's next tick (it is kicked).
func identitiesCheck(ctx context.Context, c *Context, st *store.Store, srcs []identity.Source, name string, asJSON bool) int {
	if name != "" {
		var names []string
		var picked []identity.Source
		for _, s := range srcs {
			names = append(names, s.Name())
			if s.Name() == name {
				picked = append(picked, s)
			}
		}
		if len(picked) == 0 {
			return cmdFail(c, "identities check", fmt.Errorf("unknown identity %q (configured: %s)", name, strings.Join(names, ", ")))
		}
		srcs = picked
	}
	results := []identitiesResult{}
	var verdicts []identitiesVerdict
	failed, warned := 0, 0
	for _, s := range srcs {
		cctx, cancel := context.WithTimeout(ctx, identitiesCheckTimeout)
		rep, err := s.Check(cctx)
		cancel()
		r := identitiesResult{Name: s.Name(), Kind: s.Kind(), Login: s.Login(), Pass: err == nil && rep.Pass, Lines: rep.Lines}
		if r.Lines == nil {
			r.Lines = []string{}
		}
		reason := ""
		if err != nil {
			r.Error = err.Error()
			reason = err.Error()
		} else if !rep.Pass {
			reason = "check failed"
			for _, l := range rep.Lines {
				if after, ok := strings.CutPrefix(l, "FAIL "); ok {
					reason = after
					break
				}
			}
		}
		if !r.Pass {
			failed++
		} else if slices.ContainsFunc(r.Lines, func(l string) bool { return strings.HasPrefix(l, "WARN ") }) {
			warned++
		}
		verdicts = append(verdicts, identitiesVerdict{name: s.Name(), pass: r.Pass, reason: reason})
		results = append(results, r)
		if !asJSON {
			fmt.Fprintf(c.Stdout, "== %s (%s, %s)\n", r.Name, r.Kind, r.Login)
			for _, l := range r.Lines {
				fmt.Fprintln(c.Stdout, l)
			}
			if r.Error != "" {
				fmt.Fprintf(c.Stdout, "FAIL the check could not finish: %s\n     fix: check the network and `gh auth status`, then run this again\n", r.Error)
			}
		}
	}
	identitiesRecordAll(ctx, c, st, verdicts, asJSON)
	if asJSON {
		if err := writeJSON(c.Stdout, results); err != nil {
			return cmdFail(c, "identities check", err)
		}
	} else if failed > 0 {
		fmt.Fprintf(c.Stdout, "%d of %d identities failed; PRs that post as them wait until this passes\n", failed, len(results))
	} else if warned > 0 {
		fmt.Fprintf(c.Stdout, "all %d identities pass; %d with warnings above, which hold no PR\n", len(results), warned)
	} else {
		fmt.Fprintf(c.Stdout, "all %d identities pass\n", len(results))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// identitiesRecordAll records the verdicts. While a daemon runs (its
// pidfile names a live process) they are handed to it as
// engine.ReqIdentityVerdict requests, recorded by its own code path in order
// with its checks; it is kicked and awaited for identitiesVerdictWait. When
// no daemon answers the kick, the requests are withdrawn (a daemon started
// later must not replay them over newer verdicts) and the CLI records the
// verdicts itself (identitiesRecord).
func identitiesRecordAll(ctx context.Context, c *Context, st *store.Store, verdicts []identitiesVerdict, asJSON bool) {
	var ids []int64
	if pid, err := identitiesDaemonPID(c.Layout); err == nil && pid > 0 {
		for _, v := range verdicts {
			id, err := st.EnqueueRequest(ctx, engine.ReqIdentityVerdict, engine.IdentityVerdictPayload{Name: v.name, Pass: v.pass, Reason: v.reason})
			if err != nil {
				fmt.Fprintf(c.Stderr, "magnum identities check: could not hand the verdicts to the daemon (%v); recording them here\n", err)
				identitiesWithdraw(ctx, st, ids)
				ids = nil
				break
			}
			ids = append(ids, id)
		}
	}
	pid, err := identitiesKick(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum identities check: %v\n", err)
	}
	if len(ids) > 0 && pid > 0 {
		identitiesAwait(ctx, c, st, ids, pid, asJSON)
		return
	}
	identitiesWithdraw(ctx, st, ids)
	for _, v := range verdicts {
		identitiesRecord(ctx, c, st, v.name, v.pass, v.reason)
	}
	if pid > 0 && !asJSON {
		fmt.Fprintf(c.Stdout, "(the daemon, pid %d, picks the new verdicts up now)\n", pid)
	}
}

// identitiesWithdraw fails the verdict requests no daemon took. One the
// daemon took meanwhile is already complete and stays as it is.
func identitiesWithdraw(ctx context.Context, st *store.Store, ids []int64) {
	for _, id := range ids {
		_ = st.CompleteRequest(ctx, id, store.RequestFailed, "no daemon answered; `magnum identities check` recorded the verdict itself")
	}
}

// identitiesAwait waits up to identitiesVerdictWait for the daemon to record
// the verdict requests ids and says how that went.
func identitiesAwait(ctx context.Context, c *Context, st *store.Store, ids []int64, pid int, asJSON bool) {
	deadline := inspNow().Add(identitiesVerdictWait)
	rc := inspRequests(c, st)
	for _, id := range ids {
		out := reqOutcome{Req: store.Request{ID: id}, PID: pid}
		err := rc.wait(ctx, &out, max(time.Nanosecond, deadline.Sub(inspNow())), inspPoll)
		req := out.Req
		switch {
		case err != nil:
			fmt.Fprintf(c.Stderr, "magnum identities check: waiting for the daemon (pid %d): %v\n", pid, err)
			return
		case req.State == store.RequestPending:
			if !asJSON {
				fmt.Fprintf(c.Stdout, "(the daemon, pid %d, records the new verdicts on its next tick)\n", pid)
			}
			return
		case req.State == store.RequestFailed:
			fmt.Fprintf(c.Stderr, "magnum identities check: the daemon did not record a verdict: %s\n", actClean(store.Deref(req.Result)))
		}
	}
	if !asJSON {
		fmt.Fprintf(c.Stdout, "(the daemon, pid %d, recorded the new verdicts)\n", pid)
	}
}

// identitiesRecord writes the verdict where the daemon's dispatch gate reads
// it when no daemon runs, with the daemon's own side effect (engine
// recordIdentityVerdict): a fail that turns into a pass forgets the
// "identity unhealthy" toast's dedup record, so the next failure notifies at
// once.
func identitiesRecord(ctx context.Context, c *Context, st *store.Store, name string, pass bool, reason string) {
	var err error
	msg := "identities check: pass"
	if pass {
		var prev string
		if prev, _, err = st.GetKV(ctx, store.KVIdentityCheck(name)); err == nil && prev == "fail" {
			err = st.ForgetSend(ctx, "identity:"+name)
		}
		if err == nil {
			err = st.SetKV(ctx, store.KVIdentityCheck(name), "pass")
		}
		if err == nil {
			err = st.DeleteKV(ctx, store.KVIdentityError(name))
		}
	} else {
		msg = "identities check: fail: " + reason
		err = st.SetKV(ctx, store.KVIdentityCheck(name), "fail")
		if err == nil {
			err = st.SetKV(ctx, store.KVIdentityError(name), reason)
		}
	}
	if err == nil {
		subject := "identity:" + name
		level := "info"
		if !pass {
			level = "warn"
		}
		_, err = st.AppendEvent(ctx, store.Event{Level: level, Subject: &subject, Kind: "identity.checked", Message: msg})
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum identities check: could not record the verdict for %s: %v\n", name, err)
	}
}

// identitiesList prints the configured identities with the last verdict.
func identitiesList(ctx context.Context, c *Context, st *store.Store, cfg *config.Config) int {
	tw := inspTable(c.Stdout)
	fmt.Fprintln(tw, "NAME\tKIND\tLOGIN\tPOSTS FOR\tPOLLS\tLAST CHECK")
	for _, id := range cfg.Identities {
		var posts, polls []string
		for _, w := range cfg.Watches {
			repos := w.Owner + "/{" + strings.Join(w.Include, ",") + "}"
			if len(w.Include) == 1 {
				repos = w.Owner + "/" + w.Include[0]
			}
			if w.Identity == id.Name {
				posts = append(posts, repos)
			}
			if w.PollIdentity == id.Name {
				polls = append(polls, w.Owner)
			}
		}
		check, _, _ := st.GetKV(ctx, store.KVIdentityCheck(id.Name))
		if check == "" {
			check = "never (run `magnum identities check`)"
		} else if check == "fail" {
			if why, _, _ := st.GetKV(ctx, store.KVIdentityError(id.Name)); why != "" {
				check += ": " + textx.Clip(why, 60)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", id.Name, id.Kind, id.Login, inspOrDash(strings.Join(posts, " ")),
			inspOrDash(strings.Join(polls, " ")), check)
	}
	tw.Flush()
	return 0
}
