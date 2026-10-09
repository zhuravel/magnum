package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// approvalID is the id of the first review a harness round posts when the
// rounds approve (approving): 700 + the number of rounds so far.
const approvalID = 701

// approvalGH is the fake GitHub as the App identity's client. The harness
// has none of its own (the follower would silently have nothing to dismiss
// with), and the follower's Compare calls must stay apart from the poller's
// own, which go to the plain fakeGH of the poll identity.
type approvalGH struct {
	*fakeGH
	cmu      sync.Mutex
	compares []string // "base...head", in call order
}

func (g *approvalGH) Compare(ctx context.Context, owner, repo, base, head string) (github.CompareStats, error) {
	g.cmu.Lock()
	g.compares = append(g.compares, base+"..."+head)
	g.cmu.Unlock()
	return g.fakeGH.Compare(ctx, owner, repo, base, head)
}

func (g *approvalGH) comparesMade() []string {
	g.cmu.Lock()
	defer g.cmu.Unlock()
	return slices.Clone(g.compares)
}

// newApprovalHarness is newHarness with the App identity's client wired to
// the fake GitHub (counted by the returned approvalGH).
func newApprovalHarness(t *testing.T, mods ...func(*harness)) (*harness, *approvalGH) {
	t.Helper()
	app := &approvalGH{}
	wire := func(h *harness) {
		app.fakeGH = h.gh
		plain := h.d.GitHub
		h.d.GitHub = func(id string) GitHub {
			if id == "talkable-app" {
				return app
			}
			return plain(id)
		}
	}
	return newHarness(t, append([]func(*harness){wire}, mods...)...), app
}

// withExampleWatch adds a watch of owner "example" run as the gh identity
// (no App involved) next to the harness's.
func withExampleWatch(h *harness) {
	h.cfg.Watches = append(h.cfg.Watches, config.Watch{Owner: "example", Include: []string{"*"}, Identity: "zhuravel", PollIdentity: "zhuravel"})
}

// approving makes every round post its review with event, as the real
// pipeline reports the verdict, under an id of its own (700 + the round).
func approving(h *harness, event string) {
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n := len(h.rd.all())
		res, err := h.rd.posted(h.ctx, in, n)
		res.Event = event
		res.ReviewID = int64(700 + n)
		return res, err
	}
}

// approvedPR runs the standard flow to PR n reviewed at head by the App with
// an approval (review approvalID).
func approvedPR(h *harness, n int, head string) store.PR {
	h.t.Helper()
	approving(h, "APPROVED")
	pr := h.reviewedPR(n, head)
	if deref(pr.LastReviewID) != approvalID || deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.ReviewedSHA) != head ||
		deref(pr.LastReviewLogin) != "talkable[bot]" || pr.Identity != "talkable-app" {
		h.t.Fatalf("setup: review %v event %q at %q by %q as %q", pr.LastReviewID, deref(pr.LastReviewEvent),
			deref(pr.ReviewedSHA), deref(pr.LastReviewLogin), pr.Identity)
	}
	return pr
}

// pollPR lets d pass, shows PR n at head (next to the baseline #1) and runs
// one tick.
func pollPR(h *harness, d time.Duration, n int, head string) {
	h.t.Helper()
	h.advance(d)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
}

