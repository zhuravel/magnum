package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// ghDown is what the GitHub client returned while gh could not connect, on
// talkable/talkable#12226 on 2026-11-12: gh's exit, which names no status.
func ghDown() error {
	return fmt.Errorf("github details talkable/talkable (1): %w", &execx.ExitError{
		Cmd:  execx.Cmd{Name: "gh", Args: []string{"api", "graphql", "--hostname", "github.com", "--input", "-"}},
		Code: 1, Stderr: "error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com\n"})
}

// gitDown is `git ls-remote`'s error while github.com cannot be reached.
func gitDown() error {
	return fmt.Errorf("git ls-remote: %w", &execx.ExitError{Cmd: execx.Cmd{Name: "git", Args: []string{"ls-remote", "origin"}}, Code: 128,
		Stderr: "fatal: unable to access 'https://github.com/talkable/talkable/': Could not resolve host: github.com\n"})
}

// The classifier of a failed read of the PR's head: only GitHub or the
// network not answering is transient (a connection error, a timeout, a
// 5xx), and only from what the GitHub client returns (a typed error, or
// gh's exit and its output); GitHub's own answer (401, 403, 404, a GraphQL
// error, a rate limit) and anything else is not.
func TestOnlyConnectionErrorsTimeoutsAnd5xxAreTransient(t *testing.T) {
	exit := func(stderr string) error {
		return fmt.Errorf("github details talkable/talkable (1): %w", &execx.ExitError{Cmd: execx.Cmd{Name: "gh"}, Code: 1, Stderr: stderr})
	}
	api := func(status int, msg string) error {
		return &github.APIError{Op: "details talkable/talkable (1)", Status: status, Message: msg}
	}
	graphql := func(status int) error {
		return &github.APIError{Op: "details talkable/talkable (1)", Status: status, Errors: []github.GraphQLError{{Type: "FORBIDDEN", Message: "Resource not accessible"}}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"gh cannot connect", ghDown(), true},
		{"a reset", exit("read tcp 10.0.0.2:52011->140.82.112.6:443: read: connection reset by peer"), true},
		{"a DNS failure", exit(`Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`), true},
		{"a refused dial", fmt.Errorf("github details: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), true},
		{"a DNS error", &net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}, true},
		{"gh's own timeout", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: context.DeadlineExceeded}), true},
		{"an i/o timeout", exit(`Post "https://api.github.com/graphql": net/http: TLS handshake timeout`), true},
		{"a 500", api(500, "Internal Server Error"), true},
		{"a 502", api(502, "Bad Gateway"), true},
		{"a 503", api(503, "Service Unavailable"), true},
		{"a 504", api(504, "Gateway Timeout"), true},
		{"a 5xx gh names in its exit", exit("gh: Bad Gateway (HTTP 502)"), true},
		{"git cannot reach the remote", gitDown(), true},
		{"GraphQL's time limit: an empty answer", fmt.Errorf("github details talkable/talkable (1): decode response: %w", json.Unmarshal(nil, new(any))), true},
		{"a cut answer", fmt.Errorf("github details talkable/talkable (1): decode data: %w", io.ErrUnexpectedEOF), true},
		{"an answer that is no JSON", fmt.Errorf("github details talkable/talkable (1): decode response: %w", json.Unmarshal([]byte("<html>"), new(any))), false},
		{"the read and its fallback both down", errors.Join(ghDown(), gitDown()), true},
		{"a 401", api(401, "Bad credentials"), false},
		{"a 403", api(403, "Resource not accessible by integration"), false},
		{"a 404", api(404, "Not Found"), false},
		{"a GraphQL error", graphql(0), false},
		{"a GraphQL error beside a 502", graphql(502), false},
		{"a rate limit", api(403, "API rate limit exceeded for user ID 1"), false},
		{"a 429", api(429, "Too Many Requests"), false},
		{"a rate limit gh names in its exit", exit("gh: API rate limit exceeded (HTTP 429)"), false},
		{"another exit", exit("gh: unknown flag: --input"), false},
		{"a plain error that names a 502", errors.New("graphql: 502 Bad Gateway"), false},
		{"no gh", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: exec.ErrNotFound}), false},
		{"a stop", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: context.Canceled}), false},
		{"GitHub does not say", errors.New("GitHub does not say where the PR's head is"), false},
		{"the read down but the branch gone", errors.Join(ghDown(), errors.New("origin has no branch feature")), false},
		{"a 403 and the fallback down", errors.Join(api(403, "Forbidden"), gitDown()), false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientRead(tc.err); got != tc.want {
				t.Errorf("transientRead(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// pushFixer is fixer for a fixer that pushes fixPushed with one commit,
// replies in 3 threads and leaves nothing to the author; down, when set,
// runs once its push is on GitHub (and before its run ends): what GitHub
// does to the settle's reads.
func pushFixer(h *harness, git *fixGit, down func()) {
	fixerBy(h, git, func(run store.Run) (string, string) {
		if down != nil {
			defer down()
		}
		return fixPushed, fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "none")
	})
}

// fixPushOf is PR #2's record of a push the settle confirms or could not
// confirm (ok false: none).
func (h *harness) fixPushOf(n int) (fixPush, bool) {
	h.t.Helper()
	rec, ok, err := readKVJSON[fixPush](h.ctx, h.e, kvPRFixPush(h.pr(n).ID), "fix push")
	if err != nil {
		h.t.Fatal(err)
	}
	return rec, ok
}

// fixData is the data of PR #2's last event of kind.
func (h *harness) fixData(kind string) map[string]any {
	h.t.Helper()
	ev, ok := h.fixEvent(kind)
	if !ok {
		h.t.Fatalf("no %s event", kind)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		h.t.Fatal(err)
	}
	return data
}

// wantPushedSettle checks the end of a settle of PR #2's fix as pushed: the
// slot at the pushed head, the loop waiting for its forced re-review of it
// (cycle 1), the push recorded for its CI, the fixer's run verified as
// pushed, fix.pushed and fix.replied once each, no record left and no
// fix.failed unless failed says one came first.
func (h *harness) wantPushedSettle(pr store.PR, failed int) {
	h.t.Helper()
	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || st.Head != fixPushed || st.Confirm != nil {
		h.t.Fatalf("fix state = %+v, %v; want the loop waiting for the re-review of the push", st, ok)
	}
	if cur := h.pr(2); !cur.Forced || cur.HeadSHA != fixPushed || (cur.State != store.PRRereviewPending && cur.State != store.PRQueued) {
		h.t.Errorf("PR: %s at %s forced %v, want the push forced to its re-review", cur.State, cur.HeadSHA, cur.Forced)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		h.t.Errorf("slot calls %v, want the slot at the pushed head", h.sl.all())
	}
	if v, _ := h.e.getKV(h.ctx, kvPRPushed(pr.ID)); !strings.Contains(v, fixPushed) {
		h.t.Errorf("magnum's push for its CI = %q, want %s", v, fixPushed)
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunVerified || deref(r.Outcome) != "pushed" {
		h.t.Errorf("fix run %s %s, want verified as pushed", r.State, deref(r.Outcome))
	}
	for kind, want := range map[string]int{"fix.pushed": 1, "fix.replied": 1, "fix.failed": failed} {
		if n := len(h.fixEvents(kind)); n != want {
			h.t.Errorf("%s events = %d, want %d", kind, n, want)
		}
	}
	if _, ok := h.fixPushOf(2); ok {
		h.t.Error("the record of the push stays")
	}
}

// A connection error while the settle confirms the fixer's push is retried,
// past the three looks a head GitHub shows otherwise gets, and the good
// answer that follows settles the fix as usual: on 2026-11-12 gh's "error
// connecting" during the confirmation failed a fix whose push had landed
// (talkable/talkable#12226), and the loop's row went while the fixer's pane
// stayed live.
func TestAConnectionErrorWhileThePushIsConfirmedIsRetriedAndTheFixSettlesAsUsual(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	var before int
	pushFixer(h, git, func() {
		before = h.gh.count("details:talkable/talkable:[2]")
		h.gh.mu.Lock()
		h.gh.detailsErrs = []error{ghDown(), ghDown(), ghDown(), ghDown(), ghDown()}
		h.gh.mu.Unlock()
		git.mu.Lock()
		git.lsErr = gitDown()
		git.mu.Unlock()
	})
	h.fix(FixPayload{})
	h.handle()

	h.wantPushedSettle(pr, 0)
	if n := h.gh.count("details:talkable/talkable:[2]") - before; n != 6 {
		t.Errorf("Details reads during the settle = %d, want the five that failed and the one that answered", n)
	}
}

// GitHub's own answer is no blip: a 403 or a GraphQL error while the settle
// confirms the push fails the fix at once, with no second read, as the
// typed "push not confirmed" failure.
func TestA403OrAGraphQLErrorWhileThePushIsConfirmedFailsAtOnce(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a 403", &github.APIError{Op: "details talkable/talkable (1)", Status: 403, Message: "Resource not accessible by integration"}},
		{"a GraphQL error", &github.APIError{Op: "details talkable/talkable (1)", Errors: []github.GraphQLError{{Type: "FORBIDDEN", Message: "no"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			var before int
			pushFixer(h, git, func() {
				before = h.gh.count("details:talkable/talkable:[2]")
				h.gh.fail(tc.err, nil)
				git.mu.Lock()
				git.lsErr = gitDown()
				git.mu.Unlock()
			})
			h.fix(FixPayload{})
			h.handle()

			cur := h.wantState(2, store.PRNeedsAttention)
			if le := deref(cur.LastError); !strings.HasPrefix(le, "fix settle failed: push not confirmed on GitHub: ") {
				t.Errorf("last_error = %q", le)
			}
			if n := h.gh.count("details:talkable/talkable:[2]") - before; n != 1 {
				t.Errorf("Details reads during the settle = %d, want 1", n)
			}
			if data := h.fixData("fix.failed"); data["kind"] != fixPushUnconfirmed {
				t.Errorf("fix.failed data = %v, want the kind %s", data, fixPushUnconfirmed)
			}
			if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed || rec.Pushed != fixPushed {
				t.Errorf("the record = %+v, %v; want the push unconfirmed", rec, ok)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
		})
	}
}

// shutdownDuringConfirmation runs a fix on PR #2 whose push lands while
// GitHub does not answer the settle's reads, and stops the daemon (the
// context the request was handled on ends) at the settle's first wait to
// read again. It returns the harness, the PR and the reads of the PR's
// Details during the settle.
func shutdownDuringConfirmation(t *testing.T) (*harness, store.PR, int) {
	t.Helper()
	var mu sync.Mutex
	var stop context.CancelFunc
	h, git := fixHarness(t, func(h *harness) {
		h.d.Sleep = func(ctx context.Context, _ time.Duration) error {
			mu.Lock()
			cancel := stop
			mu.Unlock()
			if cancel != nil {
				cancel()
				<-ctx.Done()
			}
			return ctx.Err()
		}
	})
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	ctx, cancel := context.WithCancel(h.ctx)
	t.Cleanup(cancel)
	var before int
	pushFixer(h, git, func() {
		before = h.gh.count("details:talkable/talkable:[2]")
		h.gh.fail(ghDown(), nil)
		git.mu.Lock()
		git.lsErr = gitDown()
		git.mu.Unlock()
		mu.Lock()
		stop = cancel
		mu.Unlock()
	})
	h.fix(FixPayload{})
	done := make(chan struct{})
	go func() {
		h.e.handleRequests(ctx)
		h.e.roundWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the settle's retry did not stop when its context ended")
	}
	mu.Lock()
	stop = nil
	mu.Unlock()
	h.settle()
	return h, pr, h.gh.count("details:talkable/talkable:[2]") - before
}

// A context that ends while the settle waits to read the PR's head again
// stops the retry at once: no read after it, no fix.failed, and the loop's
// row stays in phase fixing for the next start. A shutdown or a
// `daemon-restart --drain` never waits for the retry.
func TestAContextThatEndsDuringTheRetryStopsItAtOnce(t *testing.T) {
	h, _, reads := shutdownDuringConfirmation(t)
	if reads != 1 {
		t.Errorf("Details reads during the settle = %d, want the one before the wait the end cut", reads)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseFixing {
		t.Fatalf("fix state = %+v, %v; want the loop left for the next start", st, ok)
	}
	if n := len(h.fixEvents("fix.failed")); n != 0 {
		t.Errorf("fix.failed events = %d, want none before the restart", n)
	}
	if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushConfirming || rec.Pushed != fixPushed {
		t.Errorf("the record = %+v, %v; want the push confirming", rec, ok)
	}
}

// A shutdown while the settle retries leaves the loop for the next start,
// which fails it as the typed "push not confirmed"; the poll that then
// finds the pushed head as the PR's settles it as pushed.
func TestAShutdownDuringTheConfirmationFailsTheFixAsUnconfirmedAtTheStart(t *testing.T) {
	h, pr, _ := shutdownDuringConfirmation(t)
	h.e = New(h.d) // the next start
	h.startup()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix settle failed: push not confirmed on GitHub: ") {
		t.Errorf("last_error = %q", le)
	}
	if data := h.fixData("fix.failed"); data["kind"] != fixPushUnconfirmed {
		t.Errorf("fix.failed data = %v, want the kind %s", data, fixPushUnconfirmed)
	}

	h.gh.fail(nil, nil)
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.wantPushedSettle(pr, 1)
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 1 {
		t.Errorf("fix.push_confirmed_late events = %d, want 1", n)
	}
}

// A long outage keeps the loop in its settle: the row stays in phase
// fixing, saying it confirms the push, and nothing fails while the ticks
// try again. A tick that then finds the push as the PR's head settles the
// fix as usual; past the cap with no answer, the fix fails as the typed
// "push not confirmed".
func TestALongOutageKeepsTheLoopInItsSettleAcrossTicks(t *testing.T) {
	storetest.Parallel(t)
	for _, answers := range []bool{true, false} {
		t.Run(map[bool]string{true: "GitHub answers at a later tick", false: "GitHub never answers"}[answers], func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			pushFixer(h, git, func() {
				h.gh.mu.Lock()
				h.gh.detailsErr, h.gh.radarErr = ghDown(), ghDown()
				h.gh.mu.Unlock()
				git.mu.Lock()
				git.lsErr = gitDown()
				git.mu.Unlock()
			})
			h.fix(FixPayload{})
			h.handle()

			wantConfirming := func(when string) {
				t.Helper()
				st, ok := h.fixStateOf(2)
				if !ok || st.Phase != FixPhaseFixing || st.Confirm == nil || st.Confirm.Head != fixPushed {
					t.Fatalf("%s: fix state = %+v, %v; want the loop in its settle, confirming the push", when, st, ok)
				}
				if got, want := st.Confirming(), "confirming the push of b2b2b2b (GitHub does not answer)"; got != want {
					t.Errorf("%s: the loop says %q, want %q", when, got, want)
				}
				if n := len(h.fixEvents("fix.failed")); n != 0 {
					t.Fatalf("%s: fix.failed events = %d, want none", when, n)
				}
				if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushConfirming {
					t.Fatalf("%s: the record = %+v, %v; want the push confirming", when, rec, ok)
				}
			}
			wantConfirming("after the settle's retries")
			if n := len(h.fixEvents("fix.push_confirming")); n != 1 {
				t.Errorf("fix.push_confirming events = %d, want 1", n)
			}
			for range 3 {
				h.advance(5 * time.Minute)
				_ = h.e.Tick(h.ctx) // the poll fails: GitHub is down
				h.settle()
				wantConfirming("after a tick of the outage")
			}

			if answers {
				h.gh.mu.Lock()
				h.gh.detailsErr, h.gh.radarErr = nil, nil
				h.gh.mu.Unlock()
				h.advance(time.Minute)
				if err := h.e.poll(h.ctx); err != nil {
					t.Fatal(err)
				}
				h.settle()
				h.wantPushedSettle(pr, 0)
				if n := len(h.fixEvents("fix.push_confirmed")); n != 1 {
					t.Errorf("fix.push_confirmed events = %d, want 1", n)
				}
				if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
					t.Errorf("fix.push_confirmed_late events = %d, want none", n)
				}
				return
			}
			h.advance(fixConfirmCap)
			_ = h.e.Tick(h.ctx)
			h.settle()
			cur := h.wantState(2, store.PRNeedsAttention)
			if le := deref(cur.LastError); !strings.HasPrefix(le, "fix settle failed: push not confirmed on GitHub: ") {
				t.Errorf("last_error = %q", le)
			}
			if data := h.fixData("fix.failed"); data["kind"] != fixPushUnconfirmed {
				t.Errorf("fix.failed data = %v, want the kind %s", data, fixPushUnconfirmed)
			}
			if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed {
				t.Errorf("the record = %+v, %v; want the push unconfirmed", rec, ok)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on past the cap")
			}
			if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
				t.Errorf("slot calls %v, want the slot back at the reviewed head", h.sl.all())
			}
		})
	}
}

// unconfirmedFix runs a fix on PR #2 whose push GitHub does not show (its
// head stays the reviewed one): the settle fails it as the typed "push not
// confirmed".
func unconfirmedFix(t *testing.T) (*harness, *fixGit, store.PR) {
	t.Helper()
	return unconfirmedFixWith(t, "none")
}

// unconfirmedFixWith is unconfirmedFix whose fixer leaves needs, its
// report's "## Needs you", to the author.
func unconfirmedFixWith(t *testing.T, needs string) (*harness, *fixGit, store.PR) {
	t.Helper()
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	git.lostPush = true
	fixerBy(h, git, func(run store.Run) (string, string) {
		return fixPushed, fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, needs)
	})
	h.fix(FixPayload{})
	h.handle()
	h.wantState(2, store.PRNeedsAttention)
	if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed {
		t.Fatalf("the record = %+v, %v; want the push unconfirmed", rec, ok)
	}
	return h, git, pr
}

// A fix that failed only because its push was not confirmed is settled as
// pushed once the poll finds exactly that push as the PR's head:
// fix.push_confirmed_late, then what a settle does after a push (the
// replies, the calls the threads leave, the PR row's head, the forced
// re-review, the push's CI, the attention cleared). A crash between its
// steps, or a shutdown as its last step begins or while that step reads
// the threads, loses none of its writes and repeats none of them after a
// restart, and a later poll does nothing.
func TestAnUnconfirmedFixWhosePushThePollFindsIsSettledAsPushedOnce(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		// cut is the context of the poll that finds the push, and whether
		// that poll settles the fix whole.
		cut   func(h *harness) context.Context
		whole bool
	}{
		{name: "a crash before the last step", cut: func(h *harness) context.Context {
			return steps.WithFailpoint(h.ctx, func(_, name string, at steps.Point) error {
				if name == "fix.push_settle" && at == steps.BeforeRun {
					return errCrash
				}
				return nil
			})
		}},
		{name: "a shutdown as the last step begins", whole: true, cut: func(h *harness) context.Context {
			ctx, cancel := context.WithCancel(h.ctx)
			return steps.WithFailpoint(ctx, func(_, name string, at steps.Point) error {
				if name == "fix.push_settle" && at == steps.BeforeRun {
					cancel()
				}
				return nil
			})
		}},
		{name: "a shutdown while the last step reads the threads", whole: true, cut: func(h *harness) context.Context {
			rec, _ := h.fixPushOf(2) // the settle could not look for unmarked replies: the late settle reads the threads first
			rec.RepliesNamed = false
			if err := writeKVJSON(h.ctx, h.e, kvPRFixPush(h.pr(2).ID), "fix push", rec); err != nil {
				h.t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(h.ctx)
			h.gh.mu.Lock()
			h.gh.onThreads = func() { cancel() }
			h.gh.mu.Unlock()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, pr := unconfirmedFixWith(t, "- 2102 P2 app/lib/x.rb:7: contested")
			if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
				t.Fatalf("attention %q, want the fix's", kind)
			}
			h.gh.setHead("talkable/talkable", 2, fixPushed)
			_ = h.e.poll(tc.cut(h))
			h.settle()
			h.gh.mu.Lock()
			h.gh.onThreads = nil
			h.gh.mu.Unlock()
			if n := len(h.fixEvents("fix.push_confirmed_late")); n != 1 {
				t.Fatalf("fix.push_confirmed_late events = %d, want 1", n)
			}
			if tc.whole {
				h.wantPushedSettle(pr, 1)
			} else if n := len(h.fixEvents("fix.pushed")); n != 0 {
				t.Fatalf("fix.pushed events = %d before the crash's resume, want none", n)
			}

			h.e = New(h.d) // a restart
			h.startup()
			for range 2 {
				if err := h.e.poll(h.ctx); err != nil {
					t.Fatal(err)
				}
				h.settle()
			}
			h.wantPushedSettle(pr, 1)
			for kind, want := range map[string]int{"fix.push_confirmed_late": 1, "fix.needs_you": 1} {
				if n := len(h.fixEvents(kind)); n != want {
					t.Errorf("%s events = %d, want %d", kind, n, want)
				}
			}
			if n := h.eventCount("pr:talkable/talkable#2", "pr.forced"); n != 1 {
				t.Errorf("pr.forced events = %d, want 1", n)
			}
			if st, _ := h.fixStateOf(2); len(st.NeedsYou) != 1 {
				t.Errorf("the loop's calls = %v, want the one the fixer left", st.NeedsYou)
			}
			if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != "" {
				t.Errorf("attention %q, want it cleared", kind)
			}
			if cur := h.pr(2); deref(cur.LastError) != "" {
				t.Errorf("last_error = %q, want none", deref(cur.LastError))
			}
		})
	}
}

