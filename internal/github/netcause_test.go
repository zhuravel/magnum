package github

import "testing"

func TestConnectionCause(t *testing.T) {
	const (
		ghUnreachable   = "gh api user: gh api user --jq .login --hostname github.com exited 1: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"
		mintUnreachable = "identity talkable-app: mint installation token: POST /app/installations/2/access_tokens: gh api POST /app/installations/2/access_tokens: gh exited 1: error connecting to api.github.com"
	)
	for _, tc := range []struct{ err, want string }{
		{ghUnreachable, "GitHub unreachable"},
		{mintUnreachable, "GitHub unreachable"},
		{`GET /app: Get "https://api.github.com/app": dial tcp: lookup api.github.com: no such host`, "DNS lookup failed"},
		{`Get "https://api.github.com/app": dial tcp 140.82.112.6:443: i/o timeout`, "network timeout"},
		{`Post "https://api.github.com/app/installations/2/access_tokens": net/http: TLS handshake timeout`, "TLS failure"},
		{`Get "https://api.github.com/app": tls: handshake failure`, "TLS failure"},
		{`Get "https://api.github.com/app": EOF`, "connection closed"},
		{`read tcp 10.0.0.2:5678->140.82.112.6:443: read: connection reset by peer`, "connection reset"},
		{`Get "https://api.github.com/app": dial tcp 140.82.112.6:443: connect: connection refused`, "connection refused"},
		{`Get "https://api.github.com/app": net/http: request canceled (Client.Timeout exceeded while awaiting headers)`, "network timeout"},
		{"gh api GET /app: gh timed out: context deadline exceeded", "network timeout"},
		{"gh api user --jq .login --hostname github.com: context deadline exceeded", "network timeout"},
		{"POST /app/installations/2/access_tokens: 502 Bad Gateway", "GitHub server error"},
		{"GET /app: 503 Service Unavailable", "GitHub server error"},
		{"gh api repos/talkable/talkable exited 1: gh: Server Error (HTTP 500)", "GitHub server error"},
		{"gh: HTTP 504: Gateway Timeout (https://api.github.com/user)", "GitHub server error"},
		{"GET /app/installations/2: 429 Too Many Requests", "rate limited"},
		{"gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)", "rate limited"},
		{"GET /installation/repositories: 403 API rate limit exceeded for installation ID 2.", "rate limited"},

		{"GitHub rejected the App JWT: GET /app: 401 Bad credentials", ""},
		{"GET /app: 403 Forbidden", ""},
		{"installation 502 not found for app talkable-reviewer", ""},
		{"GET /app/installations/2: 404 Not Found", ""},
		{"gh api graphql: HTTP 404: Not Found", ""},
		{"gh api user is alice, identity zhuravel expects zhuravel", ""},
		{"identity talkable-app has login \"talkable[bot]\" but the App posts as \"other[bot]\"", ""},
		{"permission pull_requests: read (need write)", ""},
		{"private key: no PEM block", ""},
		{"gh token for github.com: gh auth token printed no token", ""},
		{"gh api repos/talkable/eof-tools with GH_CONFIG_DIR=/state/gh/app: 404 Not Found", ""},
		{"", ""},
	} {
		if got := ConnectionCause(tc.err); got != tc.want {
			t.Errorf("ConnectionCause(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// NetworkCause reads only the network's own failures, as git and ssh print
// them, in any case; GitHub's server errors and rate limits are not among
// them.
func TestNetworkCauseNamesOnlyTheNetworksFailures(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com", "DNS lookup failed"},
		{"ssh: connect to host github.com port 22: Operation timed out", "network timeout"},
		{"ssh: connect to host github.com port 22: Connection refused", "connection refused"},
		{"fatal: unable to access: SSL certificate problem: unable to get local issuer certificate", "TLS failure"},
		{"error connecting to api.github.com", ""},
		{"gh: HTTP 502: Bad Gateway", ""},
		{"fatal: couldn't find remote ref refs/pull/7/head", ""},
		{"", ""},
	} {
		if got := NetworkCause(tc.msg); got != tc.want {
			t.Errorf("NetworkCause(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}
