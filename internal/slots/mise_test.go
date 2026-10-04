package slots

import (
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	testStrip = []string{"GITHUB_PERSONAL_ACCESS_TOKEN", "CLOUDFLARE_TUNNEL_TOKEN"}
	testSet   = map[string]string{"WT_BRANCH": "review3", "CONDUCTOR_WORKSPACE_NAME": ""}
)

// decodeMise parses a rendered file, failing the test when it is not TOML.
func decodeMise(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var tree map[string]any
	if _, err := toml.Decode(string(b), &tree); err != nil {
		t.Fatalf("output is not valid TOML: %v\n%s", err, b)
	}
	return tree
}

func miseEnv(t *testing.T, b []byte) map[string]any {
	t.Helper()
	env, ok := decodeMise(t, b)["env"].(map[string]any)
	if !ok {
		t.Fatalf("no [env] table:\n%s", b)
	}
	return env
}

func TestRenderMiseLocalGolden(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "mise_input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderMiseLocal(src, testStrip, testSet)
	if err != nil {
		t.Fatalf("RenderMiseLocal: %v", err)
	}
	golden := filepath.Join("testdata", "mise_golden.toml")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if !strings.HasPrefix(string(got), miseMarker+"\n") {
		t.Fatalf("no header comment:\n%s", got)
	}
	for _, leak := range []string{"ghp_", "# ", "from-conductor", "repo1"} {
		if strings.Contains(strings.TrimPrefix(string(got), miseMarker), leak) {
			t.Fatalf("output still has %q:\n%s", leak, got)
		}
	}

	env := miseEnv(t, got)
	if env["WT_BRANCH"] != "review3" || env["CONDUCTOR_WORKSPACE_NAME"] != "" {
		t.Fatalf("env = %v", env)
	}
	for _, k := range testStrip {
		if _, ok := env[k]; ok {
			t.Fatalf("stripped key %s still present", k)
		}
	}
	// Everything outside the stripped and set keys survives with its type.
	before := decodeMise(t, src)
	after := decodeMise(t, got)
	beforeEnv, afterEnv := before["env"].(map[string]any), after["env"].(map[string]any)
	delete(before, "env")
	delete(after, "env")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("other tables changed:\n%#v\n%#v", before, after)
	}
	for _, k := range append(slices.Collect(maps.Keys(testSet)), testStrip...) {
		delete(beforeEnv, k)
		delete(afterEnv, k)
	}
	if !reflect.DeepEqual(beforeEnv, afterEnv) {
		t.Fatalf("kept [env] keys changed:\n%#v\n%#v", beforeEnv, afterEnv)
	}
}

func TestRenderMiseLocalIdempotent(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "mise_input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	once, err := RenderMiseLocal(src, testStrip, testSet)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := RenderMiseLocal(once, testStrip, testSet)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("re-render changed the file\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	// A different slot replaces the value instead of adding a second one.
	other, err := RenderMiseLocal(once, testStrip, map[string]string{"WT_BRANCH": "review4", "CONDUCTOR_WORKSPACE_NAME": ""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(other), "WT_BRANCH") != 1 || !strings.Contains(string(other), `WT_BRANCH = "review4"`) {
		t.Fatalf("re-render for another slot:\n%s", other)
	}
	if strings.Count(string(other), miseMarker) != 1 {
		t.Fatalf("header duplicated:\n%s", other)
	}
}

// TestRenderMiseLocalStripsEveryRepresentation: a stripped (or set) key is
// gone whichever valid TOML spelling put it into the env table.
func TestRenderMiseLocalStripsEveryRepresentation(t *testing.T) {
	strip := []string{"GH_TOKEN"}
	set := map[string]string{"WT_BRANCH": "review1"}
	for name, src := range map[string]string{
		"plain":                    "[env]\nGH_TOKEN = \"secret\"\n",
		"quoted key":               "[env]\n\"GH_TOKEN\" = 'secret'\n",
		"spaced header":            "[ env ]\nGH_TOKEN = \"secret\"\n",
		"multi-line string":        "[env]\nGH_TOKEN = \"\"\"\nsecret\n\"\"\"\n",
		"commented out":            "[env]\n# GH_TOKEN = \"secret\"\nA = \"1\"\n",
		"sub-table":                "[env.GH_TOKEN]\nvalue = \"secret\"\n",
		"quoted sub-table":         "[env.\"GH_TOKEN\"]\nvalue = \"secret\"\nredact = true\n",
		"dotted key in env":        "[env]\nGH_TOKEN.value = \"secret\"\n",
		"inline table in env":      "[env]\nGH_TOKEN = { value = \"secret\" }\n",
		"root dotted key":          "env.GH_TOKEN = \"secret\"\n",
		"root inline table":        "env = { GH_TOKEN = \"secret\" }\n",
		"root dotted sub-key":      "env.GH_TOKEN.value = \"secret\"\n",
		"set key as sub-table":     "[env.WT_BRANCH]\nvalue = \"secret\"\n",
		"set key as inline table":  "[env]\nWT_BRANCH = { value = \"secret\" }\n",
		"set key as dotted (root)": "env.WT_BRANCH = \"secret\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderMiseLocal([]byte(src), strip, set)
			if err != nil {
				t.Fatalf("RenderMiseLocal: %v", err)
			}
			if body := strings.TrimPrefix(string(got), miseMarker+"\n"); strings.Contains(body, "secret") || strings.Contains(body, "GH_TOKEN") {
				t.Fatalf("secret leaked:\n%s", got)
			}
			env := miseEnv(t, got)
			if env["WT_BRANCH"] != "review1" {
				t.Fatalf("WT_BRANCH = %#v\n%s", env["WT_BRANCH"], got)
			}
		})
	}
}

