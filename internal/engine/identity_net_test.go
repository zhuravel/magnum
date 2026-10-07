package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/identity"
)

// What gh and the identity package report when GitHub cannot be reached.
const (
	ghUnreachable   = "gh api user: gh api user --jq .login --hostname github.com exited 1: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"
	appUnreachable  = "GET /app: gh api GET /app: gh exited 1: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"
	mintUnreachable = "identity talkable-app: mint installation token: POST /app/installations/2/access_tokens: gh api POST /app/installations/2/access_tokens: gh exited 1: error connecting to api.github.com"
)

func (h *harness) wantHealthy(names ...string) {
	h.t.Helper()
	for _, name := range names {
		if ok, why := h.e.identityHealthy(h.ctx, name); !ok {
			h.t.Fatalf("identity %s unhealthy: %s", name, why)
		}
	}
}

func (h *harness) wantUnhealthy(name, sub string) string {
	h.t.Helper()
	ok, why := h.e.identityHealthy(h.ctx, name)
	if ok {
		h.t.Fatalf("identity %s healthy, want unhealthy with %q", name, sub)
	}
	if !strings.Contains(why, sub) {
		h.t.Fatalf("identity %s unhealthy for %q, want %q", name, why, sub)
	}
	return why
}

func (h *harness) wantToasts(sub string, n int) {
	h.t.Helper()
	if got := h.nh.titles(sub); len(got) != n {
		h.t.Fatalf("toasts %q = %v, want %d", sub, got, n)
	}
}

func TestIdentityNetworkBlipOnFirstTickKeepsIdentitiesHealthy(t *testing.T) {
	h := newHarness(t)
	gh := h.ids["zhuravel"]
	gh.fail(ghUnreachable, "", "")
	h.app.fail("", appUnreachable, mintUnreachable)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.wantHealthy("zhuravel", "talkable-app")
	h.wantToasts("identity", 0)
	if v, _ := h.e.getKV(h.ctx, KVIdentityCheck("talkable-app")); v == "fail" {
		t.Fatal("a blip recorded a failed verdict")
	}
	ghChecks, _ := gh.counts()
	appChecks, ensures := h.app.counts()

	// A tick within the backoff does not try again.
	h.advance(10 * time.Second)
	h.tick()
	if c, _ := gh.counts(); c != ghChecks {
		t.Fatalf("checked again inside the backoff: %d checks, want %d", c, ghChecks)
	}
	if _, en := h.app.counts(); en != ensures {
		t.Fatalf("token refreshed inside the backoff: %d, want %d", en, ensures)
	}

	// The network is back: the next ticks retry and the verdicts are passes.
	gh.fail("", "", "")
	h.app.fail("", "", "")
	h.advance(20 * time.Second)
	h.tick()
	if c, _ := gh.counts(); c != ghChecks+1 {
		t.Fatalf("gh identity checks = %d, want %d (retried after 30s)", c, ghChecks+1)
	}
	if c, en := h.app.counts(); c != appChecks+1 || en != ensures+1 {
		t.Fatalf("app checks, refreshes = %d, %d, want %d, %d", c, en, appChecks+1, ensures+1)
	}
	h.wantHealthy("zhuravel", "talkable-app")
	if v, _ := h.e.getKV(h.ctx, KVIdentityCheck("talkable-app")); v != "pass" {
		t.Fatalf("verdict after the retry = %q, want pass", v)
	}
	h.wantToasts("identity", 0)

	// Nothing is retried any more.
	ghChecks, _ = gh.counts()
	h.advance(5 * time.Minute)
	h.tick()
	if c, _ := gh.counts(); c != ghChecks {
		t.Fatalf("a passing identity checked again before the reconcile: %d, want %d", c, ghChecks)
	}
}

