package identity

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// GhTransport is an http.RoundTripper that sends each request through
// `gh api --include` instead of opening a TLS connection itself.
//
// Some Macs run an outbound firewall (Little Snitch) that blocks HTTPS from
// unknown, unsigned binaries such as a freshly rebuilt magnum, while the
// signed gh binary is allowed; every other GitHub call magnum makes already
// goes through gh. See docs/spikes.md (2026-10-03, gh transport).
//
// The request's own headers are passed as `-H`. gh only adds its stored token
// when the request has no Authorization header, so an App JWT passed that way
// wins. An installation token (ghs_...) never touches argv: it travels in the
// GH_TOKEN variable of that one gh process, which gh sends as the
// Authorization header and other local users cannot read. gh still refuses to
// start without some login of its own (`gh auth login` or GH_TOKEN), so for
// JWT calls the user's normal gh auth must stay available; Env must not blank
// it.
//
// Non-2xx answers become ordinary responses: gh exits 1 on an HTTP error but
// still prints the status line, headers and body, so stdout is parsed whatever
// the exit code. Only api.github.com URLs are supported.
//
// The JWT travels in argv, so other local users could see it in `ps` for the
// second the call lasts. execx.Real redacts it (Authorization: <redacted>) in
// its log lines and errors from this transport never include argv.
type GhTransport struct {
	// Run executes gh. Requests are never marked Mutates: minting a token
	// changes nothing a dry run must protect, exactly like the direct path.
	Run execx.Runner
	// Env overlays the gh environment (after the transport's own NO_COLOR,
	// CLICOLOR_FORCE and GH_FORCE_TTY, which keep the output parseable).
	Env map[string]string
}

var _ http.RoundTripper = (*GhTransport)(nil)

const ghAPIHost = "api.github.com"

// NewGhClient returns an http.Client whose requests run through gh (see
// GhTransport), with the same 30 s timeout the direct client uses. A request
// without a deadline of its own gets the same 30 s as the gh call's bound.
func NewGhClient(run execx.Runner, env map[string]string) *http.Client {
	return &http.Client{Transport: &GhTransport{Run: run, Env: env}, Timeout: callTimeout}
}

// skipHeaders are request headers gh must set itself (or that would make it
// print a body it cannot decode).
var skipHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Connection": true,
	"Transfer-Encoding": true, "Accept-Encoding": true,
}

// RoundTrip implements http.RoundTripper.
func (t *GhTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	what := "gh api " + req.Method + " " + req.URL.Path
	if t.Run == nil {
		return nil, fmt.Errorf("%s: no command runner configured", what)
	}
	if req.URL.Host != ghAPIHost {
		return nil, fmt.Errorf("%s: the gh transport only reaches %s, not %s (set [github] transport = \"direct\")", what, ghAPIHost, req.URL.Host)
	}
	ctx := req.Context()
	timeout := callTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("%s: %w", what, context.DeadlineExceeded)
	}

	args, installToken, err := ghAPIArgs(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var stdin []byte
	if req.Body != nil && req.Body != http.NoBody {
		if stdin, err = io.ReadAll(req.Body); err != nil {
			return nil, fmt.Errorf("%s: read request body: %w", what, err)
		}
		args = append(args, "--input", "-")
	}
	env := map[string]string{"NO_COLOR": "1", "CLICOLOR_FORCE": "", "GH_FORCE_TTY": ""}
	maps.Copy(env, t.Env)
	if installToken != "" {
		env["GH_TOKEN"] = installToken
	}
	cmd := execx.Cmd{
		Name:    "gh",
		Args:    args,
		Env:     env,
		Stdin:   stdin,
		Timeout: timeout,
		Label:   what,
	}
	res, runErr := t.Run.Run(ctx, cmd)
	// gh exits 1 on HTTP errors but still prints the response, so a normal
	// exit with any code is parsed. A killed or never-started gh (timeout,
	// cancellation, missing binary) may have left partial output: ignore it.
	var exit *execx.ExitError
	if runErr == nil || errors.As(runErr, &exit) {
		resp, parseErr := parseGhInclude(res.Stdout, req)
		if parseErr == nil {
			return resp, nil
		}
		if runErr == nil {
			return nil, fmt.Errorf("%s: %w", what, parseErr)
		}
	}
	return nil, ghRunError(ctx, what, res, runErr)
}

