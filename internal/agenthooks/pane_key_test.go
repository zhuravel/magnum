package agenthooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// A pane magnum started names its agent in MAGNUM_AGENT, and the event
// carries it as its pane key beside herdr's pane: a session is found by
// magnum's own key, so a pane outside herdr (no HERDR_PANE_ID) still sends
// its events. A pane with neither sends nothing, and a key that is no id is
// no key.
func TestAnEventCarriesItsPanesAgentKey(t *testing.T) {
	const key = "mg-11920-claude-review-5d01cf"
	ev, err := Parse(AgentClaude, NameStop, nil, env(map[string]string{EnvPane: "w1:p2", EnvAgent: key}))
	if err != nil || ev.PaneKey != key || ev.Pane != "w1:p2" {
		t.Fatalf("with both = %+v, %v", ev, err)
	}
	ev, err = Parse(AgentCodex, NameStop, nil, env(map[string]string{EnvAgent: key}))
	if err != nil || ev.PaneKey != key || ev.Pane != "" {
		t.Fatalf("with the key alone = %+v, %v", ev, err)
	}
	if _, err := Parse(AgentClaude, NameStop, nil, env(map[string]string{EnvAgent: "a b"})); err == nil {
		t.Fatal("a key that is no id, and no pane, gave an event")
	}
	if ev, err := Parse(AgentClaude, NameStop, nil, env(map[string]string{EnvPane: "w1:p2", EnvAgent: `a"b`})); err != nil || ev.PaneKey != "" {
		t.Fatalf("a key that is no id = %+v, %v; want the pane's event without it", ev, err)
	}
	b, err := json.Marshal(Event{V: 1, Agent: AgentClaude, Name: NameStop, PaneKey: key})
	if want := `{"v":1,"agent":"claude","name":"stop","pane":"","pane_key":"` + key + `"}`; err != nil || string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

// UserPromptSubmit is an event both agents send. Its payload carries the
// prompt, which is PR text: the event keeps the session and the turn and
// never the prompt.
func TestAUserPromptSubmitKeepsNoPrompt(t *testing.T) {
	payload := `{"session_id": "0a1b2c3d-0000-4000-8000-00000000c1a0", "turn_id": "turn-3", "hook_event_name": "UserPromptSubmit",
		"prompt": "` + secretPrompt + `", "cwd": "/Users/x/review1"}`
	for _, agent := range []string{AgentClaude, AgentCodex} {
		ev, err := Parse(agent, NameUserPromptSubmit, []byte(payload), paneEnv)
		if err != nil {
			t.Fatal(err)
		}
		if ev.SessionID != "0a1b2c3d-0000-4000-8000-00000000c1a0" || ev.TurnID != "turn-3" || ev.Name != NameUserPromptSubmit {
			t.Fatalf("%s event = %+v", agent, ev)
		}
		if b, _ := json.Marshal(ev); strings.Contains(string(b), "SECRET") {
			t.Fatalf("%s: the event's JSON carries the prompt: %s", agent, b)
		}
	}
}

// The listener takes an event that names only its pane key (a pane outside
// herdr) and drops one that names neither a pane nor a key; a key that is no
// id never reaches the handler.
func TestTheListenerTakesAnEventThatNamesOnlyItsPaneKey(t *testing.T) {
	s := serve(t, nil)
	sendRaw(t, s.path, []byte(`{"v":1,"agent":"codex","name":"stop","pane":"","pane_key":""}`))
	sendRaw(t, s.path, []byte(`{"v":1,"agent":"codex","name":"stop","pane_key":"a b"}`))
	keyed := Event{V: Version, Agent: AgentCodex, Name: NameUserPromptSubmit, PaneKey: "mg-7-codex-judge-abc123"}
	if err := Send(s.path, keyed); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != keyed {
		t.Fatalf("handled %+v, want the keyed event (the others dropped)", got)
	}
	sendRaw(t, s.path, []byte(`{"v":1,"agent":"claude","name":"stop","pane":"w1:p2","pane_key":"a<b"}`))
	if got := s.next(t); got != (Event{V: Version, Agent: AgentClaude, Name: NameStop, Pane: "w1:p2"}) {
		t.Fatalf("handled %+v, want the pane's event without the key that is no id", got)
	}
}
