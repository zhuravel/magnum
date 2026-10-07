package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// Doctor requires the herdr methods magnum sends and no others: a server
// without the ones nothing in magnum calls (events.subscribe, tab.create,
// workspace.rename, pane.list, agent.get, agent.list) passes, and one without
// a method reveal sends through the herdr CLI fails naming it.
func TestDoctorHerdrMethodsAreTheOnesMagnumSends(t *testing.T) {
	sent := []string{
		"ping", "session.snapshot", "workspace.create", "workspace.close", "workspace.report_metadata",
		"pane.split", "pane.send_input", "pane.send_keys", "pane.read", "pane.wait_for_output", "pane.rename",
		"pane.process_info", "pane.get", "agent.start", "agent.prompt", "agent.read", "agent.focus", "agent.rename",
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
