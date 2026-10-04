package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCallSendsOneRequestPerConnectionAndDecodes(t *testing.T) {
	s := newFakeServer(t)
	s.reply("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "w1:p1", "agent_status": "idle"}})
	c := s.client()

	for i := 0; i < 2; i++ {
		var out struct {
			Type string `json:"type"`
			Pane Pane   `json:"pane"`
		}
		if err := c.Call(context.Background(), "pane.get", map[string]string{"pane_id": "w1:p1"}, &out); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if out.Type != "pane_info" || out.Pane.ID != "w1:p1" || out.Pane.AgentStatus != StatusIdle {
			t.Fatalf("decoded %+v", out)
		}
	}
	reqs := s.requests()
	if len(reqs) != 2 || s.connCount() != 2 {
		t.Fatalf("want 2 requests on 2 connections, got %d requests / %d conns", len(reqs), s.connCount())
	}
	if reqs[0].ID == "" || reqs[0].ID == reqs[1].ID {
		t.Fatalf("request ids must be non-empty and unique: %q %q", reqs[0].ID, reqs[1].ID)
	}
	if reqs[0].Method != "pane.get" || string(reqs[0].Params) != `{"pane_id":"w1:p1"}` {
		t.Fatalf("request = %+v", reqs[0])
	}
}

func TestCallNilParamsSendsEmptyObject(t *testing.T) {
	s := newFakeServer(t)
	s.reply("agent.list", map[string]any{"type": "agent_list", "agents": []any{}})
	if err := s.client().Call(context.Background(), "agent.list", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(s.last().Params); got != "{}" {
		t.Fatalf("params = %s, want {}", got)
	}
}

func TestCallMapsServerErrors(t *testing.T) {
	cases := []struct {
		code, msg string
	}{
		{CodeAgentBlocked, "agent mg-x is blocked"},
		{CodeAgentPromptStalled, "no working state within 5000ms"},
		{CodeTimeout, "timed out"},
		{CodeUIBusy, "ui busy"},
		{CodeInvalidKey, "unsupported key PageUp"},
		{CodeAgentNotFound, "agent target x not found"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle("agent.prompt", func(json.RawMessage) (any, *Error) { return nil, &Error{Code: tc.code, Message: tc.msg} })
			err := s.client().Call(context.Background(), "agent.prompt", map[string]string{"target": "x", "text": "hi"}, nil)
			var he *Error
			if !errors.As(err, &he) {
				t.Fatalf("want *Error, got %T %v", err, err)
			}
			if he.Code != tc.code || he.Message != tc.msg || he.Method != "agent.prompt" {
				t.Fatalf("got %+v", he)
			}
			if !IsCode(err, tc.code) || IsCode(err, "something_else") {
				t.Fatalf("IsCode mismatch for %v", err)
			}
			if !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), "agent.prompt") {
				t.Fatalf("error text %q lacks code/method", err)
			}
			if got := IsTimeout(err); got != (tc.code == CodeTimeout) {
				t.Fatalf("IsTimeout = %v for %s", got, tc.code)
			}
		})
	}
}

func TestCallInvalidRequestWithEmptyID(t *testing.T) {
	s := newFakeServer(t) // no handler: fake answers {"id":"", error: invalid_request}
	err := s.client().Call(context.Background(), "no.such.method", nil, nil)
	if !IsCode(err, CodeInvalidRequest) {
		t.Fatalf("want invalid_request, got %v", err)
	}
}

func TestCallClientTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.get", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	c := s.client()
	c.Timeout = 100 * time.Millisecond
	start := time.Now()
	err := c.Call(context.Background(), "pane.get", map[string]string{"pane_id": "w1:p1"}, nil)
	if !IsTimeout(err) || !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
}

func TestCallContextDeadlineShorterThanTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.get", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.client().Call(ctx, "pane.get", map[string]string{"pane_id": "w1:p1"}, nil)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("ctx deadline ignored: took %v", el)
	}
}

func TestCallContextCancel(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.get", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := s.client().Call(ctx, "pane.get", map[string]string{"pane_id": "w1:p1"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestCallSocketMissing(t *testing.T) {
	c := &Client{Socket: filepath.Join(shortTempDir(t), "missing.sock")}
	err := c.Call(context.Background(), "ping", nil, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestCallConnectionClosedWithoutReply(t *testing.T) {
	s := newFakeServer(t)
	s.handle("pane.get", func(json.RawMessage) (any, *Error) { return noReply{}, nil })
	err := s.client().Call(context.Background(), "pane.get", map[string]string{"pane_id": "w1:p1"}, nil)
	if err == nil || IsTimeout(err) {
		t.Fatalf("want a closed-connection error, got %v", err)
	}
	var he *Error
	if errors.As(err, &he) {
		t.Fatalf("closed connection must not look like a server error: %v", err)
	}
}

func TestCallLogsRedactedLine(t *testing.T) {
	s := newFakeServer(t)
	s.reply("notification.show", map[string]any{"type": "notification_show", "shown": true, "reason": "shown"})
	log := &recLogger{}
	c := s.client()
	c.Logger = log
	if err := c.Call(context.Background(), "notification.show", map[string]string{"title": "t", "body": "token ghs_abcdefghijklmnop"}, nil); err != nil {
		t.Fatal(err)
	}
	lines := log.all()
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %q", lines)
	}
	if strings.Contains(lines[0], "ghs_abcdefghijklmnop") || !strings.Contains(lines[0], "notification.show") {
		t.Fatalf("log line not redacted or missing method: %q", lines[0])
	}
}

func TestDefaultSocket(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/x/y.sock")
	if got := DefaultSocket(); got != "/x/y.sock" {
		t.Fatalf("DefaultSocket = %q", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "")
	if got := DefaultSocket(); !strings.HasSuffix(got, "/.config/herdr/herdr.sock") || !filepath.IsAbs(got) {
		t.Fatalf("DefaultSocket = %q", got)
	}
}

// raceCtx cancels itself inside Deadline() and then waits for the
// cancellation to propagate, forcing "the context is cancelled between
// reading its deadline and applying it" on every watch ordering: a watch that
// registers its cancellation hook before setting the deadline loses the
// cancellation to the later deadline.
type raceCtx struct {
	context.Context
	cancel context.CancelFunc
}

func (r raceCtx) Deadline() (time.Time, bool) {
	r.cancel()
	time.Sleep(50 * time.Millisecond)
	return time.Now().Add(time.Hour), true
}

func TestWatchCancellationIsNotOverwrittenByTheDeadline(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := raceCtx{Context: base, cancel: cancel}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	stop := watch(ctx, client)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v, want a deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation was overwritten by the ctx deadline: Read still blocks")
	}
}
