package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// seedReadyCard makes task PR-12360 in the factory registry of dt, waiting
// at card 2 of its ready stage, and a queued task behind it.
func seedReadyCard(t *testing.T, dt *daemonGroupTest) (registry.Task, factory.ReadyCardFile) {
	t.Helper()
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	task, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12360", Kind: "bug", Repo: "talkable/talkable", Title: "Custom activities lose points",
		State: registry.TaskNeedsYou, Stage: "ready", Branch: "zhuravel-PR-12360-activity-points-on-copy", WaitsWho: "you",
		WaitsWhat: "ready to push (card 2): PR-12360 Custom activities lose points"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12388", Kind: "bug", Repo: "talkable/talkable", State: registry.TaskQueued,
		WaitsWho: "copy", WaitsWhat: "all 3 busy"}); err != nil {
		t.Fatal(err)
	}
	card := factory.ReadyCardFile{Revision: 2, Drawn: time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC), Head: strings.Repeat("c", 40),
		Base: strings.Repeat("b", 40), ReportHash: strings.Repeat("e", 64), DiffHash: strings.Repeat("d", 40), Branch: task.Branch,
		Title: "PR-12360 Custom activities lose points", Copy: "talkable.factory3", Flags: []string{"sensitive: Gemfile.lock"},
		Review: factory.ReviewSummary{Exit: registry.ExitClean, Rounds: 2, Head: strings.Repeat("c", 40)}}
	b, _ := json.Marshal(card)
	dir := dt.ctx.Layout.FactoryTaskDir(task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready-card.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return task, card
}

// `magnum factory decide <task>` prints the ready card and its y/N
// sentence; with a choice and --card, the revision you read, it sends what
// that card showed, and a card drawn since is refused.
func TestFactoryDecideSendsTheCardYouRead(t *testing.T) {
	storetest.Serial(t)
	var got factory.DecidePayload
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqDecide {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "PR-12360: pushes it"
	})
	task, card := seedReadyCard(t, dt)

	if code := dt.run("factory", "decide", "PR-12360"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	for _, want := range []string{"card 2", card.Title, "sensitive: Gemfile.lock",
		"Push zhuravel-PR-12360-activity-points-on-copy and open the PR? This frees talkable.factory3, and PR-12388 starts. y/N",
		"--card 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the card lacks %q:\n%s", want, out)
		}
	}

	dt.stdout.Reset()
	read := time.Now()
	if code := dt.run("factory", "decide", "PR-12360", "push", "--card", "2"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got.TaskID != task.ID || got.Choice != factory.ChoicePush || got.HeadSHA != card.Head || got.ReportHash != card.ReportHash ||
		got.DiffHash != card.DiffHash || got.Surface != "cli" {
		t.Fatalf("payload %+v, want what card 2 showed", got)
	}
	if got.Shown.Before(read) || got.Shown.After(time.Now()) { // the command's read of the card, never its drawing
		t.Fatalf("shown %v, want the time the command read the card (after %v), not its drawing %v", got.Shown, read, card.Drawn)
	}

	// A drop is bound to no card, as the daemon takes it: a stale --card, or
	// none, still drops.
	for _, args := range [][]string{{"drop", "--card", "1"}, {"drop"}} {
		got = factory.DecidePayload{}
		if code := dt.run("factory", append([]string{"decide", "PR-12360"}, args...)...); code != 0 || got.Choice != factory.ChoiceDrop ||
			got.TaskID != task.ID {
			t.Fatalf("decide %q: exit %d, stderr %s, payload %+v, want the drop sent", args, code, dt.stderr, got)
		}
	}

	got = factory.DecidePayload{}
	dt.stderr.Reset()
	if code := dt.run("factory", "decide", "PR-12360", "send_back", "--card", "1", "--reason", "scope", "--note", "keep archived out"); code != 1 ||
		!strings.Contains(dt.stderr.String(), "card 2") || got.TaskID != 0 {
		t.Fatalf("a decision on card 1 while card 2 is drawn: exit %d, stderr %s, payload %+v", code, dt.stderr, got)
	}
	if code := dt.run("factory", "decide", "PR-12360", "send_back", "--card", "2", "--reason", "scope", "--note", "keep archived out"); code != 0 ||
		got.Choice != factory.ChoiceSendBack || got.Reason != "scope" || got.Note != "keep archived out" {
		t.Fatalf("send back: exit %d, stderr %s, payload %+v", code, dt.stderr, got)
	}
	// The reason known takes no note: the factory writes it from the card's
	// known defects.
	got = factory.DecidePayload{}
	if code := dt.run("factory", "decide", "PR-12360", "send_back", "--card", "2", "--reason", factory.ReasonKnown); code != 0 ||
		got.Choice != factory.ChoiceSendBack || got.Reason != factory.ReasonKnown || got.Note != "" {
		t.Fatalf("send back of the known defects: exit %d, stderr %s, payload %+v", code, dt.stderr, got)
	}
	for _, bad := range [][]string{{"push"}, {"merge", "--card", "2"}, {"send_back", "--card", "2", "--note", "x"},
		{"send_back", "--card", "2", "--reason", factory.ReasonKnown, "--note", "my words"}} {
		if code := dt.run("factory", append([]string{"decide", "PR-12360"}, bad...)...); code == 0 {
			t.Errorf("decide %q went through", bad)
		}
	}
}

// The card `magnum factory decide <task>` prints names the factory's
// recommendation and why, as the screen's card does once you read the
// diff, and a decision on that card carries it, so the factory records
// that it was shown and whether you took it.
func TestFactoryDecideSendsTheRecommendationItsCardShowed(t *testing.T) {
	storetest.Serial(t)
	var got factory.DecidePayload
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "done"
	})
	seedReadyCard(t, dt)
	if code := dt.run("factory", "decide", "PR-12360"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if out, want := dt.stdout.String(), "recommends push: the local review ended clean after 2 rounds and the factory's proof holds at cccccccc"; !strings.Contains(out, want) {
		t.Errorf("the card lacks %q:\n%s", want, out)
	}
	if code := dt.run("factory", "decide", "PR-12360", "send_back", "--card", "2", "--reason", "scope", "--note", "keep archived out"); code != 0 ||
		got.Choice != factory.ChoiceSendBack || got.Recommended != factory.ChoicePush {
		t.Fatalf("send back: exit %d, stderr %s, payload %+v, want the card's recommendation, push", code, dt.stderr, got)
	}
}

// The card `magnum factory decide <task>` prints has its QA line, as the
// screen's does: what the QA run that passed at its head found.
func TestFactoryDecidePrintsTheCardsQALine(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 250, func(registry.Request) (string, string) { return registry.RequestDone, "" })
	task, card := seedReadyCard(t, dt)
	card.QA = &factory.QAResult{Run: 1, Pages: 3, Head: card.Head, Outcome: factory.QAOutcomeClean, Walked: true,
		Accepted: []factory.QADefect{{Class: factory.QAFlow, Severity: "P2"}}}
	b, _ := json.Marshal(card)
	if err := os.WriteFile(filepath.Join(dt.ctx.Layout.FactoryTaskDir(task.ID), "ready-card.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "decide", "PR-12360"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if out, want := dt.stdout.String(), "qa       passed with 1 defect accepted by you: 3 pages walked at ccccccc, 0 notes\n"; !strings.Contains(out, want) {
		t.Errorf("the card lacks %q:\n%s", want, out)
	}
}
