package relay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	testSecret     = "current-webhook-secret-0123456789"
	testPrevSecret = "previous-webhook-secret-987654321"
	testToken      = "relay-pull-token-abcdef0123456789abcdef"
)

// clock is the relay's time in tests: it moves only when a test moves it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// syncBuffer is a log sink the handlers write from their goroutines.
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

type harness struct {
	t     *testing.T
	relay *Relay
	srv   *httptest.Server
	clock *clock
	logs  *syncBuffer
	db    string
}

func tokenHash(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func newHarness(t *testing.T, edit ...func(*Options)) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clock: &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)},
		logs:  &syncBuffer{},
		db:    filepath.Join(t.TempDir(), "data", "relay.db"),
	}
	opts := Options{
		DB:          h.db,
		Secrets:     [][]byte{[]byte(testSecret), []byte(testPrevSecret)},
		TokenHashes: [][sha256.Size]byte{tokenHash(testToken)},
		Now:         h.clock.Now,
		Logger:      slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, f := range edit {
		f(&opts)
	}
	r, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h.relay = r
	h.srv = httptest.NewServer(r.Handler())
	t.Cleanup(func() {
		r.StopWaiting()
		h.srv.Close()
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// post delivers body as GitHub would, with the given signature header (none when sig is "").
func (h *harness) post(event, delivery string, body []byte, sig string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.srv.URL+"/github/webhook", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	return h.do(req)
}

// deliver posts a body signed with the current secret.
func (h *harness) deliver(event, delivery, body string) int {
	h.t.Helper()
	code, _ := h.post(event, delivery, []byte(body), sign(testSecret, []byte(body)))
	return code
}

func (h *harness) do(req *http.Request) (int, string) {
	h.t.Helper()
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// get lists the journal with the given query and token (none when token is "").
func (h *harness) get(query, token string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.srv.URL+"/v1/events?"+query, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return h.do(req)
}

// page lists the journal with the test token and decodes a 200 answer.
func (h *harness) page(query string) Page {
	h.t.Helper()
	code, body := h.get(query, testToken)
	if code != http.StatusOK {
		h.t.Fatalf("GET /v1/events?%s: %d %s", query, code, body)
	}
	var p Page
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		h.t.Fatalf("decode %s: %v", body, err)
	}
	return p
}

func (h *harness) count() int {
	h.t.Helper()
	var n int
	if err := h.relay.j.db.QueryRowContext(h.t.Context(), `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func seqs(events []Event) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

// prBody is a pull_request payload with text fields a relay must never keep.
func prBody(number int, action string) string {
	return `{"action":"` + action + `","number":` + strconv.Itoa(number) + `,
	  "pull_request":{"number":` + strconv.Itoa(number) + `,"title":"SECRET-TITLE","body":"SECRET-BODY",
	    "user":{"login":"alice"},"html_url":"https://github.com/talkable/example/pull/1",
	    "head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"feature"},
	    "base":{"ref":"main","sha":"89abcdef0123456789abcdef0123456789abcdef"}},
	  "repository":{"id":2,"full_name":"talkable/example","description":"SECRET-DESCRIPTION"},
	  "installation":{"id":1},"sender":{"login":"alice"}}`
}
