package agents

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// textBlocks is a user entry whose content is a list of blocks, the shape
// Claude Code writes for a prompt with an image or a pasted attachment.
func textBlocks(at time.Time, blocks ...map[string]any) tline {
	l := humanPrompt(at, "")
	content := make([]any, len(blocks))
	for i, b := range blocks {
		content[i] = b
	}
	l["message"] = map[string]any{"role": "user", "content": content}
	return l
}

// The text of the user turns of a Claude transcript: the prompts typed into
// the pane, by a person or by magnum. A subagent's own thread, the text
// Claude Code adds itself (a skill's body), a task notification, a tool
// result and the line esc writes into the turn it ends are no turns of the
// operator's.
func TestClaudeUserTurnsSkipSidechainsMetaAndNotifications(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	at := t0
	sidechain := humanPrompt(at.Add(time.Minute), "the subagent's task")
	sidechain["isSidechain"] = true
	meta := userPrompt(at.Add(2*time.Minute), "Base directory for this skill: /skills/x")
	meta["isMeta"] = true
	tr.addAll(
		[]tline{
			humanPrompt(at, "first prompt"),
			assistantSays(at.Add(10*time.Second), "working"),
			sidechain,
			meta,
		},
		bashInBackground(at.Add(3*time.Minute), "toolu_1", "bg1"),
		notification(at.Add(4*time.Minute), "toolu_1", "bg1", "completed"),
		[]tline{
			interruption(at.Add(5*time.Minute), "p1"),
			humanPrompt(at.Add(6*time.Minute), "second prompt"),
			textBlocks(at.Add(7*time.Minute),
				map[string]any{"type": "text", "text": "third prompt"}, map[string]any{"type": "text", "text": "pasted"}),
			userPrompt(at.Add(8*time.Minute), "an older Claude Code recorded no origin"),
			// A task notification of an older Claude Code, with no origin: its text says what it is.
			userPrompt(at.Add(9*time.Minute), notificationText("toolu_2", "bg2", "completed")),
			textBlocks(at.Add(10*time.Minute), map[string]any{"type": "image", "source": map[string]any{"type": "base64"}}),
		},
	)
	// A line that is not JSON and a half-written last line are skipped.
	f, err := os.OpenFile(tr.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json \"user\"\n{\"type\":\"user\",\"timestamp\":"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ClaudeUserTurns(tr.path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []UserTurn{
		{At: at, Text: "first prompt", Origin: "human"},
		{At: at.Add(6 * time.Minute), Text: "second prompt", Origin: "human"},
		{At: at.Add(7 * time.Minute), Text: "third prompt", Origin: "human"},
		{At: at.Add(8 * time.Minute), Text: "an older Claude Code recorded no origin"},
	}
	if !slices.EqualFunc(got, want, func(a, b UserTurn) bool { return a.At.Equal(b.At) && a.Text == b.Text && a.Origin == b.Origin }) {
		t.Fatalf("turns = %+v\nwant   %+v", got, want)
	}

	// since keeps the turns stamped at or after it.
	got, err = ClaudeUserTurns(tr.path, at.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "third prompt" || got[1].Text != "an older Claude Code recorded no origin" {
		t.Fatalf("turns since 7m = %+v", got)
	}
	if got, err = ClaudeUserTurns(tr.path, at.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("turns since an hour on = %+v, %v", got, err)
	}
	if _, err := ClaudeUserTurns(filepath.Join(t.TempDir(), "gone.jsonl"), time.Time{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing transcript = %v, want a not-exist error", err)
	}
}

// Each user turn carries the agent's text of its two turns before it, oldest
// first: the assistant's text between two prompts (a tool call and a tool
// result neither end a turn nor say anything), a turn that said nothing
// skipped, a subagent's own thread left out, whatever since keeps.
func TestClaudeUserTurnsCarryTheAgentsTwoTurnsBefore(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	at := t0
	sub := assistantSays(at.Add(4*time.Minute), "the subagent's own reply")
	sub["isSidechain"] = true
	tr.addAll(
		[]tline{
			humanPrompt(at, "first prompt"),
			assistantSays(at.Add(10*time.Second), "Reading the code."),
			toolUse(at.Add(20*time.Second), "toolu_1", "Read", map[string]any{"file_path": "app/models/point.rb"}),
			toolResult(at.Add(30*time.Second), "toolu_1", "class Point", nil, false),
			assistantSays(at.Add(40*time.Second), "The refund path drops points."),
			humanPrompt(at.Add(time.Minute), "second prompt"),
			assistantSays(at.Add(2*time.Minute), "Which table?"),
			humanPrompt(at.Add(3*time.Minute), "third prompt"),
			toolUse(at.Add(3*time.Minute+time.Second), "toolu_2", "Read", map[string]any{"file_path": "db/schema.rb"}),
			sub,
			humanPrompt(at.Add(5*time.Minute), "fourth prompt"),
		},
	)

	got, err := ClaudeUserTurns(tr.path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		nil,
		{"Reading the code.\nThe refund path drops points."},
		{"Reading the code.\nThe refund path drops points.", "Which table?"},
		{"Reading the code.\nThe refund path drops points.", "Which table?"}, // the turn before it said nothing
	}
	if len(got) != len(want) {
		t.Fatalf("turns = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if !slices.Equal(got[i].Before, w) {
			t.Errorf("%q: Before = %q, want %q", got[i].Text, got[i].Before, w)
		}
	}
	late, err := ClaudeUserTurns(tr.path, at.Add(3*time.Minute))
	if err != nil || len(late) != 2 || !slices.Equal(late[0].Before, want[2]) {
		t.Fatalf("turns since 3m = %+v, %v; want the replies read before since too", late, err)
	}
}

// The user entries Claude Code writes itself are no turns anybody typed: a
// slash command's output (stdout, stderr, the caveat before it), a bash-mode
// command's output and the summary a compaction leaves. They end no turn of
// the agent's either. The command a person typed, slash or bash, stays.
func TestClaudeUserTurnsSkipWhatClaudeCodeWritesItself(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	at := t0
	summary := userPrompt(at.Add(8*time.Minute), "This session is being continued from a previous conversation that ran out of context.")
	summary["isCompactSummary"] = true
	tr.add(
		humanPrompt(at, "first prompt"),
		assistantSays(at.Add(10*time.Second), "Reading the code."),
		userPrompt(at.Add(time.Minute), "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"),
		userPrompt(at.Add(2*time.Minute), "<local-command-stdout>Set model to opus</local-command-stdout>"),
		userPrompt(at.Add(3*time.Minute), "<local-command-stderr>no such model</local-command-stderr>"),
		userPrompt(at.Add(4*time.Minute), "<local-command-caveat>Caveat: the messages below were generated by the user while running local commands.</local-command-caveat>"),
		userPrompt(at.Add(5*time.Minute), "<bash-input>git status</bash-input>"),
		userPrompt(at.Add(6*time.Minute), "<bash-stdout>nothing to commit</bash-stdout><bash-stderr></bash-stderr>"),
		userPrompt(at.Add(7*time.Minute), "  <bash-stderr>fatal: not a git repository</bash-stderr>"),
		summary,
		humanPrompt(at.Add(9*time.Minute), "second prompt"),
	)

	got, err := ClaudeUserTurns(tr.path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, u := range got {
		texts = append(texts, u.Text)
	}
	want := []string{"first prompt", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>",
		"<bash-input>git status</bash-input>", "second prompt"}
	if !slices.Equal(texts, want) {
		t.Fatalf("turns %q\nwant  %q", texts, want)
	}
	if last := got[len(got)-1]; !slices.Equal(last.Before, []string{"Reading the code."}) {
		t.Fatalf("the last turn's Before = %q, want the agent's reply, the records ending no turn", last.Before)
	}
}

// SlashCommand names the slash command a Claude Code command record holds,
// whatever the order of its tags; any other text is no command.
func TestSlashCommandReadsAClaudeCommandRecord(t *testing.T) {
	for _, c := range []struct {
		text, want string
	}{
		{"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>", "/model"},
		{"<command-message>compact</command-message>\n<command-name>/compact</command-name>\n<command-args></command-args>", "/compact"},
		{"  <command-name>exit</command-name>", "/exit"},
		{"please run <command-name>/model</command-name> for me", ""},
		{"/model opus", ""},
		{"<command-name></command-name>", ""},
	} {
		got, ok := SlashCommand(c.text)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("SlashCommand(%q) = %q, %v; want %q", c.text, got, ok, c.want)
		}
	}
}

// A Codex user message carries the agent_message texts of the two turns
// before it, oldest first.
func TestCodexUserTurnsCarryTheAgentsTwoTurnsBefore(t *testing.T) {
	home := t.TempDir()
	at := t0
	line := func(at time.Time, typ, message string) map[string]any {
		return map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "event_msg",
			"payload": map[string]any{"type": typ, "message": message}}
	}
	codexRollout(t, home, at,
		map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "session_meta", "payload": map[string]any{"id": codexSID}},
		line(at.Add(time.Second), "user_message", "find the cause"),
		line(at.Add(2*time.Second), "agent_message", "Reading the ledger."),
		line(at.Add(3*time.Second), "agent_message", "The cause is the refund job."),
		taskComplete(at.Add(4*time.Second), "", ""),
		line(at.Add(time.Minute), "user_message", "and the fix?"),
		line(at.Add(2*time.Minute), "agent_message", "Skip expired points."),
		line(at.Add(3*time.Minute), "user_message", "ok"),
		line(at.Add(4*time.Minute), "agent_message", "Done."),
		line(at.Add(5*time.Minute), "user_message", "thanks"),
	)
	path, err := rolloutIn(home, codexSID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CodexUserTurns(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		nil,
		{"Reading the ledger.\nThe cause is the refund job."},
		{"Reading the ledger.\nThe cause is the refund job.", "Skip expired points."},
		{"Skip expired points.", "Done."},
	}
	if len(got) != len(want) {
		t.Fatalf("turns = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if !slices.Equal(got[i].Before, w) {
			t.Errorf("%q: Before = %q, want %q", got[i].Text, got[i].Before, w)
		}
	}
}

// A prompt typed while the agent worked is no user entry: Claude Code
// queues it (an enqueue line) and either starts a turn with it later or
// takes it into the turn it is in (a queued_command attachment). Both are
// what was typed; a queued task notification is not.
func TestClaudeQueuedTurnsReadPromptsTypedDuringATurn(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	at := t0
	tr.addAll(
		[]tline{humanPrompt(at, "first prompt"), assistantSays(at.Add(10*time.Second), "working")},
		[]tline{enqueued(at.Add(time.Minute), "say one line")},
		taken(at.Add(2*time.Minute), "say one line"),
		[]tline{enqueued(at.Add(3*time.Minute), "and another")},
		absorbed(at.Add(3*time.Minute), at.Add(4*time.Minute), "and another"),
		notification(at.Add(5*time.Minute), "toolu_1", "bg1", "completed"),
	)

	got, err := ClaudeQueuedTurns(tr.path, at.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	want := []UserTurn{
		{At: at.Add(time.Minute), Text: "say one line", Origin: OriginQueued},
		{At: at.Add(3 * time.Minute), Text: "and another", Origin: OriginQueued},
		{At: at.Add(3 * time.Minute), Text: "and another", Origin: OriginQueued},
	}
	if !slices.EqualFunc(got, want, func(a, b UserTurn) bool { return a.At.Equal(b.At) && a.Text == b.Text && a.Origin == b.Origin }) {
		t.Fatalf("queued turns = %+v\nwant          %+v", got, want)
	}
	if got, err := ClaudeQueuedTurns(tr.path, at.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("queued turns since an hour on = %+v, %v", got, err)
	}
}

// A Codex rollout records what the operator typed as a user_message event
// with the text in payload.message. The response items that carry the same
// text and the instructions Codex adds are no user messages.
func TestCodexUserTurnsReadTheMessageText(t *testing.T) {
	home := t.TempDir()
	at := t0
	line := func(at time.Time, typ string, payload map[string]any) map[string]any {
		payload["type"] = typ
		return map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": payload}
	}
	codexRollout(t, home, at,
		map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "session_meta", "payload": map[string]any{"id": codexSID}},
		line(at.Add(time.Second), "user_message", map[string]any{"message": "review"}),
		map[string]any{"timestamp": at.Add(2 * time.Second).UTC().Format(time.RFC3339Nano), "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "review"}}}},
		line(at.Add(3*time.Second), "agent_message", map[string]any{"message": "done"}),
		taskComplete(at.Add(4*time.Second), "", ""),
		line(at.Add(time.Minute), "user_message", map[string]any{"message": "second\nline", "images": []any{}}),
		line(at.Add(2*time.Minute), "user_message", map[string]any{"message": ""}),
		line(at.Add(3*time.Minute), "user_message", map[string]any{"message": "third"}),
	)
	path, err := rolloutIn(home, codexSID)
	if err != nil {
		t.Fatal(err)
	}

	got, err := CodexUserTurns(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []UserTurn{
		{At: at.Add(time.Second), Text: "review"},
		{At: at.Add(time.Minute), Text: "second\nline"},
		{At: at.Add(3 * time.Minute), Text: "third"},
	}
	if !slices.EqualFunc(got, want, func(a, b UserTurn) bool { return a.At.Equal(b.At) && a.Text == b.Text && a.Origin == b.Origin }) {
		t.Fatalf("turns = %+v\nwant   %+v", got, want)
	}

	got, err = CodexUserTurns(path, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "second\nline" || got[1].Text != "third" {
		t.Fatalf("turns since 1m = %+v", got)
	}
	if _, err := CodexUserTurns(filepath.Join(home, "gone.jsonl"), time.Time{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing rollout = %v, want a not-exist error", err)
	}
}

// A rollout longer than the tail TurnError reads still gives the turns that
// sit before it: a long turn's tool output must not push the prompt out.
func TestCodexUserTurnsReadTheWholeRollout(t *testing.T) {
	home := t.TempDir()
	at := t0
	lines := []map[string]any{{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "the prompt"}}}
	filler := map[string]any{"timestamp": at.Add(time.Second).UTC().Format(time.RFC3339Nano), "type": "event_msg",
		"payload": map[string]any{"type": "exec_command_end", "aggregated_output": strings.Repeat("x", 64<<10)}}
	for range rolloutTail/(64<<10) + 2 {
		lines = append(lines, filler)
	}
	codexRollout(t, home, at, lines...)
	path, err := rolloutIn(home, codexSID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CodexUserTurns(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "the prompt" {
		t.Fatalf("turns = %+v, want the prompt before a tail of tool output", got)
	}
}

// TranscriptPath is the file a session's turns are read from: a Claude
// session's transcript, a Codex session's rollout, each under the session's
// own config directory. A shell pane, a session without an id and a missing
// file have none.
func TestTranscriptPathFindsTheFileOfAClaudeOrACodexSession(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	tr.add(humanPrompt(t0, "hello"))
	home := t.TempDir()
	e.m.d.CodexHome = home
	codexRollout(t, home, t0, taskComplete(t0, "", ""))

	claude, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleClaude), AgentName: new("a"),
		AgentKind: new(KindClaude), SessionID: new(claudeSID), Cwd: new("/Users/x/Projects/talkable.review1"),
		HerdrPaneID: new("p1"), State: store.SessionLive, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.m.TranscriptPath(e.ctx, claude)
	if err != nil || got != tr.path {
		t.Fatalf("claude TranscriptPath = %q, %v, want %q", got, err, tr.path)
	}

	codex, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(RoleJudge), AgentName: new("b"),
		AgentKind: new(KindCodex), SessionID: new(codexSID), HerdrPaneID: new("p2"), State: store.SessionLive, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	want, err := rolloutIn(home, codexSID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = e.m.TranscriptPath(e.ctx, codex); err != nil || got != want {
		t.Fatalf("codex TranscriptPath = %q, %v, want %q", got, err, want)
	}

	// No session id, no file, no agent: no path.
	noID := codex
	noID.SessionID = nil
	missing := claude
	missing.SessionID = new("0a1b2c3d-0000-4000-8000-0000000000ff")
	shell := claude
	shell.AgentKind, shell.AgentName = new(KindShell), nil
	for name, s := range map[string]store.Session{"no id": noID, "no file": missing, "shell": shell} {
		if p, err := e.m.TranscriptPath(e.ctx, s); err == nil {
			t.Errorf("%s: TranscriptPath = %q, want an error", name, p)
		}
	}
}
