package engine

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Identity migration (migrate.go): a PR follows its watch's posting identity,
// the reviews of the identity it leaves are its own history, and what that
// identity left standing is dismissed once the new identity has reviewed.
//
// The names in these tests: talkable-app (talkable[bot]) is the harness's
// App, zhuravel-app (zhuravel[bot]) the App a watch moves to, bob-app
// (bob[bot]) a third one.

// migHarness is the harness with a second App identity and one GitHub client
// per posting identity: the dismissals of a former identity are made with
// that identity's own client, so a test can tell whose credentials were used.
// The poll identity "zhuravel" keeps the harness's own client (h.gh).
type migHarness struct {
	*harness

	mu      sync.Mutex
	clients map[string]*fakeGH
	none    map[string]bool // identities that have no GitHub client
}

// client is the GitHub client of a posting identity (created on first use).
func (m *migHarness) client(identity string) *fakeGH {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.clients[identity]
	if !ok {
		g = newFakeGH()
		m.clients[identity] = g
	}
	return g
}

// withoutClient makes the identity's GitHub client absent (Deps.GitHub nil).
func (m *migHarness) withoutClient(identity string) {
	m.mu.Lock()
	m.none[identity] = true
	m.mu.Unlock()
}

func migApp(name, login string) config.Identity {
	return config.Identity{Name: name, Kind: "app", Login: login, AppID: 3, ClientID: "Iv-" + name, InstallationID: 4,
		PrivateKeyEnv: "MAGNUM_TEST_KEY", NoFindingsEvent: "COMMENT", BlockingEvent: "REQUEST_CHANGES"}
}

// migWithApp adds an App identity to the configuration and the identity
// sources of the harness.
func migWithApp(name, login string) func(*harness) {
	return func(h *harness) {
		h.cfg.Identities = append(h.cfg.Identities, migApp(name, login))
		src := &fakeAppIdentity{fakeIdentity{name: name, login: login, kind: "app"}}
		h.d.Identities[name] = src
		h.ids[name] = &src.fakeIdentity
	}
}

func newMigHarness(t *testing.T, mods ...func(*harness)) *migHarness {
	t.Helper()
	m := &migHarness{clients: map[string]*fakeGH{}, none: map[string]bool{}}
	wire := func(h *harness) {
		poll := h.d.GitHub
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return poll(id)
			}
			m.mu.Lock()
			none := m.none[id]
			m.mu.Unlock()
			if none {
				return nil
			}
			return m.client(id)
		}
	}
	all := []func(*harness){migWithApp("zhuravel-app", "zhuravel[bot]"), wire}
	m.harness = newHarness(t, append(all, mods...)...)
	return m
}

func migReview(id int64, state, login, typ string) github.Review {
	return github.Review{DatabaseID: id, State: state, AuthorLogin: login, AuthorType: typ}
}

// migPosts makes every round post event, as review 500 for the first round,
// 501 for the second and so on.
func migPosts(h *harness, event string) {
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n := len(h.rd.all())
		res, err := h.rd.posted(h.ctx, in, n)
		res.Event, res.ReviewID = event, int64(499+n)
		return res, err
	}
}

// migRun records a verified judge run that posted reviewID as login, the way
// the pipeline leaves it.
func migRun(h *harness, pr store.PR, id string, round int, identity, login string, reviewID int64, event string) {
	h.t.Helper()
	if _, err := h.st.CreateRun(h.ctx, store.Run{ID: id, PRID: pr.ID, Round: round, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: "b1", Identity: identity, ReviewerLogin: login, State: store.RunVerified,
		ReviewID: store.Ptr(reviewID), ReviewEvent: store.Ptr(event)}); err != nil {
		h.t.Fatal(err)
	}
}

// reviewedByA runs PR #2 to "reviewed at b1 by talkable-app": its COMMENTED
// review 500, with the judge run row the pipeline would have left.
func (m *migHarness) reviewedByA() store.PR {
	m.t.Helper()
	migPosts(m.harness, "COMMENTED")
	pr := m.reviewedPR(2, "b1")
	if deref(pr.LastReviewID) != 500 || deref(pr.LastReviewEvent) != "COMMENTED" || deref(pr.LastReviewLogin) != "talkable[bot]" || pr.Identity != "talkable-app" {
		m.t.Fatalf("setup: review %v %q by %q as %q", pr.LastReviewID, deref(pr.LastReviewEvent), deref(pr.LastReviewLogin), pr.Identity)
	}
	migRun(m.harness, pr, "run-a", 1, "talkable-app", "talkable[bot]", 500, "COMMENTED")
	return pr
}

// migRereview pushes head to PR n and lets the quiet period and the
// re-review interval pass until the PR is reviewed at head.
func migRereview(h *harness, n int, head string) store.PR {
	h.t.Helper()
	pollPR(h, time.Minute, n, head)
	for range 4 {
		pollPR(h, 40*time.Minute, n, head)
		if pr := h.pr(n); pr.State == store.PRReviewed && deref(pr.ReviewedSHA) == head {
			return pr
		}
	}
	pr := h.pr(n)
	h.t.Fatalf("PR #%d never reviewed at %s: state %s reviewed_sha %q last_error %q", n, head, pr.State, deref(pr.ReviewedSHA), deref(pr.LastError))
	return pr
}

// lastInput is the pipeline input of the latest round of PR n.
func (h *harness) lastInput(n int) pipeline.RoundInput {
	h.t.Helper()
	ins := h.rd.all()
	for i := len(ins) - 1; i >= 0; i-- {
		if ins[i].PR.Number == n {
			return ins[i]
		}
	}
	h.t.Fatalf("no round ran for PR #%d", n)
	return pipeline.RoundInput{}
}

