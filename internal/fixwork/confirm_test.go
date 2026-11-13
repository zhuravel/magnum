package fixwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
)

// The classifier of a failed read of the PR's head: only GitHub or the
// network not answering is transient (a connection error, a timeout, a
// 5xx), and only from what the GitHub client returns (a typed error, or
// gh's exit and its output); GitHub's own answer (401, 403, 404, a GraphQL
// error, a rate limit) and anything else is not.
func TestOnlyConnectionErrorsTimeoutsAnd5xxAreTransient(t *testing.T) {
	exit := func(stderr string) error {
		return fmt.Errorf("github details talkable/talkable (1): %w", &execx.ExitError{Cmd: execx.Cmd{Name: "gh"}, Code: 1, Stderr: stderr})
	}
	api := func(status int, msg string) error {
		return &github.APIError{Op: "details talkable/talkable (1)", Status: status, Message: msg}
	}
	gitExit := func(stderr string) error {
		return fmt.Errorf("git ls-remote: %w", &execx.ExitError{Cmd: execx.Cmd{Name: "git", Args: []string{"ls-remote", "origin"}}, Code: 128, Stderr: stderr})
	}
	graphql := func(status int) error {
		return &github.APIError{Op: "details talkable/talkable (1)", Status: status, Errors: []github.GraphQLError{{Type: "FORBIDDEN", Message: "Resource not accessible"}}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"gh cannot connect", ghDown(), true},
		{"a reset", exit("read tcp 10.0.0.2:52011->140.82.112.6:443: read: connection reset by peer"), true},
		{"a DNS failure", exit(`Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`), true},
		{"a refused dial", fmt.Errorf("github details: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), true},
		{"a DNS error", &net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}, true},
		{"gh's own timeout", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: context.DeadlineExceeded}), true},
		{"an i/o timeout", exit(`Post "https://api.github.com/graphql": net/http: TLS handshake timeout`), true},
		{"a 500", api(500, "Internal Server Error"), true},
		{"a 502", api(502, "Bad Gateway"), true},
		{"a 503", api(503, "Service Unavailable"), true},
		{"a 504", api(504, "Gateway Timeout"), true},
		{"a 5xx gh names in its exit", exit("gh: Bad Gateway (HTTP 502)"), true},
		{"git cannot reach the remote", gitDown(), true},
		{"GraphQL's time limit: an empty answer", fmt.Errorf("github details talkable/talkable (1): decode response: %w", json.Unmarshal(nil, new(any))), true},
		{"a cut answer", fmt.Errorf("github details talkable/talkable (1): decode data: %w", io.ErrUnexpectedEOF), true},
		{"an answer that is no JSON", fmt.Errorf("github details talkable/talkable (1): decode response: %w", json.Unmarshal([]byte("<html>"), new(any))), false},
		{"the read and its fallback both down", errors.Join(ghDown(), gitDown()), true},
		{"GitHub's own timeout", api(499, "Client Closed Request"), true},
		{"a refused HTTP/2 stream", exit(`Post "https://api.github.com/graphql": stream error: stream ID 3; REFUSED_STREAM; received from peer`), true},
		{"a cancelled HTTP/2 stream", exit(`Post "https://api.github.com/graphql": stream error: stream ID 5; CANCEL; received from peer`), true},
		{"GitHub's GOAWAY", exit(`Post "https://api.github.com/graphql": http2: server sent GOAWAY and closed the connection; LastStreamID=1999`), true},
		{"a lost HTTP/2 connection", exit(`Post "https://api.github.com/graphql": http2: client connection lost`), true},
		{"git's 5xx", gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': The requested URL returned error: 503"), true},
		{"git's empty reply", gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': Empty reply from server"), true},
		{"git's cut answer", gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': Failure when receiving data from the peer"), true},
		{"git's HTTP/2 stream", gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': HTTP/2 stream 1 was not closed cleanly: PROTOCOL_ERROR (err 1)"), true},
		{"ssh closed by GitHub", gitExit("Connection closed by 140.82.112.4 port 22\r\nfatal: Could not read from remote repository."), true},
		{"ssh's key exchange cut", gitExit("kex_exchange_identification: Connection closed by remote host\r\nfatal: Could not read from remote repository."), true},
		{"the read's 502 and git's 503", errors.Join(api(502, "Bad Gateway"), gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': The requested URL returned error: 503")), true},
		{"git's 403", gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': The requested URL returned error: 403"), false},
		{"git's missing repository", gitExit("remote: Repository not found.\nfatal: repository 'https://github.com/talkable/talkable/' not found"), false},
		{"the read's 502 and git's 403", errors.Join(api(502, "Bad Gateway"), gitExit("fatal: unable to access 'https://github.com/talkable/talkable/': The requested URL returned error: 403")), false},
		{"a 401", api(401, "Bad credentials"), false},
		{"a 403", api(403, "Resource not accessible by integration"), false},
		{"a 404", api(404, "Not Found"), false},
		{"a GraphQL error", graphql(0), false},
		{"a GraphQL error beside a 502", graphql(502), false},
		{"a rate limit", api(403, "API rate limit exceeded for user ID 1"), false},
		{"a 429", api(429, "Too Many Requests"), false},
		{"a rate limit gh names in its exit", exit("gh: API rate limit exceeded (HTTP 429)"), false},
		{"another exit", exit("gh: unknown flag: --input"), false},
		{"a plain error that names a 502", errors.New("graphql: 502 Bad Gateway"), false},
		{"no gh", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: exec.ErrNotFound}), false},
		{"a stop", fmt.Errorf("github details: %w", &execx.RunError{Cmd: execx.Cmd{Name: "gh"}, Err: context.Canceled}), false},
		{"GitHub does not say", errors.New("GitHub does not say where the PR's head is"), false},
		{"the read down but the branch gone", errors.Join(ghDown(), errors.New("origin has no branch feature")), false},
		{"a 403 and the fallback down", errors.Join(api(403, "Forbidden"), gitDown()), false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientRead(tc.err); got != tc.want {
				t.Errorf("transientRead(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
