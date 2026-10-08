package execx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealRunsAndTimesOut(t *testing.T) {
	r := &Real{}
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo hi; echo err >&2"}})
	if err != nil || res.Out() != "hi" || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("unexpected: %v %q %q", err, res.Out(), res.Stderr)
	}
	_, err = r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 3"}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("want ExitError code 3, got %v", err)
	}
	_, err = r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 5"}, Timeout: 100 * time.Millisecond})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
}

func TestRealMissingDir(t *testing.T) {
	r := &Real{}
	dir := filepath.Join(t.TempDir(), "gone")
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "true"}, Dir: dir})
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "working directory") || res.Code != -1 {
		t.Fatalf("want a working-directory error and code -1, got %v (code %d)", err, res.Code)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Fatalf("a missing dir is not an exit error: %v", err)
	}
}

func TestEnvOverlayBlanks(t *testing.T) {
	r := &Real{BaseEnv: []string{"PATH=/usr/bin:/bin", "KEEP=1", "DROP=secret"}}
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", `printf '%s|%s|%s' "$KEEP" "${DROP-unset}" "$NEW"`}, Env: map[string]string{"DROP": "", "NEW": "n"}})
	if err != nil || res.Out() != "1||n" {
		t.Fatalf("got %q %v", res.Out(), err)
	}
}

func TestEnvUnset(t *testing.T) {
	r := &Real{BaseEnv: []string{"PATH=/usr/bin:/bin", "GIT_DIR=/elsewhere", "GIT_WORK_TREE=/w", "KEEP=1"}}
	res, err := r.Run(context.Background(), Cmd{Name: "sh",
		Args:  []string{"-c", `printf '%s|%s|%s' "${GIT_DIR-unset}" "${GIT_WORK_TREE-unset}" "$KEEP"`},
		Env:   map[string]string{"GIT_WORK_TREE": "kept"},
		Unset: []string{"GIT_DIR", "GIT_WORK_TREE"}})
	if err != nil || res.Out() != "unset|kept|1" {
		t.Fatalf("got %q %v", res.Out(), err)
	}
}

func TestRedact(t *testing.T) {
	cases := []struct{ in, leak, keep string }{
		{"token ghs_abcDEF123_x here", "ghs_abc", "token"},
		{"and gho_zzz", "gho_zzz", "and"},
		{"refresh ghr_abcdef123456", "ghr_abc", "refresh"},
		{"Authorization: Bearer eyJhbGciOi.eyJpYXQiOjE.c2lnbmF0dXJl", "eyJhbGciOi", "Authorization: <redacted>"},
		{"-H Authorization: Bearer opaqueTOKEN123", "opaqueTOKEN123", "Authorization: <redacted>"},
		{"authorization: token opaqueTOKEN123", "opaqueTOKEN123", "authorization: <redacted>"},
		{"Authorization: Basic dXNlcjpodW50ZXIy", "dXNlcjpodW50ZXIy", "Authorization: <redacted>"},
		{"key -----BEGIN RSA PRIVATE KEY-----\nMIIEsecretbody\n-----END RSA PRIVATE KEY----- tail", "MIIEsecretbody", "tail"},
		{"key=-----BEGIN PRIVATE KEY-----\\nMIIEescaped\\n-----END PRIVATE KEY-----", "MIIEescaped", "key="},
		{"cut -----BEGIN EC PRIVATE KEY-----\nMIIEtruncat", "MIIEtruncat", "cut"},
	}
	for _, tc := range cases {
		out := Redact(tc.in)
		if strings.Contains(out, tc.leak) || !strings.Contains(out, tc.keep) || !strings.Contains(out, "<redacted>") {
			t.Errorf("Redact(%q) = %q", tc.in, out)
		}
	}
}

