package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// `magnum status` lists the review machine's failures of the last 24 hours,
// one line per repository and command with its rounds, its newest error and
// when it last failed; the agent's text is cleaned of control characters,
// --json carries the groups, and older failures are left out.
func TestStatusListsTheReviewMachinesFailures(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	env := func(at time.Time, subject string, failures ...any) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"failures": failures})
		if err != nil {
			t.Fatal(err)
		}
		s := "pr:" + subject
		if _, err := st.AppendEvent(ctx, store.Event{At: at, Level: "warn", Subject: &s, Kind: "round.environment",
			Message: "the judge hit failures of the review machine", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		env(now.Add(-time.Duration(i)*time.Hour), "talkable/talkable#"+strconv.Itoa(11900+i),
			map[string]any{"cmd": "bundle exec rspec spec/models/x" + strconv.Itoa(i) + "_spec.rb", "error": "no test database for the worktree"})
	}
	env(now.Add(-2*time.Hour), "example/app#3", "Deadlock found "+statusNoise)
	env(now.Add(-25*time.Hour), "example/old#1", map[string]any{"cmd": "yarn test", "error": "too old to list"})

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	out := b.String()
	for _, want := range []string{
		"machine:  6 rounds of talkable: `bundle exec rspec`: no test database for the worktree, last " + inspClock(now, now) + "\n",
		"machine:  1 round of app: Deadlock found ",
		"next, last " + inspClock(now, now.Add(-2*time.Hour)) + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "too old") || strings.Contains(out, "yarn test") {
		t.Errorf("status lists a failure older than 24 hours:\n%s", out)
	}
	statusNoControls(t, "status", out)

	var j bytes.Buffer
	if err := writeJSON(&j, r); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Machine []statusMachine `json:"machine"`
	}
	if err := json.Unmarshal(j.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Machine) != 2 {
		t.Fatalf("--json machine = %+v", got.Machine)
	}
	if m := got.Machine[0]; m.Repo != "talkable/talkable" || m.Cmd != "bundle exec rspec" || m.Rounds != 6 ||
		m.Error != "no test database for the worktree" || !m.Last.Equal(now) {
		t.Errorf("first group = %+v", m)
	}
	if m := got.Machine[1]; m.Repo != "example/app" || m.Cmd != "" || m.Rounds != 1 || !m.Last.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("second group = %+v", m)
	}

	_, _, d, _ = statusFixture(t) // no failures: no line, no key
	r, _ = statusGather(ctx, d, statusOptions{})
	b.Reset()
	statusRender(&b, r)
	j.Reset()
	writeJSON(&j, r)
	if strings.Contains(b.String(), "machine:") || strings.Contains(j.String(), `"machine"`) {
		t.Fatalf("a status without machine failures:\n%s\n%s", b.String(), j.String())
	}
}
