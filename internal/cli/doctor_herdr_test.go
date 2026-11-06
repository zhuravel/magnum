package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
)

// Doctor requires the herdr methods magnum sends and no others: a server
// without the ones nothing in magnum calls (events.subscribe, tab.create,
// workspace.rename, pane.list, agent.get, agent.list) passes, and one without
// a method reveal sends through the herdr CLI fails naming it.
func TestDoctorHerdrMethodsAreTheOnesMagnumSends(t *testing.T) {
	sent := []string{
		"ping", "session.snapshot", "workspace.create", "workspace.close", "workspace.report_metadata",
		"pane.split", "pane.send_input", "pane.send_keys", "pane.read", "pane.wait_for_output", "pane.rename",
		"pane.process_info", "pane.get", "pane.close", "agent.start", "agent.prompt", "agent.read", "agent.focus", "agent.rename",
		"agent.send_keys", "notification.show", "plugin.pane.open", "plugin.list",
		"client.window_title.set", "client.window_title.clear", "worktree.list",
	}
	check := func(methods []string) doctorCheck {
		t.Helper()
		_, d, _, _ := doctorFixture(t)
		f := d.Run.(*execx.Fake)
		f.Rules = append([]execx.Rule{{Prefix: []string{"herdr", "api", "schema"},
			Result: execx.Result{Stdout: []byte(doctorSchema(methods))}}}, f.Rules...)
		return doctorByName(doctorHerdrChecks(context.Background(), d))["herdr methods"]
	}
	if c := check(sent); c.Status != doctorPass {
		t.Fatalf("herdr with exactly the sent methods: %s %q", c.Status, c.Detail)
	}
	without := slices.DeleteFunc(slices.Clone(sent), func(m string) bool { return m == "client.window_title.set" })
	if c := check(without); c.Status != doctorFail || !strings.Contains(c.Detail, "client.window_title.set") {
		t.Fatalf("herdr without client.window_title.set: %s %q", c.Status, c.Detail)
	}
}

// doctorNoServer is a herdr call to a socket no server runs on.
func doctorNoServer() error {
	return fmt.Errorf("herdr ping: %w: %w", herdr.ErrUnavailable, syscall.ENOENT)
}

// With magnum's panes in a session of their own, doctor checks both: the
// panes' session fails when it does not answer (no round can run) and names
// the command that starts it; yours only warns (the toasts show with
// osascript, and the slot guards read no agent of yours, or hold every slot
// while it does not answer), and the plugin is listed there.
func TestDoctorChecksThePaneSessionAndYours(t *testing.T) {
	_, d, panes, _ := doctorFixture(t)
	yours := &fakeDoctorHerdr{info: panes.info, plugins: []string{"zhuravel.magnum"}}
	panes.plugins = nil // linked in your session only: the plugin check must read yours
	d.Yours, d.PaneSession = yours, herdr.Session{Name: "reviews", Socket: "/run/herdr/reviews.sock"}
	run := func() map[string]doctorCheck {
		t.Helper()
		return doctorByName(doctorHerdrChecks(context.Background(), d))
	}
	plugin := func() doctorCheck { return doctorPluginCheck(context.Background(), d)[0] }

	m := run()
	if c := m["herdr"]; c.Status != doctorPass || !strings.Contains(c.Detail, "session reviews") || !strings.Contains(c.Detail, "/run/herdr/reviews.sock") {
		t.Fatalf("herdr = %+v, want a pass naming the panes' session", c)
	}
	if c := m["your herdr"]; c.Status != doctorPass || !strings.Contains(c.Detail, d.Config.Herdr.Socket) {
		t.Fatalf("your herdr = %+v, want a pass naming your socket", c)
	}
	if c := plugin(); c.Status != doctorPass {
		t.Fatalf("plugin = %+v, want it found in your session", c)
	}

	panes.pingErr = doctorNoServer()
	m = run()
	if c := m["herdr"]; c.Status != doctorFail || !strings.Contains(c.Detail, "session reviews") || !strings.Contains(c.Fix, "`herdr --session reviews`") {
		t.Fatalf("herdr = %+v, want a failure naming the panes' session and the command that starts it", c)
	}
	if c := m["your herdr"]; c.Status != doctorPass {
		t.Fatalf("your herdr = %+v while only the panes' session is down", c)
	}

	panes.pingErr, yours.pingErr = nil, doctorNoServer()
	m = run()
	if c := m["herdr"]; c.Status != doctorPass {
		t.Fatalf("herdr = %+v while only yours is down", c)
	}
	if c := m["your herdr"]; c.Status != doctorWarn || !strings.Contains(c.Detail, "osascript") || !strings.Contains(c.Detail, "no herdr server runs") {
		t.Fatalf("your herdr = %+v, want a warning: no server, toasts with osascript", c)
	}
	if c := plugin(); c.Status != doctorWarn {
		t.Fatalf("plugin = %+v with your session down", c)
	}

	yours.pingErr = fmt.Errorf("herdr ping: %w", herdr.ErrTimeout)
	if c := run()["your herdr"]; c.Status != doctorWarn || !strings.Contains(c.Detail, "does not answer") || !strings.Contains(c.Detail, "hold") {
		t.Fatalf("your herdr = %+v, want a warning that the slot guards hold", c)
	}
}

