package store

import (
	"context"
	"encoding/json"
	"testing"
)

// AppendEventOnce writes an event unless one of its subject, kind, step and
// phase already carries its key (a delivery that comes at least once, such
// as a staging deploy's events from its outbox, is written once): a repeat
// writes nothing, another phase, step or subject of the same key is
// another event, and the key goes into the event's data beside the
// caller's fields.
func TestAppendEventOnceWritesAKeyOnce(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	sub, kind := "pr:talkable/talkable#2", "staging.deploy"
	ev := func(step, phase, subject string) Event {
		return Event{Subject: &subject, Kind: kind, Step: &step, Phase: &phase, Message: step + " " + phase,
			Data: json.RawMessage(`{"effect":"void-2-x","seq":3}`)}
	}
	for _, c := range []struct {
		ev   Event
		key  string
		want bool
	}{
		{ev("push", "begin", sub), "void-2-x/3", true},
		{ev("push", "begin", sub), "void-2-x/3", false}, // the repeat
		{ev("push", "ok", sub), "void-2-x/3", true},
		{ev("watch", "begin", sub), "void-2-x/3", true},
		{ev("push", "begin", "pr:talkable/talkable#3"), "void-2-x/3", true},
		{ev("push", "begin", sub), "void-2-x/4", true},
	} {
		added, err := st.AppendEventOnce(ctx, c.ev, c.key)
		if err != nil || added != c.want {
			t.Fatalf("%s %s %s %s: added %v, %v; want %v", *c.ev.Subject, *c.ev.Step, *c.ev.Phase, c.key, added, err, c.want)
		}
	}
	evs, err := st.EventsBySubject(ctx, sub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("%d events, want 4", len(evs))
	}
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["key"] != "void-2-x/3" || data["effect"] != "void-2-x" {
		t.Fatalf("data %s (%v)", evs[0].Data, err)
	}
	if _, err := st.AppendEventOnce(ctx, ev("push", "begin", sub), ""); err == nil {
		t.Fatal("an empty key was written")
	}
}