// callsWith lists g's recorded calls that start with prefix.
func callsWith(g *fakeGH, prefix string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func setDismissErr(g *fakeGH, err error) {
	g.mu.Lock()
	g.dismissErr = err
	g.mu.Unlock()
}

func setCompareErr(g *fakeGH, err error) {
	g.mu.Lock()
	g.compareErr = err
	g.mu.Unlock()
}

// subjectEvents lists a subject's events, oldest first.
func subjectEvents(t *testing.T, h *harness, subject string) []store.Event {
	t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// kindOf keeps the events of one kind.
func kindOf(evs []store.Event, kind string) []store.Event {
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// approvalEvents lists the events of kind on PR number of talkable/talkable.
func approvalEvents(t *testing.T, h *harness, number int, kind string) []store.Event {
	t.Helper()
	return kindOf(subjectEvents(t, h, fmt.Sprintf("pr:talkable/talkable#%d", number)), kind)
}

func kvValue(h *harness, key string) (string, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return v, ok
}

// wantStands fails unless nothing was dismissed and PR n still shows its
// approval.
func wantStands(t *testing.T, h *harness, n int) {
	t.Helper()
	if got := callsWith(h.gh, "dismiss:"); len(got) != 0 {
		t.Fatalf("dismiss calls = %q, want none", got)
	}
	if ev := deref(h.pr(n).LastReviewEvent); ev != "APPROVED" {
		t.Fatalf("last_review_event = %q, want APPROVED", ev)
	}
	for _, kind := range []string{"review.approval_dismissed", "review.approval_dismiss_refused"} {
		if evs := approvalEvents(t, h, n, kind); len(evs) != 0 {
			t.Fatalf("%s events: %+v", kind, evs)
		}
	}
}

// wantUntouched is wantStands without even a compare by the App identity.
func wantUntouched(t *testing.T, h *harness, app *approvalGH, n int) {
	t.Helper()
	wantStands(t, h, n)
	if got := app.comparesMade(); len(got) != 0 {
		t.Fatalf("the App identity compared %q, want no compare", got)
	}
}

func TestAppApprovalIsDismissedBeforeTheReReviewIsQueued(t *testing.T) {
	h, app := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2, Files: 3, Additions: 12, Deletions: 1}

	pollPR(h, time.Minute, 2, "b2")
	pr := h.wantState(2, store.PRRereviewPending)
	wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID, ApprovalDismissMessage)
	if got := callsWith(h.gh, "dismiss:"); !slices.Equal(got, []string{wantCall}) {
		t.Fatalf("dismiss calls = %q, want exactly %q", got, wantCall)
	}
	if got := app.comparesMade(); !slices.Equal(got, []string{"b1...b2"}) {
		t.Fatalf("the App identity compared %q, want the reviewed commit against the head once", got)
	}
	if deref(pr.LastReviewEvent) != "DISMISSED" || deref(pr.LastReviewID) != approvalID || deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("after the dismissal: event %q review %v reviewed_sha %q, want DISMISSED on review %d at b1",
			deref(pr.LastReviewEvent), pr.LastReviewID, deref(pr.ReviewedSHA), approvalID)
	}

	evs := subjectEvents(t, h, "pr:talkable/talkable#2")
	dismissed := kindOf(evs, "review.approval_dismissed")
	if len(dismissed) != 1 || dismissed[0].Level != "info" {
		t.Fatalf("review.approval_dismissed events: %+v", dismissed)
	}
	var data map[string]any
	if err := json.Unmarshal(dismissed[0].Data, &data); err != nil {
		t.Fatalf("event data %s: %v", dismissed[0].Data, err)
	}
	if data["review_id"] != float64(approvalID) || data["identity"] != "talkable-app" || data["reviewed_sha"] != "b1" || data["head_sha"] != "b2" {
		t.Fatalf("event data = %v", data)
	}
	// The new head is noticed, then the approval goes before the re-review
	// is queued (the trivial-delta check runs between the two).
	at := func(kind string) int {
		return slices.IndexFunc(evs, func(ev store.Event) bool { return ev.Kind == kind })
	}
	dismissedAt, changedAt, queuedAt := at("review.approval_dismissed"), at("pr.head_changed"), at("pr."+store.PRRereviewPending)
	if dismissedAt < 0 || changedAt < 0 || queuedAt < 0 || dismissedAt < changedAt || dismissedAt > queuedAt {
		t.Fatalf("event order: dismissed %d, head_changed %d, queued %d; events %+v", dismissedAt, changedAt, queuedAt, evs)
	}

	// Later polls leave it alone, through the re-review that posts the new verdict.
	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, 30*time.Minute, 2, "b2")
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != approvalID+1 || deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("after the re-review: reviewed %q review %v event %q", deref(pr.ReviewedSHA), pr.LastReviewID, deref(pr.LastReviewEvent))
	}
	if got := callsWith(h.gh, "dismiss:"); !slices.Equal(got, []string{wantCall}) {
		t.Fatalf("dismiss calls after later polls = %q, want still exactly %q", got, wantCall)
	}
	if got := approvalEvents(t, h, 2, "review.approval_dismissed"); len(got) != 1 {
		t.Fatalf("review.approval_dismissed events after later polls: %d", len(got))
	}
}

