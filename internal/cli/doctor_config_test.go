package cli

import (
	"context"
	"encoding/json"
	"github.com/zhuravel/magnum/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBareConfig writes a valid config.toml that declares no identity and no
// watch (config.Warnings reports both).
func writeBareConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "custom.toml")
	if err := os.WriteFile(path, []byte("[daemon]\ndefault_repo = \"talkable/talkable\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// `magnum config` prints the file config.Load read (--config, else
// $MAGNUM_CONFIG, else the layout default) and the config's warnings.
func TestConfigCommandPrintsLoadedFileAndWarnings(t *testing.T) {
	f := newInspFixture(t)
	custom := writeBareConfig(t)
	missing := filepath.Join(f.Home, "never-loaded.toml")
	cases := []struct {
		name  string
		env   string
		args  []string
		want  string
		warns bool
	}{
		{"layout default", "", nil, filepath.Join(f.Home, "config.toml"), false},
		{"flag", "", []string{"--config", custom}, custom, true},
		{"env", custom, nil, custom, true},
		{"flag beats env", missing, []string{"--config", custom}, custom, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAGNUM_CONFIG", tc.env)
			f.Out.Reset()
			f.Err.Reset()
			c := &Context{Version: "test", Layout: f.Ctx.Layout, Stdout: f.Out, Stderr: f.Err}
			if code := execute(c, append([]string{"config"}, tc.args...)); code != 0 {
				t.Fatalf("exit %d: %s", code, f.Err.String())
			}
			out := f.Out.String()
			if !strings.Contains(out, "config:  "+tc.want+"\n") {
				t.Errorf("stdout lacks the loaded file %s:\n%s", tc.want, out)
			}
			if !strings.Contains(out, "skill:   "+filepath.Join(f.Home, "skills", "magnum-review", "SKILL.md")+"\n") {
				t.Errorf("stdout lacks the judge skill:\n%s", out)
			}
			stderr := f.Err.String()
			if tc.warns {
				for _, w := range []string{"warning: no [[identity]] configured", "warning: no [[watch]] configured"} {
					if !strings.Contains(stderr, w) {
						t.Errorf("stderr lacks %q:\n%s", w, stderr)
					}
				}
			} else if stderr != "" {
				t.Errorf("stderr = %q, want none", stderr)
			}
		})
	}
}

// Doctor reports the config's warnings as WARN checks, in the JSON too, and
// names what the config was read from (the built-in defaults and the user's file).
func TestDoctorConfigWarnings(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	custom := writeBareConfig(t)
	d.ConfigFile = custom
	d.Config.Sources = []string{config.BuiltinDefaults, custom} // what Load records it read
	d.Config.Identities, d.Config.Watches = nil, nil

	cs := doctorConfig(context.Background(), d)
	if cs[0].Name != "config" || cs[0].Status != doctorPass || !strings.Contains(cs[0].Detail, custom) {
		t.Fatalf("config check = %+v", cs[0])
	}
	var warns []doctorCheck
	for _, c := range cs {
		if c.Name == "config warning" {
			warns = append(warns, c)
		}
	}
	if len(warns) != 2 || warns[0].Status != doctorWarn || !strings.Contains(warns[0].Detail, "[[identity]]") ||
		!strings.Contains(warns[1].Detail, "[[watch]]") {
		t.Fatalf("config warnings = %+v", warns)
	}
}

// `doctor --json` through the command tree: the output parses, the config
// check names the --config file and carries its warnings.
func TestDoctorCommandJSONHasConfigFileAndWarnings(t *testing.T) {
	f := newInspFixture(t)
	doctorSandbox(t, f)
	custom := writeBareConfig(t)
	code := f.run("doctor", "--json", "--config", custom)
	var got []doctorCheck
	if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil {
		t.Fatalf("code %d json err %v out %s stderr %s", code, err, f.Out.String(), f.Err.String())
	}
	var warns int
	for _, c := range got {
		if c.Name == "config warning" && c.Status == doctorWarn {
			warns++
		}
	}
	if c := doctorByName(got)["config"]; c.Status != doctorPass || !strings.Contains(c.Detail, custom) || warns != 2 {
		t.Fatalf("config = %+v, %d config warnings in %d checks", c, warns, len(got))
	}
}
