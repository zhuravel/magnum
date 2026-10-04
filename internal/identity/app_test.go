package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestJWTClaimsAndSignature(t *testing.T) {
	key := testKey(t)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	tok, err := signJWT(key, "Iv23licS9bkgs7IGVSoS", now)
	if err != nil {
		t.Fatal(err)
	}
	hdr, cl, err := verifyJWT(tok, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		t.Fatalf("header %v", hdr)
	}
	if cl.Iat != now.Unix()-60 || cl.Exp != now.Unix()+540 || cl.Iss != "Iv23licS9bkgs7IGVSoS" {
		t.Fatalf("claims %+v (now %d)", cl, now.Unix())
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("not base64url without padding: %s", tok)
	}
}

func TestLoadPrivateKey(t *testing.T) {
	key := testKey(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyFile, []byte(pkcs8PEM(t, key)), 0o600); err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(strings.TrimSpace(pkcs1PEM(key)), "\n", `\n`)

	cases := []struct {
		name, value, wantErr string
	}{
		{"pkcs1 text", pkcs1PEM(key), ""},
		{"pkcs8 text", pkcs8PEM(t, key), ""},
		{"pkcs1 with escaped newlines", escaped, ""},
		{"file path", keyFile, ""},
		{"empty", "", "is empty"},
		{"missing file", filepath.Join(dir, "nope.pem"), "read private key file"},
		{"garbage", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n", "parse private key"},
		{"not rsa", pkcs8PEM(t, ec), "not an RSA key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadPrivateKey("KEY_ENV", tc.value)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), "AAAA") {
					t.Fatalf("error leaks key material: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(key) {
				t.Fatal("parsed a different key")
			}
		})
	}
}

func TestTokenRefreshTiming(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()

	tok, err := app.Token(ctx)
	if err != nil || tok != "ghs_test1" || f.mintCount() != 1 {
		t.Fatalf("first: %q %v mints=%d", tok, err, f.mintCount())
	}
	if want := clock.Now().Add(time.Hour); !app.Expiry().Equal(want) {
		t.Fatalf("expiry %v want %v", app.Expiry(), want)
	}
	clock.Advance(45 * time.Minute) // exactly 15m left: still fresh
	if tok, _ = app.Token(ctx); tok != "ghs_test1" || f.mintCount() != 1 {
		t.Fatalf("at 15m left: %q mints=%d", tok, f.mintCount())
	}
	clock.Advance(time.Second) // < 15m left: refresh, in the background
	if tok, _ = app.Token(ctx); tok != "ghs_test1" {
		t.Fatalf("a usable token is returned while it refreshes, got %q", tok)
	}
	waitRefresh(t, app)
	if tok, _ = app.Token(ctx); tok != "ghs_test2" || f.mintCount() != 2 {
		t.Fatalf("after refresh: %q mints=%d", tok, f.mintCount())
	}
	app.Invalidate()
	if tok, _ = app.Token(ctx); tok != "ghs_test3" || f.mintCount() != 3 {
		t.Fatalf("after invalidate: %q mints=%d", tok, f.mintCount())
	}
}

func TestTokenReturnsUsableTokenWithoutWaitingForRefresh(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()
	if _, err := app.Token(ctx); err != nil {
		t.Fatal(err)
	}
	release := f.holdMints()
	defer release()

	clock.Advance(50 * time.Minute) // 10m left: refresh due, token still usable
	done := make(chan string, 1)
	go func() { tok, _ := app.Token(ctx); done <- tok }()
	select {
	case tok := <-done:
		if tok != "ghs_test1" {
			t.Fatalf("token %q", tok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Token waited for the mint although a usable token is cached")
	}
	<-f.entered // the background refresh did start
	release()
	waitRefresh(t, app)
	if tok, _ := app.Token(ctx); tok != "ghs_test2" {
		t.Fatalf("refreshed token %q", tok)
	}
}

func TestTokenSingleFlight(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	release := f.holdMints()
	defer release()
	app, _ := newTestApp(t, f, clock)

	const n = 8
	var wg sync.WaitGroup
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = app.Token(context.Background())
		}(i)
	}
	<-f.entered
	// Release the mint only once the other n-1 callers have joined its flight.
	deadline := time.Now().Add(5 * time.Second)
	for app.joined.Load() < n-1 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d callers joined the flight", app.joined.Load(), n-1)
		}
		time.Sleep(time.Millisecond)
	}
	release()
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || toks[i] != "ghs_test1" {
			t.Fatalf("caller %d: %q %v", i, toks[i], errs[i])
		}
	}
	if f.mintCount() != 1 || f.attemptCount() != 1 {
		t.Fatalf("mints = %d (attempts %d), want 1", f.mintCount(), f.attemptCount())
	}
}

func TestTokenWaitHonoursContext(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	release := f.holdMints()
	defer release()
	app, _ := newTestApp(t, f, clock)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := app.Token(ctx); errc <- err }()
	<-f.entered
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled caller kept waiting for the mint")
	}
	// The flight itself was detached: it finishes and serves the next caller.
	release()
	if tok, err := app.Token(context.Background()); err != nil || tok != "ghs_test1" || f.attemptCount() != 1 {
		t.Fatalf("after the flight: %q %v attempts=%d", tok, err, f.attemptCount())
	}
}