func TestApprovalIsKeptWhenTheNewHeadHasNoNewCommits(t *testing.T) {
	h, app := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 0} // b2 is an ancestor of the reviewed commit

	pollPR(h, time.Minute, 2, "b2")
	wantStands(t, h, 2)
	if got := app.comparesMade(); !slices.Equal(got, []string{"b1...b2"}) {
		t.Fatalf("the App identity compared %q, want one compare", got)
	}
	kept := approvalEvents(t, h, 2, "review.approval_kept")
	if len(kept) != 1 || kept[0].Level != "info" {
		t.Fatalf("review.approval_kept events: %+v", kept)
	}
	pr := h.pr(2)
	if v, ok := kvValue(h, fmt.Sprintf("pr.%d.approval_kept", pr.ID)); !ok || v != fmt.Sprintf("%d@b2", approvalID) {
		t.Fatalf("approval_kept marker = %q (set %v), want %d@b2", v, ok, approvalID)
	}

	// The same head is not compared again, however many polls see it.
	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, time.Minute, 2, "b2")
	wantStands(t, h, 2)
	if got := app.comparesMade(); !slices.Equal(got, []string{"b1...b2"}) {
		t.Fatalf("the App identity compared %q after later polls, want still the one compare", got)
	}
	if got := approvalEvents(t, h, 2, "review.approval_kept"); len(got) != 1 {
		t.Fatalf("review.approval_kept events after later polls: %d", len(got))
	}
}

func TestKeptApprovalIsComparedAgainForANewerHead(t *testing.T) {
	h, app := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 0}
	h.gh.compare["b1...b3"] = github.CompareStats{Commits: 1, Files: 1}

	pollPR(h, time.Minute, 2, "b2")
	wantStands(t, h, 2)

	pollPR(h, time.Minute, 2, "b3")
	wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID, ApprovalDismissMessage)
	if got := callsWith(h.gh, "dismiss:"); !slices.Equal(got, []string{wantCall}) {
		t.Fatalf("dismiss calls = %q, want exactly %q", got, wantCall)
	}
	if got := app.comparesMade(); !slices.Equal(got, []string{"b1...b2", "b1...b3"}) {
		t.Fatalf("the App identity compared %q, want one compare per head", got)
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
		t.Fatalf("last_review_event = %q, want DISMISSED", ev)
	}
}

func TestKeepApprovalsLeavesTheApprovalAlone(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*config.Config)
		keep bool
	}{
		{"by default the approval goes", func(*config.Config) {}, false},
		{"[[repo]] keep_approvals", func(c *config.Config) {
			c.Repos = append(c.Repos, config.Repo{Repo: "talkable/talkable", KeepApprovals: new(true)})
		}, true},
		{"[[watch]] keep_approvals", func(c *config.Config) { c.Watches[0].KeepApprovals = true }, true},
		{"[[repo]] keep_approvals = false overrides the watch", func(c *config.Config) {
			c.Watches[0].KeepApprovals = true
			c.Repos = append(c.Repos, config.Repo{Repo: "talkable/talkable", KeepApprovals: new(false)})
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, app := newApprovalHarness(t, func(h *harness) {
				if h.cfg.Watches[0].Owner != "talkable" {
					t.Fatalf("watch 0 = %q, want talkable", h.cfg.Watches[0].Owner)
				}
				tc.mod(h.cfg)
			})
			approvedPR(h, 2, "b1")
			h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}

			pollPR(h, time.Minute, 2, "b2")
			pollPR(h, time.Minute, 2, "b2")
			if tc.keep {
				wantUntouched(t, h, app, 2)
				if evs := approvalEvents(t, h, 2, "review.approval_kept"); len(evs) != 0 {
					t.Fatalf("review.approval_kept events: %+v (kept by configuration, not by a compare)", evs)
				}
				return
			}
			if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
				t.Fatalf("dismiss calls = %q, want one", got)
			}
			if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
				t.Fatalf("last_review_event = %q, want DISMISSED", ev)
			}
		})
	}
}

