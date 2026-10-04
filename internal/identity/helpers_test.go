package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
)

var (
	keyOnce sync.Once
	keyVal  *rsa.PrivateKey
)

// testKey returns one RSA key shared by all tests (generation is slow).
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyVal = k
	})
	return keyVal
}

func pkcs1PEM(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func pkcs8PEM(t *testing.T, k any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type jwtClaims struct {
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

// verifyJWT checks the RS256 signature with pub and returns the header and claims.
func verifyJWT(tok string, pub *rsa.PublicKey) (map[string]string, jwtClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, jwtClaims{}, fmt.Errorf("want 3 parts, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, jwtClaims{}, fmt.Errorf("signature encoding: %w", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, jwtClaims{}, fmt.Errorf("signature: %w", err)
	}
	var hdr map[string]string
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, jwtClaims{}, err
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil, jwtClaims{}, err
	}
	var cl jwtClaims
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, jwtClaims{}, err
	}
	if err := json.Unmarshal(raw, &cl); err != nil {
		return nil, jwtClaims{}, err
	}
	return hdr, cl, nil
}

func appIdentity() config.Identity {
	return config.Identity{
		Name:           "talkable-app",
		Kind:           "app",
		Login:          "talkable[bot]",
		AppID:          2700610,
		ClientID:       "Iv23licS9bkgs7IGVSoS",
		InstallationID: 105365229,
		PrivateKeyEnv:  "MAGNUM_TEST_APP_KEY",
	}
}

// fakeGitHub is an httptest stand-in for the GitHub App endpoints.
type fakeGitHub struct {
	t      *testing.T
	pub    *rsa.PublicKey
	clock  *fakeClock
	id     config.Identity
	server *httptest.Server

	mu         sync.Mutex
	appPerms   map[string]string
	instPerms  map[string]string
	repos      []string
	selection  string
	mintStatus int           // non-zero: mint answers with this status
	gate       chan struct{} // non-nil: mint blocks until closed
	entered    chan struct{} // receives once per mint request
	mints      int           // successful mints
	attempts   int           // mint requests received, failed ones included
	repos401   int           // the next repos401 repository listings answer 401
	repoPages  int           // repository listing requests received
	lastToken  string
	ttl        time.Duration
}

func newFakeGitHub(t *testing.T, clock *fakeClock) *fakeGitHub {
	f := &fakeGitHub{
		t:         t,
		pub:       &testKey(t).PublicKey,
		clock:     clock,
		id:        appIdentity(),
		appPerms:  map[string]string{"pull_requests": "write", "contents": "read", "metadata": "read"},
		instPerms: map[string]string{"pull_requests": "write", "contents": "read", "metadata": "read"},
		repos:     []string{"talkable/talkable", "talkable/other"},
		selection: "selected",
		ttl:       time.Hour,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

func (f *fakeGitHub) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeGitHub) repoPageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoPages
}

// failMints makes the mint endpoint answer status (0 = succeed again).
func (f *fakeGitHub) failMints(status int) {
	f.mu.Lock()
	f.mintStatus = status
	f.mu.Unlock()
}

// holdMints makes mints block until the returned func is called; entered
// receives once per mint request.
func (f *fakeGitHub) holdMints() (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate, f.entered = gate, make(chan struct{}, 64)
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// waitRefresh waits for the refresh in progress, if any; the config dir is
// written by the time it returns.
func waitRefresh(t *testing.T, a *App) {
	t.Helper()
	a.mu.Lock()
	fl := a.flight
	a.mu.Unlock()
	if fl == nil {
		return
	}
	select {
	case <-fl.done:
	case <-time.After(5 * time.Second):
		t.Fatal("token refresh did not finish")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) jwtOK(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	_, cl, err := verifyJWT(tok, f.pub)
	if err != nil {
		return false
	}
	now := f.clock.Now().Unix()
	return cl.Iss == f.id.ClientID && cl.Iat <= now && cl.Exp > now
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "missing headers"})
		return
	}
	inst := "/app/installations/" + strconv.FormatInt(f.id.InstallationID, 10)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		if !f.jwtOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
			return
		}
		f.mu.Lock()
		perms := f.appPerms
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"id": f.id.AppID, "slug": "talkable", "name": "talkable",
			"owner":       map[string]any{"login": "talkable", "type": "Organization"},
			"permissions": perms,
		})
	case r.Method == http.MethodGet && r.URL.Path == inst:
		if !f.jwtOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		f.mu.Lock()
		perms, sel := f.instPerms, f.selection
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"id": f.id.InstallationID, "app_id": f.id.AppID,
			"account":              map[string]any{"login": "talkable", "type": "Organization"},
			"permissions":          perms,
			"repository_selection": sel,
			"suspended_at":         nil,
		})
	case r.Method == http.MethodPost && r.URL.Path == inst+"/access_tokens":
		if !f.jwtOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		f.mu.Lock()
		gate, entered, status := f.gate, f.entered, f.mintStatus
		f.attempts++
		f.mu.Unlock()
		if entered != nil {
			entered <- struct{}{}
		}
		if gate != nil {
			<-gate
		}
		if status != 0 {
			writeJSON(w, status, map[string]string{"message": "boom"})
			return
		}
		f.mu.Lock()
		f.mints++
		f.lastToken = fmt.Sprintf("ghs_test%d", f.mints)
		tok, exp := f.lastToken, f.clock.Now().Add(f.ttl)
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": tok, "expires_at": exp.UTC().Format(time.RFC3339),
			"permissions": map[string]string{"pull_requests": "write"}, "repository_selection": "selected",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
		f.mu.Lock()
		last, repos := f.lastToken, f.repos
		f.repoPages++
		revoked := f.repos401 > 0
		if revoked {
			f.repos401--
		}
		f.mu.Unlock()
		if revoked {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		if auth := r.Header.Get("Authorization"); last == "" || (auth != "Bearer "+last && auth != "token "+last) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if perPage <= 0 {
			perPage = 30
		}
		if page <= 0 {
			page = 1
		}
		var list []map[string]string
		for i := (page - 1) * perPage; i < len(repos) && i < page*perPage; i++ {
			list = append(list, map[string]string{"full_name": repos[i]})
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repositories": list})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// newTestApp wires an App to the fake server with the shared key.
func newTestApp(t *testing.T, f *fakeGitHub, clock *fakeClock, opts ...AppOption) (*App, paths.Layout) {
	t.Helper()
	layout := paths.Layout{Home: t.TempDir()}
	keyPEM := pkcs1PEM(testKey(t))
	getenv := func(k string) string {
		if k == "MAGNUM_TEST_APP_KEY" {
			return keyPEM
		}
		return ""
	}
	opts = append([]AppOption{WithBaseURL(f.server.URL)}, opts...)
	return NewApp(f.id, layout, f.server.Client(), getenv, clock.Now, opts...), layout
}

func newLayout(t *testing.T) paths.Layout { return paths.Layout{Home: t.TempDir()} }

// hasLine reports whether lines contains want exactly.
func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func assertLines(t *testing.T, r Report, want ...string) {
	t.Helper()
	for _, w := range want {
		if !hasLine(r.Lines, w) {
			t.Errorf("missing line %q in report:\n%s", w, strings.Join(r.Lines, "\n"))
		}
	}
}

func assertNoSecrets(t *testing.T, r Report) {
	t.Helper()
	all := strings.Join(r.Lines, "\n")
	for _, s := range []string{"ghs_", "gho_", "eyJ", "BEGIN"} {
		if strings.Contains(all, s) {
			t.Fatalf("report leaks %q:\n%s", s, all)
		}
	}
}
