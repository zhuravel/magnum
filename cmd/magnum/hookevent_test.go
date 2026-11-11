package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// mainArgsEnv makes the test binary run main with these arguments
// (TestMain): the binary's own dispatch, without building magnum.
const mainArgsEnv = "MAGNUM_TEST_MAIN_ARGS"

func TestMain(m *testing.M) {
	if args := os.Getenv(mainArgsEnv); args != "" {
		os.Args = append([]string{os.Args[0]}, strings.Fields(args)...)
		main()
		os.Exit(99) // main returned instead of exiting
	}
	os.Exit(m.Run())
}

const stopPayload = `{"session_id":"0a1b2c3d-0000-4000-8000-00000000c1a0","hook_event_name":"Stop",` +
	`"last_assistant_message":"SECRET the review","background_tasks":[]}`

// listener serves a hook socket under a short temp dir and returns its path
// and the events it got.
func listener(t *testing.T) (string, <-chan agenthooks.Event) {
	t.Helper()
	dir, err := os.MkdirTemp("", "he") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "hooks.sock")
	l, err := agenthooks.Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	evs := make(chan agenthooks.Event, 10)
	done := make(chan struct{})
	go func() { defer close(done); l.Serve(ctx, func(ev agenthooks.Event) { evs <- ev }) }()
	t.Cleanup(func() { cancel(); <-done })
	return path, evs
}