func TestForbiddenDismissalIsReportedOnceAndNeverRetried(t *testing.T) {
	h, _ := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}
	setDismissErr(h.gh, fmt.Errorf("dismiss review: %w", &github.APIError{Op: "dismiss talkable/talkable#2", Status: 403, Message: "Resource not accessible by integration"}))

	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
		t.Fatalf("dismiss calls = %q, want one attempt", got)
	}
	refused := approvalEvents(t, h, 2, "review.approval_dismiss_refused")
	if len(refused) != 1 || refused[0].Level != "warn" {
		t.Fatalf("review.approval_dismiss_refused events: %+v, want one warn", refused)
	}
	pr := h.pr(2)
	if v, ok := kvValue(h, fmt.Sprintf("pr.%d.approval_refused", pr.ID)); !ok || v != fmt.Sprint(approvalID) {
		t.Fatalf("approval_refused marker = %q (set %v), want %d", v, ok, approvalID)
	}
	if ev := deref(pr.LastReviewEvent); ev != "APPROVED" {
		t.Fatalf("last_review_event = %q, want APPROVED (the approval stands)", ev)
	}
	if evs := approvalEvents(t, h, 2, "review.approval_dismissed"); len(evs) != 0 {
		t.Fatalf("review.approval_dismissed events: %+v", evs)
	}

	// Not retried: not at the next polls, not even once the permission is there.
	pollPR(h, time.Minute, 2, "b2")
	setDismissErr(h.gh, nil)
	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
		t.Fatalf("dismiss calls after later polls = %q, want still one", got)
	}
	if got := approvalEvents(t, h, 2, "review.approval_dismiss_refused"); len(got) != 1 {
		t.Fatalf("review.approval_dismiss_refused events after later polls: %d, want one", len(got))
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "APPROVED" {
		t.Fatalf("last_review_event = %q, want APPROVED", ev)
	}
}

func TestRefusedDismissalIsRememberedPerReviewNotPerPR(t *testing.T) {
	h, _ := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}
	h.gh.compare["b2...b3"] = github.CompareStats{Commits: 1}
	setDismissErr(h.gh, &github.APIError{Status: 403, Message: "Resource not accessible by integration"})

	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
		t.Fatalf("dismiss calls = %q, want one refused attempt", got)
	}
	// The re-review posts a second approval, at b2; a push after it is a new question.
	setDismissErr(h.gh, nil)
	pollPR(h, 30*time.Minute, 2, "b2")
	if pr := h.wantState(2, store.PRReviewed); deref(pr.LastReviewID) != approvalID+1 || deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("after the re-review: review %v event %q", pr.LastReviewID, deref(pr.LastReviewEvent))
	}
	pollPR(h, time.Minute, 2, "b3")
	want := []string{
		fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID, ApprovalDismissMessage),
		fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID+1, ApprovalDismissMessage),
	}
	if got := callsWith(h.gh, "dismiss:"); !slices.Equal(got, want) {
		t.Fatalf("dismiss calls = %q, want %q", got, want)
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
		t.Fatalf("last_review_event = %q, want DISMISSED", ev)
	}
}

func TestTransientDismissalErrorIsRetriedAtTheNextPoll(t *testing.T) {
	h, _ := newApprovalHarness(t)
	approvedPR(h, 2, "b1")
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}
	setDismissErr(h.gh, errors.New("connection reset by peer"))

	pollPR(h, time.Minute, 2, "b2")
	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 2 {
		t.Fatalf("dismiss calls = %q, want an attempt at each of the two polls", got)
	}
	pr := h.pr(2)
	if ev := deref(pr.LastReviewEvent); ev != "APPROVED" {
		t.Fatalf("last_review_event = %q, want APPROVED while the dismissal fails", ev)
	}
	if _, ok := kvValue(h, fmt.Sprintf("pr.%d.approval_refused", pr.ID)); ok {
		t.Fatal("a transient error was recorded as a refusal")
	}
	for _, kind := range []string{"review.approval_dismissed", "review.approval_dismiss_refused"} {
		if evs := approvalEvents(t, h, 2, kind); len(evs) != 0 {
			t.Fatalf("%s events: %+v", kind, evs)
		}
	}

	setDismissErr(h.gh, nil)
	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 3 {
		t.Fatalf("dismiss calls = %q, want the third attempt to be the one that succeeds", got)
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
		t.Fatalf("last_review_event = %q, want DISMISSED", ev)
	}
	if evs := approvalEvents(t, h, 2, "review.approval_dismissed"); len(evs) != 1 {
		t.Fatalf("review.approval_dismissed events: %d, want one", len(evs))
	}
	pollPR(h, time.Minute, 2, "b2")
	if got := callsWith(h.gh, "dismiss:"); len(got) != 3 {
		t.Fatalf("dismiss calls after the success = %q, want still three", got)
	}
}

