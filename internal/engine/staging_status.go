package engine

// The review daemon's read side of internal/staging: where each open PR of
// a repository with staging environments (its [[factory.repo.staging]]
// entries, a [[watch]] covering it) stands on each of them. FromApp makes
// one client (Deps.Staging) when any such repository exists, none
// otherwise, and then nothing here runs. Each tick starts every such
// repository's Refresh on a goroutine of its own, at most one per
// repository at a time (the first one clones the mirror and a later one
// fetches: never inline in the tick), with the open PRs' numbers and bases,
// and tells every open PR's Status, a pure lookup over the last Refresh,
// once the repository's first Refresh in this daemon returned. A PR's
// status is kept compactly under KVPRStaging, written when it changed and
// dropped once the PR is no longer open, so the board and `magnum staging`
// read it without git. The stop ends the Refreshes in flight, waits for
// them and then closes the client (shutdown).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// Staging is the read side of internal/staging the review daemon runs on:
// *staging.Client.
type Staging interface {
	Refresh(ctx context.Context, repo string, open []staging.OpenPR) error
	Status(pr staging.PR, envs []staging.Env) []staging.EnvStatus
	// The deploy side (staging_deploy.go): a plan of a PR's merge per env,
	// the effects that deploy it on the operator's word, a step of every
	// live effect, the effects of a repository, and a cancel.
	Plan(ctx context.Context, pr staging.PR, envs []staging.Env) (staging.DeployPlan, error)
	Deploy(ctx context.Context, plan staging.DeployPlan, by string) ([]staging.EffectID, error)
	Step(ctx context.Context) error
	Effects(repo string) []staging.Effect
	Cancel(ctx context.Context, id staging.EffectID) error
	Close()
}

// stagingRefreshTimeout bounds one Refresh in the daemon. A first clone
// runs in the background on a context of its own, so this one only bounds
// a fetch and GitHub's reads.
const stagingRefreshTimeout = 10 * time.Minute

// The backoff of a repository whose Refresh panicked: a minute after the
// first panic in a row, twice as long after each next one, at most
// stagingPanicMax (the factory daemon's rule), so a bug costs no panic
// event per tick.
const (
	stagingPanicFirst = time.Minute
	stagingPanicMax   = 30 * time.Minute
)

// stagingEnvTimeout bounds the poll identity's git env of a fetch (an App
// mints a token for it).
const stagingEnvTimeout = time.Minute

// StagingRepos lists the repositories with staging environments a [[watch]]
// covers, as their [[factory.repo]] names them, in config order: the ones
// the review daemon tells the staging status of.
func StagingRepos(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, r := range cfg.Factory.Repos {
		if len(r.Staging) > 0 && cfg.WatchFor(r.Repo) != nil &&
			!slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, r.Repo) }) {
			out = append(out, r.Repo)
		}
	}
	return out
}

// StagingEnvs is repo's staging environments (config.Stagings) as
// internal/staging names them, in config order; nil for none.
func StagingEnvs(cfg *config.Config, repo string) []staging.Env {
	if cfg == nil {
		return nil
	}
	var out []staging.Env
	for _, s := range cfg.Stagings(repo) {
		out = append(out, staging.Env{Name: s.Name, Branch: s.Branch})
	}
	return out
}

// KVPRStaging holds where the PR stands on its repository's staging envs
// as the review daemon last told it (StagingRecord as JSON): written when
// it changes, dropped once the PR is no longer open. The board and
// `magnum staging` read it (StagingShown); the daemon alone writes it.
func KVPRStaging(prID int64) string { return fmt.Sprintf("pr.%d.staging", prID) }

// StagingRecord is a PR's KVPRStaging: the head its envs were told for and,
// in the config's order, each env's state.
type StagingRecord struct {
	Head string       `json:"head"`
	Envs []StagingEnv `json:"envs"`
}

// StagingEnv is a PR's state on one staging env (staging.EnvStatus, what
// the board and the CLI show of it): shas, times, a run URL and the
// package's own words, never PR text.
type StagingEnv struct {
	Env    string `json:"env"`
	State  string `json:"state"`            // staging.State: latest, pushed, outdated, none, unknown
	Detail string `json:"detail,omitempty"` // pushed: staging.Detail (ci_running, ci_failed_ours, ...)
	// InTip is the PR commit staging-<env>'s tip holds: the head, or for
	// outdated an older one, Behind commits before the head.
	InTip  string `json:"in,omitempty"`
	Behind int    `json:"behind,omitempty"`
	// PushedAt is when the merge that brought InTip in was made, and
	// DeployedAt, for latest, when the deployment that holds it was.
	PushedAt   time.Time `json:"pushed_at,omitzero"`
	DeployedAt time.Time `json:"deployed_at,omitzero"`
	RunURL     string    `json:"run,omitempty"` // ci.yml's run on that merge
	Why        string    `json:"why,omitempty"` // unknown, or what could not be told
}