// first is the first event the listener got after the sentinel was sent
// (the sentinel itself when nothing came before it).
func first(t *testing.T, path string, evs <-chan agenthooks.Event) agenthooks.Event {
	t.Helper()
	if err := agenthooks.Send(path, agenthooks.Event{V: 1, Agent: agenthooks.AgentCodex, Name: agenthooks.NameSessionEnd, Pane: "sentinel"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-evs:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("the listener got nothing")
		return agenthooks.Event{}
	}
}

// countingReader records whether it was read.
type countingReader struct {
	mu    sync.Mutex
	reads int
	r     *strings.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.r.Read(p)
}

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Outside a magnum pane hook-event returns at once: it reads no stdin and
// sends nothing.
func TestHookEventWithoutTheSocketSendsNothing(t *testing.T) {
	path, evs := listener(t)
	stdin := &countingReader{r: strings.NewReader(stopPayload)}
	hookEvent([]string{"claude", "stop"}, stdin, getenv(map[string]string{agenthooks.EnvPane: "w1:p1"}))
	if stdin.reads != 0 {
		t.Errorf("stdin read %d times", stdin.reads)
	}
	if ev := first(t, path, evs); ev.Pane != "sentinel" {
		t.Fatalf("the listener got %+v, want nothing before the sentinel", ev)
	}
}

// In a magnum pane it sends the parsed event, without the payload's PR text.
func TestHookEventSendsTheParsedEvent(t *testing.T) {
	path, evs := listener(t)
	hookEvent([]string{"claude", "stop"}, strings.NewReader(stopPayload),
		getenv(map[string]string{agenthooks.EnvSocket: path, agenthooks.EnvPane: "w1:p1"}))
	ev := first(t, path, evs)
	want := agenthooks.Event{V: 1, Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, SessionID: "0a1b2c3d-0000-4000-8000-00000000c1a0", Pane: "w1:p1"}
	if ev != want {
		t.Fatalf("sent %+v, want %+v", ev, want)
	}
	// Arguments it does not know send nothing.
	for _, args := range [][]string{{"claude"}, {"droid", "stop"}, {"claude", "stop", "extra"}} {
		hookEvent(args, strings.NewReader(stopPayload), getenv(map[string]string{agenthooks.EnvSocket: path, agenthooks.EnvPane: "w1:p1"}))
	}
	if ev := first(t, path, evs); ev.Pane != "sentinel" {
		t.Fatalf("bad arguments sent %+v", ev)
	}
}

// The pane's MAGNUM_AGENT goes with the event as its pane key, so a pane
// with no HERDR_PANE_ID (one outside herdr) sends its events too; a
// UserPromptSubmit goes as any other event, without its prompt.
func TestHookEventSendsThePanesAgentKey(t *testing.T) {
	path, evs := listener(t)
	const key = "mg-11920-claude-review-5d01cf"
	hookEvent([]string{"claude", "user_prompt_submit"}, strings.NewReader(`{"session_id": "s-1", "prompt": "SECRET-PROMPT"}`),
		getenv(map[string]string{agenthooks.EnvSocket: path, agenthooks.EnvAgent: key}))
	ev := first(t, path, evs)
	want := agenthooks.Event{V: 1, Agent: agenthooks.AgentClaude, Name: agenthooks.NameUserPromptSubmit, SessionID: "s-1", PaneKey: key}
	if ev != want {
		t.Fatalf("sent %+v, want %+v", ev, want)
	}
}

// A daemon that is not there, or a stdin that never ends, costs the hook
// its bound at most.
func TestHookEventEndsWithinItsBound(t *testing.T) {
	dir, err := os.MkdirTemp("", "he") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dead := filepath.Join(dir, "dead.sock")
	l, err := agenthooks.Listen(dead, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := w.WriteString(stopPayload); err != nil {
		t.Fatal(err)
	}
	for _, socket := range []string{dead, filepath.Join(dir, "missing.sock")} {
		start := time.Now()
		hookEvent([]string{"codex", "stop"}, r, getenv(map[string]string{agenthooks.EnvSocket: socket, agenthooks.EnvPane: "w1:p1"}))
		if took := time.Since(start); took > agenthooks.HookBound {
			t.Errorf("hook-event to %s took %s, over %s", socket, took, agenthooks.HookBound)
		}
	}
}

// runMain runs the binary's main with args, env and stdin.
func runMain(t *testing.T, args string, env []string, stdin string) (stdout, stderr string, code int, took time.Duration) {
	t.Helper()
	home, user := t.TempDir(), t.TempDir()
	t.Cleanup(func() { // hook-event reads no config and writes no registry: both stay empty
		for _, dir := range []string{home, user} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("%s %v: hook-event left %v (%v)", args, env, entries, err)
			}
		}
	})
	cmd := exec.Command(os.Args[0])
	cmd.Env = append([]string{mainArgsEnv + "=" + args, "MAGNUM_HOME=" + home, "HOME=" + user, "XDG_STATE_HOME=" + user,
		"XDG_DATA_HOME=" + user, "XDG_CONFIG_HOME=" + user, "PATH=/usr/bin:/bin"}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	took = time.Since(start)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code, took
}

// The binary takes hook-event before its CLI: no cobra (which would fail an
// unknown command), no config, no registry; it prints nothing and exits 0,
// whatever it is given.
func TestTheBinaryRunsHookEventBeforeItsCLI(t *testing.T) {
	path, evs := listener(t)
	out, errOut, code, took := runMain(t, "hook-event claude stop", []string{agenthooks.EnvSocket + "=" + path, agenthooks.EnvPane + "=w1:p7"}, stopPayload)
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("hook-event: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	t.Logf("hook-event took %s, the binary's start included", took)
	if ev := first(t, path, evs); ev.Pane != "w1:p7" || ev.Name != agenthooks.NameStop {
		t.Fatalf("the listener got %+v", ev)
	}
	<-evs // the sentinel
	for _, tc := range []struct {
		args string
		env  []string
	}{
		{"hook-event claude stop", []string{agenthooks.EnvPane + "=w1:p7"}},
		{"hook-event", []string{agenthooks.EnvSocket + "=" + path, agenthooks.EnvPane + "=w1:p7"}},
		{"hook-event droid stop", []string{agenthooks.EnvSocket + "=" + path, agenthooks.EnvPane + "=w1:p7"}},
		{"hook-event codex stop", []string{agenthooks.EnvSocket + "=" + filepath.Join(filepath.Dir(path), "gone.sock"), agenthooks.EnvPane + "=w1:p7"}},
	} {
		if out, errOut, code, _ := runMain(t, tc.args, tc.env, stopPayload); code != 0 || out != "" || errOut != "" {
			t.Errorf("%s %v: exit %d, stdout %q, stderr %q", tc.args, tc.env, code, out, errOut)
		}
	}
	if ev := first(t, path, evs); ev.Pane != "sentinel" {
		t.Fatalf("a hook-event that should send nothing sent %+v", ev)
	}
}