func TestOnlyApprovingReviewsAreDismissed(t *testing.T) {
	tests := []struct {
		event string
		want  bool
	}{
		{"APPROVED", true},
		{"APPROVE", true},
		{"COMMENTED", false},
		{"COMMENT", false},
		{"CHANGES_REQUESTED", false},
		{"REQUEST_CHANGES", false},
		{"DISMISSED", false},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			h, app := newApprovalHarness(t)
			approving(h, tc.event)
			h.reviewedPR(2, "b1")
			h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}

			pollPR(h, time.Minute, 2, "b2")
			pollPR(h, time.Minute, 2, "b2")
			got := callsWith(h.gh, "dismiss:")
			if tc.want {
				if len(got) != 1 {
					t.Fatalf("dismiss calls = %q, want one for a %s review", got, tc.event)
				}
				return
			}
			if len(got) != 0 || len(app.comparesMade()) != 0 {
				t.Fatalf("a %s review: dismiss calls %q, App compares %q, want neither", tc.event, got, app.comparesMade())
			}
			if ev := deref(h.pr(2).LastReviewEvent); ev != tc.event {
				t.Fatalf("last_review_event = %q, want it untouched (%s)", ev, tc.event)
			}
		})
	}
}

func TestOnlyTheConfiguredAppsReviewIsDismissed(t *testing.T) {
	tests := []struct {
		name  string
		login any // last_review_login; nil = none recorded
		want  bool
	}{
		{"the App's login", "talkable[bot]", true},
		{"a user named like the App", "talkable", false},
		{"no login recorded: the PR's identity is the App", nil, true},
		{"another login", "example-bot", false},
		{"the gh user's login", "zhuravel", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, app := newApprovalHarness(t)
			pr := approvedPR(h, 2, "b1")
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_review_login", tc.login) }); err != nil {
				t.Fatal(err)
			}
			h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}

			pollPR(h, time.Minute, 2, "b2")
			pollPR(h, time.Minute, 2, "b2")
			if tc.want {
				if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
					t.Fatalf("dismiss calls = %q, want one", got)
				}
				return
			}
			wantUntouched(t, h, app, 2)
		})
	}
}

// reviewedExamplePR runs the flow to PR n of example/notes (a watch run as
// the gh identity, a per-PR worktree) reviewed at head, next to a baseline
// #1.
func reviewedExamplePR(h *harness, n int, head string) store.PR {
	h.t.Helper()
	pollExamplePR(h, 0, n, "")
	h.startup()
	h.tick()
	pollExamplePR(h, 0, n, head)
	h.advance(5 * time.Minute)
	h.tick()
	pr := examplePR(h, n)
	if pr.State != store.PRReviewed {
		h.t.Fatalf("example/notes#%d state = %s, want reviewed (last_error %q)", n, pr.State, deref(pr.LastError))
	}
	return pr
}

// pollExamplePR lets d pass, shows PR n of example/notes at head (none when
// head is "") next to the baseline #1 and, unless head is "", runs one tick.
func pollExamplePR(h *harness, d time.Duration, n int, head string) {
	h.t.Helper()
	h.advance(d)
	prs := []prSpec{{n: 1, head: "x1", updated: h.clock.Now()}}
	if head != "" {
		prs = append(prs, prSpec{n: n, head: head, updated: h.clock.Now()})
	}
	h.gh.set("example/notes", prs...)
	if head != "" {
		h.tick()
	}
}

func examplePR(h *harness, n int) store.PR {
	h.t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, "example/notes")
	if err != nil {
		h.t.Fatal(err)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, n)
	if err != nil {
		h.t.Fatal(err)
	}
	return pr
}

