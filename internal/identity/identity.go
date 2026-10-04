// Package identity provides the GitHub identities magnum acts as.
//
// Two kinds exist, one per [[identity]] entry in config.toml:
//
//   - gh: the user's own gh login. The token lives in the macOS keyring and is
//     never read by magnum, only checked (`gh auth token --hostname
//     github.com`). Panes and subprocesses need no extra environment.
//   - app: a GitHub App installation. magnum signs an RS256 JWT with the App's
//     private key (stdlib only), mints a one-hour installation token, refreshes
//     it single-flight in the background when less than 15 minutes remain
//     (a failed mint is not retried for 30 seconds), and keeps a private gh
//     config dir (state/gh/<name>/hosts.yml + config.yml) in sync so panes and
//     subprocesses run gh as the App via GH_CONFIG_DIR. App.Reauth drops the
//     token and rewrites that dir after a 401. Its own REST calls run as
//     `gh api` through GhTransport unless [github] transport = "direct".
//
// Both kinds implement Source, including a health Check that prints PASS/FAIL
// lines with the exact fix for anything missing. Tokens are never logged or
// put into reports.
package identity

import (
	"context"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// Source is one GitHub identity magnum reads or writes as.
type Source interface {
	// Name is the identity's name from config.toml.
	Name() string
	// Login is the GitHub login reviews are posted as (REST form, e.g. "talkable[bot]").
	Login() string
	// Kind is "gh" or "app".
	Kind() string
	// Env returns the environment overlay that makes gh act as this identity
	// in a pane or subprocess: GH_CONFIG_DIR for apps (with GH_TOKEN and
	// GITHUB_TOKEN blanked so an inherited token cannot take precedence),
	// and GH_HOST=github.com for both kinds so an inherited GH_HOST cannot
	// redirect gh. For apps it first makes sure the token is fresh and the
	// config dir is written.
	Env(ctx context.Context) (map[string]string, error)
	// Check runs the identity's health checks. Problems GitHub or gh report
	// become FAIL lines; the error is non-nil only when the check could not
	// run to completion (network failure, cancelled context).
	Check(ctx context.Context) (Report, error)
}

// Report is the outcome of a health check. Lines start with "PASS ", "FAIL ",
// "WARN " (something to fix that does not fail the check), "INFO ", or
// "     fix: " (the exact remedy for the FAIL or WARN line above it).
type Report struct {
	Pass  bool
	Lines []string
}

// String joins the lines for printing.
func (r Report) String() string { return strings.Join(r.Lines, "\n") }

// reporter accumulates report lines; every line is redacted.
type reporter struct {
	lines  []string
	failed bool
}

func (r *reporter) add(prefix, msg string) { r.lines = append(r.lines, prefix+execx.Redact(msg)) }
func (r *reporter) pass(msg string)        { r.add("PASS ", msg) }
func (r *reporter) info(msg string)        { r.add("INFO ", msg) }

// warn records a problem that leaves the check passing, followed by one line
// per fix.
func (r *reporter) warn(msg string, fixes ...string) {
	r.add("WARN ", msg)
	for _, f := range fixes {
		r.add("     fix: ", f)
	}
}

// fail records a failed check followed by one line per fix.
func (r *reporter) fail(msg string, fixes ...string) {
	r.failed = true
	r.add("FAIL ", msg)
	for _, f := range fixes {
		r.add("     fix: ", f)
	}
}

func (r *reporter) report() Report { return Report{Pass: !r.failed, Lines: r.lines} }

// WatchedRepos returns the literal "owner/name" repositories that watches
// posting as identity name include (glob patterns cannot be enumerated and are
// skipped), in config order without duplicates. App checks probe these.
func WatchedRepos(c *config.Config, name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range c.Watches {
		if w.Identity != name {
			continue
		}
		for _, inc := range w.Include {
			if strings.ContainsAny(inc, `*?[\`) || !w.Matches(w.Owner, inc) {
				continue
			}
			full := w.Owner + "/" + inc
			if !seen[strings.ToLower(full)] {
				seen[strings.ToLower(full)] = true
				out = append(out, full)
			}
		}
	}
	return out
}
