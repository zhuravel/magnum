package execx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a writer safe for concurrent use that says when it first
// holds want.
type syncBuffer struct {
	mu   sync.Mutex
	b    bytes.Buffer
	want string
	seen chan struct{}
	once sync.Once
}

func newSyncBuffer(want string) *syncBuffer {
	return &syncBuffer{want: want, seen: make(chan struct{})}
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
	if s.want != "" && strings.Contains(s.b.String(), s.want) {
		s.once.Do(func() { close(s.seen) })
	}
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Stream gets what a command prints while it runs, besides the capture:
// the factory's live.log follows a precheck that runs for minutes. A
// command that prints, then sleeps until it is stopped, has its line on
// the stream before it ends; the result keeps its output as before.
func TestStreamGetsTheOutputWhileTheCommandRuns(t *testing.T) {
	out, errOut := newSyncBuffer("first\n"), newSyncBuffer("")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := (&Real{}).Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "echo first; echo oops >&2; exec sleep 30"},
			Stream: out, StreamErr: errOut})
		done <- ran{res, err}
	}()
	select {
	case <-out.seen:
	case r := <-done:
		t.Fatalf("the command ended (%v) before its line reached the stream: %q", r.err, out.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no line on the stream while the command runs: %q", out.String())
	}
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", r.err)
	}
	if string(r.res.Stdout) != "first\n" || string(r.res.Stderr) != "oops\n" {
		t.Errorf("the capture: stdout %q, stderr %q; want both as before", r.res.Stdout, r.res.Stderr)
	}
	if got := errOut.String(); got != "oops\n" {
		t.Errorf("StreamErr got %q, want stderr", got)
	}
	if strings.Contains(out.String(), "oops") {
		t.Errorf("Stream got stderr while StreamErr is set: %q", out.String())
	}
}

// Without StreamErr, Stream gets stderr too; a writer that fails changes
// nothing of the run or its capture.
func TestStreamTakesStderrWithoutStreamErrAndItsFailuresChangeNothing(t *testing.T) {
	both := newSyncBuffer("")
	res, err := (&Real{}).Run(t.Context(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}, Stream: both})
	if err != nil || !strings.Contains(both.String(), "out\n") || !strings.Contains(both.String(), "err\n") {
		t.Fatalf("err %v, stream %q; want stdout and stderr on it", err, both.String())
	}
	if res.Out() != "out" {
		t.Errorf("stdout = %q", res.Out())
	}
	res, err = (&Real{}).Run(t.Context(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}, Stream: failingWriter{}})
	if err != nil || res.Out() != "out" || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("a failing stream: %v, stdout %q, stderr %q; want the run and its capture as without one", err, res.Stdout, res.Stderr)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// The fake runner writes a rule's output to the streams, as Real would.
func TestFakeWritesARulesOutputToTheStreams(t *testing.T) {
	f := &Fake{Rules: []Rule{{Prefix: []string{"bin/jest"}, Result: Result{Stdout: []byte("PASS a.test.js\n"), Stderr: []byte("warn\n")}}}}
	out, errOut := newSyncBuffer(""), newSyncBuffer("")
	if _, err := f.Run(t.Context(), Cmd{Name: "bin/jest", Stream: out, StreamErr: errOut}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "PASS a.test.js\n" || errOut.String() != "warn\n" {
		t.Fatalf("streams %q and %q", out.String(), errOut.String())
	}
}
