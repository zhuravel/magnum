package agents

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable: %v", name, err)
	}
	return loc
}

// A reset read from pane text is capped: a review or an injected line that
// quotes a far-off date must not pause a kind for years.
func TestParseResetCapped(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"You've hit your usage limit. Try again at Jan 1st, 2099 9:00 AM.",
		"Claude AI usage limit reached|4102444800",
		"You've hit your usage limit. Try again in 400 days.",
	} {
		h := ClassifyAt(text, now)
		if h.Kind != HealthUsageLimit || h.ResetAt == nil || !h.ResetAt.Equal(now.Add(MaxReset)) {
			t.Errorf("%q: %+v, want usage_limit until %v", text, h, now.Add(MaxReset))
		}
	}
	if h := ClassifyAt("You've hit your usage limit. Try again in 2 hours.", now); h.ResetAt == nil || !h.ResetAt.Equal(now.Add(2*time.Hour)) {
		t.Errorf("a near reset is kept: %+v", h)
	}
}

func TestClassifyFixtures(t *testing.T) {
	kyiv := mustLoc(t, "Europe/Kyiv")
	ny := mustLoc(t, "America/New_York")
	// 12:00 UTC = 15:00 in Kyiv (EEST) on 2026-10-03.
	now := time.Date(2026, 10, 3, 15, 0, 0, 0, kyiv)
	at := func(loc *time.Location, y int, mo time.Month, d, h, mi int) *time.Time {
		v := time.Date(y, mo, d, h, mi, 0, 0, loc)
		return &v
	}
	cases := []struct {
		name   string
		text   string
		kind   HealthKind
		reset  *time.Time
		detail string
	}{
		{"codex usage same day", `› Review https://github.com/talkable/talkable/pull/11920

■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:45 PM.

› Implement {feature}

  100% context left · ? for shortcuts`, HealthUsageLimit, at(kyiv, 2026, 10, 3, 15, 45), "You've hit your usage limit"},
		{"codex usage other day curly", "■ You’ve hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 5th, 2026 9:05 AM.",
			HealthUsageLimit, at(kyiv, 2026, 10, 5, 9, 5), "usage limit"},
		{"codex usage duration", "🖐  You've hit your usage limit. Try again in 1 day 4 hours 3 minutes.", HealthUsageLimit,
			at(kyiv, 2026, 10, 4, 19, 3), ""},
		{"codex usage short duration", "You've hit your usage limit. Try again in 4h 30m.", HealthUsageLimit, at(kyiv, 2026, 10, 3, 19, 30), ""},
		{"codex usage later", "■ You've hit your usage limit. Try again later.", HealthUsageLimit, nil, ""},
		{"codex usage time already passed", "You've hit your usage limit. Try again at 9:15 AM.", HealthUsageLimit, at(kyiv, 2026, 10, 3, 9, 15), ""},
		{"claude reset after midnight", "You've hit your limit · resets 2am", HealthUsageLimit, at(kyiv, 2026, 10, 4, 2, 0), ""},
		{"claude limit tz", "  ⎿  You've hit your limit · resets 5pm (Europe/Kyiv)\n     /upgrade to increase your usage limit.", HealthUsageLimit,
			at(kyiv, 2026, 10, 3, 17, 0), "resets 5pm"},
		{"claude old reset", "Claude usage limit reached. Your limit will reset at 11pm (America/New_York).", HealthUsageLimit,
			at(ny, 2026, 10, 3, 23, 0), ""},
		{"claude epoch", "Claude AI usage limit reached|1759514400", HealthUsageLimit, func() *time.Time { v := time.Unix(1759514400, 0); return &v }(), ""},
		{"claude 5-hour", "5-hour limit reached ∙ resets 4:30pm", HealthUsageLimit, at(kyiv, 2026, 10, 3, 16, 30), ""},
		{"claude weekly date", "You've hit your weekly limit · resets Oct 7, 9am (Europe/Kyiv)", HealthUsageLimit, at(kyiv, 2026, 10, 7, 9, 0), ""},
		{"claude out of extra usage", "You're out of extra usage · resets 6pm", HealthUsageLimit, at(kyiv, 2026, 10, 3, 18, 0), ""},

		{"claude oauth expired", `  ⎿  API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired. Please obtain a new token or refresh your existing token."},"request_id":"req_011CTx"} · Please run /login`, HealthLoginRequired, nil, "401"},
		{"claude invalid key", "Invalid API key · Please run /login", HealthLoginRequired, nil, ""},
		{"claude not logged in", "Not logged in · Please run /login", HealthLoginRequired, nil, ""},
		{"codex 401", `■ unexpected status 401 Unauthorized: {"detail":"Could not parse your authentication token. Please try signing in again."}`, HealthLoginRequired, nil, ""},
		{"codex refresh", "■ Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.", HealthLoginRequired, nil, ""},
		{"codex onboarding", "  Sign in with ChatGPT to use Codex as part of your paid plan\n  or connect an API key for usage-based billing\n\n> 1. Sign in with ChatGPT", HealthLoginRequired, nil, ""},
		{"codex login hint", "Not logged in. Run `codex login` to authenticate.", HealthLoginRequired, nil, ""},
		{"claude auth login hint", "Credentials missing: run claude auth login", HealthLoginRequired, nil, ""},

		{"claude 529", `  ⎿  API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, HealthOverloaded, nil, "529"},
		{"claude 500", `  ⎿  API Error: 500 {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, HealthOverloaded, nil, ""},
		{"claude retrying", "  ⎿  API Error (Request timed out.) · Retrying in 5 seconds… (attempt 3/10)", HealthOverloaded, nil, ""},
		{"codex stream", "⚠ stream error: stream disconnected before completion: Transport error: error decoding response body; retrying 2/5 in 1.6s…", HealthOverloaded, nil, ""},
		{"codex demand", "■ We're currently experiencing high demand, which may cause temporary errors.", HealthOverloaded, nil, ""},
		{"codex 429", "■ exceeded retry limit, last status: 429 Too Many Requests", HealthOverloaded, nil, ""},
		{"codex 503", "■ unexpected status 503 Service Unavailable: upstream connect error", HealthOverloaded, nil, ""},
		{"codex reconnecting", "⚠ Reconnecting... 2/5", HealthOverloaded, nil, ""},
		{"rate limit exceeded", "Error: rate limit exceeded, please slow down", HealthOverloaded, nil, ""},

		{"codex approval", `  Would you like to run the following command?

  Reason: Need network access to fetch gems

  $ bundle install

› 1. Yes, proceed (y)
  2. Yes, and don't ask again for this command (a)
  3. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`, HealthBlocked, nil, ""},
		{"claude approval", `╭──────────────────────────────────────────────╮
│ Bash command                                 │
│   rm -rf tmp/cache                           │
│ Do you want to proceed?                      │
│ ❯ 1. Yes                                     │
│   2. No, and tell Claude what to do differently (esc) │
╰──────────────────────────────────────────────╯`, HealthBlocked, nil, "Do you want to proceed?"},
		{"allow yn", "Allow? (y/n)", HealthBlocked, nil, ""},
		{"claude trust", "Do you trust the files in this folder?\n\n/Users/bohdan/Projects/talkable.review1", HealthBlocked, nil, ""},
		{"codex edits", "Would you like to make the following edits?", HealthBlocked, nil, ""},

		{"codex interrupted", "■ Conversation interrupted - tell the model what to do differently. Something went wrong? Hit `/feedback` to report the issue.", HealthStalled, nil, ""},
		{"claude interrupted", "  ⎿  Interrupted · What should Claude do instead?", HealthStalled, nil, ""},
		{"codex context", "■ Codex ran out of room in the model's context window. Start a new conversation or clear earlier history before retrying.", HealthStalled, nil, ""},

		{"ok review", "Posted review 3012345678 (REQUEST_CHANGES) as talkable[bot].\nMAGNUM_RESULT {\"status\":\"posted\",\"review_id\":3012345678}", HealthOK, nil, ""},
		{"ok finding mentions limits", "P2 app/services/rate_limiter.rb:42 - the rate limit window resets at midnight; reviewers asked for a 401 test.", HealthOK, nil, ""},
		{"ok empty", "", HealthOK, nil, ""},

		{"latest wins usage after reconnect", "⚠ Reconnecting... 1/5\n⚠ Reconnecting... 2/5\n■ You've hit your usage limit. Try again in 30 minutes.", HealthUsageLimit, at(kyiv, 2026, 10, 3, 15, 30), ""},
		{"latest wins blocked after old limit", "■ You've hit your usage limit. Try again later.\n› continue\nWould you like to run the following command?\n$ git push", HealthBlocked, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ClassifyAt(tc.text, now)
			if h.Kind != tc.kind {
				t.Fatalf("Kind = %s, want %s (detail %q)", h.Kind, tc.kind, h.Detail)
			}
			// reset is what the text says; ResetAt is capped at MaxReset.
			if limit := now.Add(MaxReset); tc.reset != nil && tc.reset.After(limit) {
				tc.reset = &limit
			}
			switch {
			case tc.reset == nil && h.ResetAt != nil:
				t.Fatalf("ResetAt = %v, want nil", h.ResetAt)
			case tc.reset != nil && (h.ResetAt == nil || !h.ResetAt.Equal(*tc.reset)):
				t.Fatalf("ResetAt = %v, want %v", h.ResetAt, tc.reset)
			}
			if tc.detail != "" && !strings.Contains(h.Detail, tc.detail) {
				t.Fatalf("Detail = %q, want it to contain %q", h.Detail, tc.detail)
			}
			if tc.kind != HealthOK && h.Detail == "" {
				t.Fatal("non-ok health needs a detail")
			}
		})
	}
}

