package agenthooks

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	magnum "github.com/zhuravel/magnum"
)

// The embedded hook script names its owner, its agent and its version in
// its header, which the install's status reads back.
func TestTheHookScriptNamesItsOwnerAgentAndVersion(t *testing.T) {
	for _, agent := range []string{AgentClaude, AgentCodex} {
		b, err := Script(magnum.AgentHookScript, agent)
		if err != nil {
			t.Fatal(err)
		}
		head := string(b[:min(len(b), 400)])
		for _, want := range []string{"#!/bin/sh\n", "\n" + ScriptMarker, "MAGNUM_HOOK_ID=" + agent + "\n", "MAGNUM_HOOK_VERSION=1\n"} {
			if !strings.Contains(head, want) {
				t.Errorf("%s script's header lacks %q:\n%s", agent, want, head)
			}
		}
		id, version, managed := ScriptHeader(b)
		if id != agent || version != ScriptVersion || !managed {
			t.Errorf("ScriptHeader(%s script) = %q, %d, %v", agent, id, version, managed)
		}
	}
	if _, err := Script(magnum.AgentHookScript, "droid"); err == nil {
		t.Error("Script(droid) = no error")
	}
	if id, version, managed := ScriptHeader([]byte("#!/bin/sh\n# HERDR_INTEGRATION_ID=claude\n# HERDR_INTEGRATION_VERSION=11\n")); managed || id != "" || version != 0 {
		t.Errorf("ScriptHeader(herdr's script) = %q, %d, %v", id, version, managed)
	}
}

// installed writes the claude script into a temp dir and returns its path and
// a recorder that stands in for magnum: it writes its arguments and stdin
// into a file.
func installed(t *testing.T) (script, bin, record string) {
	t.Helper()
	dir := t.TempDir()
	b, err := Script(magnum.AgentHookScript, AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "magnum-agent-hook.sh")
	if err := os.WriteFile(script, b, 0o755); err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(dir, "record")
	bin = filepath.Join(dir, "magnum")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" > '"+record+"'\ncat >> '"+record+"'\necho noise\necho noise >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, bin, record
}

func runScript(t *testing.T, script string, env []string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", script, AgentClaude, NameStop)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	cmd.Stdin = strings.NewReader(`{"session_id":"s","last_assistant_message":"x"}`)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("the hook script failed: %v (stderr %q)", err, errb.String())
	}
	return out.String(), errb.String()
}

// Outside a magnum pane the script exits 0 at its first command: it runs
// nothing and prints nothing. In one it hands the hook to `magnum
// hook-event <agent> <event>` with the payload on stdin, and prints
// nothing of what that prints.
func TestTheHookScriptRunsMagnumOnlyInAMagnumPane(t *testing.T) {
	script, bin, record := installed(t)
	for name, env := range map[string][]string{
		"no socket":                   {"MAGNUM_HOOK_BIN=" + bin},
		"no binary":                   {"MAGNUM_HOOK_SOCKET=/tmp/x.sock"},
		"a binary that is no program": {"MAGNUM_HOOK_SOCKET=/tmp/x.sock", "MAGNUM_HOOK_BIN=" + record + ".missing"},
	} {
		if out, errOut := runScript(t, script, env); out != "" || errOut != "" {
			t.Errorf("%s: printed %q / %q", name, out, errOut)
		}
		if _, err := os.Stat(record); err == nil {
			t.Fatalf("%s: the script ran magnum", name)
		}
	}

	out, errOut := runScript(t, script, []string{"MAGNUM_HOOK_SOCKET=/tmp/x.sock", "MAGNUM_HOOK_BIN=" + bin})
	if out != "" || errOut != "" {
		t.Errorf("printed %q / %q, want nothing", out, errOut)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the script did not run magnum: %v", err)
	}
	if want := "hook-event claude stop\n" + `{"session_id":"s","last_assistant_message":"x"}`; string(got) != want {
		t.Fatalf("magnum got %q, want %q", got, want)
	}
}

// The script's first command is the socket check: nothing runs before it.
func TestTheHookScriptChecksTheSocketFirst(t *testing.T) {
	for line := range strings.Lines(string(magnum.AgentHookScript)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, `[ -n "${MAGNUM_HOOK_SOCKET:-}" ] || exit 0`) {
			t.Fatalf("the script's first command is %q", line)
		}
		return
	}
	t.Fatal("the script has no command")
}
