package cli

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// testClaudeScript is the Claude hook script's path in the settings tests.
const testClaudeScript = "/Users/x/.claude/hooks/magnum-agent-hook.sh"

// testEntries are magnum's entries for agent's events with the script at
// script.
func testEntries(agent, script string, events []hookEvent) []hookEntry {
	out := make([]hookEntry, len(events))
	for i, e := range events {
		out[i] = hookEntry{Event: e.name, Command: hookCommand(script, agent, e.arg), Timeout: e.timeout}
	}
	return out
}

// herdrSettings is a Claude settings.json with herdr's SessionStart hook,
// another tool's Stop hook and a setting written on one line, indented by
// two spaces.
const herdrSettings = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bash ~/.claude/hooks/herdr-agent-state.sh session",
            "timeout": 10
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/moshi-hook claude-hook"
          }
        ]
      }
    ]
  },
  "statusLine": {"type": "command", "command": "ccstatusline"}
}
`

// herdrSettingsInstalled is herdrSettings after the install: magnum's
// groups appended to SessionStart and Stop, and the three events it lacked
// added after the last one.
const herdrSettingsInstalled = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bash ~/.claude/hooks/herdr-agent-state.sh session",
            "timeout": 10
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude session_start",
            "async": true,
            "timeout": 5
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/opt/homebrew/bin/moshi-hook claude-hook"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop",
            "async": true,
            "timeout": 5
          }
        ]
      }
    ],
    "StopFailure": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop_failure",
            "async": true,
            "timeout": 5
          }
        ]
      }
    ],
    "PermissionRequest": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude permission_request",
            "async": true,
            "timeout": 5
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude session_end",
            "async": true,
            "timeout": 1
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude user_prompt_submit",
            "async": true,
            "timeout": 5
          }
        ]
      }
    ]
  },
  "statusLine": {"type": "command", "command": "ccstatusline"}
}
`

// leadingPairs is a line's indentation of two-space steps.
var leadingPairs = regexp.MustCompile(`(?m)^(  )+`)

// reindent is s with each two-space step of its lines' indentation made
// unit, and its line ends made nl.
func reindent(s, unit, nl string) string {
	s = leadingPairs.ReplaceAllStringFunc(s, func(lead string) string { return strings.Repeat(unit, len(lead)/2) })
	return strings.ReplaceAll(s, "\n", nl)
}

// The settings styles every splice test runs in: the golden above as
// written, with tabs, and with CRLF line ends.
var hookStyles = []struct{ name, unit, nl string }{
	{"two spaces", "  ", "\n"},
	{"tabs", "\t", "\n"},
	{"tabs and CRLF", "\t", "\r\n"},
	{"four spaces and CRLF", "    ", "\r\n"},
}

// The install keeps every byte of the settings but the groups it appends,
// in the file's own indentation and line ends, and never moves another
// tool's group: Codex keys its trust by a hook's place, and an insertion
// before another hook cost every later hook its trust on 2026-10-22.
func TestInstallKeepsEveryByteOfTheSettingsButTheAppendedGroups(t *testing.T) {
	t.Parallel()
	entries := testEntries("claude", testClaudeScript, claudeHookEvents)
	for _, st := range hookStyles {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			in, want := reindent(herdrSettings, st.unit, st.nl), reindent(herdrSettingsInstalled, st.unit, st.nl)
			out, changes, err := spliceHooks([]byte(in), agenthooks.AgentClaude, entries)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != want {
				t.Fatalf("install =\n%s\nwant\n%s", out, want)
			}
			var added []string
			for _, c := range changes {
				if c.rewrote {
					t.Errorf("change %+v rewrote an entry, want only additions", c)
				}
				added = append(added, c.event)
			}
			if !slices.Equal(added, []string{"Stop", "StopFailure", "PermissionRequest", "SessionStart", "SessionEnd", "UserPromptSubmit"}) {
				t.Errorf("added %v", added)
			}
		})
	}
}

