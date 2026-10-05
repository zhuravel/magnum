package cli

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// onScreen makes the inspect commands see terminals.
func onScreen(t *testing.T) {
	t.Helper()
	old := inspScreen
	inspScreen = func(*Context) bool { return true }
	t.Cleanup(func() { inspScreen = old })
}

func TestStatusDashData(t *testing.T) {
	f, _, d, now := statusFixture(t)
	scanner := d.Inventory.(*fakeScanner)
	scanner.inv.External = []inventory.ExternalView{{Path: "/w/talkable.repo3", Branch: "feature/x", PRNumber: 42, GHState: "OPEN"}}
	scanner.inv.OrphanDBs = make([]mysqlx.Database, 1)
	r, err := statusGather(context.Background(), d, statusOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	got := statusDashData(r, "talkable/talkable")

	if got.GeneratedAt != now || got.Daemon != (tui.DaemonInfo{Running: true, PID: 4242, Uptime: "3h", Launchd: "running"}) {
		t.Errorf("daemon %+v at %v", got.Daemon, got.GeneratedAt)
	}
	if got.Activity != (tui.ActivityInfo{LastPoll: 40 * time.Second}) {
		t.Errorf("activity %+v (never ticked: 0)", got.Activity)
	}
	if got.GitHub != (tui.GitHubInfo{Remaining: 4890, Limit: 5000}) {
		t.Errorf("github %+v", got.GitHub)
	}
	if got.Rounds.Active != 1 || got.Rounds.Max != 3 || !slices.Equal(got.Rounds.ActivePRs, []string{"talkable#11940"}) {
		t.Errorf("rounds %+v", got.Rounds)
	}
	if !reflect.DeepEqual(got.Agents, tui.AgentsInfo{CodexWorking: 1, CodexMax: 5, ClaudeWorking: 1}) {
		t.Errorf("agents %+v", got.Agents)
	}
	if got.Disk != (tui.DiskInfo{FreeGB: 20, MinGB: 1}) {
		t.Errorf("disk %+v", got.Disk)
	}

	pauses := map[string]tui.Pause{}
	for _, p := range got.Pauses {
		pauses[p.Key] = p
	}
	if p := pauses["codex"]; !strings.HasPrefix(p.Reason, "usage_limit until ") || !strings.HasSuffix(p.Reason, "(in 1h) — You've hit your usage limit") ||
		!strings.Contains(p.Fix, "magnum resume --tool codex") {
		t.Errorf("codex pause %+v", p)
	}
	if p := pauses["daemon"]; !strings.HasPrefix(p.Reason, "lunch since ") || !strings.HasSuffix(p.Reason, "· 6 requests held") ||
		p.Fix != "magnum resume" {
		t.Errorf("daemon pause %+v", p)
	}

	// A slot whose PR `magnum ignore` muted says "ignored", as the board does,
	// so the dashboard's U asks to stop ignoring it.
	ignored := store.PR{Number: 7, State: store.PRIneligible, Muted: true, SkipReason: store.Ptr(engine.SkipIgnored), GHState: store.GHOpen}
	if d := statusDashData(statusReport{Slots: []inventory.SlotView{{Slot: store.Slot{Name: "review2", RepoFullName: "talkable/talkable"}, PR: &ignored}}},
		"talkable/talkable"); len(d.Slots) != 1 || d.Slots[0].PRState != "ignored" {
		t.Errorf("an ignored PR's slot row: %+v", d.Slots)
	}

	wantSlot := tui.SlotRow{Name: "review1", Folder: inspTilde(f.Home + "/talkable.review1"), PRRef: "talkable#11920", PRState: store.PRReviewed,
		SlotState: "held [foreign_agent]", DBs: "1/2 300M", Disk: "1.0G", URL: "https://github.com/talkable/talkable/pull/11920", PRGHState: store.GHOpen}
	if len(got.Slots) != 1 || got.Slots[0] != wantSlot {
		t.Errorf("slots %+v\nwant %+v", got.Slots, wantSlot)
	}

	var refs []string
	for _, q := range got.Queue {
		refs = append(refs, q.Ref+" "+q.State)
	}
	if !slices.Equal(refs, []string{"talkable#11931 queued", "talkable#11930 queued", "zhuravel/app#3 closed"}) {
		t.Errorf("queue (queued, then closing) %v", refs)
	}
	if q := got.Queue[0]; q.Author != "@dev" || q.URL != "https://github.com/talkable/talkable/pull/11931" || q.Title != "PR xxx" ||
		!strings.Contains(q.Next, "forced") {
		t.Errorf("queue row %+v", q)
	}
	if q := got.Queue[2]; q.Next != "release in 20m" {
		t.Errorf("closing row %+v", q)
	}

	var subjects []string
	for _, a := range got.Attention {
		subjects = append(subjects, a.Subject)
	}
	for _, want := range []string{"talkable#11950", "slot:review1", "databases"} {
		if !slices.Contains(subjects, want) {
			t.Errorf("attention lacks %s: %v", want, subjects)
		}
	}
	if !slices.Contains(got.Warnings, "mysql: connection refused") {
		t.Errorf("warnings %v", got.Warnings)
	}
	wantManual := tui.ManualRow{Folder: "/w/talkable.repo3", Branch: "feature/x", PRRef: "#42?", GitHub: "OPEN", DBs: "-", Agents: "-", Disk: "-"}
	if len(got.Manual) != 1 || got.Manual[0] != wantManual {
		t.Errorf("manual %+v", got.Manual)
	}
}

func TestStatusDashSourceNeverAsksGitHub(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	scanner := d.Inventory.(*fakeScanner)
	if _, err := statusDashSource(d, statusOptions{All: true}).Gather(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !scanner.opts.External || scanner.opts.GitHubStates {
		t.Fatalf("scan options %+v: manual worktrees without GitHub states", scanner.opts)
	}
}

func TestStatusWatchOpensTheDashboard(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 11930, store.PRQueued, nil)
	st.Close()
	onScreen(t)
	var opts tui.DashboardOptions
	var data tui.StatusData
	var acts tui.DashboardActions
	old := tuiDashboard
	tuiDashboard = func(ctx context.Context, src tui.DashboardSource, act tui.DashboardActions, o tui.DashboardOptions) error {
		opts, acts = o, act
		var err error
		data, err = src.Gather(ctx)
		return err
	}
	t.Cleanup(func() { tuiDashboard = old })

	if code := f.run("status", "--watch", "--all"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if f.Out.Len() != 0 {
		t.Errorf("printed while the screen ran:\n%s", f.Out.String())
	}
	if opts.Refresh != 2*time.Second || !opts.ShowManual || acts == nil {
		t.Errorf("options %+v actions %v", opts, acts)
	}
	if len(data.Queue) != 1 || data.Queue[0].Ref != "talkable#11930" || data.Queue[0].URL != "https://github.com/talkable/talkable/pull/11930" {
		t.Errorf("queue %+v", data.Queue)
	}
	if len(data.Queue) == 1 && (data.Queue[0].Review == nil || data.Queue[0].Review.HeadSHA == "") {
		t.Errorf("queue row lacks the y/N question's facts: %+v", data.Queue[0].Review)
	}
	if code := f.run("status", "--watch", "--json"); code != 2 {
		t.Fatalf("--watch --json: code %d", code)
	}
}

func TestStatusDashActionsCaptureOutput(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true // the dashboard's terminal
	h.held = true                            // a daemon holds the lock
	h.run.Rules = []execx.Rule{{Prefix: []string{"open"}}}
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()

	text, err := acts.Pin(ctx, "talkable#5")
	if err != nil || !strings.Contains(text, "pin talkable#5 is queued as request 1") {
		t.Fatalf("pin: %q %v", text, err)
	}
	if _, err := acts.Review(ctx, "talkable#5", tui.ReviewOpts{}); err == nil || !strings.Contains(err.Error(), "nothing was queued: no daemon is running") {
		t.Fatalf("review without a daemon to answer: %v", err)
	}
	h.pid = 4242 // a review is queued only while a daemon answers
	if _, err := acts.Review(ctx, "talkable#5", tui.ReviewOpts{Simplify: true}); err != nil {
		t.Fatalf("review: %v", err)
	}
	h.pid = 0
	text, err = acts.Release(ctx, "talkable#5") // no question: the dashboard asked
	if err != nil || !strings.Contains(lastLine(text), "release of talkable#5 queued as request 3") {
		t.Fatalf("release: %q %v", text, err)
	}
	for _, fn := range []func(context.Context, string) (string, error){acts.Unpin, acts.Mute, acts.Unmute} {
		if _, err := fn(ctx, "talkable#5"); err != nil {
			t.Fatal(err)
		}
	}
	var kinds []string
	for _, r := range h.requests() {
		kinds = append(kinds, r.Kind)
	}
	if !slices.Equal(kinds, []string{engine.ReqPin, engine.ReqReview, engine.ReqRelease, engine.ReqUnpin, engine.ReqMute, engine.ReqUnmute}) {
		t.Fatalf("requests %v", kinds)
	}
	if p := actDecode[engine.ReviewPayload](t, h.requests()[1].Payload); p.Again || !p.Simplify || p.Fresh {
		t.Errorf("review payload %+v", p)
	}

	if text, err := acts.Attention(ctx); err != nil || text != "nothing needs you" {
		t.Errorf("attention: %q %v (nothing needing you is news, not a failure)", text, err)
	}
	if _, err := acts.Open(ctx, "talkable#5"); err == nil || strings.HasPrefix(err.Error(), "magnum open") {
		t.Errorf("open without a session: %v (want the reason without the command prefix)", err)
	}
	if _, err := acts.Review(ctx, "nope!", tui.ReviewOpts{}); err == nil || !strings.Contains(err.Error(), "nope!") {
		t.Errorf("bad ref: %v", err)
	}
	if err := acts.OpenBrowser(ctx, "https://github.com/talkable/talkable/pull/5"); err != nil {
		t.Fatal(err)
	}
	if opened := h.run.CallsWithPrefix("open"); len(opened) != 1 || opened[0].Args[0] != "https://github.com/talkable/talkable/pull/5" {
		t.Errorf("browser calls %+v", opened)
	}
	if h.out.Len() != 0 || h.errb.Len() != 0 {
		t.Errorf("actions printed to the terminal:\nstdout %s\nstderr %s", h.out.String(), h.errb.String())
	}
	if h.d.StdinTTY || h.d.StdoutTTY {
		t.Error("actions may not prompt on the dashboard's terminal")
	}
}

// reviewFactsOf trusts the poller's commit count only for the current head
// and the current reviewed head.
func TestReviewFactsOf(t *testing.T) {
	at := time.Date(2026, 10, 3, 11, 40, 0, 0, time.UTC)
	base := store.PR{HeadSHA: "head2", ReviewedSHA: store.Ptr("head1"), ReviewedAt: &at, LastReviewLogin: store.Ptr("zhuravel")}
	since := func(src, b, head string, n int) *store.SinceReview {
		return &store.SinceReview{Source: src, Base: b, Head: head, Commits: n, Files: 4, Additions: 10}
	}
	cases := []struct {
		name     string
		since    *store.SinceReview
		reviewed *string
		want     int // -1: no count
		wantRev  string
	}{
		{"current", since(store.SinceFromReviewed, "head1", "head2", 3), base.ReviewedSHA, 3, "head1"},
		{"older head", since(store.SinceFromReviewed, "head1", "head0", 3), base.ReviewedSHA, -1, "head1"},
		{"older review", since(store.SinceFromReviewed, "head0", "head2", 3), base.ReviewedSHA, -1, "head1"},
		{"whole PR", since(store.SinceFromBase, "main", "head2", 9), nil, -1, ""},
		{"GitHub review", since(store.SinceFromReview, "ghrev", "head2", 1), nil, 1, "ghrev"},
		{"failed", &store.SinceReview{Source: store.SinceFromReviewed, Base: "head1", Head: "head2", Error: "gone"}, base.ReviewedSHA, -1, "head1"},
		{"none", nil, base.ReviewedSHA, -1, "head1"},
	}
	for _, c := range cases {
		pr := base
		pr.SinceReview, pr.ReviewedSHA = c.since, c.reviewed
		f := reviewFactsOf(pr)
		if f.HeadSHA != "head2" || f.ReviewedSHA != c.wantRev || f.ReviewedBy != "zhuravel" || !f.ReviewedAt.Equal(at) {
			t.Errorf("%s: facts %+v", c.name, f)
		}
		switch {
		case c.want < 0 && f.SinceReview != nil:
			t.Errorf("%s: kept a count %+v", c.name, f.SinceReview)
		case c.want >= 0 && (f.SinceReview == nil || f.SinceReview.Commits != c.want || f.SinceReview.Base != "reviewed"):
			t.Errorf("%s: count %+v, want %d", c.name, f.SinceReview, c.want)
		}
	}
}