func TestTokenRefreshFailureKeepsValidToken(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()
	if _, err := app.Token(ctx); err != nil {
		t.Fatal(err)
	}
	f.failMints(500)

	clock.Advance(50 * time.Minute) // 10m left, refresh fails, old token still usable
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test1" {
		t.Fatalf("fallback: %q %v", tok, err)
	}
	waitRefresh(t, app)
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test1" || f.attemptCount() != 2 {
		t.Fatalf("within the backoff: %q %v attempts=%d, want the old token and no new attempt", tok, err, f.attemptCount())
	}
	clock.Advance(9 * time.Minute) // 1m left: too close to expiry to hand out
	tok, err := app.Token(ctx)
	if err == nil || tok != "" {
		t.Fatalf("want error near expiry, got %q %v", tok, err)
	}
	if strings.Contains(err.Error(), "ghs_") || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("error leaks a secret: %v", err)
	}
}

func TestTokenFailedMintBacksOff(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.failMints(503)
	app, _ := newTestApp(t, f, clock)
	ctx := context.Background()

	for i := range 3 {
		if _, err := app.Token(ctx); err == nil {
			t.Fatalf("call %d: want the mint error", i)
		}
	}
	if f.attemptCount() != 1 {
		t.Fatalf("attempts = %d; repeated callers must not start a mint each", f.attemptCount())
	}
	clock.Advance(failBackoff + time.Second)
	if _, err := app.Token(ctx); err == nil || f.attemptCount() != 2 {
		t.Fatalf("after the backoff: err=%v attempts=%d, want one more attempt", err, f.attemptCount())
	}
	// A recovered GitHub is noticed once the backoff ends.
	f.failMints(0)
	clock.Advance(failBackoff + time.Second)
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test1" {
		t.Fatalf("recovered: %q %v", tok, err)
	}
	// Invalidate (the 401 path) skips a running backoff.
	f.failMints(503)
	app.Invalidate()
	if _, err := app.Token(ctx); err == nil {
		t.Fatal("want the mint error")
	}
	f.failMints(0)
	app.Invalidate()
	if tok, err := app.Token(ctx); err != nil || tok != "ghs_test2" {
		t.Fatalf("after invalidate during a backoff: %q %v", tok, err)
	}
}

func TestReauth(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, layout := newTestApp(t, f, clock)
	ctx := context.Background()
	var _ func(context.Context) error = app.Reauth // what github.Client.Reauth takes
	hosts := filepath.Join(layout.GhConfigDir("talkable-app"), "hosts.yml")
	hostsToken := func() string {
		b, err := os.ReadFile(hosts)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if _, err := app.EnsureConfigDir(ctx); err != nil {
		t.Fatal(err)
	}
	// A token minted moments ago is not discarded: parallel 401s share one mint.
	if err := app.Reauth(ctx); err != nil || f.mintCount() != 1 {
		t.Fatalf("reauth right after a mint: %v mints=%d", err, f.mintCount())
	}
	clock.Advance(reauthCooldown + time.Second)
	if err := app.Reauth(ctx); err != nil {
		t.Fatal(err)
	}
	if f.mintCount() != 2 {
		t.Fatalf("mints = %d, want a fresh token", f.mintCount())
	}
	if h := hostsToken(); !strings.Contains(h, `"ghs_test2"`) || strings.Contains(h, "ghs_test1") {
		t.Fatalf("hosts.yml must carry the new token:\n%s", h)
	}
	if tok, _ := app.Token(ctx); tok != "ghs_test2" {
		t.Fatalf("token %q", tok)
	}

	// A failed mint is reported, and a backoff from an earlier failure does not block it.
	clock.Advance(reauthCooldown + time.Second)
	f.failMints(500)
	if err := app.Reauth(ctx); err == nil || !strings.Contains(err.Error(), "mint installation token") {
		t.Fatalf("err = %v", err)
	}
	f.failMints(0)
	if err := app.Reauth(ctx); err != nil || f.mintCount() != 3 {
		t.Fatalf("reauth after a failed one: %v mints=%d", err, f.mintCount())
	}
}

func TestReauthInParallelMintsOnce(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, _ := newTestApp(t, f, clock)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.Reauth(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.mintCount() != 2 {
		t.Fatalf("mints = %d, want the initial one plus a single reauth mint", f.mintCount())
	}
}

func TestMissingKeyFailsToken(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app := NewApp(f.id, newLayout(t), f.server.Client(), func(string) string { return "" }, clock.Now, WithBaseURL(f.server.URL))
	if _, err := app.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "MAGNUM_TEST_APP_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureConfigDirWritesFilesAtomically(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, layout := newTestApp(t, f, clock)
	ctx := context.Background()

	dir, err := app.EnsureConfigDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dir != layout.GhConfigDir("talkable-app") {
		t.Fatalf("dir %s", dir)
	}
	wantHosts := "# Managed by magnum for identity talkable-app; rewritten whenever the token rotates.\n" +
		"github.com:\n" +
		"    oauth_token: \"ghs_test1\"\n" +
		"    user: \"talkable[bot]\"\n" +
		"    git_protocol: \"ssh\"\n"
	wantConfig := "# Managed by magnum for identity talkable-app.\n" +
		"git_protocol: \"ssh\"\n" +
		"prompt: \"disabled\"\n"
	assertFile(t, filepath.Join(dir, "hosts.yml"), wantHosts, 0o600)
	assertFile(t, filepath.Join(dir, "config.yml"), wantConfig, 0o600)
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perms: %v %v", st.Mode(), err)
	}

	// Token rotation rewrites hosts.yml (the refresh runs in the background
	// and writes it when done); nothing else is left behind.
	clock.Advance(46 * time.Minute)
	if _, err := app.EnsureConfigDir(ctx); err != nil {
		t.Fatal(err)
	}
	waitRefresh(t, app)
	assertFile(t, filepath.Join(dir, "hosts.yml"), strings.Replace(wantHosts, "ghs_test1", "ghs_test2", 1), 0o600)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected files: %v", names)
	}

	// A deleted hosts.yml is restored even when the token did not change.
	if err := os.Remove(filepath.Join(dir, "hosts.yml")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.EnsureConfigDir(ctx); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(dir, "hosts.yml"), strings.Replace(wantHosts, "ghs_test1", "ghs_test2", 1), 0o600)
	if f.mintCount() != 2 {
		t.Fatalf("mints = %d", f.mintCount())
	}
}