// codexStop is one Codex Stop entry of magnum's, for the shorter tests.
var codexStop = []hookEntry{{Event: "Stop", Command: "sh '/c/magnum-agent-hook.sh' codex stop", Timeout: 5}}

// codexStopGroup is codexStop's group as the install writes it at base.
func codexStopGroup(base, unit string) string {
	l := func(depth int, s string) string { return base + strings.Repeat(unit, depth) + s + "\n" }
	return "{\n" +
		l(1, `"hooks": [`) +
		l(2, `{`) +
		l(3, `"type": "command",`) +
		l(3, `"command": "sh '/c/magnum-agent-hook.sh' codex stop",`) +
		l(3, `"async": true,`) +
		l(3, `"timeout": 5`) +
		l(2, `}`) +
		l(1, `]`) +
		base + "}"
}

// A file without a hooks object gets one, a hooks object without the
// event gets the event's key, an empty event array gets the group inside
// it, and a file that does not exist yet is made; a file on one line stays
// on one line.
func TestInstallAddsTheHooksObjectOrTheEventKeyWhenMissing(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"no hooks object": {
			"{\n  \"model\": \"opus\"\n}\n",
			"{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"Stop\": [\n      " + codexStopGroup("      ", "  ") + "\n    ]\n  }\n}\n",
		},
		"no event key": {
			"{\n  \"hooks\": {\n    \"SessionStart\": []\n  }\n}\n",
			"{\n  \"hooks\": {\n    \"SessionStart\": [],\n    \"Stop\": [\n      " + codexStopGroup("      ", "  ") + "\n    ]\n  }\n}\n",
		},
		"an empty event array": {
			"{\n\t\"hooks\": {\n\t\t\"Stop\": []\n\t}\n}",
			"{\n\t\"hooks\": {\n\t\t\"Stop\": [\n\t\t\t" + codexStopGroup("\t\t\t", "\t") + "\n\t\t]\n\t}\n}",
		},
		"an empty hooks object": {
			"{\n  \"hooks\": {}\n}\n",
			"{\n  \"hooks\": {\n    \"Stop\": [\n      " + codexStopGroup("      ", "  ") + "\n    ]\n  }\n}\n",
		},
		"an empty file object": {
			"{}\n",
			"{\n  \"hooks\": {\n    \"Stop\": [\n      " + codexStopGroup("      ", "  ") + "\n    ]\n  }\n}\n",
		},
		"no file": {
			"",
			"{\n  \"hooks\": {\n    \"Stop\": [\n      " + codexStopGroup("      ", "  ") + "\n    ]\n  }\n}\n",
		},
		"one line": {
			`{"hooks": {"SessionStart": []}}`,
			`{"hooks": {"SessionStart": [], "Stop": [{"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]}]}}`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var in []byte
			if c.in != "" {
				in = []byte(c.in)
			}
			out, changes, err := spliceHooks(in, agenthooks.AgentClaude, codexStop) // a file's shape, read as Claude reads its root
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != c.want {
				t.Fatalf("install =\n%s\nwant\n%s", out, c.want)
			}
			if len(changes) != 1 || changes[0].event != "Stop" || changes[0].rewrote {
				t.Errorf("changes = %+v", changes)
			}
		})
	}
}

