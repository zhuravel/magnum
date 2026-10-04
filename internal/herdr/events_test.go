package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestSubscribeStreamsEventsUntilServerCloses(t *testing.T) {
	s := newFakeServer(t)
	c := s.client()

	var mu sync.Mutex
	var got []Event
	errc := make(chan error, 1)
	go func() {
		errc <- c.Subscribe(context.Background(), []Subscription{
			{Type: SubPaneAgentStatusChanged, PaneID: "w1:p1"},
			{Type: SubPaneExited},
			{Type: SubPaneClosed},
			{Type: SubWorkspaceClosed},
		}, func(e Event) {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		})
	}()
	s.waitSubscribed()

	req := s.last()
	if req.Method != "events.subscribe" || !jsonEqual(t, req.Params,
		`{"subscriptions":[{"type":"pane.agent_status_changed","pane_id":"w1:p1"},{"type":"pane.exited"},{"type":"pane.closed"},{"type":"workspace.closed"}]}`) {
		t.Fatalf("subscribe request = %s %s", req.Method, req.Params)
	}

	// The schema's subscription_event envelope: dot-spelled kind, no "type"
	// inside data.
	s.events <- map[string]any{"event": "pane.agent_status_changed", "data": map[string]any{
		"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "working", "agent": "codex"}}
	s.events <- "not json at all" // skipped, stream continues
	s.events <- map[string]any{"event": "pane_exited", "data": map[string]any{"type": "pane_exited", "pane_id": "w1:p2", "workspace_id": "w1"}}
	s.events <- map[string]any{"event": "workspace_closed", "data": map[string]any{"type": "workspace_closed", "workspace_id": "w1"}}
	close(s.events)

	select {
	case err := <-errc:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("server close must surface as io.EOF, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after the server closed the stream")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.Event != EventPaneAgentStatusChanged || e.PaneID != "w1:p1" || e.WorkspaceID != "w1" || e.AgentStatus != StatusWorking || e.Agent != "codex" {
		t.Fatalf("status event = %+v", e)
	}
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil || data["pane_id"] != "w1:p1" || data["agent_status"] != "working" {
		t.Fatalf("raw data = %s (%v)", e.Data, err)
	}
	if got[1].Event != EventPaneExited || got[1].PaneID != "w1:p2" {
		t.Fatalf("exited event = %+v", got[1])
	}
	if got[2].Event != EventWorkspaceClosed || got[2].WorkspaceID != "w1" {
		t.Fatalf("workspace event = %+v", got[2])
	}
}

// The schema also spells the status event in snake_case (event_kinds); both
// spellings reach the handler under EventPaneAgentStatusChanged.
func TestSubscribeNormalizesStatusEventSpelling(t *testing.T) {
	s := newFakeServer(t)
	var mu sync.Mutex
	var got []Event
	errc := make(chan error, 1)
	go func() {
		errc <- s.client().Subscribe(context.Background(), []Subscription{{Type: SubPaneAgentStatusChanged, PaneID: "w1:p1"}}, func(e Event) {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		})
	}()
	s.waitSubscribed()
	s.events <- map[string]any{"event": "pane_agent_status_changed", "data": map[string]any{
		"type": "pane_agent_status_changed", "pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "idle"}}
	close(s.events)
	<-errc
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Event != EventPaneAgentStatusChanged || got[0].AgentStatus != StatusIdle {
		t.Fatalf("events = %+v", got)
	}
}

// A cancellation while the ack is still pending surfaces at once instead of
// waiting out Client.Timeout.
func TestSubscribeCancelWhileAckPending(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", func(json.RawMessage) (any, *Error) { return hang{}, nil }) // never acks
	c := s.client()
	c.Timeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	start := time.Now()
	go func() { errc <- c.Subscribe(ctx, []Subscription{{Type: SubPaneExited}}, func(Event) {}) }()
	for len(s.requests()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if el := time.Since(start); !errors.Is(err, context.Canceled) || el > time.Second {
			t.Fatalf("want a prompt context.Canceled, got %v after %v", err, el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe ignored the cancellation while waiting for the ack")
	}
}

// An ack that never comes ends with ErrTimeout after Client.Timeout.
func TestSubscribeAckTimeout(t *testing.T) {
	s := newFakeServer(t)
	s.handle("events.subscribe", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	c := s.client()
	c.Timeout = 80 * time.Millisecond
	if err := c.Subscribe(context.Background(), []Subscription{{Type: SubPaneExited}}, func(Event) {}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

func TestSubscribeReturnsOnContextCancel(t *testing.T) {
	s := newFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- s.client().Subscribe(ctx, []Subscription{{Type: SubPaneExited}}, func(Event) {})
	}()
	s.waitSubscribed()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe ignored ctx cancel")
	}
}

func TestSubscribeOutlivesClientTimeout(t *testing.T) {
	s := newFakeServer(t)
	c := s.client()
	c.Timeout = 50 * time.Millisecond
	got := make(chan Event, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- c.Subscribe(context.Background(), []Subscription{{Type: SubPaneExited}}, func(e Event) { got <- e })
	}()
	s.waitSubscribed()
	time.Sleep(150 * time.Millisecond) // longer than Client.Timeout
	s.events <- map[string]any{"event": "pane_exited", "data": map[string]any{"type": "pane_exited", "pane_id": "p", "workspace_id": "w"}}
	select {
	case e := <-got:
		if e.PaneID != "p" {
			t.Fatalf("event = %+v", e)
		}
	case err := <-errc:
		t.Fatalf("stream ended early: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestSubscribeRejectedSubscription(t *testing.T) {
	s := newFakeServer(t)
	err := s.client().Subscribe(context.Background(), []Subscription{{Type: SubPaneAgentStatusChanged}}, func(Event) {
		t.Error("handler must not run")
	})
	if !IsCode(err, CodeInvalidRequest) {
		t.Fatalf("want invalid_request, got %v", err)
	}
}

func TestSubscribeSocketMissing(t *testing.T) {
	c := &Client{Socket: shortTempDir(t) + "/none.sock"}
	if err := c.Subscribe(context.Background(), []Subscription{{Type: SubPaneExited}}, func(Event) {}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}
