package usage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAuth writes auth.json under home with body, its mtime at.
func writeAuth(t *testing.T, home, body string, at time.Time) string {
	t.Helper()
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	chatgptAuth = `{"OPENAI_API_KEY":null,"tokens":{"id_token":"eyJ-id-secret","access_token":"eyJ-access-secret",` +
		`"refresh_token":"rt-secret","account_id":"acct-one"},"last_refresh":"2026-10-21T22:00:00Z"}`
	apiKeyAuth = `{"OPENAI_API_KEY":"sk-secret","tokens":null}`
)

// TestCodexLoginIsTheAccountsFingerprintOnly: the login is the first 12 hex
// digits of the sha256 of tokens.account_id, never a token: a refresh that
// rewrote every token of the same account keeps it, another account changes
// it, and API-key mode is one fixed marker.
func TestCodexLoginIsTheAccountsFingerprintOnly(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 10, 22, 1, 11, 0, 0, time.UTC)
	writeAuth(t, home, chatgptAuth, at)
	one, err := CodexLogin(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Fingerprint) != 12 || strings.Trim(one.Fingerprint, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint %q: want 12 hex digits", one.Fingerprint)
	}
	if one.Fingerprint != fingerprint("acct-one") || !one.ChangedAt.Equal(at) {
		t.Fatalf("login = %+v, want the account's fingerprint changed at %v", one, at)
	}
	for _, secret := range []string{"eyJ", "secret", "acct-one", "rt-"} {
		if strings.Contains(one.Fingerprint, secret) {
			t.Fatalf("fingerprint %q holds %q", one.Fingerprint, secret)
		}
	}

	refreshed := strings.NewReplacer("eyJ-access-secret", "eyJ-access-new", "rt-secret", "rt-new").Replace(chatgptAuth)
	writeAuth(t, home, refreshed, at.Add(time.Hour))
	if same, err := CodexLogin(home); err != nil || same.Fingerprint != one.Fingerprint || !same.ChangedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("after a token refresh: %+v, %v; want the same fingerprint, changed later", same, err)
	}

	writeAuth(t, home, strings.Replace(chatgptAuth, "acct-one", "acct-two", 1), at)
	if two, err := CodexLogin(home); err != nil || two.Fingerprint == one.Fingerprint {
		t.Fatalf("another account: %+v, %v; want another fingerprint", two, err)
	}

	writeAuth(t, home, apiKeyAuth, at)
	if key, err := CodexLogin(home); err != nil || key.Fingerprint != fingerprint(LoginAPIKey) {
		t.Fatalf("API-key mode: %+v, %v; want the %q marker's fingerprint", key, err, LoginAPIKey)
	}
}

// TestCodexLoginFollowsTheSymlink: clauth makes auth.json a symlink to a
// profile's file; the login is the target's account, changed at the newer
// of the link's and the target's modification time, so a switch of the link
// and a refresh of the target both count.
func TestCodexLoginFollowsTheSymlink(t *testing.T) {
	home, profiles := t.TempDir(), t.TempDir()
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	target := writeAuth(t, profiles, chatgptAuth, old)
	link := filepath.Join(home, "auth.json")
	before := time.Now().Add(-time.Second)
	if err := os.Symlink(target, link); err != nil { // the switch: the link's own mtime is now
		t.Fatal(err)
	}
	got, err := CodexLogin(home)
	if err != nil || got.Fingerprint != fingerprint("acct-one") || got.ChangedAt.Before(before) || got.ChangedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("login through the link = %+v, %v; want acct-one's fingerprint changed at the link's own mtime (after %v)", got, err, before)
	}
	refreshed := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(target, refreshed, refreshed); err != nil {
		t.Fatal(err)
	}
	if got, err := CodexLogin(home); err != nil || !got.ChangedAt.Equal(refreshed) {
		t.Fatalf("after the target's refresh: %+v, %v; want changed at %v", got, err, refreshed)
	}
}

// TestNoAuthFileOrNoAccountIsNoLogin: no auth.json, one without an account
// or a key, and a dangling link are no login; a file that is not JSON is an
// error that quotes nothing of it.
func TestNoAuthFileOrNoAccountIsNoLogin(t *testing.T) {
	home := t.TempDir()
	if _, err := CodexLogin(home); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("no file: %v, want ErrNoLogin", err)
	}
	for _, body := range []string{`{}`, `{"OPENAI_API_KEY":null,"tokens":null}`, `{"OPENAI_API_KEY":"","tokens":{"account_id":""}}`} {
		writeAuth(t, home, body, time.Now())
		if _, err := CodexLogin(home); !errors.Is(err, ErrNoLogin) {
			t.Fatalf("%s: %v, want ErrNoLogin", body, err)
		}
	}
	writeAuth(t, home, `{"tokens":{"access_token":"eyJ-secret`, time.Now())
	_, err := CodexLogin(home)
	if err == nil || errors.Is(err, ErrNoLogin) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("truncated file: %v, want an error that quotes nothing of it", err)
	}
	dangling := t.TempDir()
	if err := os.Symlink(filepath.Join(dangling, "gone.json"), filepath.Join(dangling, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexLogin(dangling); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("dangling link: %v, want ErrNoLogin", err)
	}
}