// An install over its own install changes nothing, wherever magnum's group
// stands, and says so with no change.
func TestASecondInstallChangesNothing(t *testing.T) {
	t.Parallel()
	entries := testEntries("claude", testClaudeScript, claudeHookEvents)
	for _, st := range hookStyles {
		want := reindent(herdrSettingsInstalled, st.unit, st.nl)
		out, changes, err := spliceHooks([]byte(want), agenthooks.AgentClaude, entries)
		if err != nil || string(out) != want || len(changes) != 0 {
			t.Fatalf("%s: second install = %v changes %+v, out changed: %v", st.name, err, changes, string(out) != want)
		}
	}
	// A magnum group that another tool's group came after stays where it is.
	moved := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]}, {"hooks": [{"type": "command", "command": "moshi-hook"}]}]}}`
	if out, changes, err := spliceHooks([]byte(moved), agenthooks.AgentCodex, codexStop); err != nil || string(out) != moved || len(changes) != 0 {
		t.Fatalf("not last = %v %+v\n%s", err, changes, out)
	}
}

// A magnum entry whose command, timeout or async drifted is rewritten in
// its place (its group keeps its index), alone, and the change says it was
// rewritten, which costs its Codex trust.
func TestADriftedCommandOrTimeoutIsRewrittenInPlace(t *testing.T) {
	t.Parallel()
	entries := testEntries("claude", testClaudeScript, claudeHookEvents)
	stop := `            "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop",
            "async": true,
            "timeout": 5`
	drifts := map[string]string{
		"timeout": strings.Replace(stop, `"timeout": 5`, `"timeout": 30`, 1),
		"command": strings.Replace(stop, "/Users/x/.claude/hooks/", "/Users/x/old/", 1),
		"async":   strings.Replace(stop, `"async": true,`, `"async": false,`, 1),
		"no async": strings.Replace(stop, `
            "async": true,`, "", 1),
	}
	for name, drifted := range drifts {
		in := strings.Replace(herdrSettingsInstalled, stop, drifted, 1)
		if in == herdrSettingsInstalled {
			t.Fatalf("%s: the drift did not apply", name)
		}
		out, changes, err := spliceHooks([]byte(in), agenthooks.AgentClaude, entries)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != herdrSettingsInstalled {
			t.Errorf("%s: install =\n%s\nwant\n%s", name, out, herdrSettingsInstalled)
		}
		if len(changes) != 1 || changes[0].event != "Stop" || !changes[0].rewrote || changes[0].place != "stop:1:0" {
			t.Errorf("%s: changes = %+v, want Stop rewritten at stop:1:0", name, changes)
		}
	}
	// A handler on one line is rewritten on one line.
	in := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "timeout": 9}]}]}}`
	want := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]}]}}`
	if out, changes, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codexStop); err != nil || string(out) != want || len(changes) != 1 || !changes[0].rewrote {
		t.Fatalf("one line = %v %+v\n%s", err, changes, out)
	}
}

// A settings file that is no JSON, or whose hooks or event are not what
// Claude and Codex read, is refused with nothing written.
func TestInstallRefusesSettingsThatAreNotValidJSON(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"cut short":              `{"hooks": {"Stop": [`,
		"a trailing comma":       `{"hooks": {},}`,
		"no object":              `["hooks"]`,
		"hooks not an object":    `{"hooks": []}`,
		"event not an array":     `{"hooks": {"Stop": {}}}`,
		"two hooks objects":      `{"hooks": {}, "hooks": {}}`,
		"two keys for one event": `{"hooks": {"Stop": [], "Stop": []}}`,
		"only spaces":            "  \n",
	} {
		out, changes, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codexStop)
		if err == nil || out != nil || changes != nil {
			t.Errorf("%s: install = %v, %+v, %q; want a refusal", name, err, changes, out)
		}
		if _, _, err := unspliceHooks([]byte(in), agenthooks.AgentCodex); err == nil {
			t.Errorf("%s: uninstall took it", name)
		}
		if out, _, err := spliceHooks([]byte(in), agenthooks.AgentClaude, codexStop); err == nil || out != nil {
			t.Errorf("%s: Claude's install = %v, want a refusal", name, err)
		}
	}
}

// Uninstall takes away exactly what the install added, so an install and
// an uninstall give back the file byte for byte, in every style.
func TestUninstallGivesBackTheFileTheInstallFound(t *testing.T) {
	t.Parallel()
	for _, st := range hookStyles {
		in, installed := reindent(herdrSettings, st.unit, st.nl), reindent(herdrSettingsInstalled, st.unit, st.nl)
		out, removed, err := unspliceHooks([]byte(installed), agenthooks.AgentClaude)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Fatalf("%s: uninstall =\n%s\nwant\n%s", st.name, out, in)
		}
		if removed != len(claudeHookEvents) {
			t.Errorf("%s: removed %d, want %d", st.name, removed, len(claudeHookEvents))
		}
	}
	for _, in := range []string{"{\n  \"model\": \"opus\"\n}\n", "{\n  \"hooks\": {\n    \"SessionStart\": []\n  }\n}\n", `{"hooks": {"SessionStart": []}}`} {
		installed, _, err := spliceHooks([]byte(in), agenthooks.AgentClaude, codexStop)
		if err != nil {
			t.Fatal(err)
		}
		if out, _, err := unspliceHooks(installed, agenthooks.AgentClaude); err != nil || string(out) != in {
			t.Errorf("round trip of %q = %v\n%s", in, err, out)
		}
	}
	if out, removed, err := unspliceHooks([]byte(herdrSettings), agenthooks.AgentClaude); err != nil || string(out) != herdrSettings || removed != 0 {
		t.Errorf("nothing of magnum's = %v %d\n%s", err, removed, out)
	}
}

// Uninstall removes magnum's handler and leaves the group that holds
// another tool's handler too; a group with nothing else in it goes.
func TestUninstallRemovesOnlyMagnumsHandlers(t *testing.T) {
	t.Parallel()
	in := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi-hook"}, {"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]}], "Interrupt": [{"matcher": "", "hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt"}]}, {"hooks": []}]}}`
	want := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi-hook"}]}], "Interrupt": [{"hooks": []}]}}`
	out, removed, err := unspliceHooks([]byte(in), agenthooks.AgentCodex)
	if err != nil || string(out) != want || removed != 2 {
		t.Fatalf("uninstall = %v %d\n%s\nwant\n%s", err, removed, out, want)
	}
}

// Removing a Codex group or handler that is not last moves every later
// hook of its event to another place, where Codex no longer trusts it:
// the shifts name each one.
func TestUninstallNamesTheCodexHooksWhosePlaceItMoves(t *testing.T) {
	t.Parallel()
	in := `{"hooks": {
  "Stop": [
    {"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]},
    {"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook codex-hook"}]}
  ],
  "SessionStart": [
    {"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex session_start"}, {"type": "command", "command": "bash '/c/herdr-agent-state.sh' session"}]}
  ],
  "PermissionRequest": [
    {"hooks": [{"type": "command", "command": "notchbar"}]},
    {"hooks": [{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex permission_request"}]}
  ]
}}`
	out, removed, err := unspliceHooks([]byte(in), agenthooks.AgentCodex)
	if err != nil || removed != 3 {
		t.Fatalf("uninstall = %v %d", err, removed)
	}
	shifts, err := hookShifts([]byte(in), out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range shifts {
		got = append(got, s.String())
	}
	want := []string{"moshi-hook (stop:1:0 to stop:0:0)", "herdr-agent-state.sh (session_start:0:1 to session_start:0:0)"}
	if !slices.Equal(got, want) {
		t.Fatalf("shifts = %q, want %q", got, want)
	}
	base := []byte(`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook codex-hook"}]}]}}`)
	appended, _, err := spliceHooks(base, agenthooks.AgentCodex, testEntries(agenthooks.AgentCodex, "/c/magnum-agent-hook.sh", codexHookEvents))
	if err != nil {
		t.Fatal(err)
	}
	if shifts, err := hookShifts(appended, base); err != nil || len(shifts) != 0 {
		t.Fatalf("appended groups' removal = %v %v, want no shift", shifts, err)
	}
}

// Status reads each event's entry: current, drifted or absent, and the
// places of magnum's handlers, which Codex keys its trust by.
func TestReadHookEntriesTellsEachEventsState(t *testing.T) {
	t.Parallel()
	entries := testEntries("claude", testClaudeScript, claudeHookEvents)
	got, err := readHookEntries([]byte(herdrSettingsInstalled), agenthooks.AgentClaude, entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.state != entryCurrent {
			t.Errorf("%s = %d, want current", e.event, e.state)
		}
	}
	if got[0].event != "Stop" || !slices.Equal(got[0].places, []string{"stop:1:0"}) || !slices.Equal(got[3].places, []string{"session_start:1:0"}) {
		t.Errorf("places = %+v", got)
	}
	drifted := strings.Replace(herdrSettings, `"/opt/homebrew/bin/moshi-hook claude-hook"`, `"sh /elsewhere/magnum-agent-hook.sh claude stop"`, 1)
	got, err = readHookEntries([]byte(drifted), agenthooks.AgentClaude, entries)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, e := range got {
		states[e.event] = e.state
	}
	if states["Stop"] != entryDrifted || states["SessionStart"] != entryAbsent || states["SessionEnd"] != entryAbsent {
		t.Errorf("states = %v", states)
	}
	if got, err := readHookEntries(nil, agenthooks.AgentClaude, entries); err != nil || got[0].state != entryAbsent {
		t.Errorf("no file = %v %+v", err, got)
	}
}

// The timeouts are part of Codex's trust hash (with the command and async),
// so they never change once installed: a change is a rewrite in place,
// which asks for the trust again in every Codex home. Each is within the
// agent's documented limits. Codex runs Interrupt hooks for 1 s by default
// and at most 3 s, background ones too; its Stop, SessionStart and
// PermissionRequest default to 600 s. Claude does not enforce a timeout on
// an async hook, but a SessionEnd hook's own timeout raises the 1.5 s budget
// all SessionEnd hooks share, so magnum's stays under it. The hook bounds
// itself to 300 ms (agenthooks.HookBound), so 1 s is room enough where the
// limit is tight, and 5 s elsewhere.
func TestTheHookTimeoutsStayWithinTheAgentsLimitsAndNeverChange(t *testing.T) {
	t.Parallel()
	want := map[string]map[string]int{
		"claude": {"Stop": 5, "StopFailure": 5, "PermissionRequest": 5, "SessionStart": 5, "SessionEnd": 1, "UserPromptSubmit": 5},
		"codex":  {"Stop": 5, "SessionStart": 5, "PermissionRequest": 5, "Interrupt": 1, "UserPromptSubmit": 5},
	}
	for agent, events := range map[string][]hookEvent{"claude": claudeHookEvents, "codex": append(slices.Clone(codexHookEvents), codexPromptSubmit)} {
		if len(events) != len(want[agent]) {
			t.Fatalf("%s hooks %d events, want %d", agent, len(events), len(want[agent]))
		}
		for _, e := range events {
			if e.timeout != want[agent][e.name] {
				t.Errorf("%s %s timeout = %d s, want %d s (frozen: it is in Codex's trust hash)", agent, e.name, e.timeout, want[agent][e.name])
			}
			if float64(e.timeout) < agenthooks.HookBound.Seconds() {
				t.Errorf("%s %s timeout %d s is under the hook's own bound", agent, e.name, e.timeout)
			}
		}
	}
	if codexInterrupt := want["codex"]["Interrupt"]; codexInterrupt < 1 || codexInterrupt > 3 {
		t.Errorf("Codex's Interrupt takes 1 to 3 s, not %d", codexInterrupt)
	}
	if float64(want["claude"]["SessionEnd"]) > 1.5 {
		t.Errorf("Claude's SessionEnd timeout %d s raises the shared 1.5 s budget", want["claude"]["SessionEnd"])
	}
}

// Claude and Codex read a handler's keys exactly, so a handler whose key is
// "Command" has no command and is no entry of magnum's; a key twice in a
// group or a handler is refused, as at the event's level.
func TestHandlersAreReadWithExactKeysAndNoKeyTwice(t *testing.T) {
	t.Parallel()
	capital := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "Command": "sh '/c/magnum-agent-hook.sh' codex stop", "async": true, "timeout": 5}]}]}}`
	got, err := readHookEntries([]byte(capital), agenthooks.AgentCodex, codexStop)
	if err != nil || got[0].state != entryAbsent {
		t.Fatalf("a Command key = %+v %v, want no entry", got, err)
	}
	for name, in := range map[string]string{
		"a handler key twice": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x", "command": "sh '/c/magnum-agent-hook.sh' codex stop"}]}]}}`,
		"a group key twice":   `{"hooks": {"Stop": [{"hooks": [], "hooks": [{"type": "command", "command": "x"}]}]}}`,
	} {
		if out, _, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codexStop); err == nil || out != nil {
			t.Errorf("%s: install = %v, want a refusal", name, err)
		}
	}
}

