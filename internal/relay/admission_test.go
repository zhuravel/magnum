package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stall opens a connection, sends a delivery's request line, headers and the first sent bytes of its body,
// and then sends nothing more.
func (h *harness) stall(headers map[string]string, body []byte, sent int) net.Conn {
	h.t.Helper()
	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	var b strings.Builder
	fmt.Fprintf(&b, "POST /github/webhook HTTP/1.1\r\nHost: relay.example.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	b.Write(body[:sent])
	if _, err := io.WriteString(conn, b.String()); err != nil {
		h.t.Fatal(err)
	}
	return conn
}

// answer reads the response on conn within the given time: its status, or 0 when none came.
func answer(conn net.Conn, within time.Duration) int {
	_ = conn.SetReadDeadline(time.Now().Add(within))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// closedBy reports whether the server closed conn within the given time.
func closedBy(conn net.Conn, within time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(within))
	_, err := io.Copy(io.Discard, conn)
	return err == nil // io.Copy reads to EOF: the server closed the connection
}

func TestAStalledBodyIsCutWithinTheDeadlineAndADeliveryStillGetsThrough(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.BodyTimeout = 500 * time.Millisecond })
	body := []byte(prBody(45, "opened"))
	began := time.Now()
	var conns []net.Conn
	for i := range concurrentBodies {
		conns = append(conns, h.stall(map[string]string{
			"X-GitHub-Event":      "pull_request",
			"X-GitHub-Delivery":   fmt.Sprintf("d-stall-%d", i),
			"X-Hub-Signature-256": sign(testSecret, body),
		}, body, len(body)/2))
	}
	for i, conn := range conns {
		if code := answer(conn, 5*time.Second); code != http.StatusRequestTimeout {
			t.Errorf("stalled delivery %d: %d, want 408", i, code)
		}
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the stalled bodies were cut after %v, want about 500ms", took)
	}
	if code := h.deliver("pull_request", "d-real", string(body)); code != http.StatusAccepted {
		t.Fatalf("a signed delivery after the stalls: %d, want 202", code)
	}
	if n := h.count(); n != 1 {
		t.Fatalf("rows: %d, want the signed delivery alone", n)
	}
}

func TestADeliveryWithoutWellFormedHeadersIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // the default body timeout: a refusal must not wait for it
	body := []byte(prBody(45, "opened"))
	good := sign(testSecret, body)
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no signature", map[string]string{"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "d-1"}, http.StatusUnauthorized},
		{"a malformed signature", map[string]string{"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "d-2", "X-Hub-Signature-256": "sha256=nothex"}, http.StatusUnauthorized},
		{"a sha1 signature", map[string]string{"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "d-3", "X-Hub-Signature-256": "sha1=" + strings.Repeat("a", 40)}, http.StatusUnauthorized},
		{"no event", map[string]string{"X-GitHub-Delivery": "d-4", "X-Hub-Signature-256": good}, http.StatusBadRequest},
		{"no delivery", map[string]string{"X-GitHub-Event": "pull_request", "X-Hub-Signature-256": good}, http.StatusBadRequest},
		{"a malformed delivery", map[string]string{"X-GitHub-Event": "pull_request", "X-GitHub-Delivery": "d/6", "X-Hub-Signature-256": good}, http.StatusBadRequest},
	}
	began := time.Now()
	var conns []net.Conn
	for _, c := range cases {
		conns = append(conns, h.stall(c.headers, body, 10))
	}
	for i, c := range cases {
		if code := answer(conns[i], 3*time.Second); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
		if !closedBy(conns[i], 3*time.Second) {
			t.Errorf("%s: the relay kept the connection open to read the body", c.name)
		}
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the refusals took %v: they waited for the body", took)
	}
	// None of them held a slot: with every refused connection still open, a signed delivery is stored at once.
	began = time.Now()
	if code := h.deliver("pull_request", "d-real", string(body)); code != http.StatusAccepted || time.Since(began) > 2*time.Second {
		t.Fatalf("a signed delivery beside the refused ones: %d after %v", code, time.Since(began))
	}
}

func TestALongPollOutlivesTheServersTimeouts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A server whose read and write timeouts are shorter than the poll's wait still gets the poll's answer out.
	srv := httptest.NewUnstartedServer(h.relay.Handler())
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
	srv.Start()
	defer srv.Close()
	h.deliverN(1)
	start := h.page("")
	done := make(chan Page, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/events?wait=5s&after="+url.QueryEscape(start.Next), nil)
		if err != nil {
			done <- Page{}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Errorf("long poll: %v", err)
			done <- Page{}
			return
		}
		defer resp.Body.Close()
		var p Page
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &p) != nil {
			t.Errorf("long poll: %d %q", resp.StatusCode, b)
		}
		done <- p
	}()
	time.Sleep(900 * time.Millisecond) // three of the server's timeouts
	if code := h.deliver("pull_request", "d-2", prBody(2, "opened")); code != http.StatusAccepted {
		t.Fatal(code)
	}
	select {
	case p := <-done:
		if len(p.Events) != 1 || p.Events[0].Seq != 2 {
			t.Fatalf("long poll past the read timeout: %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll never answered")
	}
}

func TestLongPollsWaitingAtOnceAreCapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.MaxWaiting = 1 })
	first := make(chan int, 1)
	go func() {
		code, _ := h.get("wait=10s", testToken)
		first <- code
	}()
	time.Sleep(200 * time.Millisecond)
	if code, body := h.get("wait=10s", testToken); code != http.StatusTooManyRequests {
		t.Fatalf("a second waiting poll: %d %s, want 429", code, body)
	}
	if code, _ := h.get("wait=0", testToken); code != http.StatusOK {
		t.Fatalf("a poll that does not wait: %d, want 200", code)
	}
	h.deliverN(1)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the waiting poll: %d", code)
	}
	// Its place is free again.
	if code, _ := h.get("wait=200ms&after="+url.QueryEscape(h.page("").Next), testToken); code != http.StatusOK {
		t.Fatalf("a waiting poll after the first ended: %d, want 200", code)
	}
}

