package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

func TestInfraCauseClassifiesOutsideFailures(t *testing.T) {
	for _, tc := range []struct{ name, msg, want string }{
		{"a refused SSH key", "git fetch origin: git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "SSH key refused"},
		{"a failed DNS lookup", "fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com", "DNS lookup failed"},
		{"a network timeout", "ssh: connect to host github.com port 22: Operation timed out", "network timeout"},
		{"a refused connection", "ssh: connect to host github.com port 22: Connection refused", "connection refused"},
		{"a TLS failure", "fatal: unable to access: SSL certificate problem: unable to get local issuer certificate", "TLS failure"},
		{"a missing ref is the PR's", "fatal: couldn't find remote ref refs/pull/7/head", ""},
		{"a failed setup is the PR's", "slots: deps review1: bin/setup: exit 1: Gemfile.lock out of date", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := infraCause(errors.New(tc.msg)); got != tc.want {
				t.Errorf("infraCause(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// TestSameDepsFailureOnTwoPRsIsInfra: a dependency step failing with the
// same error for two different PRs within ten minutes is not the PRs' fault;
// the same PR twice, or a different error, or a later failure, is.
func TestSameDepsFailureOnTwoPRsIsInfra(t *testing.T) {
	h := newHarness(t)
	depsErr := func(slot string) error {
		xe := &execx.ExitError{Cmd: execx.Cmd{Name: "mise"}, Code: 1,
			Stderr: "fetching gems\nCould not find gem 'pg' in /x/" + slot + "/vendor\n"}
		return fmt.Errorf("checkout in %s: slots: deps %s: bin/setup-deps: %w", slot, slot, xe)
	}
	if got := h.e.checkoutInfra(depsErr("review1"), 1, "review1", "/x/review1"); got != "" {
		t.Fatalf("first failure: %q", got)
	}
	if got := h.e.checkoutInfra(depsErr("review1"), 1, "review1", "/x/review1"); got != "" {
		t.Fatalf("same PR again: %q", got)
	}
	h.advance(5 * time.Minute)
	if got := h.e.checkoutInfra(depsErr("review2"), 2, "review2", "/x/review2"); got != "dependency step fails for every PR" {
		t.Fatalf("second PR, same failure: %q", got)
	}
	h.advance(11 * time.Minute)
	if got := h.e.checkoutInfra(depsErr("review1"), 1, "review1", "/x/review1"); got != "" {
		t.Fatalf("after the window: %q", got)
	}
	other := fmt.Errorf("checkout in review2: slots: deps review2: bin/setup-deps: %w", &execx.ExitError{Code: 2, Stderr: "other\n"})
	if got := h.e.checkoutInfra(other, 2, "review2", "/x/review2"); got != "" {
		t.Fatalf("another failure: %q", got)
	}
}

// TestInfraFailurePausesDispatchOnceAndProbesUntilRecovery: a checkout
// failing with "Permission denied (publickey)" does not charge the PR or
// move it to needs_attention; dispatch pauses with one urgent toast; the
// probe (git ls-remote of the pool's clone) runs when the backoff ends,
// doubles the wait while it fails and lifts the pause when it succeeds.
func TestInfraFailurePausesDispatchOnceAndProbesUntilRecovery(t *testing.T) {
	var mu sync.Mutex
	probeErr := errors.New("git ls-remote: Permission denied (publickey)")
	var probed []string
	h := newHarness(t, func(h *harness) {
		h.d.Probe = func(_ context.Context, dir string) error {
			mu.Lock()
			defer mu.Unlock()
			probed = append(probed, dir)
			return probeErr
		}
	})
	clone := h.cfg.Pools[0].MainClone
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	h.sl.holdErr = errors.New("fetch: git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.")

	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick() // the round's checkout fails

	pr := h.wantState(2, store.PRQueued)
	if pr.Attempts != 0 || !strings.Contains(deref(pr.LastError), "infrastructure (SSH key refused)") {
		t.Fatalf("PR charged or unexplained: attempts %d, last_error %q", pr.Attempts, deref(pr.LastError))
	}
	p, ok := h.e.infraPause(h.ctx)
	if !ok || p.Reason != "SSH key refused" || !p.Until.Equal(h.clock.Now().Add(infraBackoffFirst)) {
		t.Fatalf("infra pause = %+v %v", p, ok)
	}
	if got := h.nh.titles("reviews paused"); len(got) != 1 || got[0] != "magnum: reviews paused (SSH key refused)" {
		t.Fatalf("toasts = %v", got)
	}
	checkouts := func() int {
		return len(slices.DeleteFunc(h.sl.all(), func(c string) bool { return !strings.HasPrefix(c, "checkout:") }))
	}
	before := checkouts()

	// Dispatch stays closed while the pause waits for its probe.
	h.advance(time.Minute)
	h.tick()
	if checkouts() != before || len(probed) != 0 {
		t.Fatalf("dispatched or probed during the pause: checkouts %d→%d, probes %v", before, checkouts(), probed)
	}
	if !strings.Contains(h.e.tabBar(h.ctx), "infra paused") {
		t.Fatalf("tab bar: %q", h.e.tabBar(h.ctx))
	}

	// The probe fails: the wait doubles, still one toast.
	h.advance(time.Minute)
	h.tick()
	if len(probed) != 1 || probed[0] != clone {
		t.Fatalf("probes = %v, want the pool's clone", probed)
	}
	p, _ = h.e.infraPause(h.ctx)
	if want := h.clock.Now().Add(2 * infraBackoffFirst); !p.Until.Equal(want) {
		t.Fatalf("after a failed probe until = %v, want %v", p.Until, want)
	}
	if checkouts() != before || len(h.nh.titles("reviews paused")) != 1 {
		t.Fatalf("dispatched or toasted again after a failed probe")
	}

	// The network is back: the probe lifts the pause and the PR is reviewed.
	mu.Lock()
	probeErr = nil
	mu.Unlock()
	h.sl.holdErr = nil
	h.advance(2 * infraBackoffFirst)
	h.tick()
	if _, ok := h.e.infraPause(h.ctx); ok {
		t.Fatal("pause not lifted after a successful probe")
	}
	if v, ok := h.e.getKV(h.ctx, KVInfraBackoff); ok {
		t.Fatalf("backoff kept after recovery: %q", v)
	}
	h.wantState(2, store.PRReviewed)
	if pr := h.pr(2); pr.Attempts != 0 {
		t.Fatalf("attempts = %d", pr.Attempts)
	}
	// The toast's dedupe record is gone: the next outage notifies at once.
	if ok, err := h.st.ShouldSend(h.ctx, "infra", time.Hour); err != nil || !ok {
		t.Fatalf("infra toast key still reserved: %v %v", ok, err)
	}
}

// TestResumeLiftsTheInfraPause: `magnum resume` lifts an infrastructure
// pause at once.
func TestResumeLiftsTheInfraPause(t *testing.T) {
	h := newHarness(t)
	h.e.pauseInfra(h.ctx, "network timeout", errors.New("Operation timed out"), "")
	h.e.toastWG.Wait()
	if reason := h.e.pauseReason(h.ctx); !strings.Contains(reason, "infrastructure paused (network timeout)") {
		t.Fatalf("pauseReason = %q", reason)
	}
	if _, err := h.e.requestPause(h.ctx, PausePayload{}, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.e.infraPause(h.ctx); ok || h.e.pauseReason(h.ctx) != "" {
		t.Fatal("resume left the infra pause")
	}
}