// Claude reads a key that is twice in an object as its JSON parser does,
// the last one wins, so another tool's quirk never makes magnum refuse a
// settings.json that Claude reads; only a key twice in a group or a hook
// that holds magnum's own command, or twice for an event magnum hooks, is
// refused there. Codex's derived structs reject a key twice, so Codex skips
// such a hooks.json, and magnum refuses it with that reason.
func TestClaudeReadsAKeyTwiceAsClaudeDoesAndCodexRefusesIt(t *testing.T) {
	t.Parallel()
	entries := testEntries(agenthooks.AgentClaude, testClaudeScript, claudeHookEvents)
	quirks := map[string]string{
		"another tool's hook with a timeout twice":  `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook stop", "timeout": 5, "timeout": 10}]}]}}`,
		"another tool's group with a matcher twice": `{"hooks": {"SessionStart": [{"matcher": "", "matcher": "startup", "hooks": [{"type": "command", "command": "bash herdr.sh session"}]}]}}`,
		"an event magnum does not hook, twice":      `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "x", "command": "y"}]}], "PreToolUse": []}}`,
	}
	for name, in := range quirks {
		out, changes, err := spliceHooks([]byte(in), agenthooks.AgentClaude, entries)
		if err != nil || len(changes) != len(entries) {
			t.Fatalf("%s: Claude's install = %v, %d changes", name, err, len(changes))
		}
		got, err := readHookEntries(out, agenthooks.AgentClaude, entries)
		if err != nil {
			t.Fatalf("%s: status = %v", name, err)
		}
		for _, e := range got {
			if e.state != entryCurrent {
				t.Errorf("%s: %s = %d after the install, want current", name, e.event, e.state)
			}
		}
		if back, _, err := unspliceHooks(out, agenthooks.AgentClaude); err != nil || string(back) != in {
			t.Errorf("%s: uninstall = %v\n%s", name, err, back)
		}
		codex := testEntries(agenthooks.AgentCodex, "/c/magnum-agent-hook.sh", codexHookEvents)
		if _, _, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codex); err == nil || !strings.Contains(err.Error(), "Codex skips a hooks.json with one of its own keys twice") {
			t.Errorf("%s: Codex's install = %v, want a refusal that says Codex skips the file", name, err)
		}
	}
	// The last of a key twice wins: the group's last "hooks" is the one read.
	last := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi"}], "hooks": []}]}}`
	if out, _, err := spliceHooks([]byte(last), agenthooks.AgentClaude, entries[:1]); err != nil || !strings.Contains(string(out), "claude stop") {
		t.Fatalf("a group's hooks twice = %v\n%s", err, out)
	}
	for name, in := range map[string]string{
		"magnum's hook with a key twice":     `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop", "async": true, "async": false, "timeout": 5}]}]}}`,
		"magnum's group with a key twice":    `{"hooks": {"Stop": [{"matcher": "", "matcher": "x", "hooks": [{"type": "command", "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop"}]}]}}`,
		"magnum's command in an earlier key": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sh '/Users/x/.claude/hooks/magnum-agent-hook.sh' claude stop", "command": "moshi"}]}]}}`,
		"an event magnum hooks, twice":       `{"hooks": {"Stop": [], "Stop": []}}`,
	} {
		if out, _, err := spliceHooks([]byte(in), agenthooks.AgentClaude, entries); err == nil || out != nil {
			t.Errorf("%s: Claude's install = %v, want a refusal", name, err)
		}
	}
}