// ParseStagingRecord reads a KVPRStaging value; false when it cannot be
// read or names no head.
func ParseStagingRecord(v string) (StagingRecord, bool) {
	var r StagingRecord
	if json.Unmarshal([]byte(v), &r) != nil || r.Head == "" {
		return StagingRecord{}, false
	}
	return r, true
}

// stagingRecordOf is the record of the statuses ss told for head.
func stagingRecordOf(head string, ss []staging.EnvStatus) StagingRecord {
	r := StagingRecord{Head: head, Envs: make([]StagingEnv, 0, len(ss))}
	for _, s := range ss {
		e := StagingEnv{Env: s.Env.Name, State: string(s.State), Detail: string(s.Detail), Why: s.Why}
		switch s.State {
		case staging.StateLatest, staging.StatePushed, staging.StateOutdated:
			e.InTip, e.Behind, e.PushedAt, e.RunURL = s.InTip, s.Behind, s.PushedAt, s.RunURL
		}
		if s.State == staging.StateLatest {
			e.DeployedAt = s.DeployedAt
		}
		r.Envs = append(r.Envs, cleanStagingEnv(e))
	}
	return r
}

// stagings is the tick's part: every staging repository's Refresh started
// off the tick, every open PR's status told and written when it changed,
// and the records of the PRs no longer open dropped. Nothing without a
// staging client.
func (e *Engine) stagings(ctx context.Context) {
	s := e.d.Staging
	if s == nil {
		return
	}
	repos := StagingRepos(e.cfg)
	if len(repos) == 0 {
		return
	}
	e.tendStagingDeploys(ctx, s)  // staging_events.go
	e.tendStagingResolves(ctx, s) // staging_resolve.go
	records, err := e.st.KVValues(ctx, "pr.*.staging")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging: read the records")
		return
	}
	keep, sweep := map[string]bool{}, true
	for _, repo := range repos {
		r, err := e.st.RepoByFullName(ctx, repo)
		if errors.Is(err, store.ErrNotFound) {
			continue // not polled yet: no PR to tell
		}
		if err == nil {
			var prs []store.PR
			if prs, err = e.st.ListPRs(ctx, store.PRFilter{RepoID: r.ID, GHOpen: true}); err == nil {
				e.tellStagings(ctx, s, repo, prs, records, keep)
				continue
			}
		}
		e.warnUnlessStopped(ctx, err, "staging: read the open PRs", "repo", repo)
		sweep = false // its PRs are not known: drop nothing this tick
	}
	if !sweep {
		return
	}
	for _, k := range slices.Sorted(maps.Keys(records)) {
		var id int64
		if _, err := fmt.Sscanf(k, "pr.%d.staging", &id); err == nil && KVPRStaging(id) == k && !keep[k] {
			e.delKV(ctx, k)
		}
	}
}

// tellStagings starts repo's Refresh with its open PRs and, once one
// returned in this daemon, writes each PR's status that changed from
// records; every PR's key goes into keep.
func (e *Engine) tellStagings(ctx context.Context, s Staging, repo string, prs []store.PR, records map[string]string, keep map[string]bool) {
	e.startStagingRefresh(ctx, s, repo, prs)
	e.stagingMu.Lock()
	read := e.stagingRead[strings.ToLower(repo)]
	e.stagingMu.Unlock()
	envs := StagingEnvs(e.cfg, repo)
	for _, pr := range prs {
		k := KVPRStaging(pr.ID)
		keep[k] = true
		if !read { // "no refresh yet" on every env: the record a restart left stands
			continue
		}
		ss := s.Status(staging.PR{Repo: repo, Number: pr.Number, Head: pr.HeadSHA, Base: deref(pr.BaseRef)}, envs)
		b, err := json.Marshal(stagingRecordOf(pr.HeadSHA, ss))
		if err != nil {
			continue
		}
		if v := string(b); records[k] != v {
			e.setKV(ctx, k, v)
		}
	}
}

