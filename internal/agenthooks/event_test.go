package agenthooks

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// env is a getenv over a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var paneEnv = env(map[string]string{EnvPane: "w1:p2"})

// The text an agent's payload carries that is PR text: the last assistant
// message, a prompt typed into the pane, a tool's input.
const (
	secretMessage = "SECRET-LAST-MESSAGE the review found a token in config/x.yml"
	secretPrompt  = "SECRET-PROMPT review https://github.com/talkable/talkable/pull/1"
	secretInput   = "SECRET-TOOL-INPUT rm -rf /"
)

// A Claude Stop's payload keeps the session, the transcript and the
// background tasks; the last assistant message, a prompt and a tool's input
// are PR text and never leave the hook, in the Event or in its JSON.
func TestParseKeepsOnlyTheAllowedFields(t *testing.T) {
	payload := `{
		"session_id": "0a1b2c3d-0000-4000-8000-00000000c1a0",
		"transcript_path": "/Users/x/.claude/projects/-Users-x-review1/0a1b2c3d-0000-4000-8000-00000000c1a0.jsonl",
		"cwd": "/Users/x/review1",
		"permission_mode": "bypassPermissions",
		"hook_event_name": "Stop",
		"stop_hook_active": false,
		"turn_id": "turn-7",
		"source": "startup",
		"error": "rate_limit",
		"tool_name": "Bash",
		"agent_id": "",
		"last_assistant_message": "` + secretMessage + `",
		"prompt": "` + secretPrompt + `",
		"tool_input": {"command": "` + secretInput + `"},
		"background_tasks": [{"id": "bg1", "description": "` + secretInput + `"}, {"id": "bg2"}]
	}`
	ev, err := Parse(AgentClaude, NameStop, []byte(payload), env(map[string]string{
		EnvPane: "w1:p2", EnvCodexThread: "thread-9", "HOME": "/Users/x", "GH_TOKEN": "ghp_secret"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Event{V: Version, Agent: AgentClaude, Name: NameStop, SessionID: "0a1b2c3d-0000-4000-8000-00000000c1a0",
		TranscriptPath: "/Users/x/.claude/projects/-Users-x-review1/0a1b2c3d-0000-4000-8000-00000000c1a0.jsonl",
		TurnID:         "turn-7", Source: "startup", Error: "rate_limit", ToolName: "Bash", Pane: "w1:p2", CodexThread: "thread-9", Background: 2}
	if ev != want {
		t.Fatalf("event = %+v\nwant    %+v", ev, want)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"SECRET", "ghp_secret", "/Users/x/review1\"", "bypassPermissions", "stop_hook_active", "cwd"} {
		if strings.Contains(string(b), s) {
			t.Errorf("the event's JSON carries %q: %s", s, b)
		}
	}
	v := reflect.ValueOf(ev)
	for i := range v.NumField() {
		if f, ok := reflect.TypeAssert[string](v.Field(i)); ok && strings.Contains(f, "SECRET") {
			t.Errorf("field %s carries PR text: %q", v.Type().Field(i).Name, f)
		}
	}
}

// Its keys are lower-case snake, and a field left empty is left out.
func TestAnEventsJSONHasSnakeKeys(t *testing.T) {
	b, err := json.Marshal(Event{V: 1, Agent: AgentCodex, Name: NameSessionStart, SessionID: "s", TranscriptPath: "/p", TurnID: "t",
		Source: "startup", Error: "e", ToolName: "Bash", AgentID: "a", Pane: "w1:p1", CodexThread: "s", Background: 1})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"agent":"codex","name":"session_start","session_id":"s","transcript_path":"/p","turn_id":"t","source":"startup",` +
		`"error":"e","tool_name":"Bash","agent_id":"a","pane":"w1:p1","codex_thread":"s","background":1}`
	if string(b) != want {
		t.Fatalf("JSON = %s\nwant   %s", b, want)
	}
	b, _ = json.Marshal(Event{V: 1, Agent: AgentClaude, Name: NameStop, Pane: "w1:p1"})
	if want := `{"v":1,"agent":"claude","name":"stop","pane":"w1:p1"}`; string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

// A value of another type than the field's (an object for the error, a
// string for the background tasks) is no value: the field stays empty.
func TestParseTakesStringsAsStringsAndNothingElse(t *testing.T) {
	ev, err := Parse(AgentClaude, NameStopFailure, []byte(`{"session_id": 12, "error": {"type": "rate_limit"}, "background_tasks": "two",
		"tool_name": ["Bash"], "agent_id": null, "source": "resume"}`), paneEnv)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Event{V: Version, Agent: AgentClaude, Name: NameStopFailure, Source: "resume", Pane: "w1:p2"}); ev != want {
		t.Fatalf("event = %+v, want %+v", ev, want)
	}
}

// The agent and the event come from the hook's arguments, the pane from the
// env: an agent or an event magnum does not know, and a hook outside a
// herdr pane, give no event.
func TestParseRefusesUnknownAgentsEventsAndHooksOutsideAPane(t *testing.T) {
	for _, tc := range []struct{ agent, name string }{{"droid", NameStop}, {AgentCodex, "Stop"}, {AgentClaude, "pre_tool_use"}, {"", ""}} {
		if _, err := Parse(tc.agent, tc.name, []byte(`{}`), paneEnv); err == nil {
			t.Errorf("Parse(%q, %q) = no error", tc.agent, tc.name)
		}
	}
	if _, err := Parse(AgentClaude, NameStop, []byte(`{"session_id": "s"}`), env(nil)); err == nil {
		t.Error("Parse without HERDR_PANE_ID = no error")
	}
	for _, name := range []string{NameStop, NameStopFailure, NamePermissionRequest, NameSessionStart, NameSessionEnd, NameInterrupt} {
		if _, err := Parse(AgentCodex, name, nil, paneEnv); err != nil {
			t.Errorf("Parse(codex, %s) without a payload = %v, want an event", name, err)
		}
	}
}

// A payload cut at MaxPayload, or one that is not JSON at all, still gives
// the fields that came before the cut (Claude's session_id comes first).
func TestParseKeepsWhatCameBeforeACut(t *testing.T) {
	full := `{"session_id": "abc", "transcript_path": "/t.jsonl", "last_assistant_message": "` + strings.Repeat("x", 100) + `", "turn_id": "late"}`
	ev, err := Parse(AgentClaude, NameStop, []byte(full[:60]), paneEnv)
	if err != nil {
		t.Fatal(err)
	}
	if ev.SessionID != "abc" || ev.TranscriptPath != "/t.jsonl" || ev.TurnID != "" {
		t.Fatalf("event = %+v, want the two fields before the cut", ev)
	}
	ev, err = Parse(AgentClaude, NameStop, []byte("not json"), paneEnv)
	if err != nil || ev.SessionID != "" || ev.Pane != "w1:p2" {
		t.Fatalf("event of a payload that is no JSON = %+v, %v", ev, err)
	}
}

// Whatever the payload holds, the event fits one datagram: the free text
// (the transcript's path, the error, the tool, the source) is cut or left
// out, never the pane or the session.
func TestParseFitsTheEventIntoADatagram(t *testing.T) {
	long := strings.Repeat("<&>\x01é", 4000)
	id := strings.Repeat("0a1b2c3d-", 14) // 126 bytes
	payload, _ := json.Marshal(map[string]any{"session_id": id, "transcript_path": "/" + long, "turn_id": id, "source": long,
		"error": long, "tool_name": long, "agent_id": id})
	ev, err := Parse(AgentCodex, NameStop, payload, env(map[string]string{EnvPane: "w1:p2", EnvCodexThread: id}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxDatagram {
		t.Fatalf("the event's JSON is %d bytes, over %d", len(b), MaxDatagram)
	}
	if ev.Pane != "w1:p2" || ev.SessionID != id || ev.TurnID != id || ev.AgentID != id || ev.CodexThread != id {
		t.Fatalf("the fit cut an id: %+v", ev)
	}
	if strings.ContainsRune(ev.Error+ev.Source+ev.ToolName+ev.TranscriptPath, '\x01') {
		t.Fatalf("a control character got through: %+v", ev)
	}
}

// An id is an id or nothing: one too long or with characters no id has is
// left out whole, never cut into another id.
func TestParseLeavesOutAnIDThatIsNoID(t *testing.T) {
	for _, bad := range []string{strings.Repeat("a", 200), `a"b`, "a b", "a\\b", "<x>", "a\nb", "é"} {
		payload, _ := json.Marshal(map[string]any{"session_id": bad, "agent_id": bad, "turn_id": bad})
		ev, err := Parse(AgentClaude, NameStop, payload, env(map[string]string{EnvPane: "w1:p2", EnvCodexThread: bad}))
		if err != nil {
			t.Fatal(err)
		}
		if ev.SessionID != "" || ev.AgentID != "" || ev.TurnID != "" || ev.CodexThread != "" {
			t.Errorf("id %q kept: %+v", bad, ev)
		}
		if _, err := Parse(AgentClaude, NameStop, nil, env(map[string]string{EnvPane: bad})); err == nil {
			t.Errorf("pane %q accepted", bad)
		}
	}
}

// ReadPayload reads at most MaxPayload, and gives up on a stdin that never
// ends after ReadTimeout with what it read by then.
func TestReadPayloadStopsAtTheLimitAndTheDeadline(t *testing.T) {
	big := strings.Repeat("y", MaxPayload+100)
	if got := ReadPayload(strings.NewReader(big)); len(got) != MaxPayload {
		t.Fatalf("read %d bytes, want %d", len(got), MaxPayload)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := w.WriteString(`{"session_id": "s"}`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := ReadPayload(r) // the writer never closes
	if took := time.Since(start); took < ReadTimeout || took > ReadTimeout+150*time.Millisecond {
		t.Fatalf("ReadPayload took %s on an open stdin, want about %s", took, ReadTimeout)
	}
	if string(got) != `{"session_id": "s"}` {
		t.Fatalf("read %q, want what came before the deadline", got)
	}
}