// migFormer is the PR's kv former_identities list (nil when absent).
func migFormer(h *harness, prID int64) []string {
	h.t.Helper()
	v, ok := kvValue(h, KVPRFormerIdentities(prID))
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		h.t.Fatalf("former_identities %q: %v", v, err)
	}
	return out
}

// migDispatch runs migrateIdentity on PR n the way the dispatcher does, with
// the watch as configured now, and returns the PR row after it.
func migDispatch(h *harness, n int) store.PR {
	h.t.Helper()
	pr := h.pr(n)
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.migrateIdentity(h.ctx, &pr, repo, h.cfg.WatchFor("talkable/talkable"))
	return h.pr(n)
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// migDismissCall is the call a DismissReview of the former identity's review
// id makes on its client, for a review of head posted as login.
func migDismissCall(id int64, head, login string) string {
	return fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", id, fmt.Sprintf(FormerDismissMessage, textx.ShortSHA(head), login))
}

// ---- 1. the live case ----

// PR #2 was reviewed by the App A: its last review is COMMENTED and an older
// change request of A is DISMISSED. The watch moves to the App B and a new
// head arrives: the round posts as B, with A's login as a former one, A's
// COMMENTED review as the previous review, and the old sessions parked.
// Nothing is left to dismiss.
func TestMigrationLiveCaseKeepsTheOldAppsHistory(t *testing.T) {
	m := newMigHarness(t)
	pr := m.reviewedByA()
	a, b := m.client("talkable-app"), m.client("zhuravel-app")
	a.allReviews = map[int][]github.Review{2: {
		migReview(400, "DISMISSED", "talkable", "Bot"),
		migReview(500, "COMMENTED", "talkable", "Bot"),
	}}
	parksBefore := m.ag.count(fmt.Sprintf("park:%d", pr.ID))

	m.cfg.Watches[0].Identity = "zhuravel-app"
	got := migRereview(m.harness, 2, "b2")

	in := m.lastInput(2)
	if in.PR.Identity != "zhuravel-app" || got.Identity != "zhuravel-app" {
		t.Fatalf("identity: round input %q, PR row %q, want zhuravel-app", in.PR.Identity, got.Identity)
	}
	// The fresh sessions of the new identity have no conversation of the PR:
	// the judge re-reads its history (a recovery round, which gets Previous).
	if in.Kind != pipeline.KindRecovery || in.TargetSHA != "b2" {
		t.Fatalf("round = %s at %s, want a recovery of b2", in.Kind, in.TargetSHA)
	}
	wantStrings(t, "FormerLogins", in.FormerLogins, []string{"talkable[bot]"})
	prev := in.Previous
	if prev == nil || prev.ID != 500 || prev.Event != "COMMENTED" || prev.SHA != "b1" || !github.SameLogin(prev.Login, "talkable[bot]") || prev.Login == "" {
		t.Fatalf("Previous = %+v, want A's COMMENTED review 500 at b1 posted by talkable", prev)
	}
	wantStrings(t, "former_identities", migFormer(m.harness, pr.ID), []string{"talkable-app"})

	migrated := approvalEvents(t, m.harness, 2, "pr.identity_migrated")
	if len(migrated) != 1 || migrated[0].Level != "info" {
		t.Fatalf("pr.identity_migrated events: %+v", migrated)
	}
	var data map[string]any
	if err := json.Unmarshal(migrated[0].Data, &data); err != nil {
		t.Fatalf("event data %s: %v", migrated[0].Data, err)
	}
	if data["from"] != "talkable-app" || data["to"] != "zhuravel-app" || fmt.Sprint(data["former_identities"]) != "[talkable-app]" {
		t.Fatalf("pr.identity_migrated data = %v", data)
	}
	changed := approvalEvents(t, m.harness, 2, "pr.identity_changed")
	if len(changed) != 1 {
		t.Fatalf("pr.identity_changed events: %+v", changed)
	}
	if n := m.ag.count(fmt.Sprintf("park:%d", pr.ID)); n <= parksBefore {
		t.Fatalf("parks of the PR = %d, was %d: the old identity's sessions must be parked", n, parksBefore)
	}
	if v, _ := m.e.getKV(m.ctx, kvPRSessionsIdentity(pr.ID)); v != "zhuravel-app" {
		t.Fatalf("sessions identity = %q, want zhuravel-app", v)
	}

	// B's review stands; the review it follows is A's COMMENTED one, and
	// there is nothing of A's to dismiss (COMMENTED and DISMISSED stay).
	if deref(got.LastReviewID) != 501 || deref(got.LastReviewLogin) != "zhuravel[bot]" || deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("after the round: review %v by %q at %q", got.LastReviewID, deref(got.LastReviewLogin), deref(got.ReviewedSHA))
	}
	for name, g := range map[string]*fakeGH{"talkable-app": a, "zhuravel-app": b, "the poll client": m.gh} {
		if calls := callsWith(g, "dismiss:"); len(calls) != 0 {
			t.Errorf("%s dismissed %q, want nothing", name, calls)
		}
	}
	for _, kind := range []string{"review.former_dismissed", "review.former_dismiss_failed"} {
		if evs := approvalEvents(t, m.harness, 2, kind); len(evs) != 0 {
			t.Errorf("%s events: %+v", kind, evs)
		}
	}
}

// ---- 2. what the former identity left standing ----