// A failed fix whose PR's head is now a later commit (someone pushed after
// the fixer) is not settled: the failure stands and the record goes.
func TestAnUnconfirmedFixWhosePRMovedToALaterCommitIsNotSettled(t *testing.T) {
	h, _, pr := unconfirmedFix(t)
	h.gh.setHead("talkable/talkable", 2, fixPushed2)
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
		t.Errorf("fix.push_confirmed_late events = %d, want none", n)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop runs")
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
		t.Errorf("fix run %s, want it still failed", r.State)
	}
	if _, ok := h.fixPushOf(2); ok {
		t.Error("the record stays for a head that moved on")
	}
	h.gh.setHead("talkable/talkable", 2, fixPushed)
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
		t.Errorf("fix.push_confirmed_late events after the head came back = %d, want none", n)
	}
}

// Only the typed kind is ever settled late: a fix that failed otherwise is
// not, even when its words say "push not confirmed" and GitHub shows its
// fixer's push, and neither is a record of another kind.
func TestAFixThatFailedOtherwiseIsNeverSettledLate(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	const words = "push not confirmed on GitHub: the PR's head is a1a1a1a, not the pushed b2b2b2b"
	h.ag.mu.Lock()
	h.ag.onSubmit = func(run store.Run, _ string) { // a fixer that pushed, reported, then failed its run
		path := deref(run.ReportPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			h.t.Error(err)
		}
		if err := os.WriteFile(path, []byte(fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "none")), 0o600); err != nil {
			h.t.Error(err)
		}
		git.setHead(fixPushed)
		h.gh.setHead("talkable/talkable", 2, fixPushed)
		if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunFailed, func(u *store.RunUpdate) {
			u.Set("error", words)
			u.Set("ended_at", h.clock.Now())
		}); err != nil {
			h.t.Error(err)
		}
	}
	h.ag.mu.Unlock()
	h.fix(FixPayload{})
	h.handle()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.Contains(le, "push not confirmed") {
		t.Fatalf("last_error = %q, want the run's words", le)
	}
	if _, ok := h.fixPushOf(2); ok {
		t.Fatal("a fix that failed otherwise has a record of its push")
	}
	run := h.fixRuns(pr.ID)[0]
	other := fixPush{Kind: "failed", Run: run.ID, Pushed: fixPushed, Job: fixPushJob{Role: "claude-fix", Head: fixHead,
		State: FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Report: deref(run.ReportPath), By: fixBy}}}
	if err := writeKVJSON(h.ctx, h.e, kvPRFixPush(pr.ID), "fix push", other); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
	}
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
		t.Errorf("fix.push_confirmed_late events = %d, want none", n)
	}
	if n := len(h.fixEvents("fix.pushed")); n != 0 {
		t.Errorf("fix.pushed events = %d, want none", n)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop runs")
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
		t.Errorf("fix run %s, want it still failed", r.State)
	}
	if _, ok := h.fixPushOf(2); ok {
		t.Error("the record of another kind stays")
	}
}

