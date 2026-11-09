package engine

// A staging resolver job's agent (staging_resolve.go): the fix role's kind,
// model, effort and launch (resolveRole), in a magnum pane of its own (the
// herdr workspace "staging merge", one job at a time) that works in the
// job's slot, driven as the skill editor's is (retro_pane.go's paneAgent:
// a scratch registry in the job's directory, an observer of its own). The
// pane gets no GitHub credentials and none of the operator's: no token, an
// empty gh config dir, no git config but the checkout's, no askpass and no
// SSH agent (resolvePaneEnv); it needs none, since it never pushes. Its
// prompt (prompts/staging-resolve.md) names the conflicted paths, the
// db-lock line its specs run through, the result file and, on a second
// try, why the first one failed; it carries shas and paths, never PR text.
// The result file is data, read strictly (readResolveResult): whether the
// agent resolved the conflicts or gave up, why, the specs it ran and how
// they ended. A missing or invalid one gets one nudge.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// StagingResolvePrompt is the staging resolver's prompt file.
const StagingResolvePrompt = "staging-resolve.md"

// stagingWorkspace labels the herdr workspace of the resolver's agent.
const stagingWorkspace = "staging merge"

// resolveResultName is the result file's name in the job's directory.
const resolveResultName = "result.json"

// maxResolveResult bounds the result file.
const maxResolveResult = 16 << 10

// The outcomes a result file names.
const (
	resolveOutcomeResolved = "resolved"
	resolveOutcomeGaveUp   = "gave_up"
)

// The specs' results a result file names.
const (
	resolveSpecsPassed = "passed"
	resolveSpecsFailed = "failed"
	resolveSpecsNotRun = "not_run"
)

// stagingAgent is a job's agent (paneStagingAgent).
type stagingAgent interface {
	// Prompt starts the agent unless it runs and sends it text as a new
	// turn; it answers where the agent runs and the turn's run.
	Prompt(ctx context.Context, text string) (stagingAgentAt, error)
	// Wait waits for the turn run to end, at most until until (timedOut:
	// it was interrupted then).
	Wait(ctx context.Context, run string, until time.Time) (timedOut bool, err error)
	// Resume finds the agent an earlier daemon left at work (a restart):
	// true when it still runs, and Wait goes on with its run.
	Resume(ctx context.Context) (bool, error)
	// Close stops the agent and closes its workspace.
	Close(ctx context.Context) error
	// Detach lets go of the agent without stopping it (the daemon stops:
	// the next start finds it again).
	Detach()
}

// stagingAgentAt is where a job's agent runs: its herdr name and pane, and
// the run of its turn.
type stagingAgentAt struct{ Name, Pane, Run string }

// stagingAgentJob is what a job's agent is made from: the job's id and
// directory, the slot it works in, its role and its pane's env.
type stagingAgentJob struct {
	ID, Dir, Cwd string
	Role         config.Role
	Env          map[string]string
}

// resolveResultFile is what the agent writes: resolved or gave up, why,
// the specs it ran and their result.
type resolveResultFile struct {
	Outcome     string   `json:"outcome"`
	Why         string   `json:"why"`
	Specs       []string `json:"specs"`
	SpecsResult string   `json:"specs_result"`
}

// specsWords names the specs that passed, for a done job's Why.
func (r resolveResultFile) specsWords() string {
	return "specs passed: " + listedPaths(r.Specs)
}