// aLeftReviews are the reviews on PR #2 as the former App's client lists
// them: two of A's that stand (a change request and an approval), two that do
// not (COMMENTED, DISMISSED), and reviews of other logins.
func aLeftReviews() []github.Review {
	return []github.Review{
		migReview(401, "CHANGES_REQUESTED", "talkable", "Bot"),
		migReview(402, "APPROVED", "talkable", "Bot"),
		migReview(403, "COMMENTED", "talkable", "Bot"),
		migReview(404, "DISMISSED", "talkable", "Bot"),
		migReview(405, "CHANGES_REQUESTED", "alice", "User"),
		migReview(406, "CHANGES_REQUESTED", "talkable", "User"), // a user account that shares the App's name
		migReview(407, "CHANGES_REQUESTED", "zhuravel", "Bot"),  // the new identity's own
		migReview(408, "APPROVED", "bob", "User"),
	}
}

// Once B's review is posted, A's own CHANGES_REQUESTED and APPROVED reviews
// are dismissed with A's client; nothing else is touched. The knobs that
// turn parts of it off: keep_approvals keeps the approval, an App that does
// not dismiss its stale change requests keeps the change request.
func TestMigrationDismissesWhatTheFormerAppLeftStanding(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*harness)
		want []int64
	}{
		{"both", func(*harness) {}, []int64{401, 402}},
		{"keep_approvals on the watch keeps the approval", func(h *harness) { h.cfg.Watches[0].KeepApprovals = true }, []int64{401}},
		{"keep_approvals on the repo keeps the approval", func(h *harness) {
			h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", KeepApprovals: store.Ptr(true)})
		}, []int64{401}},
		{"dismiss_own_stale_change_requests off keeps the change request", func(h *harness) {
			h.cfg.IdentityByName("talkable-app").DismissOwnStale = store.Ptr(false)
		}, []int64{402}},
		{"both off leaves everything", func(h *harness) {
			h.cfg.Watches[0].KeepApprovals = true
			h.cfg.IdentityByName("talkable-app").DismissOwnStale = store.Ptr(false)
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMigHarness(t, tc.mod)
			m.reviewedByA()
			a, b := m.client("talkable-app"), m.client("zhuravel-app")
			a.allReviews = map[int][]github.Review{2: aLeftReviews()}

			m.cfg.Watches[0].Identity = "zhuravel-app"
			got := migRereview(m.harness, 2, "b2")

			var want []string
			for _, id := range tc.want {
				want = append(want, migDismissCall(id, "b2", "zhuravel[bot]"))
			}
			wantStrings(t, "dismiss calls on A's client", callsWith(a, "dismiss:"), want)
			if calls := callsWith(b, "dismiss:"); len(calls) != 0 {
				t.Errorf("the new identity's client dismissed %q, want nothing", calls)
			}
			if calls := callsWith(m.gh, "dismiss:"); len(calls) != 0 {
				t.Errorf("the poll client dismissed %q, want nothing", calls)
			}
			if deref(got.LastReviewID) != 501 || got.State != store.PRReviewed {
				t.Fatalf("PR after the round: %s, review %v", got.State, got.LastReviewID)
			}

			evs := approvalEvents(t, m.harness, 2, "review.former_dismissed")
			if len(evs) != len(tc.want) {
				t.Fatalf("review.former_dismissed events = %d, want %d: %+v", len(evs), len(tc.want), evs)
			}
			for i, ev := range evs {
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatalf("event data %s: %v", ev.Data, err)
				}
				if ev.Level != "info" || data["review_id"] != float64(tc.want[i]) || data["identity"] != "talkable-app" || data["superseded_by"] != float64(501) {
					t.Errorf("event %d: level %s data %v", i, ev.Level, data)
				}
			}
			if evs := approvalEvents(t, m.harness, 2, "review.former_dismiss_failed"); len(evs) != 0 {
				t.Errorf("review.former_dismiss_failed events: %+v", evs)
			}
		})
	}
}

// A dismissal that fails (a missing permission, say) is a warning per review:
// the other review is still tried, and the round ends reviewed all the same.
func TestMigrationFormerDismissErrorIsAWarning(t *testing.T) {
	m := newMigHarness(t)
	m.reviewedByA()
	a := m.client("talkable-app")
	a.allReviews = map[int][]github.Review{2: aLeftReviews()}
	setDismissErr(a, errors.New("403 Resource not accessible by integration"))

	m.cfg.Watches[0].Identity = "zhuravel-app"
	got := migRereview(m.harness, 2, "b2")

	wantStrings(t, "dismiss attempts", callsWith(a, "dismiss:"),
		[]string{migDismissCall(401, "b2", "zhuravel[bot]"), migDismissCall(402, "b2", "zhuravel[bot]")})
	failed := approvalEvents(t, m.harness, 2, "review.former_dismiss_failed")
	if len(failed) != 2 || failed[0].Level != "warn" || failed[1].Level != "warn" {
		t.Fatalf("review.former_dismiss_failed events: %+v", failed)
	}
	if evs := approvalEvents(t, m.harness, 2, "review.former_dismissed"); len(evs) != 0 {
		t.Fatalf("review.former_dismissed events after errors: %+v", evs)
	}
	if got.State != store.PRReviewed || deref(got.LastReviewID) != 501 || deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("PR after the round: %s review %v at %q, want reviewed by 501 at b2", got.State, got.LastReviewID, deref(got.ReviewedSHA))
	}
}

// busyReviews are 45 reviews on PR #2, oldest first: the former App's
// change request is the 5th, with 40 newer reviews (replies, other
// reviewers) after it, past the last 30 a single read returns.
func busyReviews() []github.Review {
	var out []github.Review
	for i := range int64(45) {
		rv := migReview(600+i, "COMMENTED", "rev-ann", "User")
		if i == 4 {
			rv = migReview(600+i, "CHANGES_REQUESTED", "talkable", "Bot")
		}
		out = append(out, rv)
	}
	return out
}

