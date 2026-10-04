package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

const (
	refreshBefore = 15 * time.Minute // refresh when less than this remains
	minUsable     = 2 * time.Minute  // never hand out a token closer to expiry
	// callTimeout bounds one network or gh call: a REST request, a token
	// mint, a gh subprocess.
	callTimeout = 30 * time.Second
	// failBackoff is how long a failed mint is remembered: until then Token
	// does not start another one (and so does not spawn gh again).
	failBackoff = 30 * time.Second
	// reauthCooldown: Reauth keeps a token minted this recently. A burst of
	// 401s from parallel calls then costs one mint, not one per call.
	reauthCooldown = 10 * time.Second
	maxBody        = 4 << 20
)

// App is a GitHub App installation identity (kind = "app").
type App struct {
	cfg     config.Identity
	layout  paths.Layout
	client  *http.Client
	getenv  func(string) string
	now     func() time.Time
	baseURL string
	run     execx.Runner
	repos   []string

	mu         sync.Mutex
	token      string
	expires    time.Time
	mintedAt   time.Time // when token was minted
	flight     *flight
	retryAfter time.Time    // no new mint before this, after a failed one
	failErr    error        // the failure retryAfter remembers
	joined     atomic.Int32 // callers that joined a flight already running (tests)

	wmu     sync.Mutex // serializes config dir writes
	written string     // token currently in hosts.yml
}

type flight struct {
	done chan struct{}
	err  error
}

var _ Source = (*App)(nil)

// AppOption customizes an App.
type AppOption func(*App)

// WithBaseURL points the REST calls at another API root (tests).
func WithBaseURL(u string) AppOption {
	return func(a *App) { a.baseURL = strings.TrimRight(u, "/") }
}

// WithRunner sets the runner Check uses for its `gh api repos/<repo>` probe.
func WithRunner(run execx.Runner) AppOption { return func(a *App) { a.run = run } }

// WithRepos sets the "owner/name" repositories Check expects in the
// installation (see WatchedRepos); the first one is probed with gh.
func WithRepos(fullNames ...string) AppOption {
	return func(a *App) { a.repos = append([]string(nil), fullNames...) }
}

// NewApp returns the App identity described by cfg. The PEM comes from the
// env var cfg.PrivateKeyEnv read through getenv (PEM text or a file path).
// Nil client, getenv or now default to a 30 s http.Client, os.Getenv and
// time.Now.
func NewApp(cfg config.Identity, layout paths.Layout, client *http.Client, getenv func(string) string, now func() time.Time, opts ...AppOption) *App {
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if now == nil {
		now = time.Now
	}
	a := &App{cfg: cfg, layout: layout, client: client, getenv: getenv, now: now, baseURL: DefaultBaseURL}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name implements Source.
func (a *App) Name() string { return a.cfg.Name }

// Login implements Source.
func (a *App) Login() string { return a.cfg.Login }

// Kind implements Source.
func (a *App) Kind() string { return "app" }

// ConfigDir is the identity's private GH_CONFIG_DIR.
func (a *App) ConfigDir() string { return a.layout.GhConfigDir(a.cfg.Name) }

// Env implements Source: a fresh token written to ConfigDir, selected via GH_CONFIG_DIR.
func (a *App) Env(ctx context.Context) (map[string]string, error) {
	dir, err := a.EnsureConfigDir(ctx)
	if err != nil {
		return nil, err
	}
	return appEnv(dir), nil
}

// appEnv selects the App's gh config dir and blanks the variables that would
// override it. GH_HOST is pinned (see pinnedHost).
func appEnv(dir string) map[string]string {
	return map[string]string{"GH_CONFIG_DIR": dir, "GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_HOST": pinnedHost}
}

// pinnedHost is the GH_HOST every identity env carries: magnum only works
// with github.com, so an inherited GH_HOST (an Enterprise login in the user's
// shell) must not send a pane's gh calls, or the identity's token, elsewhere.
const pinnedHost = "github.com"

// Expiry reports when the current installation token expires (zero if none).
// Safe to persist; the token itself never is.
func (a *App) Expiry() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expires
}

// Invalidate forgets the cached token's validity so the next Token call mints
// a new one, even right after a failed mint. Reauth is the 401 entry point.
func (a *App) Invalidate() {
	a.mu.Lock()
	a.expires, a.retryAfter = time.Time{}, time.Time{}
	a.mu.Unlock()
}

// Reauth discards the cached installation token (GitHub answered 401: it was
// revoked, or the installation was suspended and restored), mints a fresh one
// and rewrites ConfigDir so panes and gh subprocesses pick it up. Its
// signature matches github.Client.Reauth. A token minted less than 10 seconds
// ago is kept, so parallel 401s share one mint.
func (a *App) Reauth(ctx context.Context) error {
	a.mu.Lock()
	if a.token == "" || a.now().Sub(a.mintedAt) >= reauthCooldown {
		a.expires, a.retryAfter = time.Time{}, time.Time{}
	}
	a.mu.Unlock()
	_, err := a.EnsureConfigDir(ctx)
	return err
}