func TestClassifyDetailIsRedactedAndBounded(t *testing.T) {
	h := ClassifyAt("API Error: 401 Authorization: Bearer ghs_abcdefghijklmnopqrstuvwxyz0123456789 "+strings.Repeat("x", 600), time.Now())
	if h.Kind != HealthLoginRequired {
		t.Fatalf("Kind = %s", h.Kind)
	}
	if strings.Contains(h.Detail, "ghs_abc") || len(h.Detail) > 300 {
		t.Fatalf("Detail not redacted/bounded: %q", h.Detail)
	}
}

func TestPreflight(t *testing.T) {
	cases := []struct {
		name string
		kind string
		rule execx.Rule
		want error // nil = ok; ErrLoginRequired; errAny = some other error
	}{
		{"codex logged in exit 1", KindCodex, execx.Rule{Prefix: []string{"codex", "login", "status"},
			Result: execx.Result{Stderr: []byte("Logged in using ChatGPT\n"), Code: 1}}, nil},
		{"codex logged in stdout", KindCodex, execx.Rule{Prefix: []string{"codex", "login", "status"},
			Result: execx.Result{Stdout: []byte("Logged in using an API key - sk-***\n")}}, nil},
		{"codex logged out", KindCodex, execx.Rule{Prefix: []string{"codex", "login", "status"},
			Result: execx.Result{Stderr: []byte("Not logged in\n"), Code: 1}}, ErrLoginRequired},
		{"codex missing", KindCodex, execx.Rule{Prefix: []string{"codex"}, Err: errors.New(`exec: "codex": executable file not found in $PATH`)}, errAny},
		{"claude logged in", KindClaude, execx.Rule{Prefix: []string{"claude", "auth", "status"},
			Result: execx.Result{Stdout: []byte(`{"loggedIn": true, "authMethod": "claude.ai"}`)}}, nil},
		{"claude logged out", KindClaude, execx.Rule{Prefix: []string{"claude", "auth", "status"},
			Result: execx.Result{Stdout: []byte(`{"loggedIn": false}`), Code: 1}}, ErrLoginRequired},
		// Output login_ok cannot read is logged and taken as logged in.
		{"claude garbage", KindClaude, execx.Rule{Prefix: []string{"claude", "auth", "status"},
			Result: execx.Result{Stdout: []byte("unknown command auth")}}, nil},
		{"claude missing", KindClaude, execx.Rule{Prefix: []string{"claude"}, Err: errors.New(`exec: "claude": executable file not found in $PATH`)}, errAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.run.Rules = []execx.Rule{tc.rule}
			err := e.m.Preflight(e.ctx, tc.kind)
			switch tc.want {
			case nil:
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			case errAny:
				if err == nil || errors.Is(err, ErrLoginRequired) {
					t.Fatalf("err = %v, want a non-login error", err)
				}
			default:
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
			}
			for _, c := range e.run.Calls {
				if c.Mutates || c.Timeout == 0 {
					t.Fatalf("preflight cmd must be read-only with a timeout: %+v", c)
				}
			}
			if logs := e.logs.all(); (tc.name == "claude garbage") != (len(logs) == 1) {
				t.Fatalf("logs = %q", logs)
			}
		})
	}
	e := newEnv(t)
	if err := e.m.Preflight(e.ctx, "gemini"); err == nil {
		t.Fatal("unknown kind: want error")
	}
}