// A rewrite that Codex hashes the same as the hook it replaces changes
// nothing Codex trusts, so it needs no record: Codex clamps an Interrupt
// timeout to 1 to 3 s (none is 1 s, magnum's value; 9 is 3 s), and reads a
// missing async as false. One that changes the command, the timeout or
// async as Codex reads them does need one.
func TestARewriteSaysWhetherCodexTrustsTheNewHookAsTheOld(t *testing.T) {
	t.Parallel()
	interrupt := []hookEntry{{Event: "Interrupt", Command: "sh '/c/magnum-agent-hook.sh' codex interrupt", Timeout: 1}}
	group := func(handler string) string {
		return `{"hooks": {"Interrupt": [{"hooks": [` + handler + `]}]}}`
	}
	for handler, newTrust := range map[string]bool{
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "async": true, "timeout": 0}`:   false,
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "async": true}`:                 false,
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "async": true, "timeout": 9}`:   true, // Codex clamps it to 3, not 1
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "async": false, "timeout": 1}`:  true,
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "timeout": 1}`:                  true,
		`{"type": "command", "command": "sh '/old/magnum-agent-hook.sh' codex interrupt", "async": true, "timeout": 1}`: true,
		`{"type": "command", "command": "sh '/c/magnum-agent-hook.sh' codex interrupt", "async": true, "timeout": 2}`:   true,
	} {
		out, changes, err := spliceHooks([]byte(group(handler)), agenthooks.AgentCodex, interrupt)
		if err != nil || len(changes) != 1 || !changes[0].rewrote {
			t.Fatalf("%s: install = %v %+v\n%s", handler, err, changes, out)
		}
		if changes[0].newTrust != newTrust {
			t.Errorf("%s: needs a new trust = %v, want %v", handler, changes[0].newTrust, newTrust)
		}
	}
}

// Codex's structs reject a key twice only among the keys they define (its
// events in "hooks"; "matcher" and "hooks" in a group; a handler's type,
// command, commandWindows, timeout, async, statusMessage,
// additionalContextLimit and an MCP hook's server, tool and input), and
// ignore any other key, twice or not: so magnum refuses only those, and
// installs into a hooks.json with an unknown key twice, as Codex reads it.
func TestCodexRefusesAKeyTwiceOnlyWhereItsOwnStructsReadIt(t *testing.T) {
	t.Parallel()
	codex := testEntries(agenthooks.AgentCodex, "/c/magnum-agent-hook.sh", codexHookEvents)
	for name, in := range map[string]string{
		"an unknown handler key twice": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi", "x-note": 1, "x-note": 2}]}]}}`,
		"an unknown group key twice":   `{"hooks": {"Stop": [{"x-note": 1, "x-note": 2, "hooks": [{"type": "command", "command": "moshi"}]}]}}`,
		"an unknown event twice":       `{"hooks": {"Notification": [], "Notification": []}}`,
	} {
		out, changes, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codex)
		if err != nil || len(changes) != len(codex) {
			t.Fatalf("%s: Codex's install = %v, %d changes", name, err, len(changes))
		}
		if back, _, err := unspliceHooks(out, agenthooks.AgentCodex); err != nil || string(back) != in {
			t.Errorf("%s: uninstall = %v\n%s", name, err, back)
		}
	}
	for name, in := range map[string]string{
		"a handler's commandWindows under both names": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi", "commandWindows": "a", "command_windows": "b"}]}]}}`,
		"a handler's statusMessage twice":             `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi", "statusMessage": "a", "statusMessage": "b"}]}]}}`,
		"an MCP hook's tool twice":                    `{"hooks": {"Stop": [{"hooks": [{"type": "mcp_tool", "server": "s", "tool": "a", "tool": "b"}]}]}}`,
		"a group's hooks twice":                       `{"hooks": {"Stop": [{"hooks": [], "hooks": []}]}}`,
		"an event Codex runs, twice":                  `{"hooks": {"PreModelRequest": [], "PreModelRequest": []}}`,
		"the root's description twice":                `{"description": "a", "description": "b", "hooks": {}}`,
	} {
		_, _, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codex)
		if err == nil || !strings.Contains(err.Error(), "Codex skips a hooks.json with one of its own keys twice") {
			t.Errorf("%s: Codex's install = %v, want a refusal with Codex's reason", name, err)
		}
	}
}