// readResolveResult reads the result file at path strictly: one JSON
// object of at most maxResolveResult bytes with exactly its fields, an
// outcome and a specs result it knows, one-line words, a resolution that
// ran specs and saw them pass, and a give-up that says why. Its words are
// the agent's: they reach a Why one line long and cut.
func readResolveResult(path string) (resolveResultFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return resolveResultFile{}, fmt.Errorf("the result file %s was not written", path)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResolveResult+1))
	if err != nil {
		return resolveResultFile{}, err
	}
	if len(b) > maxResolveResult {
		return resolveResultFile{}, fmt.Errorf("the result file is over %d bytes", maxResolveResult)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r resolveResultFile
	if err := dec.Decode(&r); err != nil {
		return resolveResultFile{}, fmt.Errorf("the result file is no result: %s", oneLine(err.Error(), 120))
	}
	if dec.More() {
		return resolveResultFile{}, errors.New("the result file holds more than one object")
	}
	lines := append([]string{r.Outcome, r.Why, r.SpecsResult}, r.Specs...)
	for _, s := range lines {
		if strings.ContainsAny(s, "\n\r") || len(s) > 300 {
			return resolveResultFile{}, errors.New("the result file's words must be one line each, at most 300 bytes")
		}
	}
	switch {
	case len(r.Specs) > 50:
		return resolveResultFile{}, errors.New("the result file names more than 50 specs")
	case r.Outcome != resolveOutcomeResolved && r.Outcome != resolveOutcomeGaveUp:
		return resolveResultFile{}, fmt.Errorf("the result file's outcome %q is not %s or %s", oneLine(r.Outcome, 40), resolveOutcomeResolved, resolveOutcomeGaveUp)
	case r.SpecsResult != resolveSpecsPassed && r.SpecsResult != resolveSpecsFailed && r.SpecsResult != resolveSpecsNotRun:
		return resolveResultFile{}, fmt.Errorf("the result file's specs_result %q is not %s, %s or %s", oneLine(r.SpecsResult, 40),
			resolveSpecsPassed, resolveSpecsFailed, resolveSpecsNotRun)
	case r.Outcome == resolveOutcomeResolved && (len(r.Specs) == 0 || r.SpecsResult != resolveSpecsPassed):
		return resolveResultFile{}, errors.New("a resolution needs the specs it ran, passed: nothing unverified goes to a staging")
	case r.Outcome == resolveOutcomeGaveUp && strings.TrimSpace(r.Why) == "":
		return resolveResultFile{}, errors.New("a give-up needs its why")
	}
	r.Why = oneLine(r.Why, 200)
	return r, nil
}

// resolveData feeds the resolver's prompt (prompts/staging-resolve.md):
// shas, paths and magnum's own words, never PR text.
type resolveData struct {
	Ref           string   // owner/name#N
	Target        string   // the staging branch
	TargetTip     string   // its tip, checked out (detached)
	Head          string   // the PR's head, merged into it
	Checkout      string   // the slot
	Conflicts     []string // the conflicted paths
	DBLockCommand string   // the line every command that touches the databases runs through
	ResultFile    string   // where the agent writes its result
	Try           int      // 1 or 2
	LastWhy       string   // on try 2: why try 1 failed
}

// resolvePrompt renders the job's prompt.
func (e *Engine) resolvePrompt(job resolveJob, slotPath, result, lastWhy string) (string, error) {
	p, err := e.cfg.ResolvePrompt(StagingResolvePrompt)
	if err != nil {
		return "", fmt.Errorf("the staging resolver's prompt cannot be read: %w", err)
	}
	return agents.RenderPrompt(p, resolveData{Ref: job.label(), Target: job.Target, TargetTip: job.TargetTip, Head: job.Head,
		Checkout: slotPath, Conflicts: job.Conflicts, DBLockCommand: agents.DBLockLine(e.d.Layout.Binary(), slotPath, "staging-resolve"),
		ResultFile: result, Try: job.Try, LastWhy: lastWhy})
}

// resolveNudge is the one reminder after a turn without a valid result:
// magnum's own words and the file's path.
func resolveNudge(result string, problem error) string {
	return fmt.Sprintf("The result file %s is not valid yet: %s. Write it as the first prompt says (write %s.tmp first, then move it into place), then stop.",
		result, oneLine(problem.Error(), 200), result)
}

// resolveBlanked are the variables the agent's pane gets empty: the
// GitHub tokens gh and git read, and the SSH agent's socket.
var resolveBlanked = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SSH_AUTH_SOCK"}

// resolvePaneEnv is the agent's pane env: the slot's own (its databases),
// with no GitHub token (resolveBlanked), an empty gh config dir under the
// job's directory, no global or system git config (no credential helper
// of yours), no askpass and no SSH agent.
func resolvePaneEnv(dir string, slotEnv map[string]string) map[string]string {
	env := map[string]string{}
	maps.Copy(env, slotEnv)
	for _, k := range resolveBlanked {
		env[k] = ""
	}
	maps.Copy(env, map[string]string{ //nolint:gosec // G101: the names of git's askpass variables, no credential
		"GH_CONFIG_DIR":     filepath.Join(dir, "gh"),
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS": "/usr/bin/false", "SSH_ASKPASS": "/usr/bin/false",
	})
	return env
}

