package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

func TestAgentPath(t *testing.T) {
	got := AgentPath("/Users/me", "zhuravel.magnum")
	if want := "/Users/me/Library/LaunchAgents/zhuravel.magnum.plist"; got != want {
		t.Fatalf("AgentPath = %q, want %q", got, want)
	}
}

const goldenFull = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>zhuravel.magnum</string>
	<key>ProgramArguments</key>
	<array>
		<string>/opt/homebrew/bin/mise</string>
		<string>-C</string>
		<string>/Users/me/Projects/magnum</string>
		<string>exec</string>
		<string>--</string>
		<string>/Users/me/Projects/magnum/bin/magnum</string>
		<string>daemon</string>
	</array>
	<key>WorkingDirectory</key>
	<string>/Users/me/Projects/magnum</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HERDR_SOCKET_PATH</key>
		<string>/Users/me/.config/herdr/herdr.sock</string>
		<key>HOME</key>
		<string>/Users/me</string>
		<key>PATH</key>
		<string>/Users/me/.local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ExitTimeOut</key>
	<integer>45</integer>
	<key>StandardOutPath</key>
	<string>/Users/me/Projects/magnum/state/logs/launchd.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/me/Projects/magnum/state/logs/launchd.log</string>
</dict>
</plist>
`

func fullOptions() Options {
	return Options{
		Label:            "zhuravel.magnum",
		ProgramArguments: []string{"/opt/homebrew/bin/mise", "-C", "/Users/me/Projects/magnum", "exec", "--", "/Users/me/Projects/magnum/bin/magnum", "daemon"},
		WorkingDir:       "/Users/me/Projects/magnum",
		Env: map[string]string{
			"PATH":              "/Users/me/.local/bin:/opt/homebrew/bin:/usr/bin:/bin",
			"HOME":              "/Users/me",
			"HERDR_SOCKET_PATH": "/Users/me/.config/herdr/herdr.sock",
		},
		StdoutPath:      "/Users/me/Projects/magnum/state/logs/launchd.log",
		StderrPath:      "/Users/me/Projects/magnum/state/logs/launchd.log",
		KeepAlive:       true,
		ThrottleSeconds: 10,
	}
}

func TestPlistGolden(t *testing.T) {
	got := string(Plist(fullOptions()))
	if got != goldenFull {
		t.Fatalf("plist mismatch\n--- got ---\n%s\n--- want ---\n%s", got, goldenFull)
	}
}

// Env is a map; the rendering must not depend on iteration order.
func TestPlistDictOrderingIsStable(t *testing.T) {
	opts := fullOptions()
	first := Plist(opts)
	for range 50 {
		if !bytes.Equal(first, Plist(opts)) {
			t.Fatal("plist output differs between calls")
		}
	}
}

const goldenMinimal = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>zhuravel.magnum</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/true</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ExitTimeOut</key>
	<integer>45</integer>
</dict>
</plist>
`

// KeepAlive=false and ThrottleSeconds=0 omit their keys (launchd defaults);
// empty optional fields are not rendered at all.
func TestPlistMinimalOmitsOptionalKeys(t *testing.T) {
	got := string(Plist(Options{Label: "zhuravel.magnum", ProgramArguments: []string{"/bin/true"}}))
	if got != goldenMinimal {
		t.Fatalf("plist mismatch\n--- got ---\n%s\n--- want ---\n%s", got, goldenMinimal)
	}
}

const goldenEscaped = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>a&amp;b</string>
	<key>ProgramArguments</key>
	<array>
		<string>--msg=a &amp; b &lt; c &gt; d "q" 'q'</string>
		<string>&lt;/string&gt;&lt;!-- x --&gt;</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>K&amp;&lt;&gt;</key>
		<string>v&amp;&lt;&gt;</string>
		<key>Z</key>
		<string>bad�ctl</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>ExitTimeOut</key>
	<integer>45</integer>
</dict>
</plist>
`

func TestPlistEscapesXML(t *testing.T) {
	got := string(Plist(Options{
		Label:            "a&b",
		ProgramArguments: []string{`--msg=a & b < c > d "q" 'q'`, `</string><!-- x -->`},
		Env:              map[string]string{"K&<>": "v&<>", "Z": "bad\x00ctl"},
	}))
	if got != goldenEscaped {
		t.Fatalf("plist mismatch\n--- got ---\n%s\n--- want ---\n%s", got, goldenEscaped)
	}
}

// The output must be well-formed XML whose string values round-trip.
func TestPlistIsWellFormedXML(t *testing.T) {
	opts := fullOptions()
	opts.ProgramArguments = append(opts.ProgramArguments, `x&y<z>"'`)
	dec := xml.NewDecoder(bytes.NewReader(Plist(opts)))
	dec.Strict = true
	var strs []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := string(bytes.TrimSpace(cd)); s != "" {
				strs = append(strs, s)
			}
		}
	}
	found := false
	for _, s := range strs {
		if s == `x&y<z>"'` {
			found = true
		}
	}
	if !found {
		t.Fatalf("escaped argument did not round-trip, strings: %q", strs)
	}
}

// Real macOS validation when plutil is present (read-only, no launchctl).
func TestPlistPassesPlutilLint(t *testing.T) {
	r := &execx.Real{}
	dir := t.TempDir()
	path := dir + "/x.plist"
	if err := os.WriteFile(path, Plist(fullOptions()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), execx.Cmd{Name: "plutil", Args: []string{"-lint", path}})
	if err != nil {
		if _, ok := errors.AsType[*execx.ExitError](err); ok {
			t.Fatalf("plutil -lint: %v\n%s", err, res.Stdout)
		}
		t.Skipf("plutil unavailable: %v", err)
	}
}

// launchd's default exit timeout (5 s) SIGKILLs the daemon while it is still
// cancelling rounds; every plist raises it above the CLI's 30 s stop budget.
func TestPlistRaisesExitTimeOut(t *testing.T) {
	if ExitTimeOut != 45 {
		t.Fatalf("ExitTimeOut = %d, want 45", ExitTimeOut)
	}
	for _, opts := range []Options{fullOptions(), {Label: "x", ProgramArguments: []string{"/bin/true"}}} {
		out := string(Plist(opts))
		if !strings.Contains(out, "\t<key>ExitTimeOut</key>\n\t<integer>45</integer>\n") {
			t.Errorf("plist lacks ExitTimeOut=45:\n%s", out)
		}
	}
}

func TestLogPathIsNotTheDaemonLog(t *testing.T) {
	if got := LogPath("/Users/me/Projects/magnum/state/logs"); got != "/Users/me/Projects/magnum/state/logs/launchd.log" {
		t.Fatalf("LogPath = %s", got)
	}
}