// The former identity's blocking review is found among all the PR's
// reviews, however many came after it, and dismissed.
func TestMigrationDismissesAFormerReviewFortyReviewsBack(t *testing.T) {
	m := newMigHarness(t)
	m.reviewedByA()
	a := m.client("talkable-app")
	a.allReviews = map[int][]github.Review{2: busyReviews()}

	m.cfg.Watches[0].Identity = "zhuravel-app"
	migRereview(m.harness, 2, "b2")

	wantStrings(t, "dismiss calls on A's client", callsWith(a, "dismiss:"), []string{migDismissCall(604, "b2", "zhuravel[bot]")})
}

// A list of the PR's reviews cut at GitHub's page limit may lack the former
// identity's latest reviews: nothing is dismissed from it, and a warning
// says why.
func TestMigrationDismissesNothingFromAnIncompleteList(t *testing.T) {
	m := newMigHarness(t)
	m.reviewedByA()
	a := m.client("talkable-app")
	a.allReviews = map[int][]github.Review{2: aLeftReviews()}
	a.reviewsCut = map[int]bool{2: true}

	m.cfg.Watches[0].Identity = "zhuravel-app"
	got := migRereview(m.harness, 2, "b2")

	if calls := callsWith(a, "dismiss:"); len(calls) != 0 {
		t.Fatalf("dismiss calls = %q, want none from an incomplete list", calls)
	}
	failed := approvalEvents(t, m.harness, 2, "review.former_dismiss_failed")
	if len(failed) != 1 || failed[0].Level != "warn" || !strings.Contains(failed[0].Message, "incomplete") {
		t.Fatalf("review.former_dismiss_failed events: %+v", failed)
	}
	if got.State != store.PRReviewed || deref(got.LastReviewID) != 501 {
		t.Fatalf("PR after the round: %s review %v", got.State, got.LastReviewID)
	}
}

// A former identity without a GitHub client cannot dismiss: one warning, and
// the round is not affected.
func TestMigrationFormerWithoutAClientIsAWarning(t *testing.T) {
	m := newMigHarness(t)
	m.reviewedByA()
	m.client("talkable-app").allReviews = map[int][]github.Review{2: aLeftReviews()}
	m.withoutClient("talkable-app")

	m.cfg.Watches[0].Identity = "zhuravel-app"
	got := migRereview(m.harness, 2, "b2")

	failed := approvalEvents(t, m.harness, 2, "review.former_dismiss_failed")
	if len(failed) != 1 || failed[0].Level != "warn" {
		t.Fatalf("review.former_dismiss_failed events: %+v", failed)
	}
	if calls := callsWith(m.client("talkable-app"), "dismiss:"); len(calls) != 0 {
		t.Fatalf("dismiss calls = %q, want none", calls)
	}
	if got.State != store.PRReviewed || deref(got.LastReviewID) != 501 {
		t.Fatalf("PR after the round: %s review %v", got.State, got.LastReviewID)
	}
}

// A gh identity that the watch leaves dismisses nothing by default (it keeps
// its change requests and its approvals); with
// dismiss_own_stale_change_requests only its change requests go, with its own
// client (the poll client here), and an approval of a user account stays.
func TestMigrationFromAUserDismissesOnlyWhenOptedIn(t *testing.T) {
	userReviews := []github.Review{
		migReview(501, "CHANGES_REQUESTED", "zhuravel", "User"),
		migReview(502, "APPROVED", "zhuravel", "User"),
		migReview(503, "CHANGES_REQUESTED", "alice", "User"),
		migReview(504, "CHANGES_REQUESTED", "zhuravel", "Bot"), // an App that shares the user's name
	}
	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("dismiss_own_stale_change_requests %v", optIn), func(t *testing.T) {
			m := newMigHarness(t, func(h *harness) {
				h.cfg.Watches[0].Identity = "zhuravel"
				if optIn {
					h.cfg.IdentityByName("zhuravel").DismissOwnStale = store.Ptr(true)
				}
			})
			migPosts(m.harness, "COMMENTED")
			pr := m.reviewedPR(2, "b1")
			if pr.Identity != "zhuravel" {
				t.Fatalf("setup: identity %q", pr.Identity)
			}
			m.gh.allReviews = map[int][]github.Review{2: userReviews}

			m.cfg.Watches[0].Identity = "talkable-app"
			got := migRereview(m.harness, 2, "b2")

			in := m.lastInput(2)
			wantStrings(t, "FormerLogins", in.FormerLogins, []string{"zhuravel"})
			wantStrings(t, "former_identities", migFormer(m.harness, pr.ID), []string{"zhuravel"})
			var want []string
			if optIn {
				want = []string{migDismissCall(501, "b2", "talkable[bot]")}
			}
			wantStrings(t, "dismiss calls", callsWith(m.gh, "dismiss:"), want)
			if got.State != store.PRReviewed || got.Identity != "talkable-app" {
				t.Fatalf("PR after the round: %s as %s", got.State, got.Identity)
			}
		})
	}
}