func TestTheScheduledPruneRunsWithoutAManualCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(2)
	h.clock.Add(31 * 24 * time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		h.relay.RunRetention(ctx, 20*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.count() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduled prune never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := h.page(""); p.Oldest != cursor(p.Epoch, 2) {
		t.Fatalf("oldest after the scheduled prune: %s", p.Oldest)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRetention did not return when its context ended")
	}
}

func TestHealthzReadsTheJournalAtMostOncePerSnapshot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(1)
	get := func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/healthz", nil)
		if err != nil {
			t.Error(err)
			return
		}
		if code, body := h.do(req); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
			t.Errorf("healthz: %d %s", code, body)
		}
	}
	flood := func() {
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(get)
		}
		wg.Wait()
	}
	h.clock.Add(time.Minute) // the snapshot Open took is stale
	reads := h.relay.healthReads.Load()
	flood()
	if n := h.relay.healthReads.Load() - reads; n != 1 {
		t.Fatalf("50 health checks read the journal %d times, want 1", n)
	}
	flood()
	if n := h.relay.healthReads.Load() - reads; n != 1 {
		t.Fatalf("50 more within the snapshot's interval read the journal %d more times, want 0", n-1)
	}
}

func TestHealthzCountsWhatThePruneLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deliverN(3)
	h.clock.Add(31 * 24 * time.Hour)
	h.deliver("pull_request", "d-4", prBody(4, "opened"))
	// A counter gone wrong is set right by the next prune, which counts the events again.
	if _, err := h.relay.j.db.ExecContext(t.Context(), `UPDATE meta SET event_count = 99`); err != nil {
		t.Fatal(err)
	}
	if n, err := h.relay.Prune(t.Context()); err != nil || n != 3 {
		t.Fatalf("Prune: %d %v", n, err)
	}
	h.clock.Add(healthEvery)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := h.do(req); code != http.StatusOK || !strings.Contains(body, `"events":1,`) || !strings.Contains(body, `"last_received_at":"2026-11-20T12:00:00Z"`) {
		t.Fatalf("healthz after the prune: %d %s", code, body)
	}
}

func TestOneTokenLengthBoundsTheListing(t *testing.T) {
	t.Parallel()
	short, shortest, longest, long := strings.Repeat("s", MinTokenLength-1), strings.Repeat("a", MinTokenLength),
		strings.Repeat("b", MaxTokenLength), strings.Repeat("l", MaxTokenLength+1)
	h := newHarness(t, func(o *Options) {
		for _, tok := range []string{short, shortest, longest, long} {
			o.TokenHashes = append(o.TokenHashes, tokenHash(tok))
		}
	})
	for tok, want := range map[string]int{short: 401, shortest: 200, longest: 200, long: 401} {
		if code, _ := h.get("", tok); code != want {
			t.Errorf("a token of %d bytes: %d, want %d", len(tok), code, want)
		}
	}
	for tok, want := range map[string]bool{short: false, shortest: true, longest: true, long: false, "two words " + shortest: false, "\x01" + shortest: false} {
		if got := CheckToken(tok) == nil; got != want {
			t.Errorf("CheckToken(%d bytes) = %v, want %v", len(tok), got, want)
		}
	}
}