// newAgent makes the job's agent (Deps.stagingAgents), in its slot.
func (w *resolveWork) newAgent() (stagingAgent, error) {
	job, e := w.job, w.e
	if e.d.stagingAgents == nil {
		return nil, resolveFail("no agent: the daemon has no herdr for the resolver's pane")
	}
	slot, err := e.st.SlotByID(context.Background(), job.SlotID)
	if err != nil {
		return nil, err
	}
	until := max(job.Began.Add(resolveRunFor).Sub(e.now()), time.Minute)
	role, err := e.resolveRole(job.Repo, until)
	if err != nil {
		return nil, err
	}
	dir := e.resolveDir(job.ID)
	if err := os.MkdirAll(filepath.Join(dir, "gh"), 0o700); err != nil {
		return nil, err
	}
	var env map[string]string
	if repo, err := e.st.RepoByFullName(context.Background(), job.Repo); err == nil {
		env = e.slotEnv(&roundJob{repo: repo, pool: e.cfg.PoolFor(job.Repo), slot: slot, pr: store.PR{Number: job.Number}})
	}
	return e.d.stagingAgents(stagingAgentJob{ID: job.ID, Dir: dir, Cwd: slot.Path, Role: role, Env: resolvePaneEnv(dir, env)})
}

// resolveWithAgent has the agent resolve the conflicts: a restart's job
// goes on with the agent it left at work, else the agent starts and gets
// the prompt (on try 2 with try 1's why); a turn without a valid result
// gets one nudge. It answers the result file.
func (w *resolveWork) resolveWithAgent(ctx context.Context, deadline time.Time) (resolveResultFile, error) {
	job, e := w.job, w.e
	slot, err := e.st.SlotByID(ctx, job.SlotID)
	if err != nil {
		return resolveResultFile{}, err
	}
	result := filepath.Join(e.resolveDir(job.ID), resolveResultName)
	if w.agent == nil {
		if w.agent, err = w.newAgent(); err != nil {
			return resolveResultFile{}, err
		}
	}
	over := resolveFail("ran over %s; its agent was stopped and nothing was resolved", textx.DurationWords(resolveRunFor))
	turn := func(text string) error {
		at, err := w.agent.Prompt(ctx, text)
		if err != nil {
			return err
		}
		job.Agent, job.Pane, job.Run = at.Name, at.Pane, at.Run
		if err := w.say(ctx, fmt.Sprintf("the agent resolves %s in slot %s: pane %s of the herdr workspace %q",
			textx.Count(len(job.Conflicts), "conflicted path", "conflicted paths"), slot.Name, at.Pane, stagingWorkspace)); err != nil {
			return err
		}
		timedOut, err := w.agent.Wait(ctx, at.Run, deadline)
		switch {
		case timedOut || err == nil && !e.now().Before(deadline):
			return over
		case err != nil:
			return err
		}
		return nil
	}
	if job.Run != "" { // a restart: the agent goes on, or is gone
		alive, err := w.agent.Resume(ctx)
		switch {
		case err != nil:
			return resolveResultFile{}, err
		case !alive:
			return resolveResultFile{}, resolveFail("its agent is gone: the daemon restarted while it worked, and its pane %s no longer runs it", cmpPane(job.Pane))
		}
		timedOut, err := w.agent.Wait(ctx, job.Run, deadline)
		switch {
		case timedOut || err == nil && !e.now().Before(deadline):
			return resolveResultFile{}, over
		case err != nil:
			return resolveResultFile{}, err
		}
	} else {
		if err := os.Remove(result); err != nil && !errors.Is(err, os.ErrNotExist) {
			return resolveResultFile{}, err
		}
		text, err := e.resolvePrompt(*job, slot.Path, result, w.lastWhy(ctx))
		if err != nil {
			return resolveResultFile{}, resolveFail("%s", oneLine(err.Error(), 200))
		}
		if err := turn(text); err != nil {
			return resolveResultFile{}, err
		}
	}
	res, err := readResolveResult(result)
	if err == nil {
		return res, nil
	}
	if err := turn(resolveNudge(result, err)); err != nil {
		return resolveResultFile{}, err
	}
	if res, err = readResolveResult(result); err != nil {
		return resolveResultFile{}, resolveFail("the agent wrote no valid result after a nudge: %s", oneLine(err.Error(), 200))
	}
	return res, nil
}

// cmpPane names a pane for a Why ("its pane" when none is known).
func cmpPane(pane string) string {
	if pane == "" {
		return "(unknown)"
	}
	return pane
}

