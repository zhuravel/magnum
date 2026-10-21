package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// [jira] is off by default, in Defaults() and in the committed defaults:
// no site, no account, no token file, and no round reads a ticket.
func TestJiraDefaultsToOff(t *testing.T) {
	if j := Defaults().Jira; j != (Jira{}) || j.On() {
		t.Fatalf("jira defaults: %+v (on %v)", j, j.On())
	}
	cfg, err := loadCommittedWithLocal(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jira != (Jira{}) || cfg.Jira.On() {
		t.Fatalf("the committed defaults: %+v", cfg.Jira)
	}
}

// jiraConfig is a valid configuration whose [jira] table is j, with a token
// file of mode 0600 at the path it returns (j.TokenFile "token" names it).
func jiraConfig(t *testing.T, j Jira) (*Config, string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "jira.token")
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Herdr.Socket = "/tmp/herdr.sock"
	cfg.Identities = []Identity{{Name: "z", Kind: "gh", Login: "z"}}
	cfg.Watches = []Watch{{Owner: "talkable", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
	if j.TokenFile == "token" {
		j.TokenFile = token
	}
	cfg.Jira = j
	return cfg, token
}

// The site is read over HTTPS only: a URL that is not https, has no host,
// carries credentials, a query or a fragment, or characters curl's config
// would have to quote, is refused; with a site set, the account's email
// (no colon, no space) and the token file must be named too. Without a
// site the table is off, whatever else it sets.
func TestJiraURLIsHTTPSOnly(t *testing.T) {
	for _, ok := range []string{"https://example.atlassian.net", "https://example.atlassian.net/", "https://jira.example.com:8443/jira"} {
		cfg, _ := jiraConfig(t, Jira{URL: ok, User: "alice@example.com", TokenFile: "token"})
		if err := cfg.Validate(); err != nil {
			t.Errorf("url %q: %v", ok, err)
		}
		if !cfg.Jira.On() {
			t.Errorf("url %q: not on", ok)
		}
	}
	for _, bad := range []string{"http://example.atlassian.net", "example.atlassian.net", "https://", "https://alice:pw@example.atlassian.net",
		"https://example.atlassian.net/?x=1", "https://example.atlassian.net/#x", "https://example.atlassian.net/a b",
		`https://example.atlassian.net/"x`, `https://example.atlassian.net/a\b`, "ftp://example.atlassian.net", "HTTPS://example.atlassian.net"} {
		cfg, _ := jiraConfig(t, Jira{URL: bad, User: "alice@example.com", TokenFile: "token"})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "jira.url") {
			t.Errorf("url %q: err = %v, want a jira.url error", bad, err)
		}
	}
	for user, want := range map[string]string{"": "jira.user", "alice:x@example.com": "jira.user", "alice @example.com": "jira.user", "alice@example.com\n": "jira.user"} {
		cfg, _ := jiraConfig(t, Jira{URL: "https://example.atlassian.net", User: user, TokenFile: "token"})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("user %q: err = %v, want %q", user, err, want)
		}
	}
	cfg, _ := jiraConfig(t, Jira{URL: "https://example.atlassian.net", User: "alice@example.com"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "jira.token_file") {
		t.Errorf("no token_file: err = %v", err)
	}
	cfg.Jira.URL = ""
	if err := cfg.Validate(); err != nil || cfg.Jira.On() {
		t.Errorf("jira off with an account and no token file: %v (on %v)", err, cfg.Jira.On())
	}
}

// The token file must be a regular file only its owner can read or write
// (0600, or 0400): the API token reads every issue the account sees. One
// open to its group or others, not a regular file, or missing is a warning
// naming it (no round reads a ticket with it, and round.ticket says why),
// never a config error: a ticket is a hint that must not keep the daemon
// from starting. With [jira] off there is no warning.
func TestJiraTokenFileModeIsAWarningNotAnError(t *testing.T) {
	cfg, token := jiraConfig(t, Jira{URL: "https://example.atlassian.net", User: "alice@example.com", TokenFile: "token"})
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(token, mode); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
		if w := cfg.Warnings(); len(w) != 0 {
			t.Errorf("mode %04o: warnings %q", mode, w)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o644, 0o606, 0o660} {
		if err := os.Chmod(token, mode); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %04o: %v, want a warning only", mode, err)
		}
		if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "jira.token_file "+token) || !strings.Contains(w, "chmod 600") {
			t.Errorf("mode %04o: warnings %q, want the token file named", mode, w)
		}
	}
	dir := t.TempDir()
	for file, want := range map[string]string{dir: "not a regular file", filepath.Join(dir, "missing.token"): "does not exist"} {
		cfg.Jira.TokenFile = file
		if err := cfg.Validate(); err != nil {
			t.Errorf("token file %s: %v", file, err)
		}
		if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, file) || !strings.Contains(w, want) {
			t.Errorf("token file %s: warnings %q, want %q", file, w, want)
		}
	}
	cfg.Jira.URL = ""
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings with jira off = %q, want none", w)
	}
}

// The user config's [jira] keys override the base's key by key, and a
// relative token_file is read beside the user config, as a private key file
// is.
func TestJiraOverlayMergesKeyByKey(t *testing.T) {
	root := repoRoot(t)
	defaults, err := os.ReadFile(filepath.Join(root, "config.defaults.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(defaults), "\n[jira]\n") {
		t.Fatal("config.defaults.toml has no [jira] table")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.toml")
	withSite := strings.Replace(string(defaults), "\n[jira]\n", "\n[jira]\nurl = \"https://example.atlassian.net\"\nuser = \"bob@example.com\"\n", 1)
	if err := os.WriteFile(base, []byte(withSite), 0o600); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "user", "config.toml")
	if err := os.MkdirAll(filepath.Dir(user), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("[jira]\nuser = \"alice@example.com\"\ntoken_file = \"keys/jira.token\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: root, UserConfig: user}, base)
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Jira
	if j.URL != "https://example.atlassian.net" || j.User != "alice@example.com" || !j.On() {
		t.Fatalf("merged jira: %+v", j)
	}
	if want := filepath.Join(filepath.Dir(user), "keys", "jira.token"); j.TokenFile != want {
		t.Fatalf("token_file = %q, want %q", j.TokenFile, want)
	}
}