func TestPreflightKindConfig(t *testing.T) {
	e := newEnv(t)
	e.run.Rules = nil // any command would fail
	// droid and omp have no login_check: nothing runs.
	for _, kind := range []string{KindDroid, KindOMP, KindShell, ""} {
		if err := e.m.Preflight(e.ctx, kind); err != nil {
			t.Fatalf("%q: %v", kind, err)
		}
	}
	if len(e.run.Calls) != 0 {
		t.Fatalf("calls = %v", e.run.Calls)
	}
	// A configured check: argv split on spaces, judged by login_ok.
	e.setKind(KindDroid, func(k *config.Kind) { k.LoginCheck, k.LoginOK = "droid auth whoami", "regex:(?i)signed in as" })
	e.run.Rules = []execx.Rule{{Prefix: []string{"droid", "auth", "whoami"}, Result: execx.Result{Stdout: []byte("Signed in as bohdan\n")}}}
	if err := e.m.Preflight(e.ctx, KindDroid); err != nil {
		t.Fatalf("signed in: %v", err)
	}
	e.run.Rules = []execx.Rule{{Prefix: []string{"droid", "auth", "whoami"}, Result: execx.Result{Stdout: []byte("Session expired\n"), Code: 1}}}
	if err := e.m.Preflight(e.ctx, KindDroid); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("signed out: err = %v, want ErrLoginRequired", err)
	}
	// login_ok "" = the exit status.
	e.setKind(KindOMP, func(k *config.Kind) { k.LoginCheck = "omp whoami" })
	e.run.Rules = []execx.Rule{{Prefix: []string{"omp", "whoami"}, Result: execx.Result{Code: 2}}}
	if err := e.m.Preflight(e.ctx, KindOMP); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("omp exit 2: err = %v, want ErrLoginRequired", err)
	}
	e.run.Rules = []execx.Rule{{Prefix: []string{"omp", "whoami"}}}
	if err := e.m.Preflight(e.ctx, KindOMP); err != nil {
		t.Fatalf("omp exit 0: %v", err)
	}
	if c := e.run.Calls[len(e.run.Calls)-1]; c.Name != "omp" || strings.Join(c.Args, " ") != "whoami" || c.Mutates || c.Timeout == 0 {
		t.Fatalf("omp check = %+v", c)
	}
}