// A user account and an App can share a name (zhuravel and zhuravel[bot] are
// two accounts; the pipeline tells them apart, isOwnHistory). Moving a watch
// from the user to such an App must keep the user as a former login and
// dismiss the user's change request: formerLogins and dismissFormer compare
// logins with github.SameLogin alone, which ignores "[bot]", and so drop the
// former identity as if it were the current one.
func TestMigrationFromAUserToAnAppOfTheSameName(t *testing.T) {
	setup := func(t *testing.T) (*migHarness, store.PR) {
		m := newMigHarness(t, func(h *harness) {
			h.cfg.Watches[0].Identity = "zhuravel"
			h.cfg.IdentityByName("zhuravel").DismissOwnStale = store.Ptr(true)
		})
		migPosts(m.harness, "COMMENTED")
		pr := m.reviewedPR(2, "b1")
		m.gh.allReviews = map[int][]github.Review{2: {
			migReview(501, "CHANGES_REQUESTED", "zhuravel", "User"),
			migReview(502, "CHANGES_REQUESTED", "zhuravel", "Bot"), // the new App's own
		}}
		m.cfg.Watches[0].Identity = "zhuravel-app"
		return m, pr
	}
	t.Run("the user's login stays a former login", func(t *testing.T) {
		m, pr := setup(t)
		migRereview(m.harness, 2, "b2")
		wantStrings(t, "former_identities", migFormer(m.harness, pr.ID), []string{"zhuravel"})
		wantStrings(t, "FormerLogins", m.lastInput(2).FormerLogins, []string{"zhuravel"})
	})
	t.Run("the user's change request is dismissed", func(t *testing.T) {
		m, _ := setup(t)
		migRereview(m.harness, 2, "b2")
		wantStrings(t, "dismiss calls", callsWith(m.gh, "dismiss:"), []string{migDismissCall(501, "b2", "zhuravel[bot]")})
	})
}

// dismissFormer is a no-op when nothing was posted (ReviewID 0, as in a
// round that ended otherwise), in a dry run, and for a former identity whose
// login is the current one.
func TestDismissFormerGuards(t *testing.T) {
	setup := func(t *testing.T) (*migHarness, store.PR, *roundJob) {
		m := newMigHarness(t)
		pr := m.reviewedByA()
		m.client("talkable-app").allReviews = map[int][]github.Review{2: aLeftReviews()}
		m.client("zhuravel-app").allReviews = map[int][]github.Review{2: aLeftReviews()}
		repo, err := m.st.RepoByFullName(m.ctx, "talkable/talkable")
		if err != nil {
			t.Fatal(err)
		}
		m.e.setKV(m.ctx, KVPRFormerIdentities(pr.ID), `["talkable-app"]`)
		pr.Identity = "zhuravel-app"
		return m, pr, &roundJob{repo: repo}
	}
	dismissed := func(m *migHarness) int {
		return len(callsWith(m.client("talkable-app"), "dismiss:")) + len(callsWith(m.client("zhuravel-app"), "dismiss:")) + len(callsWith(m.gh, "dismiss:"))
	}

	t.Run("it dismisses with a review id", func(t *testing.T) {
		m, pr, job := setup(t)
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{ReviewID: 501})
		if n := dismissed(m); n != 2 {
			t.Fatalf("dismissals = %d, want 2 (the control for the cases below)", n)
		}
	})
	t.Run("no review id", func(t *testing.T) {
		m, pr, job := setup(t)
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{})
		if n := dismissed(m); n != 0 {
			t.Fatalf("dismissals = %d, want none", n)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		m, pr, job := setup(t)
		m.e.d.DryRun = true
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{ReviewID: 501})
		if n := dismissed(m); n != 0 {
			t.Fatalf("dismissals = %d, want none", n)
		}
	})
	t.Run("a post-merge review", func(t *testing.T) {
		m, pr, job := setup(t)
		job.postMerge = true
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{ReviewID: 501})
		if n := dismissed(m); n != 0 {
			t.Fatalf("dismissals = %d, want none after the merge", n)
		}
	})
	t.Run("the current identity is never its own former one", func(t *testing.T) {
		m, pr, job := setup(t)
		m.e.setKV(m.ctx, KVPRFormerIdentities(pr.ID), `["zhuravel-app"]`) // data from a bad migration
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{ReviewID: 501})
		if n := dismissed(m); n != 0 {
			t.Fatalf("dismissals = %d, want none", n)
		}
	})
	t.Run("a former identity that is no longer configured", func(t *testing.T) {
		m, pr, job := setup(t)
		m.e.setKV(m.ctx, KVPRFormerIdentities(pr.ID), `["gone-app"]`)
		m.e.dismissFormer(m.ctx, job, pr, "b2", pipeline.RoundResult{ReviewID: 501})
		if n := dismissed(m); n != 0 {
			t.Fatalf("dismissals = %d, want none", n)
		}
	})
}

// ---- 3. --as pins ----

// A PR pinned by `magnum review --as zhuravel` keeps that identity on its
// next rounds although the watch posts as talkable-app: no migration, no
// former identity.
func TestMigrationSkipsAPRPinnedByReviewAs(t *testing.T) {
	m := newMigHarness(t)
	migPosts(m.harness, "COMMENTED")
	pr := m.reviewedPR(2, "b1")

	id := m.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, As: "zhuravel"})
	m.tick()
	if r := m.request(id); r.State != store.RequestDone {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
	if v, _ := m.e.getKV(m.ctx, KVPRIdentityPinned(pr.ID)); v != "zhuravel" {
		t.Fatalf("identity_pinned = %q, want zhuravel", v)
	}
	in := m.lastInput(2)
	if in.PR.Identity != "zhuravel" || len(in.FormerLogins) != 0 {
		t.Fatalf("the --as round: identity %q former logins %q, want zhuravel and none", in.PR.Identity, in.FormerLogins)
	}

	// The next, automatic round keeps the pinned identity as well.
	got := migRereview(m.harness, 2, "b2")
	in = m.lastInput(2)
	if in.TargetSHA != "b2" || in.PR.Identity != "zhuravel" || got.Identity != "zhuravel" || len(in.FormerLogins) != 0 {
		t.Fatalf("round at %s: identity %q (PR %q), former logins %q, want zhuravel and none", in.TargetSHA, in.PR.Identity, got.Identity, in.FormerLogins)
	}
	if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != 0 {
		t.Fatalf("pr.identity_migrated events: %+v", evs)
	}
	if f := migFormer(m.harness, pr.ID); f != nil {
		t.Fatalf("former_identities = %q, want none", f)
	}
	if got := deref(got.LastReviewLogin); got != "zhuravel" {
		t.Fatalf("last_review_login = %q, want zhuravel", got)
	}
}

