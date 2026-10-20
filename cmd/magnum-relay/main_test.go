package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/relay"
)

const (
	secret = "webhook-secret-for-the-tests-0123456789"
	token  = "pull-token-for-the-tests-0123456789abcdef0123"
)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestHashTokenPrintsTheSHA256OfTheTokenOnStdin(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"hash-token"}, envOf(nil), strings.NewReader(token+"\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("hash-token: exit %d, %s", code, errOut.String())
	}
	if out.String() != hashOf(token)+"\n" {
		t.Fatalf("hash-token printed %q, want the token's sha256", out.String())
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("hash-token printed the token")
	}
}

func TestHashTokenRefusesAShortOrEmptyToken(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":           "\n",
		"short":           "abc\n",
		"two words":       strings.Repeat("a", 20) + " " + strings.Repeat("b", 20),
		"two lines":       token + "\n" + token + "\n",
		"one under":       strings.Repeat("a", relay.MinTokenLength-1) + "\n",
		"one over":        strings.Repeat("a", relay.MaxTokenLength+1) + "\n",
		"a control":       strings.Repeat("a", relay.MinTokenLength) + "\x01\n",
		"over the reader": strings.Repeat("a", 5000),
	} {
		var out, errOut bytes.Buffer
		if code := run(t.Context(), []string{"hash-token"}, envOf(nil), strings.NewReader(in), &out, &errOut); code == 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d, printed %q", name, code, out.String())
		}
	}
	// The bounds are the listing's: what hash-token takes, the relay accepts.
	for _, n := range []int{relay.MinTokenLength, relay.MaxTokenLength} {
		tok := strings.Repeat("t", n)
		var out bytes.Buffer
		if code := run(t.Context(), []string{"hash-token"}, envOf(nil), strings.NewReader(tok+"\n"), &out, io.Discard); code != 0 || out.String() != hashOf(tok)+"\n" {
			t.Errorf("a token of %d characters: exit %d, %q", n, code, out.String())
		}
		if err := relay.CheckToken(tok); err != nil {
			t.Errorf("the relay refuses a token of %d characters that hash-token takes: %v", n, err)
		}
	}
}

func TestServeReadsItsSettingsFromTheEnvironmentAndFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := map[string]string{
		"MAGNUM_RELAY_SECRET_FILE":          writeFile(t, dir, "secret", secret+"\n"),
		"MAGNUM_RELAY_PREVIOUS_SECRET_FILE": writeFile(t, dir, "previous", "previous-secret-for-the-tests-012345\n"),
		"MAGNUM_RELAY_TOKEN_HASHES":         hashOf(token) + ", " + strings.ToUpper(hashOf("other-token-0123456789abcdef0123456789")),
		"MAGNUM_RELAY_RETENTION":            "168h",
	}
	cfg, err := parseServe(nil, envOf(env), io.Discard)
	if err != nil {
		t.Fatalf("parseServe: %v", err)
	}
	if cfg.addr != ":8080" || cfg.db != "/data/relay.db" || cfg.retention != 168*time.Hour {
		t.Fatalf("defaults: %+v", cfg)
	}
	if len(cfg.secrets) != 2 || string(cfg.secrets[0]) != secret || string(cfg.secrets[1]) != "previous-secret-for-the-tests-012345" {
		t.Fatalf("secrets: %d read", len(cfg.secrets))
	}
	if len(cfg.tokens) != 2 || hex.EncodeToString(cfg.tokens[0][:]) != hashOf(token) {
		t.Fatalf("token hashes: %d read", len(cfg.tokens))
	}
	env["MAGNUM_RELAY_ADDR"], env["MAGNUM_RELAY_DB"] = "127.0.0.1:9000", filepath.Join(dir, "env.db")
	delete(env, "MAGNUM_RELAY_PREVIOUS_SECRET_FILE")
	delete(env, "MAGNUM_RELAY_RETENTION")
	cfg, err = parseServe([]string{"-db", filepath.Join(dir, "flag.db")}, envOf(env), io.Discard)
	if err != nil {
		t.Fatalf("parseServe: %v", err)
	}
	if cfg.addr != "127.0.0.1:9000" || cfg.db != filepath.Join(dir, "flag.db") || cfg.retention != 720*time.Hour || len(cfg.secrets) != 1 {
		t.Fatalf("env and flags: %+v", cfg)
	}
}