func TestRenderMiseLocalCases(t *testing.T) {
	set := map[string]string{"WT_BRANCH": "review1"}
	h := miseMarker + "\n"
	cases := []struct {
		name, src string
		set       map[string]string
		want      string
	}{
		{name: "empty source", src: "", set: set, want: h + "[env]\nWT_BRANCH = \"review1\"\n"},
		{name: "empty source, nothing to set", src: "", want: h + "[env]\n"},
		{
			name: "no env table adds one",
			src:  "[tools]\nnode = \"22\"",
			set:  set,
			want: h + "[env]\nWT_BRANCH = \"review1\"\n\n[tools]\nnode = \"22\"\n",
		},
		{
			name: "mise directives kept",
			src:  "[env]\nA = \"1\"\n_.path = [\"bin\"]\n_.file = '.env'\n",
			set:  set,
			want: h + "[env]\nA = \"1\"\nWT_BRANCH = \"review1\"\n[env._]\nfile = \".env\"\npath = [\"bin\"]\n",
		},
		{
			name: "env sub-table of another key is kept",
			src:  "[env.OTHER]\nvalue = \"x\"\n",
			set:  set,
			want: h + "[env]\nWT_BRANCH = \"review1\"\n[env.OTHER]\nvalue = \"x\"\n",
		},
		{
			name: "non-string values kept",
			src:  "min_version = \"2024.9.5\"\n[settings]\njobs = 4\nexperimental = true\nratio = 1.5\n[tools]\npython = [\"3.12\", \"3.11\"]\n[tools.ruby]\nversion = \"3.3\"\n",
			set:  set,
			want: h + "min_version = \"2024.9.5\"\n\n[env]\nWT_BRANCH = \"review1\"\n\n[settings]\nexperimental = true\njobs = 4\nratio = 1.5\n\n[tools]\npython = [\"3.12\", \"3.11\"]\n[tools.ruby]\nversion = \"3.3\"\n",
		},
		{
			name: "local dates and times keep their wall clock",
			src:  "[tools]\nd = 1979-05-27\nt = 07:32:00\ndt = 1979-05-27T07:32:00.5\nodt = 1979-05-27T07:32:00-07:00\n",
			want: h + "[env]\n\n[tools]\nd = 1979-05-27\ndt = 1979-05-27T07:32:00.5\nodt = 1979-05-27T07:32:00-07:00\nt = 07:32:00\n",
		},
		{
			name: "crlf input",
			src:  "[env]\r\nA = \"1\"\r\n",
			set:  set,
			want: h + "[env]\nA = \"1\"\nWT_BRANCH = \"review1\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderMiseLocal([]byte(tc.src), testStrip, tc.set)
			if err != nil {
				t.Fatalf("RenderMiseLocal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
			}
			again, err := RenderMiseLocal(got, testStrip, tc.set)
			if err != nil || string(again) != string(got) {
				t.Fatalf("not idempotent (%v):\n%q", err, again)
			}
		})
	}
	if got, err := RenderMiseLocal(nil, nil, nil); err != nil || string(got) != h+"[env]\n" {
		t.Fatalf("nil source = %q, %v", got, err)
	}
}

