package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Subscription types (dot form, as sent in events.subscribe).
// SubPaneAgentStatusChanged requires PaneID; the others are global.
const (
	SubPaneAgentStatusChanged = "pane.agent_status_changed"
	SubPaneExited             = "pane.exited"
	SubPaneClosed             = "pane.closed"
	SubWorkspaceClosed        = "workspace.closed"
)

// Streamed event names, as they arrive in Event.Event. The pane-scoped
// status event uses the dot spelling of the bundled schema's
// subscription_event.SubscriptionEventKind (the envelope events.subscribe
// streams); the lifecycle events use the snake_case event_kinds spelling.
const (
	EventPaneAgentStatusChanged = "pane.agent_status_changed"
	EventPaneExited             = "pane_exited"
	EventPaneClosed             = "pane_closed"
	EventWorkspaceClosed        = "workspace_closed"
)

// Subscription selects one event type. herdr rejects pane-scoped types
// (pane.agent_status_changed, pane.output_matched, pane.scroll_changed)
// without PaneID.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// Event is one streamed event. The common fields are decoded from Data when
// present; Data keeps the full payload.
type Event struct {
	Event       string // kind, e.g. EventPaneAgentStatusChanged or "pane_exited"
	PaneID      string
	WorkspaceID string
	AgentStatus Status // EventPaneAgentStatusChanged only
	Agent       string
	Data        json.RawMessage
}

// Subscribe opens an events.subscribe stream and calls handler, on this
// goroutine, for every event until ctx is done (ctx.Err() wrapped), the
// server closes the stream (io.EOF wrapped) or the connection fails.
// The ack must arrive within Client.Timeout; after that only ctx bounds the
// stream. Reconnecting (with jitter) and re-snapshotting are the caller's
// job. Malformed lines are logged and skipped.
func (c *Client) Subscribe(ctx context.Context, subs []Subscription, handler func(Event)) error {
	const method = "events.subscribe"
	if subs == nil {
		subs = []Subscription{}
	}
	params := struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}{subs}
	cli := socketLine(method, params)
	start := time.Now()

	conn, err := c.dial(ctx, method)
	if err != nil {
		c.logf(cli, time.Since(start), err)
		return err
	}
	defer conn.Close()

	// The ack is bounded by Client.Timeout (or an earlier ctx deadline).
	ackCtx, cancelAck := context.WithTimeout(ctx, c.timeout())
	stopAck := watch(ackCtx, conn)
	id := nextID()
	r := bufio.NewReader(conn)
	err = writeRequest(conn, id, method, params)
	var line []byte
	if err == nil {
		line, err = r.ReadBytes('\n')
	}
	if err != nil {
		err = ioErr(ackCtx, method, err)
	} else {
		err = decodeResponse(method, id, line, nil)
	}
	stopAck()
	cancelAck()
	c.logf(cli, time.Since(start), err)
	if err != nil {
		return err
	}

	// Streaming: only ctx bounds the connection from here on.
	stop := watch(ctx, conn)
	defer stop()
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			if ev, ok := c.parseEvent(line); ok {
				handler(ev)
			}
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("herdr %s: %w", method, cerr)
		}
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("herdr %s: stream closed: %w", method, err)
		}
		return fmt.Errorf("herdr %s: %w", method, err)
	}
}

func (c *Client) parseEvent(line []byte) (Event, bool) {
	var env struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &env); err != nil || env.Event == "" {
		if c.Logger != nil {
			c.Logger.Printf("debug herdr: events.subscribe: skipping unexpected line: %s", logArg(string(line)))
		}
		return Event{}, false
	}
	ev := Event{Event: env.Event, Data: env.Data}
	if ev.Event == "pane_agent_status_changed" {
		// The schema also lists this status event in its snake_case
		// event_kinds spelling; report one name for it.
		ev.Event = EventPaneAgentStatusChanged
	}
	var common struct {
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
		AgentStatus Status `json:"agent_status"`
		Agent       string `json:"agent"`
	}
	if json.Unmarshal(env.Data, &common) == nil {
		ev.PaneID, ev.WorkspaceID, ev.AgentStatus, ev.Agent = common.PaneID, common.WorkspaceID, common.AgentStatus, common.Agent
	}
	return ev, true
}