// ---- 4. paused rounds ----

// A paused PR continues its judge's turn as the identity it started with: the
// dispatch of the continue round does not migrate it. Its following full
// round does.
func TestMigrationWaitsForAPausedRoundToContinue(t *testing.T) {
	m := newMigHarness(t)
	var pause atomic.Bool
	m.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n := len(m.rd.all())
		if pause.CompareAndSwap(true, false) {
			run := m.judgeRun(in.PR, in.Kind, in.TargetSHA, store.RunFailed, pipeline.OutcomeUsageLimit, "judge pane: usage limit")
			return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 1, JudgeRunID: run.ID,
				Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Until: m.clock.Now().Add(time.Hour)}}, nil
		}
		res, err := m.rd.posted(m.ctx, in, n)
		res.Event, res.ReviewID = "COMMENTED", int64(499+n)
		return res, err
	}
	pr := m.reviewedPR(2, "b1")

	// The rereview of b2 pauses (a usage limit) while the watch still posts as A.
	pause.Store(true)
	pollPR(m.harness, time.Minute, 2, "b2")
	for range 4 {
		if m.pr(2).State == store.PRPaused {
			break
		}
		pollPR(m.harness, 40*time.Minute, 2, "b2")
	}
	m.wantState(2, store.PRPaused)

	// The configuration moves the watch to B; the paused round continues.
	m.cfg.Watches[0].Identity = "zhuravel-app"
	m.advance(time.Hour + time.Minute)
	m.tick()
	got := m.wantState(2, store.PRReviewed)
	in := m.lastInput(2)
	if in.Kind != pipeline.KindContinue {
		t.Fatalf("round after the pause = %s, want a continue", in.Kind)
	}
	if in.PR.Identity != "talkable-app" || got.Identity != "talkable-app" || len(in.FormerLogins) != 0 {
		t.Fatalf("continue: identity %q (PR %q) former logins %q, want talkable-app and none", in.PR.Identity, got.Identity, in.FormerLogins)
	}
	if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != 0 {
		t.Fatalf("migrated while paused: %+v", evs)
	}
	if f := migFormer(m.harness, pr.ID); f != nil {
		t.Fatalf("former_identities after the continue = %q, want none", f)
	}
	if deref(got.LastReviewLogin) != "talkable[bot]" || deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("continue posted as %q at %q, want talkable[bot] at b2", deref(got.LastReviewLogin), deref(got.ReviewedSHA))
	}
	continued := deref(got.LastReviewID)

	// The next full round migrates.
	got = migRereview(m.harness, 2, "b3")
	in = m.lastInput(2)
	if in.Kind != pipeline.KindRecovery || in.PR.Identity != "zhuravel-app" || got.Identity != "zhuravel-app" {
		t.Fatalf("full round: %s as %q (PR %q), want a recovery (fresh sessions) as zhuravel-app", in.Kind, in.PR.Identity, got.Identity)
	}
	wantStrings(t, "FormerLogins", in.FormerLogins, []string{"talkable[bot]"})
	wantStrings(t, "former_identities", migFormer(m.harness, pr.ID), []string{"talkable-app"})
	if in.Previous == nil || in.Previous.ID != continued || !github.SameLogin(in.Previous.Login, "talkable") {
		t.Fatalf("Previous = %+v, want the continue's review %d by talkable", in.Previous, continued)
	}
	if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != 1 {
		t.Fatalf("pr.identity_migrated events: %d, want 1", len(evs))
	}
}

// ---- 5. the former identities list ----

// migrateIdentity keeps the PR's former identities newest last, without
// duplicates and never the current identity, through A -> B -> C -> A -> B.
func TestMigrationFormerIdentitiesHistory(t *testing.T) {
	m := newMigHarness(t, migWithApp("bob-app", "bob[bot]"))
	pr := m.reviewedByA()

	steps := []struct {
		to      string
		want    []string
		wantNow string // the identity the PR row shows afterwards
	}{
		{"zhuravel-app", []string{"talkable-app"}, "zhuravel-app"},
		{"bob-app", []string{"talkable-app", "zhuravel-app"}, "bob-app"},
		{"talkable-app", []string{"zhuravel-app", "bob-app"}, "talkable-app"},
		{"zhuravel-app", []string{"bob-app", "talkable-app"}, "zhuravel-app"},
	}
	for i, st := range steps {
		m.cfg.Watches[0].Identity = st.to
		got := migDispatch(m.harness, 2)
		if got.Identity != st.wantNow {
			t.Fatalf("step %d: identity = %q, want %q", i+1, got.Identity, st.wantNow)
		}
		former := migFormer(m.harness, pr.ID)
		wantStrings(t, fmt.Sprintf("step %d former_identities", i+1), former, st.want)
		if slices.Contains(former, got.Identity) {
			t.Fatalf("step %d: the current identity %q is listed as a former one: %q", i+1, got.Identity, former)
		}
		if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != i+1 {
			t.Fatalf("step %d: pr.identity_migrated events = %d, want %d", i+1, len(evs), i+1)
		}
		// The dispatcher meets the PR again: the identity is the watch's, nothing moves.
		migDispatch(m.harness, 2)
		wantStrings(t, fmt.Sprintf("step %d former_identities after a second dispatch", i+1), migFormer(m.harness, pr.ID), st.want)
		if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != i+1 {
			t.Fatalf("step %d: a second dispatch recorded another migration (%d events)", i+1, len(evs))
		}
	}

	// The former logins follow the list, minus the current login.
	cur := m.pr(2)
	wantStrings(t, "formerLogins", m.e.formerLogins(m.ctx, cur), []string{"bob[bot]", "talkable[bot]"})

	// Identities that are no longer configured are left out.
	m.cfg.Identities = slices.DeleteFunc(m.cfg.Identities, func(id config.Identity) bool { return id.Name == "bob-app" })
	wantStrings(t, "formerLogins after bob-app was removed", m.e.formerLogins(m.ctx, cur), []string{"talkable[bot]"})
}