func TestServeRefusesAMissingOrWeakSecretAndBadTokenHashes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := map[string]string{
		"MAGNUM_RELAY_SECRET_FILE":  writeFile(t, dir, "secret", secret),
		"MAGNUM_RELAY_TOKEN_HASHES": hashOf(token),
	}
	for name, c := range map[string]struct{ key, value string }{
		"no secret file":        {"MAGNUM_RELAY_SECRET_FILE", ""},
		"a missing secret file": {"MAGNUM_RELAY_SECRET_FILE", filepath.Join(dir, "nope")},
		"a short secret":        {"MAGNUM_RELAY_SECRET_FILE", writeFile(t, dir, "short", "WEAK-SECRET\n")},
		"an empty secret":       {"MAGNUM_RELAY_SECRET_FILE", writeFile(t, dir, "empty", "\n")},
		"an empty previous":     {"MAGNUM_RELAY_PREVIOUS_SECRET_FILE", writeFile(t, dir, "empty-previous", "")},
		"a missing previous":    {"MAGNUM_RELAY_PREVIOUS_SECRET_FILE", filepath.Join(dir, "no-previous")},
		"a directory as secret": {"MAGNUM_RELAY_SECRET_FILE", dir},
		"a huge secret file":    {"MAGNUM_RELAY_SECRET_FILE", writeFile(t, dir, "huge", strings.Repeat("s", 5000))},
		"no token hashes":       {"MAGNUM_RELAY_TOKEN_HASHES", ""},
		"a token as its hash":   {"MAGNUM_RELAY_TOKEN_HASHES", token},
		"a short hash":          {"MAGNUM_RELAY_TOKEN_HASHES", hashOf(token)[:63]},
		"an empty hash":         {"MAGNUM_RELAY_TOKEN_HASHES", hashOf(token) + ",,"},
		"a bad retention":       {"MAGNUM_RELAY_RETENTION", "a month"},
		"a short retention":     {"MAGNUM_RELAY_RETENTION", "10m"},
	} {
		env := maps.Clone(good)
		env[c.key] = c.value
		var out, errOut bytes.Buffer
		if code := run(t.Context(), nil, envOf(env), strings.NewReader(""), &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, errOut.String())
		}
		if strings.Contains(errOut.String(), "WEAK-SECRET") || strings.Contains(errOut.String(), token) {
			t.Errorf("%s: the error quotes a secret: %s", name, errOut.String())
		}
	}
}

func TestServeStoresADeliveryAnswersItsHealthCheckAndShutsDownGracefully(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{
		addr:      "127.0.0.1:0",
		db:        filepath.Join(dir, "data", "relay.db"),
		secrets:   [][]byte{[]byte(secret)},
		retention: 720 * time.Hour,
		shutdown:  shutdownTimeout,
	}
	sum := sha256.Sum256([]byte(token))
	cfg.tokens = [][sha256.Size]byte{sum}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addrs := make(chan net.Addr, 1)
	logs := &lockedWriter{}
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), "test", func(a net.Addr) { addrs <- a })
	}()
	var addr string
	select {
	case a := <-addrs:
		addr = a.String()
	case err := <-served:
		t.Fatalf("serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}

	body := []byte(`{"action":"opened","pull_request":{"number":45,"title":"SECRET-TITLE"},"repository":{"id":2,"full_name":"talkable/example"}}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/github/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery: %d", resp.StatusCode)
	}
	if err := healthcheck(t.Context(), addr); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	// A long poll waiting at shutdown is answered at once, and serve returns.
	list := func(query string) (int, string) {
		req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, "http://"+addr+"/v1/events?"+query, nil)
		if err != nil {
			return 0, ""
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		var p struct {
			Next string `json:"next"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&p)
		return resp.StatusCode, p.Next
	}
	code, next := list("")
	if code != http.StatusOK || !strings.HasSuffix(next, ":1") {
		t.Fatalf("listing: %d next %q", code, next)
	}
	waiting := make(chan int, 1)
	go func() {
		code, _ := list("wait=30s&after=" + next)
		waiting <- code
	}()
	time.Sleep(300 * time.Millisecond)
	began := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}
	if code := <-waiting; code != http.StatusOK {
		t.Fatalf("long poll at shutdown: %d, want 200", code)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
	if fi, err := os.Stat(cfg.db); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("journal: %v %v", fi, err)
	}
	if s := logs.String(); strings.Contains(s, "SECRET") || strings.Contains(s, secret) || strings.Contains(s, token) || !strings.Contains(s, "d-1") {
		t.Fatalf("logs: %s", s)
	}
}

