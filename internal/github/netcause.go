package github

// The connection-class failures of a GitHub call or a git remote: the one
// classifier the engine's identity checks and infrastructure pause and the
// pipeline's verification share (a network blip is no verdict on an identity
// and no reason to fail a round that posted its review).

import (
	"regexp"
	"strings"
)

// errPattern is an error text (lower case) and the cause it names.
type errPattern struct{ match, cause string }

// networkPatterns are the infrastructure failures that are the network's
// (DNS, timeouts, refusals, resets, TLS); ConnectionCause reads them too.
var networkPatterns = []errPattern{
	{"could not resolve host", "DNS lookup failed"},
	{"temporary failure in name resolution", "DNS lookup failed"},
	{"nodename nor servname provided", "DNS lookup failed"},
	{"connection timed out", "network timeout"},
	{"operation timed out", "network timeout"},
	{"connection refused", "connection refused"},
	{"connection reset by peer", "connection reset"},
	{"network is unreachable", "network unreachable"},
	{"no route to host", "network unreachable"},
	{"ssl certificate problem", "TLS failure"},
	{"server certificate verification failed", "TLS failure"},
	{"ssl_error", "TLS failure"},
	{"ssl_connect", "TLS failure"},
	{"gnutls_handshake", "TLS failure"},
	{"tls handshake", "TLS failure"},
}

// githubPatterns are the connection-class failures of a GitHub API call that
// networkPatterns do not name: gh's own "error connecting to", Go's dial,
// TLS and timeout errors, and GitHub's server errors and rate limits.
var githubPatterns = []errPattern{
	{"error connecting to", "GitHub unreachable"},
	{"no such host", "DNS lookup failed"},
	{"server misbehaving", "DNS lookup failed"},
	{"tls: ", "TLS failure"},
	{"x509: ", "TLS failure"},
	{"i/o timeout", "network timeout"},
	{"timed out", "network timeout"},
	{"timeout exceeded", "network timeout"},
	{"deadline exceeded", "network timeout"},
	{"broken pipe", "connection reset"},
	{"network is down", "network unreachable"},
	{"rate limit", "rate limited"}, // a secondary one too
	{"too many requests", "rate limited"},
	{"internal server error", "GitHub server error"},
	{"bad gateway", "GitHub server error"},
	{"service unavailable", "GitHub server error"},
	{"gateway timeout", "GitHub server error"},
}

var (
	// serverErrorRe and rateLimitRe find a status in gh's "(HTTP 502)" or
	// "HTTP 502:" and in the identity package's "POST /path: 502 Bad Gateway".
	serverErrorRe = regexp.MustCompile(`\bhttp 5\d\d\b|\b(?:get|post|put|patch|delete) /\S*: 5\d\d\b`)
	rateLimitRe   = regexp.MustCompile(`\bhttp 429\b|\b(?:get|post|put|patch|delete) /\S*: 429\b`)
	// eofRe is a connection closed mid-answer (`Get "...": EOF`, "unexpected EOF").
	eofRe = regexp.MustCompile(`(?:: |unexpected )eof\b`)
)

// matchPattern is the cause of the first of patterns that msg (lower case)
// contains ("" = none).
func matchPattern(msg string, patterns []errPattern) string {
	for _, p := range patterns {
		if strings.Contains(msg, p.match) {
			return p.cause
		}
	}
	return ""
}

// NetworkCause names the failure of the network itself that msg reports
// ("" = none): DNS, a timeout, a refused or reset connection, an unreachable
// network, TLS. What git and ssh print when a remote cannot be reached.
func NetworkCause(msg string) string {
	return matchPattern(strings.ToLower(msg), networkPatterns)
}

// ConnectionCause names the connection-class failure msg reports ("" = none:
// a real verdict from GitHub, or about an identity): the network's (DNS, a
// timeout, a refused or reset connection, TLS), gh unable to connect, a
// closed connection, a GitHub server error (5xx) or a rate limit (429,
// secondary or primary).
func ConnectionCause(msg string) string {
	msg = strings.ToLower(msg)
	if cause := matchPattern(msg, networkPatterns); cause != "" {
		return cause
	}
	if cause := matchPattern(msg, githubPatterns); cause != "" {
		return cause
	}
	switch {
	case serverErrorRe.MatchString(msg):
		return "GitHub server error"
	case rateLimitRe.MatchString(msg):
		return "rate limited"
	case eofRe.MatchString(msg):
		return "connection closed"
	}
	return ""
}
