package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
)

func TestUIOpenPopupWithManifestSizes(t *testing.T) {
	h := newActHarness(t)
	manifest, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, "herdr-plugin.toml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	h.env["MAGNUM_PICK_QUERY"] = "https://github.com/talkable/talkable/pull/5"
	h.env["HERDR_PLUGIN_CONTEXT_JSON"] = `{"workspace_id":"w_3","focused_pane_cwd":"/x"}`
	if code := h.cmd("ui", "open", "picker"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	got := h.hd.opened[0]
	if got.PluginID != "zhuravel.magnum" || got.Entrypoint != "picker" || got.Placement != herdr.PlacementPopup ||
		got.Width != "90%" || got.Height != "60%" || got.WorkspaceID != "w_3" || !got.Focus ||
		got.Env["MAGNUM_PICK_QUERY"] != "https://github.com/talkable/talkable/pull/5" {
		t.Fatalf("plugin pane options %+v", got)
	}
	if code := h.cmd("ui", "open", "status", "--height", "50%"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := h.hd.opened[1]; got.Width != "95%" || got.Height != "50%" {
		t.Fatalf("status popup %+v", got)
	}
	h.errb.Reset()
	if code := h.cmd("ui", "open", "nope"); code != 1 || !strings.Contains(h.errb.String(), "use one of cleanup, doctor, picker, status") {
		t.Fatalf("unknown entrypoint: exit %d %s", code, h.errb.String())
	}
}

func TestUIUsageAndFallbackSizes(t *testing.T) {
	h := newActHarness(t) // no manifest in the temp home: shipped sizes
	if code := h.cmd("ui", "close", "picker"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := h.cmd("ui", "open", "doctor"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := h.hd.opened[0]; got.Width != "95%" || got.Height != "80%" || got.WorkspaceID != "" || got.Env != nil {
		t.Fatalf("doctor popup %+v", got)
	}
	h.hd.openErr = herdr.ErrUnavailable
	if code := h.cmd("ui", "open", "doctor"); code != 1 || !strings.Contains(h.errb.String(), "herdr is not running") {
		t.Fatalf("herdr down: exit %d %s", code, h.errb.String())
	}
}

// The popup command does not inherit this process's environment, so uiOpen
// forwards the config file the parent loaded: --config, else $MAGNUM_CONFIG,
// as an absolute MAGNUM_CONFIG, and nothing for the layout's config.toml.
func TestUIOpenForwardsTheEffectiveConfigFile(t *testing.T) {
	h := newActHarness(t)
	custom := filepath.Join(h.home, "custom.toml")
	inherited := filepath.Join(h.home, "inherited.toml")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  string   // $MAGNUM_CONFIG of this process (and of the herdr context)
		args []string // after `ui open picker`
		want string   // popup MAGNUM_CONFIG; "" means not set
	}{
		{"no override", "", nil, ""},
		{"flag", "", []string{"--config", custom}, custom},
		{"flag beats the inherited variable", inherited, []string{"--config", custom}, custom},
		{"variable alone", inherited, nil, inherited},
		{"relative flag is made absolute", "", []string{"--config", "rel/custom.toml"}, filepath.Join(cwd, "rel", "custom.toml")},
		{"flag naming the default drops the inherited variable", inherited, []string{"--config", filepath.Join(h.home, "config.toml")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAGNUM_CONFIG", tc.env)
			h.env["MAGNUM_CONFIG"] = tc.env
			h.c.cfgPath = ""
			before := len(h.hd.opened)
			if code := h.cmd("ui", append([]string{"open", "picker"}, tc.args...)...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb.String())
			}
			if len(h.hd.opened) != before+1 {
				t.Fatalf("opened %d popups, want 1", len(h.hd.opened)-before)
			}
			got, ok := h.hd.opened[before].Env["MAGNUM_CONFIG"]
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("popup MAGNUM_CONFIG = %q (set %v), want %q; env %v", got, ok, tc.want, h.hd.opened[before].Env)
			}
		})
	}
}