// Another tool's keys twice in an event magnum hooks, and a root key
// twice, keep every byte of a Claude settings.json through an install, a
// second install and an uninstall (the re-review's probe).
func TestAClaudeSettingsFileWithAnotherToolsKeysTwiceRoundTrips(t *testing.T) {
	t.Parallel()
	claude := testEntries(agenthooks.AgentClaude, testClaudeScript, claudeHookEvents)
	in := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "moshi"}], "hooks": [{"type": "command", "command": "moshi2", "timeout": 1, "timeout": 2}], "matcher": "", "matcher": "x"}]}, "model": "a", "model": "b"}`
	out, changes, err := spliceHooks([]byte(in), agenthooks.AgentClaude, claude)
	if err != nil || len(changes) != len(claude) {
		t.Fatalf("install = %v, %d changes", err, len(changes))
	}
	if again, ch, err := spliceHooks(out, agenthooks.AgentClaude, claude); err != nil || string(again) != string(out) || len(ch) != 0 {
		t.Fatalf("second install = %v %+v", err, ch)
	}
	if back, _, err := unspliceHooks(out, agenthooks.AgentClaude); err != nil || string(back) != in {
		t.Fatalf("uninstall = %v\n%s", err, back)
	}
}

// Codex reads hooks.json's root strictly (deny_unknown_fields): a root key
// besides description and hooks makes Codex skip the whole file, so magnum
// refuses it for Codex, whose hooks would never run, and keeps reading
// Claude's settings, whose root holds many keys.
func TestCodexSkipsAHooksJSONWithARootKeyItDoesNotKnow(t *testing.T) {
	t.Parallel()
	codex := testEntries(agenthooks.AgentCodex, "/c/magnum-agent-hook.sh", codexHookEvents)
	in := `{"x-note": 1, "hooks": {}}`
	out, _, err := spliceHooks([]byte(in), agenthooks.AgentCodex, codex)
	var skip *codexSkipsFileError
	if err == nil || out != nil || !errors.As(err, &skip) || skip.key != "x-note" ||
		!strings.Contains(err.Error(), `Codex skips this file: unknown key "x-note"`) {
		t.Fatalf("Codex's install = %v, want a refusal naming the key", err)
	}
	if _, err := readHookEntries([]byte(in), agenthooks.AgentCodex, codex); err == nil {
		t.Fatal("Codex's status read the file")
	}
	if _, _, err := spliceHooks([]byte(`{"description": "mine", "hooks": {}}`), agenthooks.AgentCodex, codex); err != nil {
		t.Fatalf("description = %v, want it read", err)
	}
	claude := testEntries(agenthooks.AgentClaude, testClaudeScript, claudeHookEvents)
	if _, _, err := spliceHooks([]byte(in), agenthooks.AgentClaude, claude); err != nil {
		t.Fatalf("Claude's install = %v, want its settings read", err)
	}
}