// [herdr] review_session that names your own session ([herdr] socket and
// [terminal] session point at it) puts magnum's panes among your agents, and
// no guard reads another session: a stopgap that pointed your session at
// the review session's socket, kept when the key was added, does just that.
// Doctor warns; review_session "" (the panes in yours on purpose) and a
// review session of its own pass.
func TestDoctorWarnsOfAReviewSessionThatIsYourOwn(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	reviews := herdr.SessionSocket("reviews")
	d.Config.Terminal.Session, d.Config.Herdr.Socket, d.Config.Herdr.ReviewSession = "reviews", reviews, "reviews"
	d.PaneSession = d.Config.YourHerdr()
	c, ok := doctorByName(doctorHerdrChecks(context.Background(), d))["herdr review session"]
	if !ok || c.Status != doctorWarn || !strings.Contains(c.Detail, "your own session") || !strings.Contains(c.Fix, "[herdr] socket") {
		t.Fatalf("herdr review session = %+v (found %v), want a warning that it is your own session", c, ok)
	}

	for name, set := range map[string]func(){
		"the key unset":        func() { d.Config.Herdr.ReviewSession = "" },
		"a session of its own": func() { d.Config.Terminal.Session, d.Config.Herdr.Socket = "", herdr.SessionSocket("default") },
	} {
		d.Config.Terminal.Session, d.Config.Herdr.Socket, d.Config.Herdr.ReviewSession = "reviews", reviews, "reviews"
		set()
		if c, ok := doctorByName(doctorHerdrChecks(context.Background(), d))["herdr review session"]; ok {
			t.Fatalf("%s: herdr review session = %+v, want no such check", name, c)
		}
	}
}

// With magnum's panes in your session, doctor pings it once and has no
// second herdr check.
func TestDoctorPingsOneSessionOnce(t *testing.T) {
	_, d, h, _ := doctorFixture(t)
	d.PaneSession = d.Config.YourHerdr()
	m := doctorByName(doctorHerdrChecks(context.Background(), d))
	if n := h.pings.Load(); n != 1 {
		t.Fatalf("%d pings of your session, want 1", n)
	}
	if c, ok := m["your herdr"]; ok {
		t.Fatalf("a second check of one session: %+v", c)
	}
	if c := m["herdr"]; c.Status != doctorPass {
		t.Fatalf("herdr = %+v", c)
	}
}

// A move of magnum's panes holds every round: doctor warns while one waits,
// saying on what and why, and while the config names another session than
// the one the registry's panes are in; with no move, it says nothing.
func TestDoctorWarnsOfAMoveThatWaits(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	d.PaneSession = d.Config.YourHerdr()
	if c, ok := doctorByName(doctorHerdrChecks(context.Background(), d))["herdr move"]; ok {
		t.Fatalf("no move is due, yet %+v", c)
	}

	d.Config.Herdr.ReviewSession = "reviews"
	c := doctorByName(doctorHerdrChecks(context.Background(), d))["herdr move"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "herdr session default") || !strings.Contains(c.Detail, "reviews") {
		t.Fatalf("herdr move = %+v, want a warning of the move the config asks for", c)
	}

	mv := engine.PanesMove{From: d.PaneSession, To: d.Config.ReviewHerdr(), Since: time.Now().Add(-40 * time.Minute),
		Subject: "pr:talkable/talkable#5", Reason: "an agent works in its pane"}
	raw, _ := json.Marshal(mv)
	if err := d.Store.SetKV(context.Background(), engine.KVHerdrPanesMove, string(raw)); err != nil {
		t.Fatal(err)
	}
	c = doctorByName(doctorHerdrChecks(context.Background(), d))["herdr move"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "waits") || !strings.Contains(c.Detail, "talkable/talkable#5") ||
		!strings.Contains(c.Detail, "an agent works in its pane") || strings.Contains(c.Detail, "pr:") {
		t.Fatalf("herdr move = %+v, want the wait, its PR and its reason", c)
	}
}