func TestIdentityNetworkFailureMarksUnhealthyAfterFiveMinutes(t *testing.T) {
	h := newHarness(t)
	h.app.fail("", appUnreachable, "")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup() // the first failure
	h.tick()
	checks := func() int { c, _ := h.app.counts(); return c }
	for i, step := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		h.advance(step)
		h.tick()
		h.wantHealthy("talkable-app")
		if got := checks(); got != i+2 {
			t.Fatalf("after %v: %d checks, want %d (retried with backoff)", step, got, i+2)
		}
	}
	h.wantToasts("identity", 0)
	// The next wait (4m) would pass the 5-minute mark: the retry is at the mark.
	h.advance(time.Minute)
	h.tick()
	if got := checks(); got != 4 {
		t.Fatalf("4m30s: %d checks, want 4", got)
	}
	h.advance(30 * time.Second)
	h.tick()
	if got := checks(); got != 5 {
		t.Fatalf("5m: %d checks, want 5", got)
	}
	why := h.wantUnhealthy("talkable-app", "network failure (GitHub unreachable) since ")
	if !strings.Contains(why, "error connecting to api.github.com") {
		t.Fatalf("reason without the error: %q", why)
	}
	h.wantToasts("identity talkable-app unhealthy", 1)

	// Re-checked every 4 minutes, before the reconcile (10m): the verdict
	// and its toast stand.
	h.advance(3*time.Minute + 30*time.Second)
	h.tick()
	if got := checks(); got != 5 {
		t.Fatalf("8m30s: %d checks, want 5", got)
	}
	h.advance(30 * time.Second)
	h.tick()
	if got := checks(); got != 6 {
		t.Fatalf("9m: %d checks, want 6 (re-checked on the backoff)", got)
	}
	h.wantUnhealthy("talkable-app", "network failure")
	h.wantToasts("identity talkable-app unhealthy", 1)

	// The network is back: the next re-check passes and clears it.
	h.app.fail("", "", "")
	h.advance(4 * time.Minute)
	h.tick()
	h.wantHealthy("talkable-app")
	if sent, err := h.st.ShouldSend(h.ctx, "identity:talkable-app", time.Hour); err != nil || !sent {
		t.Fatalf("the unhealthy toast's dedup record survived the pass (%v, %v)", sent, err)
	}
}

func TestIdentityRealFailureMarksUnhealthyAtOnce(t *testing.T) {
	h := newHarness(t)
	h.app.fail("GitHub rejected the App JWT: GET /app: 401 Bad credentials", "", "")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.wantUnhealthy("talkable-app", "401 Bad credentials")
	h.wantToasts("identity talkable-app unhealthy", 1)
	// A real verdict keeps the reconcile's cadence.
	checks, _ := h.app.counts()
	for range 4 {
		h.advance(2 * time.Minute)
		h.tick()
	}
	if c, _ := h.app.counts(); c != checks {
		t.Fatalf("a real failure re-checked before the reconcile: %d checks, want %d", c, checks)
	}
	h.advance(2 * time.Minute) // 10m: the reconcile re-checks it
	h.tick()
	if c, _ := h.app.counts(); c != checks+1 {
		t.Fatalf("reconcile: %d checks, want %d", c, checks+1)
	}
}

func TestIdentityTokenRefreshRealFailureMarksUnhealthyAtOnce(t *testing.T) {
	h := newHarness(t)
	h.app.fail("", "", "identity talkable-app: mint installation token: POST /app/installations/2/access_tokens: 401 A JSON web token could not be decoded")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.wantUnhealthy("talkable-app", "token refresh failed: identity talkable-app: mint installation token")
	h.wantToasts("token refresh failed", 1)
}

