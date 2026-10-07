package engine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/store"
)

// machineEvent is a round.environment event of PR subject (owner/name#N)
// at at, listing failures (objects with cmd and error, or bare strings as
// older rows have them).
func machineEvent(id int64, at time.Time, subject string, failures ...any) store.Event {
	data, _ := json.Marshal(map[string]any{"failures": failures})
	s := "pr:" + subject
	return store.Event{ID: id, At: at, Level: "warn", Subject: &s, Kind: MachineEventKind, Message: "the judge hit failures of the review machine", Data: data}
}

func failure(cmd, err string) map[string]any { return map[string]any{"cmd": cmd, "error": err} }

// The review machine's failures group by repository and command, a test
// file's path aside (`bundle exec rspec spec/a_spec.rb` is `bundle exec
// rspec`): a round counts once per group however often it lists the
// command, the group keeps its newest error, and the groups come most
// rounds first, then newest. A bare string is a failure without a command;
// other kinds and events without a PR subject are left out.
func TestMachineGroupsCountRoundsByRepositoryAndCommand(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	other := machineEvent(5, t0.Add(4*time.Hour), "talkable/talkable#9", failure("bundle exec rspec", "x"))
	other.Kind = "round.local_paths"
	noSubject := machineEvent(6, t0.Add(5*time.Hour), "talkable/talkable#9", failure("bundle exec rspec", "x"))
	noSubject.Subject = nil
	evs := []store.Event{
		machineEvent(1, t0, "talkable/talkable#1",
			failure("bundle exec rspec spec/models/a_spec.rb", "no test database"),
			failure("bundle exec rspec  spec/models/b_spec.rb:12", "no test database (b)")),
		machineEvent(2, t0.Add(time.Hour), "talkable/talkable#2", "Deadlock found when trying to get lock"),
		machineEvent(3, t0.Add(2*time.Hour), "talkable/talkable#3", failure("bundle exec rspec", "no test database for the worktree")),
		machineEvent(4, t0.Add(3*time.Hour), "example/app#7", failure("RAILS_ENV=test bin/rails db:seed", "seed failed")),
		other, noSubject,
	}
	got := MachineGroups(evs)
	want := []MachineGroup{
		{Repo: "talkable/talkable", Cmd: "bundle exec rspec", Error: "no test database for the worktree", Rounds: 2, First: t0, Last: t0.Add(2 * time.Hour)},
		{Repo: "example/app", Cmd: "RAILS_ENV=test bin/rails db:seed", Error: "seed failed", Rounds: 1, First: t0.Add(3 * time.Hour), Last: t0.Add(3 * time.Hour)},
		{Repo: "talkable/talkable", Error: "Deadlock found when trying to get lock", Rounds: 1, First: t0.Add(time.Hour), Last: t0.Add(time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("groups = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A command that fails on the review machine in MachineToastRounds rounds
// of one repository within MachineWindow toasts once: not at two rounds,
// and neither on the next tick nor at the fourth round. The toast names the
// command and the repository, and the agent's text reaches it on one line
// without control characters.
func TestAMachineFailureToastsOnceAtThreeRounds(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	ctx := context.Background()
	round := func(n int) {
		t.Helper()
		ev := machineEvent(0, time.Time{}, "talkable/talkable#"+strconv.Itoa(n),
			failure("bin/rails db:seed", "Table 'app_test.snapshots' doesn't exist\x1b[31m\r\nnext"))
		if _, err := h.st.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	machine := func() []string { return toastsWith(h, "review machine") }
	round(1)
	h.advance(10 * time.Minute)
	round(2)
	h.tick()
	h.flush()
	if got := machine(); len(got) != 0 {
		t.Fatalf("toasted at two rounds: %q", got)
	}
	h.advance(10 * time.Minute)
	round(3)
	h.tick()
	h.flush()
	got := machine()
	if len(got) != 1 {
		t.Fatalf("machine toasts = %q, want one", got)
	}
	since := h.clock.Now().Add(-24 * time.Minute).Local().Format("15:04") // the first round's
	for _, want := range []string{"magnum: `bin/rails db:seed` fails on the review machine (talkable) | ",
		"3 rounds of talkable/talkable since " + since + ": Table 'app_test.snapshots' doesn't exist [31m next;",
		"`magnum status` lists the machine's failures."} {
		if !strings.Contains(got[0], want) {
			t.Errorf("toast lacks %q: %q", want, got[0])
		}
	}
	for _, r := range got[0] {
		if unicode.IsControl(r) {
			t.Fatalf("the toast carries the control character %U: %q", r, got[0])
		}
	}
	h.tick()
	h.flush()
	round(4)
	h.tick()
	h.flush()
	if got := machine(); len(got) != 1 {
		t.Fatalf("toasted again: %q", got)
	}
	// A restarted daemon has no memory of the toast: the registry's dedupe
	// keeps the group quiet within the window.
	h.e = New(h.d)
	h.startup()
	h.tick()
	h.flush()
	if got := machine(); len(got) != 1 {
		t.Fatalf("the restart toasted again: %q", got)
	}
}