// lastWhy is why the effect's try 1 failed, for try 2's prompt ("" for a
// first try).
func (w *resolveWork) lastWhy(ctx context.Context) string {
	if w.job.Try < 2 {
		return ""
	}
	jobs, err := w.e.resolveJobs(ctx)
	if err != nil {
		return ""
	}
	why := ""
	for _, j := range jobs {
		if j.Effect == w.job.Effect && j.Try == w.job.Try-1 && j.State == resolveFailed {
			why = j.Why
		}
	}
	return oneLine(why, 200)
}

// stagingPaneSpec is the resolver's pane agent for role.
func stagingPaneSpec(role config.Role) paneSpec {
	return paneSpec{what: "staging resolver", workspace: stagingWorkspace, owner: learnRepoOwner, name: "staging",
		role: func(*config.Config) config.Role { return role }}
}

// paneStagingAgents is Deps.stagingAgents for the pane agent.
func paneStagingAgents(d paneDeps) func(stagingAgentJob) (stagingAgent, error) {
	d = d.withDefaults()
	return func(j stagingAgentJob) (stagingAgent, error) {
		return &paneStagingAgent{paneAgent: &paneAgent{d: d, spec: stagingPaneSpec(j.Role), dir: j.Dir, id: j.ID, cwd: j.Cwd, env: j.Env}}, nil
	}
}

// paneStagingAgent is one job's agent.
type paneStagingAgent struct{ *paneAgent }

func (a *paneStagingAgent) Prompt(ctx context.Context, text string) (stagingAgentAt, error) {
	if err := a.setup(ctx); err != nil {
		return stagingAgentAt{}, fmt.Errorf("the agent could not be set up: %w", err)
	}
	if pause, err := a.ensureAgent(ctx); err != nil {
		if pause != nil {
			return stagingAgentAt{}, resolveFail("the agent could not start: %s %s", pause.Tool, pause.Kind)
		}
		return stagingAgentAt{}, fmt.Errorf("the agent could not start: %w", err)
	}
	a.observe(ctx)
	kind := store.RunInitial
	if a.turns > 0 {
		kind = store.RunNudge
	}
	a.turns++
	run, err := a.mgr.NewRun(ctx, a.pr, agents.Role(a.role.Name), kind, a.turns)
	if err != nil {
		return stagingAgentAt{}, err
	}
	if err := a.mgr.Submit(ctx, run, text); err != nil {
		cur, rerr := a.st.RunByID(context.WithoutCancel(ctx), run.ID)
		switch {
		case rerr != nil:
			return stagingAgentAt{}, errors.Join(err, rerr)
		case cur.State == store.RunPending, cur.State == store.RunFailed, cur.State == store.RunAbandoned:
			a.finish(ctx, run.ID, err)
			return stagingAgentAt{}, fmt.Errorf("the prompt did not reach the agent: %w", err)
		}
		a.d.Logger.Warn("staging resolver: the prompt was not acknowledged as working; waiting for the turn", "run", run.ID, "err", err)
	}
	at := stagingAgentAt{Run: run.ID}
	if s, err := a.st.LiveSessionByPRRole(ctx, a.pr.ID, a.role.Name); err == nil {
		at.Name, at.Pane = store.Deref(s.AgentName), store.Deref(s.HerdrPaneID)
	}
	return at, nil
}

func (a *paneStagingAgent) Wait(ctx context.Context, run string, until time.Time) (bool, error) {
	a.role.Timeout = config.Duration{Duration: max(until.Sub(a.d.Now()), time.Second)}
	t := a.wait(ctx, run)
	return t.timedOut, t.err
}

func (a *paneStagingAgent) Resume(ctx context.Context) (bool, error) {
	if err := a.setup(ctx); err != nil {
		return false, err
	}
	s, err := a.st.LiveSessionByPRRole(ctx, a.pr.ID, a.role.Name)
	if errors.Is(err, store.ErrNotFound) || err == nil && s.State != store.SessionLive {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if a.d.Herdr == nil {
		return false, nil
	}
	snap, err := a.d.Herdr.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	if _, ok := snap.AgentByName(store.Deref(s.AgentName)); !ok {
		return false, nil
	}
	a.observe(ctx)
	return true, nil
}

func (a *paneStagingAgent) Detach() { a.detach() }

// Close stops the agent. A job a restart found has its scratch registry
// reopened first, so the agent it records is parked or quit and its
// workspace closed.
func (a *paneStagingAgent) Close(ctx context.Context) error {
	if a.mgr == nil {
		if _, err := os.Stat(filepath.Join(a.dir, learnScratch)); err == nil {
			if err := a.setup(ctx); err != nil {
				return err
			}
		}
	}
	return a.paneAgent.Close(ctx)
}
