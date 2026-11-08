package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir is a directory whose socket paths stay under the 104 bytes a
// Unix socket's path may hold (t.TempDir's are longer).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ah") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// served is a Listener at work: the events it handled, in order, and how to
// stop it.
type served struct {
	l    *Listener
	path string
	evs  chan Event
	stop func()
}

func serve(t *testing.T, log *slog.Logger) *served {
	t.Helper()
	path := filepath.Join(shortDir(t), "run", "hooks.sock")
	l, err := Listen(path, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &served{l: l, path: path, evs: make(chan Event, 100)}
	go func() {
		defer close(done)
		l.Serve(ctx, func(ev Event) { s.evs <- ev })
	}()
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Serve did not return after its context ended")
			}
		})
	}
	t.Cleanup(s.stop)
	return s
}

// next is the next event handled, failing after a few seconds.
func (s *served) next(t *testing.T) Event {
	t.Helper()
	select {
	case ev := <-s.evs:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event handled")
		return Event{}
	}
}

// sendRaw sends data as one datagram to path, with a send buffer that lets
// a datagram over MaxDatagram leave the sender.
func sendRaw(t *testing.T, path string, data []byte) {
	t.Helper()
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetWriteBuffer(16 << 10); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("send %d bytes: %v", len(data), err)
	}
}

func TestAnEventCrossesTheSocketWhole(t *testing.T) {
	s := serve(t, nil)
	ev := Event{V: Version, Agent: AgentClaude, Name: NameStop, SessionID: "0a1b2c3d-0000-4000-8000-00000000c1a0",
		TranscriptPath: "/Users/x/.claude/projects/p/0a1b2c3d-0000-4000-8000-00000000c1a0.jsonl", TurnID: "t1", Source: "startup",
		Error: "overloaded", ToolName: "Bash", AgentID: "a1", Pane: "w1:p2", CodexThread: "th", Background: 3}
	if err := Send(s.path, ev); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != ev {
		t.Fatalf("handled %+v\nwant    %+v", got, ev)
	}
}

// The socket's directory and the socket are the user's alone, and a socket
// a daemon left behind (it stopped, or it was killed) is replaced; a file
// that is no socket is never removed.
func TestListenMakesAPrivateSocketInPlaceOfAStaleOne(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	path := filepath.Join(dir, "hooks.sock")
	old, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	old.Close() // the file stays, as a killed daemon's would
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the closed socket's file: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(path, nil)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		st, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", p, got, want)
		}
	}
	if st, _ := os.Lstat(path); st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is no socket", path)
	}

	file := filepath.Join(shortDir(t), "run", "hooks.sock")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := Listen(file, nil); err == nil {
		l.Close()
		t.Fatal("Listen replaced a regular file")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "mine" {
		t.Fatalf("the regular file = %q, %v", b, err)
	}
}

// A path of 104 bytes or more cannot name a Unix socket.
func TestListenRefusesAPathTooLongForASocket(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, strings.Repeat("d", SocketPathLimit-len(dir)-len("/")-len("/x.sock")), "x.sock")
	if len(path) != SocketPathLimit {
		t.Fatalf("test path is %d bytes", len(path))
	}
	if _, err := Listen(path, nil); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("Listen(%d bytes) = %v, want an error naming its length", len(path), err)
	}
	if err := Send(path, Event{V: 1, Agent: AgentCodex, Name: NameStop, Pane: "w1:p1"}); err == nil {
		t.Fatal("Send to a path too long = no error")
	}
}

// What is not an event of this version is dropped, and the listener goes on
// reading: a datagram over MaxDatagram, another version, JSON that does not
// parse, and an event without a pane, an agent or a name it knows.
func TestServeDropsWhatIsNoEvent(t *testing.T) {
	s := serve(t, nil)
	good := Event{V: Version, Agent: AgentCodex, Name: NameStop, Pane: "w1:p9", SessionID: "s"}
	big := good
	big.TranscriptPath = "/" + strings.Repeat("p", MaxDatagram)
	for name, data := range map[string][]byte{
		"oversized":    mustJSON(t, big),
		"version 2":    []byte(`{"v":2,"agent":"codex","name":"stop","pane":"w1:p1"}`),
		"version 0":    []byte(`{"agent":"codex","name":"stop","pane":"w1:p1"}`),
		"malformed":    []byte(`{"v":1,"agent":"codex","name":"stop","pane":"w1:p1"`),
		"not json":     []byte("stop w1:p1"),
		"no pane":      []byte(`{"v":1,"agent":"codex","name":"stop"}`),
		"other agent":  []byte(`{"v":1,"agent":"droid","name":"stop","pane":"w1:p1"}`),
		"other event":  []byte(`{"v":1,"agent":"codex","name":"pre_tool_use","pane":"w1:p1"}`),
		"empty":        {},
		"a JSON array": []byte(`[1,2]`),
	} {
		if len(data) > MaxDatagram && name != "oversized" {
			t.Fatalf("%s: %d bytes", name, len(data))
		}
		sendRaw(t, s.path, data)
	}
	if err := Send(s.path, good); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != good {
		t.Fatalf("first event handled = %+v, want the good one (the others dropped)", got)
	}
}

// A later field under V=1 is additive: an event that carries a field this
// build does not know is still the event.
func TestServeTakesAnEventWithAFieldItDoesNotKnow(t *testing.T) {
	s := serve(t, nil)
	sendRaw(t, s.path, []byte(`{"v":1,"agent":"claude","name":"session_end","pane":"w1:p3","reason":"exit"}`))
	if got := s.next(t); got != (Event{V: 1, Agent: AgentClaude, Name: NameSessionEnd, Pane: "w1:p3"}) {
		t.Fatalf("handled %+v", got)
	}
}