// Every error path renders the command redacted: a missing dir, a failed
// start, a timeout and a non-zero exit, plus the transcript line.
func TestRunErrorsRedactArgv(t *testing.T) {
	const secret = "ghs_0123456789abcdefghij"
	var log logBuf
	r := &Real{Log: &log}
	header := "Authorization: Bearer " + secret
	cases := []struct {
		name string
		cmd  Cmd
		is   error
	}{
		{"missing dir", Cmd{Name: "sh", Args: []string{"-c", "true", header}, Dir: filepath.Join(t.TempDir(), "gone")}, fs.ErrNotExist},
		{"start failure", Cmd{Name: filepath.Join(t.TempDir(), "no-such-binary"), Args: []string{header}}, fs.ErrNotExist},
		{"timeout", Cmd{Name: "sh", Args: []string{"-c", "sleep 5", "x", secret}, Timeout: 100 * time.Millisecond}, context.DeadlineExceeded},
		{"exit", Cmd{Name: "sh", Args: []string{"-c", "exit 4", "x", secret}}, nil},
	}
	for _, tc := range cases {
		_, err := r.Run(context.Background(), tc.cmd)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks or is nil: %v", tc.name, err)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%s: want errors.Is %v, got %v", tc.name, tc.is, err)
		}
		var re *RunError
		var ee *ExitError
		if tc.is != nil && !errors.As(err, &re) || tc.is == nil && !errors.As(err, &ee) {
			t.Errorf("%s: wrong error type %T", tc.name, err)
		}
	}
	if strings.Contains(log.String(), secret) || strings.Count(log.String(), "<redacted>") < len(cases) {
		t.Fatalf("transcript leaks or is incomplete:\n%s", log.String())
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"plain-arg_1":   "plain-arg_1",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"a;rm -rf x":    "'a;rm -rf x'",
		"refs/magnum/*": "'refs/magnum/*'",
		"$HOME":         "'$HOME'",
		"x=y,z:1@2%3+":  "x=y,z:1@2%3+",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := (Cmd{Name: "git", Args: []string{"log", "a b", ""}}).String(); got != "git log 'a b' ''" {
		t.Fatalf("Cmd.String() = %s", got)
	}
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestFakeAndDryRun(t *testing.T) {
	f := &Fake{Rules: []Rule{{Prefix: []string{"git", "fetch"}, Result: Result{Stdout: []byte("ok")}}}}
	res, err := f.Run(context.Background(), Cmd{Name: "git", Args: []string{"fetch", "origin"}})
	if err != nil || res.Out() != "ok" {
		t.Fatal(err)
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "git", Args: []string{"push"}}); err == nil {
		t.Fatal("unmatched command must fail")
	}
	d := &DryRun{Inner: f}
	if _, err := d.Run(context.Background(), Cmd{Name: "git", Args: []string{"push"}, Mutates: true}); err != nil {
		t.Fatal(err)
	}
	if len(d.Planned) != 1 || len(f.CallsWithPrefix("git", "fetch")) != 1 {
		t.Fatalf("planned=%d calls=%d", len(d.Planned), len(f.Calls))
	}
}

// levelBuf records each transcript line with its level.
type levelBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *levelBuf) Printf(format string, args ...any) { l.Logf(slog.LevelInfo, format, args...) }

func (l *levelBuf) Logf(level slog.Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level.String()+" "+fmt.Sprintf(format, args...))
}

// Successful commands are routine (87% of the daemon log): debug. Failures
// stay visible at warn with the command line redacted.
func TestRunLogsSuccessAtDebugAndFailureAtWarn(t *testing.T) {
	const secret = "ghs_0123456789abcdefghij"
	var log levelBuf
	r := &Real{Log: &log}
	if _, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "true", "x", secret}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 3", "x", secret}}); err == nil {
		t.Fatal("exit 3 must fail")
	}
	if _, err := r.Run(context.Background(), Cmd{Name: "sh", Dir: filepath.Join(t.TempDir(), "gone")}); err == nil {
		t.Fatal("a missing dir must fail")
	}
	if len(log.lines) != 3 {
		t.Fatalf("lines = %q", log.lines)
	}
	for i, want := range []string{"DEBUG exec sh -c true", "WARN exec sh -c 'exit 3'", "WARN exec sh dir="} {
		if !strings.HasPrefix(log.lines[i], want) {
			t.Errorf("line %d = %q, want prefix %q", i, log.lines[i], want)
		}
		if strings.Contains(log.lines[i], secret) {
			t.Errorf("line %d leaks the token: %q", i, log.lines[i])
		}
	}
}

// A probe's non-zero exit is an expected answer and logs at Debug; a plain
// command's failure still warns.
func TestProbeFailureLogsAtDebug(t *testing.T) {
	for _, tc := range []struct {
		probe bool
		want  slog.Level
	}{{true, slog.LevelDebug}, {false, slog.LevelWarn}} {
		var got []slog.Level
		r := &Real{Log: levelRecorder(func(l slog.Level) { got = append(got, l) })}
		_, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 2"}, Probe: tc.probe})
		if err == nil {
			t.Fatal("exit 2 must be an error")
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("probe=%v: levels %v, want [%v]", tc.probe, got, tc.want)
		}
	}
}

// An exit that Expected takes for an answer logs at Debug; any other exit
// of the same command still warns (gh api exits 1 for a 404 and a 500
// alike, git config --get exits 1 for a key not set and 3 for a broken file).
func TestExpectedExitLogsAtDebug(t *testing.T) {
	for _, tc := range []struct {
		code string
		want slog.Level
	}{{"1", slog.LevelDebug}, {"3", slog.LevelWarn}} {
		var got []slog.Level
		r := &Real{Log: levelRecorder(func(l slog.Level) { got = append(got, l) })}
		answer := func(res Result) bool { return res.Code == 1 }
		_, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit " + tc.code}, Expected: answer})
		if err == nil {
			t.Fatalf("exit %s must be an error", tc.code)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("exit %s: levels %v, want [%v]", tc.code, got, tc.want)
		}
	}
}

type levelRecorder func(slog.Level)

func (f levelRecorder) Printf(string, ...any)                 { f(slog.LevelInfo) }
func (f levelRecorder) Logf(l slog.Level, _ string, _ ...any) { f(l) }