// installationToken returns the token of an `Authorization: Bearer ghs_...`
// (or `token ghs_...`) header value, "" for anything else (an App JWT, say).
func installationToken(value string) string {
	for _, scheme := range []string{"Bearer ", "token "} {
		if tok, ok := strings.CutPrefix(value, scheme); ok && strings.HasPrefix(tok, "ghs_") && !strings.ContainsAny(tok, " \t") {
			return tok
		}
	}
	return ""
}

// ghAPIArgs builds `api --method M --include --hostname github.com <path> -H ...`.
// An installation token in the Authorization header is left out of argv and
// returned for the caller to pass as GH_TOKEN.
func ghAPIArgs(req *http.Request) (args []string, token string, err error) {
	endpoint := strings.TrimPrefix(req.URL.RequestURI(), "/")
	if endpoint == "" {
		return nil, "", errors.New("empty request path")
	}
	args = []string{"api", "--method", req.Method, "--include", "--hostname", "github.com", endpoint}
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		if !skipHeaders[textproto.CanonicalMIMEHeaderKey(name)] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range req.Header[name] {
			if strings.ContainsAny(name+v, "\r\n") {
				return nil, "", fmt.Errorf("header %s contains a line break", name)
			}
			if textproto.CanonicalMIMEHeaderKey(name) == "Authorization" {
				if token = installationToken(v); token != "" {
					continue
				}
			}
			args = append(args, "-H", name+": "+v)
		}
	}
	return args, token, nil
}

// ghRunError describes a gh run that produced no HTTP response, without argv
// (which carries the Authorization header) and with secrets redacted.
func ghRunError(ctx context.Context, what string, res execx.Result, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%s: %w", what, cerr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: gh timed out: %w", what, context.DeadlineExceeded)
	}
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		msg := strings.TrimSpace(execx.Redact(string(res.Stderr)))
		if msg == "" {
			msg = strings.TrimSpace(ee.Stderr)
		}
		if msg == "" {
			msg = "no output"
		}
		return fmt.Errorf("%s: gh exited %d: %s", what, ee.Code, msg)
	}
	// execx.RunError renders the command line first; its cause alone is the
	// useful part (missing binary, start failure).
	if re, ok := errors.AsType[*execx.RunError](err); ok {
		err = re.Err
	}
	return fmt.Errorf("%s: %s", what, execx.Redact(err.Error()))
}

// parseGhInclude turns `gh api --include` output into a response: a status
// line ("HTTP/2.0 401 Unauthorized\n"), "Name: value\r\n" header lines (gh
// joins repeated values with ", "), a blank line, then the raw body.
func parseGhInclude(out []byte, req *http.Request) (*http.Response, error) {
	if len(out) == 0 {
		return nil, errors.New("gh printed no response")
	}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(out)))
	line, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("read status line: %w", err)
	}
	proto, status, ok := strings.Cut(line, " ")
	major, minor, protoOK := http.ParseHTTPVersion(proto)
	if !ok || !protoOK {
		return nil, fmt.Errorf("unexpected status line %q", firstChars(execx.Redact(line), 80))
	}
	codeText, _, _ := strings.Cut(status, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil || len(codeText) != 3 {
		return nil, fmt.Errorf("unexpected status line %q", firstChars(execx.Redact(line), 80))
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("read headers: %w", err)
	}
	body, err := io.ReadAll(tp.R)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if hdr == nil {
		hdr = textproto.MIMEHeader{}
	}
	return &http.Response{
		Status:        status,
		StatusCode:    code,
		Proto:         proto,
		ProtoMajor:    major,
		ProtoMinor:    minor,
		Header:        http.Header(hdr),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