// Token returns a usable installation token, minting one when needed.
// Concurrent callers share a single mint. A token with at least 15 minutes
// left is returned at once; with 2 to 15 minutes left it is returned at once
// too and a refresh runs in the background (every new token is written to
// ConfigDir); with less, or none, callers wait for the mint, giving up when
// ctx ends. After a failed mint no new one starts for 30 seconds.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	now := a.now()
	left := a.expires.Sub(now)
	if a.token != "" && left >= refreshBefore {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	tok, usable := a.token, a.token != "" && left > minUsable
	fl := a.flight
	switch {
	case fl != nil:
		a.joined.Add(1)
	case now.Before(a.retryAfter):
		err := a.failErr
		a.mu.Unlock()
		if usable {
			return tok, nil
		}
		return "", err
	default:
		fl = &flight{done: make(chan struct{})}
		a.flight = fl
		// Detached from ctx so one caller giving up does not fail the others.
		go a.refresh(context.WithoutCancel(ctx), fl)
	}
	a.mu.Unlock()
	if usable {
		return tok, nil
	}

	select {
	case <-fl.done:
	case <-ctx.Done():
		return "", fmt.Errorf("identity %s: wait for token: %w", a.cfg.Name, ctx.Err())
	}
	a.mu.Lock()
	tok, exp := a.token, a.expires
	a.mu.Unlock()
	if fl.err == nil || (tok != "" && exp.Sub(a.now()) > minUsable) {
		return tok, nil
	}
	return "", fl.err
}

// install records a freshly minted token (callers hold no lock). A token that
// would expire sooner than the cached one is ignored.
func (a *App) install(tok string, exp time.Time) {
	a.mu.Lock()
	if a.token == "" || !exp.Before(a.expires) {
		a.token, a.expires = tok, exp
	}
	a.mintedAt, a.retryAfter, a.failErr = a.now(), time.Time{}, nil
	a.mu.Unlock()
}

func (a *App) refresh(ctx context.Context, fl *flight) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	tok, exp, err := a.mint(ctx)
	if err == nil {
		a.install(tok, exp)
	}
	a.mu.Lock()
	if err != nil {
		a.retryAfter, a.failErr = a.now().Add(failBackoff), err
	}
	fl.err = err
	a.flight = nil
	a.mu.Unlock()
	if err == nil {
		// Best effort here; EnsureConfigDir retries and reports write errors.
		_ = a.syncConfigDir()
	}
	close(fl.done)
}

// EnsureConfigDir makes sure a fresh token exists and that ConfigDir holds it
// (hosts.yml + config.yml, 0600 files in a 0700 dir, written atomically). It
// rewrites the files whenever the token changed or a file is missing; call it
// on every daemon tick so long-lived panes never see an expired token.
func (a *App) EnsureConfigDir(ctx context.Context) (string, error) {
	if _, err := a.Token(ctx); err != nil {
		return "", err
	}
	if err := a.syncConfigDir(); err != nil {
		return "", fmt.Errorf("identity %s: %w", a.cfg.Name, err)
	}
	return a.ConfigDir(), nil
}

func (a *App) issuer() string {
	if a.cfg.ClientID != "" {
		return a.cfg.ClientID
	}
	return strconv.FormatInt(a.cfg.AppID, 10)
}

// privateKey loads the App's key: from private_key_file when set, else
// from the env var private_key_env.
func (a *App) privateKey() (*rsa.PrivateKey, error) {
	if a.cfg.PrivateKeyFile != "" {
		return loadPrivateKeyFile(a.cfg.PrivateKeyFile)
	}
	return loadPrivateKey(a.cfg.PrivateKeyEnv, a.getenv(a.cfg.PrivateKeyEnv))
}

// keySource names where the App's key comes from, for messages.
func (a *App) keySource() string {
	if a.cfg.PrivateKeyFile != "" {
		return "private_key_file " + a.cfg.PrivateKeyFile
	}
	return "$" + a.cfg.PrivateKeyEnv
}

func (a *App) privateKeyJWT() (string, error) {
	key, err := a.privateKey()
	if err != nil {
		return "", err
	}
	return signJWT(key, a.issuer(), a.now())
}

// mint exchanges a fresh JWT for an installation token.
func (a *App) mint(ctx context.Context) (string, time.Time, error) {
	jwt, err := a.privateKeyJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity %s: %w", a.cfg.Name, err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", a.cfg.InstallationID)
	if err := a.call(ctx, http.MethodPost, path, jwt, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("identity %s: mint installation token: %w", a.cfg.Name, err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", time.Time{}, fmt.Errorf("identity %s: mint installation token: response without token or expires_at", a.cfg.Name)
	}
	return out.Token, out.ExpiresAt, nil
}

// apiError is a non-2xx GitHub answer.
type apiError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// call performs one REST request authenticated with bearer and decodes a 2xx
// JSON body into out. Non-2xx answers return *apiError; anything else is a
// transport error.
func (a *App) call(ctx context.Context, method, path, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "magnum")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Message == "" {
			msg.Message = http.StatusText(resp.StatusCode)
		}
		return &apiError{Method: method, Path: path, Status: resp.StatusCode, Message: execx.Redact(msg.Message)}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func isAPIError(err error) (*apiError, bool) {
	var ae *apiError
	ok := errors.As(err, &ae)
	return ae, ok
}