func TestFormerLoginsReadsTheList(t *testing.T) {
	m := newMigHarness(t)
	pr := m.reviewedByA()
	key := KVPRFormerIdentities(pr.ID)

	for _, tc := range []struct {
		name    string
		current string // the PR's identity ("" = zhuravel-app)
		kv      string // "" = no value
		want    []string
	}{
		{"no list", "", "", nil},
		{"the list is not JSON", "", "talkable-app", nil},
		{"one identity", "", `["talkable-app"]`, []string{"talkable[bot]"}},
		{"a duplicate name", "", `["talkable-app","talkable-app"]`, []string{"talkable[bot]"}},
		{"the current identity", "", `["zhuravel-app","talkable-app"]`, []string{"talkable[bot]"}},
		{"an identity that is not configured", "", `["gone-app","talkable-app"]`, []string{"talkable[bot]"}},
		{"a gh identity", "talkable-app", `["zhuravel","talkable-app"]`, []string{"zhuravel"}},
		{"only the current identity", "", `["zhuravel-app"]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr.Identity = cmp.Or(tc.current, "zhuravel-app")
			if tc.kv == "" {
				m.e.delKV(m.ctx, key)
			} else {
				m.e.setKV(m.ctx, key, tc.kv)
			}
			got := m.e.formerLogins(m.ctx, pr)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("formerLogins = %q, want %q", got, tc.want)
			}
		})
	}
}

// migrateIdentity leaves the PR alone when the watch has no (known) identity,
// when the PR already has the watch's, when `--as` pinned it, when it is
// paused, and when its row changed since it was read.
func TestMigrateIdentityLeavesThePRAlone(t *testing.T) {
	cases := []struct {
		name  string
		watch string
		setup func(m *migHarness, pr store.PR) store.PR // returns the PR copy handed to migrateIdentity
	}{
		{"the PR already has the watch's identity", "talkable-app", nil},
		{"the watch names no identity", "", nil},
		{"the watch's identity is not configured", "ghost-app", nil},
		{"pinned by --as", "zhuravel-app", func(m *migHarness, pr store.PR) store.PR {
			m.e.setKV(m.ctx, KVPRIdentityPinned(pr.ID), "talkable-app")
			return pr
		}},
		{"paused", "zhuravel-app", func(m *migHarness, pr store.PR) store.PR {
			if err := m.st.TransitionPR(m.ctx, pr.ID, nil, store.PRPaused, nil); err != nil {
				m.t.Fatal(err)
			}
			return m.pr(2)
		}},
		{"the row changed since it was read", "zhuravel-app", func(m *migHarness, pr store.PR) store.PR {
			pr.Identity = "bob-app" // the copy is stale: the row says talkable-app
			return pr
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMigHarness(t, migWithApp("bob-app", "bob[bot]"))
			pr := m.reviewedByA()
			m.cfg.Watches[0].Identity = tc.watch
			if tc.setup != nil {
				pr = tc.setup(m, pr)
			}
			repo, err := m.st.RepoByFullName(m.ctx, "talkable/talkable")
			if err != nil {
				t.Fatal(err)
			}
			m.e.migrateIdentity(m.ctx, &pr, repo, m.cfg.WatchFor("talkable/talkable"))

			if got := m.pr(2); got.Identity != "talkable-app" {
				t.Fatalf("identity = %q, want talkable-app untouched", got.Identity)
			}
			if f := migFormer(m.harness, pr.ID); f != nil {
				t.Fatalf("former_identities = %q, want none", f)
			}
			if evs := approvalEvents(t, m.harness, 2, "pr.identity_migrated"); len(evs) != 0 {
				t.Fatalf("pr.identity_migrated events: %+v", evs)
			}
		})
	}

	t.Run("no watch", func(t *testing.T) {
		m := newMigHarness(t)
		pr := m.reviewedByA()
		repo, _ := m.st.RepoByFullName(m.ctx, "talkable/talkable")
		m.e.migrateIdentity(m.ctx, &pr, repo, nil)
		if got := m.pr(2); got.Identity != "talkable-app" || migFormer(m.harness, pr.ID) != nil {
			t.Fatalf("identity = %q, former %q", got.Identity, migFormer(m.harness, pr.ID))
		}
	})
}

// ---- 6. previousReview with former logins ----

// A review by a former login is the PR's own previous review (its id and
// event kept, Login naming who posted it); without the former login the same
// review is another identity's (the --as contrast of
// TestPreviousReviewOnlyFromTheCurrentLogin).
func TestPreviousReviewCountsAFormerLoginsReview(t *testing.T) {
	m := newMigHarness(t)
	pr := m.reviewedByA()
	migRun(m.harness, pr, "run-a0", 0, "talkable-app", "talkable[bot]", 400, "CHANGES_REQUESTED")
	cur := m.pr(2)
	const nowLogin = "zhuravel[bot]"
	former := []string{"talkable[bot]"}

	t.Run("the latest review is a former login's", func(t *testing.T) {
		prev := m.e.previousReview(m.ctx, cur, nowLogin, former)
		if prev.ID != 500 || prev.Event != "COMMENTED" || prev.SHA != "b1" || prev.Login != "talkable[bot]" || !prev.Former {
			t.Fatalf("previous = %+v, want review 500 COMMENTED at b1 by talkable[bot], marked former", prev)
		}
	})
	t.Run("a former login matches its account, not a user of the same name", func(t *testing.T) {
		for _, f := range []string{"talkable[bot]", "Talkable[bot]"} {
			prev := m.e.previousReview(m.ctx, cur, nowLogin, []string{f})
			if prev.ID != 500 || prev.Event != "COMMENTED" {
				t.Errorf("former %q: previous = %+v, want review 500 COMMENTED", f, prev)
			}
		}
		// "talkable" is a user; the App talkable[bot] posted review 500.
		if prev := m.e.previousReview(m.ctx, cur, nowLogin, []string{"talkable"}); prev.ID != 0 {
			t.Errorf("former user talkable: previous = %+v, want none", prev)
		}
	})
	t.Run("without former logins it is another identity's review", func(t *testing.T) {
		prev := m.e.previousReview(m.ctx, cur, nowLogin, nil)
		if prev.ID != 0 || prev.Event != "" || prev.Login != "" || prev.SHA != "b1" {
			t.Fatalf("previous = %+v, want no review id or event, Login empty, SHA b1", prev)
		}
	})
	t.Run("the current login's own review", func(t *testing.T) {
		prev := m.e.previousReview(m.ctx, cur, "talkable[bot]", nil)
		if prev.ID != 500 || prev.Event != "COMMENTED" || prev.Login != "talkable[bot]" || prev.Former {
			t.Fatalf("previous = %+v", prev)
		}
	})
	// A user "zhuravel" whose watch moved to the App "zhuravel[bot]": the
	// user's review is a former login's, never the App's to dismiss.
	t.Run("a former user named like the current App", func(t *testing.T) {
		m := newMigHarness(t)
		pr := m.reviewedByA()
		migRun(m.harness, pr, "run-user", 4, "zhuravel", "zhuravel", 800, "CHANGES_REQUESTED")
		user := m.pr(2)
		user.LastReviewID, user.LastReviewEvent, user.LastReviewLogin = store.Ptr(int64(800)), store.Ptr("CHANGES_REQUESTED"), store.Ptr("zhuravel")
		prev := m.e.previousReview(m.ctx, user, "zhuravel[bot]", []string{"zhuravel"})
		if prev.ID != 800 || prev.Login != "zhuravel" || !prev.Former {
			t.Fatalf("previous = %+v, want the user's review 800 marked former", prev)
		}
		// Without former logins the user's review is not the App's own.
		if prev := m.e.previousReview(m.ctx, user, "zhuravel[bot]", nil); prev.Former || prev.ID == 800 {
			t.Fatalf("without former logins: %+v, want neither former nor review 800", prev)
		}
	})
	t.Run("the latest review is another login's: the newest review of the own or former logins stands in", func(t *testing.T) {
		migRun(m.harness, pr, "run-bob", 2, "zhuravel", "bob", 700, "CHANGES_REQUESTED")
		migRun(m.harness, pr, "run-b", 3, "zhuravel-app", "zhuravel[bot]", 600, "APPROVED")
		last := cur
		last.LastReviewID, last.LastReviewEvent, last.LastReviewLogin = store.Ptr(int64(700)), store.Ptr("CHANGES_REQUESTED"), store.Ptr("bob")

		prev := m.e.previousReview(m.ctx, last, nowLogin, former)
		if prev.ID != 600 || prev.Event != "APPROVED" || prev.Login != "zhuravel[bot]" {
			t.Fatalf("newest of B and A: previous = %+v, want B's 600", prev)
		}
		prev = m.e.previousReview(m.ctx, last, "talkable[bot]", former)
		if prev.ID != 500 || prev.Event != "COMMENTED" || prev.Login != "talkable[bot]" {
			t.Fatalf("A with its own history: previous = %+v, want A's 500", prev)
		}
		// Only a former login's review is older than the other login's: it stands in.
		prev = m.e.previousReview(m.ctx, last, "carol[bot]", former)
		if prev.ID != 500 || prev.Login != "talkable[bot]" {
			t.Fatalf("a login with only a former login's history: previous = %+v, want 500", prev)
		}
		prev = m.e.previousReview(m.ctx, last, "carol[bot]", nil)
		if prev.ID != 0 || prev.Login != "" {
			t.Fatalf("a login without history: previous = %+v, want none", prev)
		}
	})
	t.Run("no run on record: the PR row's reviewer names it", func(t *testing.T) {
		alone := cur
		alone.LastReviewID = store.Ptr(int64(999))
		prev := m.e.previousReview(m.ctx, alone, nowLogin, former)
		if prev.ID != 999 || prev.Login != "talkable[bot]" {
			t.Fatalf("previous = %+v, want review 999 by talkable[bot] (last_review_login)", prev)
		}
		alone.LastReviewLogin = nil
		if prev := m.e.previousReview(m.ctx, alone, nowLogin, former); prev.ID != 999 || prev.Login != "" {
			t.Fatalf("previous = %+v, want review 999 with Login unknown", prev)
		}
	})
}