func TestHealthcheckFailsUnlessTheRelaySaysOK(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer down.Close()
	if err := healthcheck(t.Context(), down.Listener.Addr().String()); err == nil {
		t.Fatal("healthcheck passed on a 503")
	}
	// The status counts on its own: an error status fails even with "ok": true.
	okButFailing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer okButFailing.Close()
	if err := healthcheck(t.Context(), okButFailing.Listener.Addr().String()); err == nil {
		t.Fatal(`healthcheck passed on a 500 saying "ok": true`)
	}
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"healthcheck", "-addr", okButFailing.Listener.Addr().String()}, envOf(nil), strings.NewReader(""), &out, &errOut); code != 1 {
		t.Fatalf("magnum-relay healthcheck on a 500: exit %d, want 1", code)
	}
	notOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer notOK.Close()
	if err := healthcheck(t.Context(), notOK.Listener.Addr().String()); err == nil {
		t.Fatal("healthcheck passed on ok:false")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	// A listen address without a host, or on every interface, is checked on the loopback.
	for _, addr := range []string{":" + port, "0.0.0.0:" + port, up.Listener.Addr().String()} {
		if err := healthcheck(t.Context(), addr); err != nil {
			t.Errorf("healthcheck %s: %v", addr, err)
		}
	}
	out.Reset()
	errOut.Reset()
	if code := run(t.Context(), []string{"healthcheck", "-addr", ":" + port}, envOf(nil), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("magnum-relay healthcheck: exit %d %s", code, errOut.String())
	}
}

func TestAnUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, envOf(nil), strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("unknown command: exit %d %s", code, errOut.String())
	}
	out.Reset()
	if code := run(t.Context(), []string{"version"}, envOf(nil), strings.NewReader(""), &out, &errOut); code != 0 || out.String() != "magnum-relay dev\n" {
		t.Fatalf("version: exit %d %q", code, out.String())
	}
}

// lockedWriter lets the server's goroutines log while the test reads.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startServe runs serve on a loopback port until the returned cancel, and returns its address and its result.
func startServe(t *testing.T, cfg config) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	addrs := make(chan net.Addr, 1)
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, cfg, slog.New(slog.DiscardHandler), "test", func(a net.Addr) { addrs <- a })
	}()
	select {
	case a := <-addrs:
		return a.String(), cancel, served
	case err := <-served:
		t.Fatalf("serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	return "", cancel, served
}

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		addr:      "127.0.0.1:0",
		db:        filepath.Join(t.TempDir(), "relay.db"),
		secrets:   [][]byte{[]byte(secret)},
		tokens:    [][sha256.Size]byte{sha256.Sum256([]byte(token))},
		retention: 720 * time.Hour,
		shutdown:  shutdownTimeout,
	}
}

// sendPart opens a connection and sends a signed delivery's headers and the first half of its body.
func sendPart(t *testing.T, addr string, body []byte) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	head := "POST /github/webhook HTTP/1.1\r\nHost: relay.example.com\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\nX-GitHub-Event: pull_request\r\nX-GitHub-Delivery: d-flight\r\n" +
		"X-Hub-Signature-256: sha256=" + hex.EncodeToString(m.Sum(nil)) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head+string(body[:len(body)/2])); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestShutdownLetsADeliveryInFlightFinish(t *testing.T) {
	t.Parallel()
	addr, cancel, served := startServe(t, testConfig(t))
	body := []byte(`{"action":"opened","pull_request":{"number":45},"repository":{"id":2,"full_name":"talkable/example"}}`)
	conn := sendPart(t, addr, body)
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)
	if _, err := conn.Write(body[len(body)/2:]); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the delivery in flight got no answer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the delivery in flight: %d, want 202", resp.StatusCode)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestShutdownWaitsForTheBodyDeadlineAndClosesWhatOutlastsIt(t *testing.T) {
	t.Parallel()
	if shutdownTimeout < relay.WebhookDeadline {
		t.Fatalf("shutdown waits %v, less than a delivery may take (%v)", shutdownTimeout, relay.WebhookDeadline)
	}
	cfg := testConfig(t)
	cfg.shutdown = 300 * time.Millisecond
	addr, cancel, served := startServe(t, cfg)
	conn := sendPart(t, addr, []byte(`{"action":"opened","pull_request":{"number":45}}`))
	time.Sleep(100 * time.Millisecond)
	began := time.Now()
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept a stalled delivery past its shutdown timeout")
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("shutdown took %v with a 300ms timeout", took)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("the stalled connection was not closed: %v", err)
	}
}