func TestEnsureConfigDirTightensExistingDir(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, layout := newTestApp(t, f, clock)
	dir := layout.GhConfigDir("talkable-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perms %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(dir, "hosts.yml")); st.Mode().Perm() != 0o600 {
		t.Fatalf("hosts perms %v", st.Mode())
	}
}

func TestAppEnv(t *testing.T) {
	clock := newClock()
	f := newFakeGitHub(t, clock)
	app, layout := newTestApp(t, f, clock)
	env, err := app.Env(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GH_CONFIG_DIR": layout.GhConfigDir("talkable-app"), "GH_TOKEN": "", "GITHUB_TOKEN": "", "GH_HOST": "github.com"}
	if len(env) != len(want) {
		t.Fatalf("env %v", env)
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Fatalf("env[%s] = %q, want %q", k, got, v)
		}
	}
	if _, err := os.Stat(filepath.Join(env["GH_CONFIG_DIR"], "hosts.yml")); err != nil {
		t.Fatal("Env must leave a usable config dir behind:", err)
	}
	if app.Name() != "talkable-app" || app.Login() != "talkable[bot]" || app.Kind() != "app" {
		t.Fatal(app.Name(), app.Login(), app.Kind())
	}
}

func assertFile(t *testing.T, path, want string, perm os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("%s:\n%s\nwant:\n%s", path, b, want)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != perm {
		t.Fatalf("%s perms %v (%v), want %v", path, st.Mode().Perm(), err, perm)
	}
}

// The tracked .mise.toml ships a sentence where the key belongs. It must be
// reported as the placeholder, not as a missing key file.
func TestLoadPrivateKeyReportsThePlaceholder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // a bare name is looked up relative to the working directory
	if err := os.WriteFile(filepath.Join(dir, "appkey"), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "the private key in $KEY_ENV is the placeholder from .mise.toml; put the PEM in .mise.local.toml"
	for name, value := range map[string]string{
		"tracked placeholder":      "you-must-configure-this-in-your-.mise.local.toml",
		"placeholder any case":     "  You-Must-Configure-This  ",
		"placeholder with a slash": "you-must-configure/this",
		"a word, not a path":       "TODO",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadPrivateKey("KEY_ENV", value)
			if !errors.Is(err, ErrPlaceholderKey) || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
		})
	}
	for name, value := range map[string]string{
		"missing absolute path": filepath.Join(dir, "nope"),
		"missing home path":     "~/nope-magnum-test.pem",
		"missing bare pem name": "app.pem",
		"existing bare name":    "appkey",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadPrivateKey("KEY_ENV", value)
			if err == nil || errors.Is(err, ErrPlaceholderKey) {
				t.Fatalf("err = %v; a path must be read as a path", err)
			}
		})
	}
}

// The value .mise.toml really sets for every App key variable is the
// placeholder, whatever it is changed to later.
func TestTrackedMiseTomlKeysArePlaceholders(t *testing.T) {
	var mise struct {
		Env map[string]any `toml:"env"`
	}
	if _, err := toml.DecodeFile(filepath.Join("..", "..", ".mise.toml"), &mise); err != nil {
		t.Fatal(err)
	}
	n := 0
	for name, v := range mise.Env {
		s, ok := v.(string)
		if !ok || !strings.HasSuffix(name, "_PRIVATE_KEY") {
			continue
		}
		n++
		if _, err := loadPrivateKey(name, s); !errors.Is(err, ErrPlaceholderKey) {
			t.Errorf("%s from .mise.toml: err = %v, want the placeholder error", name, err)
		}
	}
	if n == 0 {
		t.Fatal("no *_PRIVATE_KEY placeholder in .mise.toml's [env]")
	}
}
