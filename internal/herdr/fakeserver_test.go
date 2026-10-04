package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRequest is one request the fake server received.
type fakeRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Special handler results.
type (
	hang    struct{} // never answer; block until the client goes away
	noReply struct{} // close the connection without answering
	slow    struct {
		d time.Duration
		v any
	} // answer v after d
)

// handlerFunc answers one request with a result object or a herdr error.
type handlerFunc func(p json.RawMessage) (any, *Error)

// fakeServer is an in-process herdr socket. Like the real server it answers
// exactly one request per connection and then closes it, except for
// events.subscribe, which acks and then streams whatever is pushed on events.
type fakeServer struct {
	t      *testing.T
	path   string
	ln     net.Listener
	events chan any

	mu       sync.Mutex
	handlers map[string]handlerFunc
	reqs     []fakeRequest
	conns    int
	// subscribed is closed (once) when a subscription ack has been written.
	subscribed chan struct{}
	subOnce    sync.Once
	done       chan struct{}
}

// shortTempDir returns a temp dir short enough for a unix socket path
// (macOS limits sun_path to 104 bytes; t.TempDir() can exceed it).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{
		t:          t,
		path:       path,
		ln:         ln,
		events:     make(chan any, 16),
		handlers:   map[string]handlerFunc{},
		subscribed: make(chan struct{}),
		done:       make(chan struct{}),
	}
	go s.serve()
	t.Cleanup(func() {
		close(s.done)
		ln.Close()
	})
	return s
}

// client returns a Client pointed at the fake socket with a short timeout.
func (s *fakeServer) client() *Client {
	return &Client{Socket: s.path, Timeout: 2 * time.Second}
}

// handle registers a handler for method.
func (s *fakeServer) handle(method string, h handlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// reply registers a fixed result for method.
func (s *fakeServer) reply(method string, result any) {
	s.handle(method, func(json.RawMessage) (any, *Error) { return result, nil })
}

// requests returns a copy of every request received so far.
func (s *fakeServer) requests() []fakeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeRequest(nil), s.reqs...)
}

// last returns the most recent request.
func (s *fakeServer) last() fakeRequest {
	s.t.Helper()
	reqs := s.requests()
	if len(reqs) == 0 {
		s.t.Fatal("fake server received no requests")
	}
	return reqs[len(reqs)-1]
}

func (s *fakeServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *fakeServer) write(conn net.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	conn.Write(append(b, '\n'))
}

func (s *fakeServer) serveConn(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req fakeRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.write(conn, map[string]any{"id": "", "error": map[string]string{"code": "invalid_request", "message": "invalid request: " + err.Error()}})
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	h := s.handlers[req.Method]
	s.mu.Unlock()

	if req.Method == "events.subscribe" && h == nil {
		s.serveSubscribe(conn, req)
		return
	}
	if h == nil {
		s.write(conn, map[string]any{"id": "", "error": map[string]string{
			"code": "invalid_request", "message": fmt.Sprintf("invalid request: unknown variant `%s`", req.Method)}})
		return
	}
	res, herr := h(req.Params)
	if sl, ok := res.(slow); ok {
		select {
		case <-time.After(sl.d):
		case <-s.done:
			return
		}
		res = sl.v
	}
	switch res.(type) {
	case hang:
		// Block until the client closes its end or the test ends.
		go func() { <-s.done; conn.Close() }()
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	case noReply:
		return
	}
	if herr != nil {
		s.write(conn, map[string]any{"id": req.ID, "error": map[string]string{"code": herr.Code, "message": herr.Message}})
		return
	}
	s.write(conn, map[string]any{"id": req.ID, "result": res})
}

func (s *fakeServer) serveSubscribe(conn net.Conn, req fakeRequest) {
	var p struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Subscriptions == nil {
		s.write(conn, map[string]any{"id": "", "error": map[string]string{"code": "invalid_request", "message": "invalid request: missing field `subscriptions`"}})
		return
	}
	for _, sub := range p.Subscriptions {
		typ, _ := sub["type"].(string)
		if strings.HasPrefix(typ, "pane.agent_status_changed") || typ == "pane.output_matched" || typ == "pane.scroll_changed" {
			if _, ok := sub["pane_id"]; !ok {
				s.write(conn, map[string]any{"id": "", "error": map[string]string{"code": "invalid_request", "message": "invalid request: missing field `pane_id`"}})
				return
			}
		}
	}
	s.write(conn, map[string]any{"id": req.ID, "result": map[string]string{"type": "subscription_started"}})
	s.subOnce.Do(func() { close(s.subscribed) })
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return // server goes away: client sees EOF
			}
			if raw, isRaw := ev.(string); isRaw {
				conn.Write([]byte(raw + "\n"))
				continue
			}
			s.write(conn, ev)
		case <-s.done:
			return
		}
	}
}

// waitSubscribed blocks until a subscription has been acked.
func (s *fakeServer) waitSubscribed() {
	s.t.Helper()
	select {
	case <-s.subscribed:
	case <-time.After(2 * time.Second):
		s.t.Fatal("no subscription within 2s")
	}
}

// recLogger records log lines.
type recLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}