func TestGhUsersApprovalIsNeverDismissed(t *testing.T) {
	h, app := newApprovalHarness(t, withExampleWatch)
	approving(h, "APPROVED")
	pr := reviewedExamplePR(h, 2, "y1")
	if pr.Identity != "zhuravel" || deref(pr.LastReviewEvent) != "APPROVED" || deref(pr.LastReviewID) != approvalID {
		t.Fatalf("setup: as %s, review %v event %q", pr.Identity, pr.LastReviewID, deref(pr.LastReviewEvent))
	}

	h.gh.compare["y1...y2"] = github.CompareStats{Commits: 2}
	pollExamplePR(h, time.Minute, 2, "y2")
	pollExamplePR(h, time.Minute, 2, "y2")

	if got := callsWith(h.gh, "dismiss:"); len(got) != 0 {
		t.Fatalf("dismiss calls = %q, want none for a gh user's approval", got)
	}
	if got := app.comparesMade(); len(got) != 0 {
		t.Fatalf("the App identity compared %q", got)
	}
	pr = examplePR(h, 2)
	if pr.HeadSHA != "y2" || deref(pr.ReviewedSHA) != "y1" || deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("PR: head %s reviewed %q event %q, want the approval standing on y1", pr.HeadSHA, deref(pr.ReviewedSHA), deref(pr.LastReviewEvent))
	}
}

func TestApprovalIsDismissedWhenTheCompareFails(t *testing.T) {
	tests := []struct {
		name string
		fail func(h *harness)
	}{
		{"compare error", func(h *harness) { setCompareErr(h.gh, errors.New("compare unavailable")) }},
		{"compare finds no such commit", func(h *harness) {}}, // no compare entry: the fake answers 404
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, app := newApprovalHarness(t)
			approvedPR(h, 2, "b1")
			tc.fail(h)

			pollPR(h, time.Minute, 2, "b2")
			wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID, ApprovalDismissMessage)
			if got := callsWith(h.gh, "dismiss:"); !slices.Equal(got, []string{wantCall}) {
				t.Fatalf("dismiss calls = %q, want exactly %q", got, wantCall)
			}
			if got := app.comparesMade(); !slices.Equal(got, []string{"b1...b2"}) {
				t.Fatalf("the App identity compared %q, want one compare", got)
			}
			pr := h.wantState(2, store.PRRereviewPending)
			if ev := deref(pr.LastReviewEvent); ev != "DISMISSED" {
				t.Fatalf("last_review_event = %q, want DISMISSED", ev)
			}
			if evs := approvalEvents(t, h, 2, "review.approval_dismissed"); len(evs) != 1 {
				t.Fatalf("review.approval_dismissed events: %d, want one", len(evs))
			}
			if evs := approvalEvents(t, h, 2, "review.approval_kept"); len(evs) != 0 {
				t.Fatalf("review.approval_kept events: %+v", evs)
			}
		})
	}
}

// A dry run reads GitHub but changes nothing on it. Both halves run the same
// scenario so that the dry one cannot pass by never reaching the follower.
func TestDryRunDismissesNothing(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dry), func(t *testing.T) {
			h, app := newApprovalHarness(t, func(h *harness) { h.d.DryRun = dry })
			h.open(prSpec{n: 1, head: "base1"})
			if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
				t.Fatal(err)
			}
			pollPR(h, time.Minute, 2, "b1") // a new PR; no round runs in a dry run, so its review is recorded by hand
			pr := h.pr(2)
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
				u.Set("reviewed_sha", "b1")
				u.Set("last_review_id", approvalID)
				u.Set("last_review_event", "APPROVED")
				u.Set("last_review_login", "talkable[bot]")
			}); err != nil {
				t.Fatal(err)
			}
			h.gh.compare["b1...b2"] = github.CompareStats{Commits: 2}

			pollPR(h, time.Minute, 2, "b2")
			pollPR(h, time.Minute, 2, "b2")
			if dry {
				wantUntouched(t, h, app, 2)
				return
			}
			if got := callsWith(h.gh, "dismiss:"); len(got) != 1 {
				t.Fatalf("dismiss calls = %q, want one (the control half)", got)
			}
			if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
				t.Fatalf("last_review_event = %q, want DISMISSED", ev)
			}
		})
	}
}