// While the settle confirms the push, the loop says so: the board's state
// cell, the PR's status line (the look's verdict) and the dispatch wait.
func TestTheLoopSaysItConfirmsThePushWhileGitHubDoesNotAnswer(t *testing.T) {
	const head = "e6c1f37" + "000000000000000000000000000000000"
	st := FixState{Cycle: 1, Cap: 5, Phase: FixPhaseFixing, Role: "claude-fix", Confirm: &FixConfirm{Head: head}}
	if got, want := st.Short(), "fixing 1/5 · confirming e6c1f37"; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
	if got, want := st.Confirming(), "confirming the push of e6c1f37 (GitHub does not answer)"; got != want {
		t.Errorf("Confirming = %q, want %q", got, want)
	}
	if got := (FixState{Cycle: 1, Cap: 5, Phase: FixPhaseFixing, Role: "claude-fix"}).Confirming(); got != "" {
		t.Errorf("Confirming of a loop at work = %q, want none", got)
	}
	f := openFacts{fix: &st, pr: store.PR{HeadSHA: fixHead}}
	if v, _, _ := decideOpen(f); v.Verdict != OpenFixing || v.Why != "fixing, confirming the push of e6c1f37 (GitHub does not answer)" {
		t.Errorf("the verdict = %s %q", v.Verdict, v.Why)
	}
}
