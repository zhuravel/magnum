package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// Doctor checks [jira] without a network call: off is a SKIP; on, the
// site's scheme and the token file (it exists, only its owner can open
// it, it holds a token) pass, or warn with the fix, since a round goes on
// without a ticket. Only MAGNUM_LIVE_JIRA=1 asks Jira whether it accepts
// the account (GET /rest/api/2/myself), a refusal warning with its class,
// never the answer's body.
func TestDoctorChecksTheJiraTokenFileAndAsksJiraOnlyWhenLive(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "jira.token")
	if err := os.WriteFile(token, []byte("jira-api-token-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	on := config.Jira{URL: "https://example.atlassian.net", User: "alice@example.com", TokenFile: token}
	run := func(j config.Jira, live, status, body string) (doctorCheck, *execx.Fake) {
		t.Helper()
		fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Result: execx.Result{Stdout: []byte(body + "\n" + status)}}}}
		d := doctorDeps{Config: &config.Config{Jira: j}, Run: fake, Getenv: func(k string) string {
			if k == "MAGNUM_LIVE_JIRA" {
				return live
			}
			return ""
		}}
		cs := doctorJira(t.Context(), d)
		if len(cs) != 1 || cs[0].Name != "jira" {
			t.Fatalf("checks = %+v, want one jira check", cs)
		}
		return cs[0], fake
	}

	c, fake := run(config.Jira{}, "1", "200", "{}")
	if c.Status != doctorSkip || !strings.Contains(c.Detail, "off") || len(fake.Calls) != 0 {
		t.Errorf("off: %+v, curl calls %d", c, len(fake.Calls))
	}
	c, fake = run(on, "", "200", "{}")
	if c.Status != doctorPass || !strings.Contains(c.Detail, "https://example.atlassian.net") || !strings.Contains(c.Detail, "MAGNUM_LIVE_JIRA=1") || len(fake.Calls) != 0 {
		t.Errorf("on: %+v, curl calls %d: want a pass without a network call", c, len(fake.Calls))
	}
	c, fake = run(on, "1", "200", `{"displayName":"Alice Example","emailAddress":"alice@example.com"}`)
	calls := fake.CallsWithPrefix("curl")
	if c.Status != doctorPass || !strings.Contains(c.Detail, "accepts") || len(calls) != 1 ||
		!strings.Contains(string(calls[0].Stdin), "/rest/api/2/myself") || strings.Contains(c.Detail, "Alice") {
		t.Errorf("live: %+v, curl calls %d", c, len(calls))
	}
	c, _ = run(on, "1", "401", `{"message":"Client must be authenticated"}`)
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "HTTP 401") || strings.Contains(c.Detail, "authenticated") || c.Fix == "" {
		t.Errorf("live 401: %+v", c)
	}

	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	c, fake = run(on, "1", "200", "{}")
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "chmod 600") || !strings.Contains(c.Fix, "chmod 600 "+token) || len(fake.Calls) != 0 {
		t.Errorf("open token file: %+v, curl calls %d", c, len(fake.Calls))
	}
	missing := on
	missing.TokenFile = filepath.Join(dir, "missing.token")
	c, _ = run(missing, "", "200", "{}")
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "does not exist") || !strings.Contains(c.Fix, missing.TokenFile) {
		t.Errorf("missing token file: %+v", c)
	}
}