var errAny = errors.New("any error")

// A wait in pane text whose number is huge never wraps into a short (or
// negative) one: each number is clamped, the sum too, and a usage limit
// with such a wait still pauses for MaxReset.
func TestAHugeWaitInPaneTextIsClamped(t *testing.T) {
	const month = 30 * 24 * time.Hour
	for _, text := range []string{
		"106752 days",                 // just past time.Duration's range in days
		"99999999999999999999 days",   // past int's range: Atoi gives the largest int
		"9223372036854775807 seconds", // the largest int, in seconds
		"20 days 20 days",             // a sum past the cap
		"213503982 hours 1 minute",
	} {
		if d := parseDuration(text); d != month {
			t.Errorf("parseDuration(%q) = %v, want the %v cap", text, d, month)
		}
	}
	if d := parseDuration("2 days 3 hours"); d != 51*time.Hour {
		t.Errorf("a real wait = %v, want 51h", d)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"You've hit your usage limit. Try again in 106752 days.",
		"You've hit your usage limit. Try again in 99999999999999999999 days.",
	} {
		if h := ClassifyAt(text, now); h.Kind != HealthUsageLimit || h.ResetAt == nil || !h.ResetAt.Equal(now.Add(MaxReset)) {
			t.Errorf("%q: %+v, want usage_limit until %v", text, h, now.Add(MaxReset))
		}
	}
}
