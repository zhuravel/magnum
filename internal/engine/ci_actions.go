package engine

// The Actions calls of the look, the poll and magnum's pushes: the start of
// [[repo]] ci_dispatch's workflow, the re-run of a run that started no job,
// and the reads of a head's runs, a run's jobs and a workflow's runs, as
// ciDispatchIdentity, one batch at a time (ciActions). A gh login, and an
// App whose tokens may use Actions, make them with the identity's own
// client, as before. An App whose token_permissions leave actions =
// "write" out (a fixer App: contents and pull_requests write alone, which
// its panes hold in its gh config dir) makes them over a token of the
// daemon's own with Actions write added (identity.App.ActionsEnv,
// push_token.go's mint), minted at a batch's first call, handed only in
// the env of its gh commands (GH_TOKEN) and revoked at the batch's end:
// never in a file, so no pane gains a token that may start workflows.

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
)

// actionsMinter is an identity that mints a token of the daemon's own for
// a batch of Actions calls (*identity.App has it).
type actionsMinter interface {
	ActionsEnv(ctx context.Context) (map[string]string, func(), error)
}

var _ actionsMinter = (*identity.App)(nil)

// errNoGitHub is a batch's error when the engine has no client of its
// identity.
var errNoGitHub = errors.New("no GitHub client")

// ciActions is one batch of Actions calls as identity: one look of a PR,
// one poll of a repository, one decision on magnum's push. It runs on one
// goroutine.
type ciActions struct {
	e        *Engine
	identity string
	asked    bool
	gh       GitHub
	err      error
	release  func()
}

// ciActions starts a batch of Actions calls as identity; its end revokes
// the token minted for it.
func (e *Engine) ciActions(identity string) *ciActions { return &ciActions{e: e, identity: identity} }

// client is the batch's client, made at its first call: identity's own,
// or, for an App whose tokens leave Actions out (mintsActions), one over a
// token minted for the batch. Later calls return the first's, so a batch
// mints once.
func (b *ciActions) client(ctx context.Context) (GitHub, error) {
	if b.asked {
		return b.gh, b.err
	}
	b.asked = true
	own := b.e.gh(b.identity)
	m, ok := b.e.d.Identities[b.identity].(actionsMinter)
	switch {
	case own == nil:
		b.err = fmt.Errorf("%w of %s", errNoGitHub, b.identity)
	case !ok || !b.e.mintsActions(b.identity):
		b.gh = own
	case b.e.d.GitHubEnv == nil:
		b.err = fmt.Errorf("%w of %s over a token of its own", errNoGitHub, b.identity)
	default:
		env, release, err := m.ActionsEnv(ctx)
		if err != nil {
			b.err = err
			break
		}
		b.release = release
		if b.gh = b.e.d.GitHubEnv(b.identity, env); b.gh == nil {
			b.err = fmt.Errorf("%w of %s over a token of its own", errNoGitHub, b.identity)
		}
	}
	return b.gh, b.err
}

// end revokes the batch's token, if it minted one.
func (b *ciActions) end() {
	if b.release != nil {
		b.release()
		b.release = nil
	}
}

// mintsActions reports whether identity is an App whose token_permissions
// leave actions = "write" out: its own tokens cannot start workflows, and
// may not read Actions either. An App without token_permissions has its
// installation's whole access, as its own tokens do: no mint.
func (e *Engine) mintsActions(identity string) bool {
	id := e.cfg.IdentityByName(identity)
	return id != nil && id.Kind == "app" && len(id.TokenPermissions) > 0 && id.TokenPermissions["actions"] != "write"
}

// actionsFailed is how a start or a re-run as who that failed with err
// says it in its record (ciStart.Failed, ciRerun.Failed), never in
// GitHub's message: no client as such, else GitHub's status, a 403 with
// cannotStartWorkflows' words.
func (e *Engine) actionsFailed(who string, err error) string {
	if errors.Is(err, errNoGitHub) {
		return err.Error()
	}
	failed := "the call failed"
	var api *github.APIError
	if errors.As(err, &api) && api.Status != 0 {
		failed = fmt.Sprintf("GitHub answered HTTP %d", api.Status)
	}
	if errors.Is(err, github.ErrForbidden) {
		failed += ": " + e.cannotStartWorkflows(who)
	}
	return failed
}