func TestIdentityTokenRefreshNetworkFailure(t *testing.T) {
	h := newHarness(t)
	h.app.fail("", "", mintUnreachable)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // the first failure
	ensures := func() int { _, en := h.app.counts(); return en }
	first := ensures()
	h.wantHealthy("talkable-app")
	for _, step := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		h.advance(step)
		h.tick()
		h.wantHealthy("talkable-app")
	}
	if got := ensures(); got != first+3 {
		t.Fatalf("%d token refreshes, want %d (retried with backoff)", got, first+3)
	}
	h.wantToasts("token refresh failed", 0)
	h.advance(90 * time.Second) // 5m
	h.tick()
	h.wantUnhealthy("talkable-app", "token refresh failed: network failure (GitHub unreachable) since ")
	h.wantToasts("token refresh failed", 1)

	// Re-tried every 4 minutes; once the network is back the error clears.
	h.app.fail("", "", "")
	h.advance(time.Minute)
	h.tick()
	h.wantUnhealthy("talkable-app", "token refresh failed")
	h.advance(3 * time.Minute)
	h.tick()
	h.wantHealthy("talkable-app")
}

func TestIdentityMarkedForTheNetworkElsewhereIsRecheckedOnBackoff(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	// `magnum identities check` (or a judge's own check) recorded a failure
	// that is the network's: the next tick re-checks it, not the reconcile.
	if _, err := h.e.requestIdentityVerdict(h.ctx, IdentityVerdictPayload{Name: "talkable-app", Reason: appUnreachable}); err != nil {
		t.Fatal(err)
	}
	h.wantUnhealthy("talkable-app", "error connecting")
	checks, _ := h.app.counts()
	h.advance(30 * time.Second)
	h.tick()
	if c, _ := h.app.counts(); c != checks+1 {
		t.Fatalf("%d checks, want %d", c, checks+1)
	}
	h.wantHealthy("talkable-app")

	// A real failure recorded there waits for the reconcile.
	if _, err := h.e.requestIdentityVerdict(h.ctx, IdentityVerdictPayload{Name: "talkable-app", Reason: "permission pull_requests: read (need write)"}); err != nil {
		t.Fatal(err)
	}
	checks, _ = h.app.counts()
	h.advance(time.Minute)
	h.tick()
	if c, _ := h.app.counts(); c != checks {
		t.Fatalf("a real failure re-checked at once: %d checks, want %d", c, checks)
	}
	h.wantUnhealthy("talkable-app", "permission pull_requests")
}

func TestIdentityNetworkFailureKeepsARealVerdict(t *testing.T) {
	h := newHarness(t)
	h.app.fail("permission pull_requests: read (need write)", "", "")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.wantUnhealthy("talkable-app", "permission pull_requests")
	// The reconcile's re-check cannot reach GitHub: the verdict stands as it
	// was, and it is retried on the backoff until a verdict comes back.
	h.app.fail("", appUnreachable, "")
	h.advance(10 * time.Minute)
	h.tick()
	h.wantUnhealthy("talkable-app", "permission pull_requests")
	h.app.fail("", "", "")
	h.advance(30 * time.Second)
	h.tick()
	h.wantHealthy("talkable-app")
}

func TestCheckConnectionCauseNeedsEveryFailureToBeTheNetwork(t *testing.T) {
	netErr := errors.New("identity talkable-app check: mint installation token: " + mintUnreachable)
	for _, tc := range []struct {
		name string
		rep  identity.Report
		err  error
		want string
	}{
		{"transport failure", identity.Report{Lines: []string{"PASS private key", "FAIL GET /app: " + appUnreachable}},
			errors.New("identity talkable-app check: " + appUnreachable), "GitHub unreachable"},
		{"FAIL line only", identity.Report{Lines: []string{"PASS gh token for github.com", "FAIL " + ghUnreachable}}, nil, "GitHub unreachable"},
		{"a real failure before it", identity.Report{Lines: []string{"FAIL permission pull_requests: read (need write)", "     fix: open ...", "FAIL mint installation token: " + mintUnreachable}},
			netErr, ""},
		{"a real failure", identity.Report{Lines: []string{"FAIL GitHub rejected the App JWT: GET /app: 401 Bad credentials"}}, nil, ""},
		{"no FAIL line", identity.Report{Lines: []string{"PASS x"}}, nil, ""},
	} {
		if got := checkConnectionCause(tc.rep, tc.err); got != tc.want {
			t.Errorf("%s: checkConnectionCause = %q, want %q", tc.name, got, tc.want)
		}
	}
}