func TestRenderMiseLocalQuotesValues(t *testing.T) {
	got, err := RenderMiseLocal(nil, nil, map[string]string{"A": "x\"y\\z\n", "weird key": "v", "B": "'''"})
	if err != nil {
		t.Fatal(err)
	}
	env := miseEnv(t, got)
	if env["A"] != "x\"y\\z\n" || env["weird key"] != "v" || env["B"] != "'''" {
		t.Fatalf("env = %#v", env)
	}
}

func TestRenderMiseLocalRejects(t *testing.T) {
	for _, src := range []string{
		"[[env]]\nA = \"1\"\n",
		"env = \"x\"\n",
		"env = [\"A=1\"]\n",
		"env = 1\n",
		"[env\nA = \"1\"\n",
		"[env]\nA = \"1\"\nA = \"2\"\n",
		"[env]\nA = \n",
	} {
		if out, err := RenderMiseLocal([]byte(src), nil, testSet); !errors.Is(err, ErrMiseLocal) || out != nil {
			t.Errorf("%q: %q, %v; want ErrMiseLocal", src, out, err)
		}
	}
}

// TestPoolRenderStripsDefaultTokens: a pool slot drops the GitHub tokens of
// DefaultStripEnv on top of the pool's strip_env, like a per-PR worktree.
func TestPoolRenderStripsDefaultTokens(t *testing.T) {
	h := newHarness(t)
	writeFile(t, filepath.Join(h.main, MiseLocal),
		"[env]\nGH_TOKEN = \"gho_x\"\nGITHUB_TOKEN = \"ghs_x\"\nGITHUB_PERSONAL_ACCESS_TOKEN = \"ghp_x\"\nPOOL_SECRET = \"s\"\nKEEP = \"k\"\n")
	h.pool.StripEnv = []string{"POOL_SECRET", "GH_TOKEN"}
	sl := h.provisioned(1)
	local := readFile(t, filepath.Join(sl.Path, MiseLocal))
	for _, gone := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "POOL_SECRET"} {
		if strings.Contains(local, gone) {
			t.Fatalf("rendered %s still has %s:\n%s", MiseLocal, gone, local)
		}
	}
	if !strings.Contains(local, `KEEP = "k"`) || !strings.Contains(local, `WT_BRANCH = "review1"`) {
		t.Fatalf("rendered:\n%s", local)
	}
}

// TestRenderMiseConfinedToCheckout: a slot whose checkout turns copy_files'
// parent and .mise.local.toml into symlinks out of it cannot make magnum
// write there.
func TestRenderMiseConfinedToCheckout(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "orig")
	writeFile(t, filepath.Join(h.main, "config", "initializers", "local.rb"), "LOCAL = 1\n")
	h.pool.CopyFiles = append(h.pool.CopyFiles, "config/initializers/local.rb")
	for _, p := range []string{MiseLocal, filepath.Join("config", "initializers")} {
		if err := os.RemoveAll(filepath.Join(sl.Path, p)); err != nil {
			t.Fatal(err)
		}
	}
	symlink(t, victim, filepath.Join(sl.Path, MiseLocal))
	symlink(t, outside, filepath.Join(sl.Path, "config", "initializers"))

	if err := h.m.renderMise(h.ctx, sl, h.pool); err == nil {
		t.Fatal("copy through a symlink out of the checkout succeeded")
	}
	if fi, err := os.Lstat(filepath.Join(sl.Path, MiseLocal)); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, %v; want a regular 0600 file", MiseLocal, fi, err)
	}
	if got := readFile(t, victim); got != "orig" {
		t.Fatalf("symlink target outside the checkout rewritten: %q", got)
	}
	if names := dirNames(t, outside); !slices.Equal(names, []string{"victim"}) {
		t.Fatalf("outside = %v", names)
	}
}
