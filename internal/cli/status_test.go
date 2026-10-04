package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/store"
)

type fakeScanner struct {
	inv  inventory.Inventory
	opts inventory.Options
	err  error
}

func (f *fakeScanner) Scan(_ context.Context, o inventory.Options) (inventory.Inventory, error) {
	f.opts = o
	return f.inv, f.err
}

type fakeSnap struct {
	snap herdr.Snapshot
	err  error
}

func (f fakeSnap) Snapshot(context.Context) (herdr.Snapshot, error) { return f.snap, f.err }

// statusFixture seeds a registry with one PR in each interesting state.
func statusFixture(t *testing.T) (*inspFixture, *store.Store, statusDeps, time.Time) {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	later := now.Add(20 * time.Minute)
	_, held := inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", "abcdef0123456789abcdef0123456789abcdef01")
		u.Set("last_review_event", "COMMENTED")
	})
	inspSeedPR(t, st, "talkable/talkable", 11930, store.PRQueued, func(u *store.PRUpdate) { u.Set("next_eligible_at", later) })
	inspSeedPR(t, st, "talkable/talkable", 11931, store.PRQueued, func(u *store.PRUpdate) { u.Set("forced", true) })
	inspSeedPR(t, st, "talkable/talkable", 11940, store.PRReviewing, nil)
	inspSeedPR(t, st, "talkable/talkable", 11950, store.PRNeedsAttention, func(u *store.PRUpdate) { u.Set("last_error", "no review after nudge") })
	inspSeedPR(t, st, "zhuravel/app", 3, store.PRClosed, func(u *store.PRUpdate) { u.Set("release_after", later) })

	for k, v := range map[string]string{
		"daemon.last_poll":                     store.FormatTime(now.Add(-40 * time.Second)),
		"daemon.started_at":                    store.FormatTime(now.Add(-3 * time.Hour)),
		"gh.remaining":                         "4890",
		"gh.limit":                             "5000",
		engine.KVToolPausedUntil("codex"):      store.FormatTime(now.Add(time.Hour)),
		engine.KVToolPausedReason("codex"):     "usage_limit",
		"codex.paused_detail":                  "You've hit your usage limit",
		engine.KVDaemonPaused:                  "1",
		engine.KVDaemonPausedReason:            "lunch",
		engine.KVWatchPaused("talkable"):       "review 77 posted by zhuravel",
		engine.KVIdentityCheck("talkable-app"): "fail",
		engine.KVIdentityError("talkable-app"): "pull_requests permission is read",
	} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	slot, err := st.CreateSlot(ctx, store.Slot{Name: "review1", Kind: store.SlotKindPool, Path: f.Home + "/talkable.review1",
		MainClone: f.Home + "/talkable", RepoFullName: "talkable/talkable", State: store.SlotHeld, PRID: &held.ID,
		CheckedOutSHA: store.Ptr("abcdef0123456789abcdef0123456789abcdef01")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(ctx, store.Session{PRID: held.ID, Role: store.RoleJudge, State: store.SessionLive,
		AgentName: store.Ptr("mg-talkable-11920-judge"), AgentKind: store.Ptr("codex"), SessionID: store.Ptr("01a0fe66-uuid"),
		HerdrPaneID: store.Ptr("p7"), Cwd: store.Ptr(slot.Path)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(ctx, store.Session{PRID: held.ID, Role: store.RoleClaude, State: store.SessionParked,
		AgentKind: store.Ptr("claude"), SessionID: store.Ptr("c-123")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, store.Run{PRID: held.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: "abcdef0123456789", State: store.RunVerified, ReviewEvent: store.Ptr("COMMENTED"),
		ReviewURL: store.Ptr("https://github.com/talkable/talkable/pull/11920#pullrequestreview-1"), Identity: "talkable-app",
		ReviewerLogin: "talkable[bot]", PromptText: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, store.Run{PRID: held.ID, Round: 1, Role: store.RoleClaude, Kind: store.RunInitial,
		TargetSHA: "abcdef0123456789", State: store.RunVerified, Outcome: store.Ptr("ok"), Identity: "talkable-app",
		ReviewerLogin: "talkable[bot]", PromptText: "x"}); err != nil {
		t.Fatal(err)
	}
	size := int64(1 << 20)
	scanner := &fakeScanner{inv: inventory.Inventory{
		DatabasesListed: true,
		Slots: []inventory.SlotView{{Slot: slot, Exists: true, Worktree: true, PR: &held, SizeKB: &size, Drift: []string{"foreign_agent"},
			Databases: []inventory.DBView{
				{Name: "talkable_development__review1", SizeMB: 300, Present: true, Expected: true},
				{Name: "talkable_test__review1", Expected: true},
			}}},
		OrphanDBs: nil,
		Drift: []inventory.Finding{{Kind: inventory.KindForeignAgent, Subject: "slot:review1", Message: "codex working in review1"},
			{Kind: inventory.KindLostSlot, Subject: "slot:review9", Message: "missing", Safe: true}},
		Warnings: []string{"mysql: connection refused"},
	}}
	snap := herdr.Snapshot{
		Agents: []herdr.AgentInfo{{Agent: "codex", AgentStatus: herdr.StatusWorking}, {Agent: "codex", AgentStatus: herdr.StatusIdle},
			{Agent: "claude", AgentStatus: herdr.StatusWorking}},
		Panes: []herdr.Pane{{ID: "p7", AgentStatus: herdr.StatusIdle}},
	}
	f.Ctx.LoadConfig()
	d := statusDeps{
		Store: st, Config: f.Ctx.Config, Layout: f.Ctx.Layout, Inventory: scanner, Herdr: fakeSnap{snap: snap},
		Launchd: func(context.Context) (launchd.Info, error) {
			return launchd.Info{State: launchd.Running, PID: 4242}, nil
		},
		DaemonPID: func() (int, error) { return 4242, nil },
		DiskFree:  func(string) (uint64, error) { return 20 << 30, nil }, DiskPath: "/Users/x",
		Now: func() time.Time { return now },
	}
	return f, st, d, now
}

func TestStatusGather(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	r, err := statusGather(context.Background(), d, statusOptions{All: true, Sizes: true})
	if err != nil {
		t.Fatal(err)
	}
	sc := d.Inventory.(*fakeScanner)
	if !sc.opts.External || !sc.opts.Sizes || !sc.opts.GitHubStates {
		t.Fatalf("scan options = %+v", sc.opts)
	}
	if !r.Daemon.Running || r.Daemon.PID != 4242 || r.Daemon.Launchd != "running" || r.Daemon.LastPoll == nil {
		t.Fatalf("daemon = %+v", r.Daemon)
	}
	if r.GitHub.Remaining == nil || *r.GitHub.Remaining != 4890 || *r.GitHub.Limit != 5000 {
		t.Fatalf("github = %+v", r.GitHub)
	}
	if r.Rounds.Active != 1 || r.Rounds.Max != 3 || r.Rounds.PRs[0] != "talkable#11940" {
		t.Fatalf("rounds = %+v", r.Rounds)
	}
	if r.Agents == nil || r.Agents.WorkingCodex != 1 || r.Agents.WorkingClaude != 1 || r.Agents.MaxCodex != 5 {
		t.Fatalf("agents = %+v", r.Agents)
	}
	scopes := map[string]statusPause{}
	for _, p := range r.Pauses {
		scopes[p.Scope] = p
	}
	for _, s := range []string{"daemon", "codex", "watch:talkable", "identity:talkable-app"} {
		if _, ok := scopes[s]; !ok {
			t.Errorf("missing pause %s in %+v", s, r.Pauses)
		}
	}
	if scopes["daemon"].Reason != "lunch" || scopes["codex"].Reason != "usage_limit" ||
		scopes["identity:talkable-app"].Detail != "pull_requests permission is read" {
		t.Fatalf("pauses = %+v", r.Pauses)
	}
	if len(r.Queue) != 2 || r.Queue[0].Number != 11931 || !r.Queue[0].Forced {
		t.Fatalf("queue (forced first) = %+v", r.Queue)
	}
	if !strings.Contains(r.Queue[1].Next, "eligible in 20m") {
		t.Fatalf("queue next = %q", r.Queue[1].Next)
	}
	if len(r.Closing) != 1 || r.Closing[0].Repo != "zhuravel/app" || !strings.Contains(r.Closing[0].Next, "release in 20m") {
		t.Fatalf("closing = %+v", r.Closing)
	}
	var att []string
	for _, a := range r.Attention {
		att = append(att, a.Subject+"="+a.Message)
	}
	joined := strings.Join(att, "\n")
	if !strings.Contains(joined, "talkable#11950=no review after nudge") ||
		!strings.Contains(joined, "slot:review1=foreign_agent") || strings.Contains(joined, "review9") {
		t.Fatalf("attention = %s", joined)
	}
	if r.Disk.FreeBytes != 20<<30 || r.Disk.MinGB != 1 {
		t.Fatalf("disk = %+v", r.Disk)
	}
}

func TestStatusRenderAndDetail(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	r, err := statusGather(context.Background(), d, statusOptions{Ref: "11920"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Detail == nil || r.Detail.PR == nil || r.Detail.Slot == nil || len(r.Detail.Sessions) != 2 || len(r.Detail.Runs) != 2 {
		t.Fatalf("detail = %+v", r.Detail)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	for _, want := range []string{
		"daemon:   running (pid 4242, up 3h), launchd running",
		"last poll 40s ago",
		"github:   4890/5000 points left",
		"rounds:   1/3 active (talkable#11940); working agents: codex 1/5, claude 1",
		"daemon: lunch",
		"codex: usage_limit until",
		"fix: magnum identities check --name talkable-app",
		"SLOT", "FOLDER", "DATABASES", "review1",
		"#11920 reviewed",
		"held [foreign_agent]",
		"1/2 300M",
		"1.0G",
		"QUEUE (2)",
		"talkable#11931     queued            forced, waiting for a slot",
		"CLOSED, pending release (1)",
		"app#3",
		"ATTENTION",
		"warning: mysql: connection refused",
		"talkable/talkable#11920  PR xxx",
		"next:      watching for new pushes",
		"talkable_test__review1 (missing)",
		"judge: cd ",
		"&& codex resume 01a0fe66-uuid",
		"claude-review: claude --resume c-123",
		"round 1 initial   abcdef0 verified  COMMENTED",
		"reviewers: claude-review ok",
		"timings:   round 1 initial: claude-review 0s · codex-judge 0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q\n%s", want, out)
		}
	}
	// The live pane status wins over the stored one.
	if !strings.Contains(out, "codex-judge    live    idle") {
		t.Errorf("judge session line missing live status:\n%s", out)
	}
}

func TestStatusDetailSlotWithoutPR(t *testing.T) {
	f, st, d, _ := statusFixture(t)
	if _, err := st.CreateSlot(context.Background(), store.Slot{Name: "review2", Kind: store.SlotKindPool, Path: f.Home + "/talkable.review2",
		MainClone: f.Home + "/talkable", RepoFullName: "talkable/talkable", State: store.SlotFree}); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(context.Background(), d, statusOptions{Ref: "review2"})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if !strings.Contains(b.String(), "slot review2 (pool, free)") || !strings.Contains(b.String(), "no PR assigned") {
		t.Fatalf("slot card:\n%s", b.String())
	}
}

func TestStatusSourcesDegrade(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	d.Herdr = fakeSnap{err: errors.New("herdr: unavailable")}
	d.Launchd = func(context.Context) (launchd.Info, error) { return launchd.Info{}, errors.New("no launchctl") }
	d.DaemonPID = func() (int, error) { return 0, nil }
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	for _, want := range []string{"daemon:   not running, launchd unknown", "herdr unreachable", "warning: launchctl: no launchctl"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q:\n%s", want, b.String())
		}
	}
	d.Inventory = &fakeScanner{err: errors.New("store gone")}
	if _, err := statusGather(context.Background(), d, statusOptions{}); err == nil {
		t.Fatal("inventory failure must fail status")
	}
}

func TestStatusCommand(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 11930, store.PRQueued, nil)
	st.Close()
	if code := f.run("status", "--json"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	var r statusReport
	if err := json.Unmarshal(f.Out.Bytes(), &r); err != nil {
		t.Fatalf("json: %v\n%s", err, f.Out.String())
	}
	if len(r.Queue) != 1 || r.Queue[0].Number != 11930 || r.Daemon.Running {
		t.Fatalf("report = %+v", r)
	}
	if code := f.run("status"); code != 0 || !strings.Contains(f.Out.String(), "QUEUE (1)") {
		t.Fatalf("code %d out %s err %s", code, f.Out.String(), f.Err.String())
	}
	if code := f.run("status", "a", "b"); code != 2 {
		t.Fatalf("two refs: code %d", code)
	}
	if code := f.run("status", "--watch", "--json"); code != 2 {
		t.Fatalf("--watch --json: code %d", code)
	}
	if code := f.run("status", "nope!"); code != 1 || !strings.Contains(f.Err.String(), "neither a slot name nor a PR reference") {
		t.Fatalf("bad ref: code %d err %s", code, f.Err.String())
	}
}

// statusNoise is untrusted text with terminal control sequences in it: SGR,
// an OSC title change, a carriage return and a newline.
const statusNoise = "\x1b[31mred\x1b]0;pwned\a\x1b[0m\r\nnext"

func statusNoControls(t *testing.T, what, out string) {
	t.Helper()
	for _, r := range out {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("%s prints the control character %U:\n%q", what, r, out)
		}
	}
}

func TestStatusRenderCleansUntrustedText(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	pr := &store.PR{Number: 7, URL: "https://github.com/talkable/talkable/pull/7?" + statusNoise, State: store.PRReviewed, GHState: "open",
		Title: store.Ptr("title " + statusNoise), LastError: store.Ptr("boom " + statusNoise), AuthorLogin: store.Ptr("dev" + statusNoise),
		Identity: "talkable-app", HeadSHA: "abcdef0123456789"}
	slot := store.Slot{Name: "review1" + statusNoise, Kind: store.SlotKindPool, Path: "/x/talkable.review1" + statusNoise,
		State: store.SlotHeld, HoldReason: store.Ptr("hold " + statusNoise)}
	r := statusReport{
		GeneratedAt: now,
		Pauses: []statusPause{{Scope: "codex" + statusNoise, Reason: "usage " + statusNoise, Until: &until,
			Detail: "pane " + statusNoise, Fix: "wait " + statusNoise}},
		Slots: []inventory.SlotView{{Slot: slot, Exists: true, Drift: []string{"drift" + statusNoise},
			Databases: []inventory.DBView{{Name: "db" + statusNoise, Present: true, Expected: true}}}},
		External: []inventory.ExternalView{{Path: "/x/wt" + statusNoise, Branch: "feat" + statusNoise, GHState: "open" + statusNoise,
			Agents: []inventory.AgentView{{Agent: "codex" + statusNoise}}}},
		Queue: []statusPRLine{{Repo: "talkable/talkable", Number: 1, State: store.PRQueued, Title: "title " + statusNoise,
			Next: "skipped: " + statusNoise}},
		Closing:   []statusPRLine{{Repo: "talkable/talkable", Number: 2, State: store.PRClosed, Next: "next " + statusNoise}},
		Attention: []statusAttention{{Subject: "talkable#3" + statusNoise, Message: "needs_attention: " + statusNoise, Fix: "fix " + statusNoise}},
		Warnings:  []string{"mysql: " + statusNoise},
		Detail: &statusDetail{Repo: "talkable/talkable", PR: pr, Next: "needs you: " + statusNoise, Slot: &inventory.SlotView{Slot: slot},
			Sessions: []statusSession{{Session: store.Session{Role: store.RoleJudge, State: store.SessionLive,
				AgentName: store.Ptr("mg" + statusNoise), HerdrPaneID: store.Ptr("p" + statusNoise)},
				Live: "idle" + statusNoise, Resume: "cd '/x' && codex resume 'a" + statusNoise + "'"}},
			Runs: []store.Run{{Round: 1, Role: store.RoleJudge, Kind: store.RunInitial, TargetSHA: "abcdef0123456789", State: store.RunFailed,
				ReviewEvent: store.Ptr("COMMENTED" + statusNoise), ReviewURL: store.Ptr("https://x/" + statusNoise),
				Error: store.Ptr("err " + statusNoise), CreatedAt: now}}},
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	statusNoControls(t, "statusRender", out)
	// Control characters become spaces; the rest of the text stays readable.
	clean := actClean(statusNoise)
	// The stored error shows as its one-line explanation (attention.Explain).
	if !strings.Contains(out, "error:     boom red\n") {
		t.Errorf("error line is not the explanation:\n%s", out)
	}
	for _, prefix := range []string{"title ", "usage ", "pane ", "needs_attention: ", "mysql: ", "hold ", "skipped: "} {
		if !strings.Contains(out, prefix+clean) {
			t.Errorf("cleaned output lacks %q:\n%s", prefix+clean, out)
		}
	}
	// A detail card on its own (a slot without a PR too).
	var card bytes.Buffer
	statusRenderDetail(&card, *r.Detail, now)
	statusNoControls(t, "statusRenderDetail", card.String())
	card.Reset()
	statusRenderDetail(&card, statusDetail{Slot: &inventory.SlotView{Slot: slot}}, now)
	statusNoControls(t, "statusRenderDetail (slot)", card.String())
	statusNoControls(t, "statusPauseText", statusPauseText(r.Pauses[0], now))

	// --json keeps the raw values.
	var js bytes.Buffer
	if err := writeJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	var back statusReport
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Queue[0].Title != "title "+statusNoise || back.Pauses[0].Detail != "pane "+statusNoise ||
		store.Deref(back.Detail.PR.LastError) != "boom "+statusNoise {
		t.Fatalf("json lost the raw text: %+v", back.Queue[0])
	}
}

func TestStatusResumeQuotesEveryWord(t *testing.T) {
	id, cwd := "a b;rm -rf $HOME", "/tmp/it's here"
	s := store.Session{Role: "codex-judge", SessionID: &id, AgentKind: store.Ptr("codex"), Cwd: &cwd}
	want := `cd '/tmp/it'\''s here' && codex resume 'a b;rm -rf $HOME'`
	if got := statusResume(config.Defaults(), s); got != want {
		t.Fatalf("statusResume = %q\nwant         %q", got, want)
	}
	// Plainly safe words stay bare, and a home path keeps its ~.
	t.Setenv("HOME", "/Users/x")
	id, cwd = "s-1", "/Users/x/talkable.review1"
	s = store.Session{Role: "codex-judge", SessionID: &id, AgentKind: store.Ptr("codex"), Cwd: &cwd}
	if got := statusResume(config.Defaults(), s); got != "cd ~/talkable.review1 && codex resume s-1" {
		t.Fatalf("statusResume = %q", got)
	}
}