// startStagingRefresh starts repo's Refresh with prs on a goroutine of its
// own, on ctx bounded by stagingRefreshTimeout, unless one runs; when it
// returns, the repository counts as read, and a failure is logged once
// per cause.
func (e *Engine) startStagingRefresh(ctx context.Context, s Staging, repo string, prs []store.PR) {
	k := strings.ToLower(repo)
	e.stagingMu.Lock()
	if e.stagingBusy[k] || ctx.Err() != nil || e.stagingCtx.Err() != nil || e.now().Before(e.stagingRetryAt[k]) {
		e.stagingMu.Unlock()
		return
	}
	e.stagingBusy[k] = true
	e.stagingMu.Unlock()
	open := make([]staging.OpenPR, 0, len(prs))
	for _, p := range prs {
		open = append(open, staging.OpenPR{Number: p.Number, Base: deref(p.BaseRef)})
	}
	e.stagingWG.Go(func() {
		defer func() {
			e.stagingMu.Lock()
			delete(e.stagingBusy, k)
			e.stagingRead[k] = true
			e.stagingMu.Unlock()
		}()
		panicked := e.safely(ctx, "staging refresh", repo, func() {
			rctx, cancel := context.WithTimeout(ctx, stagingRefreshTimeout)
			defer cancel()
			defer context.AfterFunc(e.stagingCtx, cancel)()
			e.noteStagingRefresh(rctx, repo, s.Refresh(rctx, repo, open))
		}, nil)
		e.stagingMu.Lock()
		defer e.stagingMu.Unlock()
		if !panicked {
			delete(e.stagingPanics, k)
			delete(e.stagingRetryAt, k)
			return
		}
		e.stagingPanics[k]++
		e.stagingRetryAt[k] = e.now().Add(stagingBackoff(e.stagingPanics[k]))
	})
}

// noteStagingRefresh logs repo's failed Refresh once while its cause stays
// the same, and that it reads again after one.
func (e *Engine) noteStagingRefresh(ctx context.Context, repo string, err error) {
	k := strings.ToLower(repo)
	if err != nil && isStop(ctx, err) {
		return
	}
	msg := ""
	if err != nil {
		msg = execx.Redact(err.Error())
	}
	e.stagingMu.Lock()
	last := e.stagingLogged[k]
	if msg == "" {
		delete(e.stagingLogged, k)
	} else {
		e.stagingLogged[k] = msg
	}
	e.stagingMu.Unlock()
	switch {
	case msg != "" && msg != last:
		e.log.Warn("staging: the status refresh failed; the PRs' staging status says why", "repo", repo, "err", msg)
	case msg == "" && last != "":
		e.log.Info("staging: the status refresh reads again", "repo", repo)
	}
}

// stopStaging ends the Refreshes in flight, waits for them at most wait,
// and then closes the client: its background clones end, and its env
// flocks are let go. No Refresh starts after it.
func (e *Engine) stopStaging(wait time.Duration) {
	e.stagingStop()
	if e.d.Staging == nil {
		return
	}
	done := make(chan struct{})
	go func() { e.stagingWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		e.log.Warn("staging: a status refresh did not stop in time; closing the client under it", "wait", wait)
	}
	e.d.Staging.Close()
}

// stagingFromApp is the review daemon's staging client (FromApp) on
// stagingDepsOf's deps, b's parts bound to the engine once it exists, when
// a watched repository has staging environments; nil otherwise, for a dry
// run and for a tagged App (a replay, the factory's local review).
func stagingFromApp(a *app.App, b *stagingBound) Staging {
	cfg := a.Config
	if cfg == nil || a.DryRun || a.AgentTag != "" || a.Runner == nil || len(StagingRepos(cfg)) == 0 {
		return nil
	}
	return staging.New(stagingDepsOf(a, b))
}

// stagingRunner runs the mirrors' git in the C locale, since
// internal/staging reads git's English answers ("not a git repository"),
// and quiet (logged at Debug), since it runs every minute and Refresh
// reports its failures itself.
type stagingRunner struct{ execx.Runner }

func (r stagingRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	env := make(map[string]string, len(c.Env)+1)
	maps.Copy(env, c.Env)
	env["LC_ALL"] = "C"
	c.Env, c.Quiet = env, true
	return r.Runner.Run(ctx, c)
}

// stagingIDs are the poll identities the staging status reads as: the
// watch's poll_identity (else its identity) of each repository.
type stagingIDs struct {
	a      *app.App
	mu     sync.Mutex
	warned map[string]string // by repository: the cause last logged
}

// identity is repo's poll identity's name ("" = no watch covers it).
func (s *stagingIDs) identity(repo string) string {
	if w := s.a.Config.WatchFor(repo); w != nil {
		return cmp.Or(w.PollIdentity, w.Identity)
	}
	return ""
}

// client is repo's poll identity's GitHub client; nil for none.
func (s *stagingIDs) client(repo string) *github.Client {
	if s.a.GitHub == nil {
		return nil
	}
	return s.a.GitHub(s.identity(repo))
}