// One pane's flood is cut at rateBurst events until its bucket refills;
// another pane's events still go through.
func TestServeLimitsEachPanesRate(t *testing.T) {
	path := filepath.Join(shortDir(t), "hooks.sock")
	l, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	l.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return at } // no refill while the flood arrives
	ctx, cancel := context.WithCancel(context.Background())
	evs := make(chan Event, 100)
	done := make(chan struct{})
	go func() { defer close(done); l.Serve(ctx, func(ev Event) { evs <- ev }) }()
	t.Cleanup(func() { cancel(); <-done })

	flood := Event{V: Version, Agent: AgentClaude, Name: NameStop, Pane: "w1:p1"}
	for range 3 * rateBurst {
		if err := Send(path, flood); err != nil {
			t.Fatal(err)
		}
	}
	other := Event{V: Version, Agent: AgentClaude, Name: NameStop, Pane: "w1:p2"}
	if err := Send(path, other); err != nil {
		t.Fatal(err)
	}
	n := 0
	for ev := range evs {
		if ev.Pane == other.Pane {
			break
		}
		n++
	}
	if n != rateBurst {
		t.Fatalf("handled %d of the flood, want %d", n, rateBurst)
	}

	mu.Lock()
	at = at.Add(rateEvery) // one token back
	mu.Unlock()
	for range 2 {
		if err := Send(path, flood); err != nil {
			t.Fatal(err)
		}
	}
	if err := Send(path, other); err != nil {
		t.Fatal(err)
	}
	n = 0
	for ev := range evs {
		if ev.Pane == other.Pane {
			break
		}
		n++
	}
	if n != 1 {
		t.Fatalf("handled %d after one refill, want 1", n)
	}
}

// The log names the event and the pane, and nothing the payload carried.
func TestServeLogsTheEventNameAndPaneOnly(t *testing.T) {
	var buf syncBuffer
	s := serve(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ev := Event{V: Version, Agent: AgentClaude, Name: NameStopFailure, SessionID: "sess-123", TranscriptPath: "/x/tr.jsonl",
		Error: "authentication_failed", ToolName: "Bash", AgentID: "agent-7", Pane: "w1:p4", TurnID: "turn-5"}
	if err := Send(s.path, ev); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	sendRaw(t, s.path, []byte(`{"v":1,"agent":"claude","name":"stop","pane":"w1:p4","session_id":"sess-dropped"`))
	if err := Send(s.path, Event{V: Version, Agent: AgentClaude, Name: NameSessionEnd, Pane: "w1:p5"}); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	s.stop()
	out := buf.String()
	for _, want := range []string{"stop_failure", "w1:p4"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"sess-123", "tr.jsonl", "authentication_failed", "Bash", "agent-7", "turn-5", "sess-dropped"} {
		if strings.Contains(out, never) {
			t.Errorf("log carries %q:\n%s", never, out)
		}
	}
}

// A handler that panics costs its event, not the listener.
func TestServeOutlivesAHandlerThatPanics(t *testing.T) {
	path := filepath.Join(shortDir(t), "hooks.sock")
	l, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	evs := make(chan Event, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Serve(ctx, func(ev Event) {
			if ev.Name == NameStop {
				panic("handler")
			}
			evs <- ev
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	_ = Send(path, Event{V: 1, Agent: AgentClaude, Name: NameStop, Pane: "w1:p1"})
	_ = Send(path, Event{V: 1, Agent: AgentClaude, Name: NameSessionEnd, Pane: "w1:p1"})
	select {
	case ev := <-evs:
		if ev.Name != NameSessionEnd {
			t.Fatalf("handled %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener stopped after a handler panicked")
	}
}

// The listener closes when its context ends: Serve returns and a send fails.
func TestServeClosesWhenItsContextEnds(t *testing.T) {
	s := serve(t, nil)
	s.stop()
	if err := Send(s.path, Event{V: 1, Agent: AgentClaude, Name: NameStop, Pane: "w1:p1"}); err == nil {
		t.Fatal("Send to a closed listener = no error")
	}
}

// Send never waits on a daemon that is not there: a missing socket and one
// nobody reads fail at once, well within SendTimeout.
func TestSendToNoDaemonFailsAtOnce(t *testing.T) {
	dir := shortDir(t)
	dead := filepath.Join(dir, "dead.sock")
	l, err := Listen(dead, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	for _, path := range []string{filepath.Join(dir, "missing.sock"), dead} {
		start := time.Now()
		if err := Send(path, Event{V: 1, Agent: AgentCodex, Name: NameStop, Pane: "w1:p1"}); err == nil {
			t.Errorf("Send(%s) = no error", path)
		}
		if took := time.Since(start); took > SendTimeout {
			t.Errorf("Send(%s) took %s", path, took)
		}
	}
}

// Send refuses an event over MaxDatagram rather than send a cut one.
func TestSendRefusesAnEventOverADatagram(t *testing.T) {
	s := serve(t, nil)
	big := Event{V: 1, Agent: AgentCodex, Name: NameStop, Pane: "w1:p1", Error: strings.Repeat("e", MaxDatagram)}
	if err := Send(s.path, big); err == nil {
		t.Fatal("Send of an oversized event = no error")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// syncBuffer is a bytes.Buffer safe for a logger's goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
