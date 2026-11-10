package engine

// The Actions calls of the look, the poll and magnum's pushes: the start of
// [[repo]] ci_dispatch's workflow, the re-run of a run that started no job,
// and the reads of a head's runs, a run's jobs and a workflow's runs, as
// ciDispatchIdentity, one batch at a time (ciActions).

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/github"
)

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
}

// ciActions starts a batch of Actions calls as identity.
func (e *Engine) ciActions(identity string) *ciActions { return &ciActions{e: e, identity: identity} }

// client is the batch's client, made at its first call: identity's own.
// Later calls return the first's.
func (b *ciActions) client(context.Context) (GitHub, error) {
	if b.asked {
		return b.gh, b.err
	}
	b.asked = true
	if b.gh = b.e.gh(b.identity); b.gh == nil {
		b.err = fmt.Errorf("%w of %s", errNoGitHub, b.identity)
	}
	return b.gh, b.err
}

// end ends the batch.
func (b *ciActions) end() {}

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