// gitEnv is staging.Deps.GitEnv: repo's poll identity's env (an App's gh
// config dir with a fresh token, GH_HOST pinned), which gh, git's only
// credential helper there, reads its login from; nil when there is none,
// and the fetch then reads nothing (logged once per cause).
func (s *stagingIDs) gitEnv(repo string) map[string]string {
	name := s.identity(repo)
	src := s.a.Identities[name]
	if src == nil {
		s.warn(repo, fmt.Errorf("identity %q is not configured", name))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), stagingEnvTimeout)
	defer cancel()
	env, err := src.Env(ctx)
	if err != nil {
		s.warn(repo, err)
		return nil
	}
	s.mu.Lock()
	delete(s.warned, repo)
	s.mu.Unlock()
	return env
}

func (s *stagingIDs) warn(repo string, err error) {
	msg := execx.Redact(err.Error())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warned[repo] == msg {
		return
	}
	s.warned[repo] = msg
	if s.a.Logger != nil {
		s.a.Logger.Warn("staging: no git env for the status fetch", "repo", repo, "err", msg)
	}
}

// StagingShown is what a reader shows of a PR's staging status, per env of
// envs (the config's, in its order; nil without any): the env's state in
// rec, or unknown, Why saying so, when there is no record (ok false) or
// the record lacks the env (not read yet), it was told for another head
// than head (the head moved since; the daemon's next tick tells it), or
// the daemon's last tick (lastTick; zero: it never ran) is more than
// staging.StaleAfter before now, so the record no longer follows staging.
func StagingShown(rec StagingRecord, ok bool, envs []staging.Env, head string, lastTick, now time.Time) []StagingEnv {
	if len(envs) == 0 {
		return nil
	}
	why := StagingStale(lastTick, now)
	switch {
	case why != "":
	case !ok:
		why = "not read yet"
	case rec.Head != head:
		why = "the head moved since the last read"
	}
	out := make([]StagingEnv, len(envs))
	for i, env := range envs {
		j := slices.IndexFunc(rec.Envs, func(e StagingEnv) bool { return e.Env == env.Name })
		switch {
		case why != "":
			out[i] = StagingEnv{Env: env.Name, State: string(staging.StateUnknown), Why: why}
		case j < 0:
			out[i] = StagingEnv{Env: env.Name, State: string(staging.StateUnknown), Why: "not read yet"}
		default:
			out[i] = cleanStagingEnv(rec.Envs[j])
		}
	}
	return out
}

// StagingFirst is the migration a PR at head must take to a staging
// before its merge: staging.DeployBeforeMerge over files, the head's file
// list (store.PRFilesOf, which the poll writes from GitHub's Details), and
// shown, the envs as StagingShown shows them (unknown holds nothing back),
// its control characters spaces; "" when it need not, or files is another
// head's or not listed (listed false). The look's verdict and the board
// share it, and say it in staging.DeployBeforeMergeWords.
func StagingFirst(files store.PRFiles, listed bool, head string, shown []StagingEnv) string {
	if !listed || files.HeadSHA != head || len(shown) == 0 {
		return ""
	}
	envs := make([]staging.EnvStatus, len(shown))
	for i, s := range shown {
		envs[i] = staging.EnvStatus{Env: staging.Env{Name: s.Env}, State: staging.State(s.State)}
	}
	if m, needed := staging.DeployBeforeMerge(files.Paths, envs); needed {
		return stagingClean(m)
	}
	return ""
}

// cleanStagingEnv is e with no control character in its words (a failing
// job's paths come from the PR's files) and its run only as an https page.
func cleanStagingEnv(e StagingEnv) StagingEnv {
	clean := stagingClean
	e.Env, e.State, e.Detail, e.InTip, e.Why = clean(e.Env), clean(e.State), clean(e.Detail), clean(e.InTip), clean(e.Why)
	if u := clean(e.RunURL); u != e.RunURL || !strings.HasPrefix(u, "https://") || strings.ContainsAny(u, " ") {
		e.RunURL = ""
	}
	return e
}

// stagingClean is s with every control character a space: staging's
// words may name a PR's paths.
func stagingClean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// StagingStale is why the daemon's staging records no longer follow
// staging at now, its last tick being lastTick: "the daemon has not run"
// (zero), "stale: the daemon's last tick is 1h old" (more than
// staging.StaleAfter ago); "" while they do.
func StagingStale(lastTick, now time.Time) string {
	age := now.Sub(lastTick)
	switch {
	case lastTick.IsZero():
		return "the daemon has not run"
	case age <= staging.StaleAfter:
		return ""
	}
	s := strings.TrimSuffix(age.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return "stale: the daemon's last tick is " + s + " old"
}
