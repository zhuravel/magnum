package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// realTemp is t.TempDir() with symlinks resolved (on macOS /var is a link to
// /private/var, and Resolve returns resolved paths).
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLayout(t *testing.T) {
	mh := filepath.Join(realTemp(t), "mh") // does not exist yet: allowed
	t.Setenv("MAGNUM_HOME", mh)
	l, err := Resolve()
	if err != nil || l.Home != mh {
		t.Fatalf("%v %+v", err, l)
	}
	if l.DB() != mh+"/state/magnum.db" || l.GhConfigDir("talkable-app") != mh+"/state/gh/talkable-app" {
		t.Fatal(l.DB(), l.GhConfigDir("talkable-app"))
	}
	if l.Lock() != mh+"/state/magnum.lock" || l.OpsLock() != mh+"/state/ops.lock" {
		t.Fatal(l.Lock(), l.OpsLock())
	}
	if got := l.ReviewDir("talkable", "talkable", 11920, "0123456789abcdef0123"); got != filepath.Join(mh, "state/reviews/talkable/talkable/11920/0123456789ab") {
		t.Fatal(got)
	}
	l = Layout{Home: t.TempDir()}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
}

// MAGNUM_HOME reaches agents whose working directory is a PR checkout (as
// GH_CONFIG_DIR and the report dir), so it must be absolute and symlink-free.
func TestResolveMagnumHomeIsAbsoluteAndResolved(t *testing.T) {
	base := realTemp(t)
	target := filepath.Join(base, "checkout")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	cases := []struct {
		name, env, want string
	}{
		{"absolute", target, target},
		{"relative", "checkout", target},
		{"dot", ".", base},
		{"dot-dot", "checkout/..", base},
		{"symlink", link, target},
		{"relative symlink", "link", target},
		{"trailing slash", target + "/", target},
		{"messy", filepath.Join(base, "x", "..", "checkout"), target},
		{"missing under real dir", filepath.Join(target, "new", "home"), filepath.Join(target, "new", "home")},
		{"missing under symlink", filepath.Join(link, "new", "home"), filepath.Join(target, "new", "home")},
		{"missing relative", "checkout/new", filepath.Join(target, "new")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MAGNUM_HOME", c.env)
			l, err := Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if l.Home != c.want {
				t.Fatalf("Home = %q, want %q", l.Home, c.want)
			}
			if !filepath.IsAbs(l.Home) || !filepath.IsAbs(l.GhConfigDir("x")) {
				t.Fatalf("layout paths must be absolute: %q", l.GhConfigDir("x"))
			}
		})
	}
}

func TestResolveMagnumHomeExpandsTilde(t *testing.T) {
	home := realTemp(t)
	t.Setenv("HOME", home)
	t.Setenv("MAGNUM_HOME", "~/magnum")
	l, err := Resolve()
	if err != nil || l.Home != filepath.Join(home, "magnum") {
		t.Fatalf("%+v, %v", l, err)
	}
}

// A path that exists but cannot be resolved is an error, not a guess.
func TestResolveMagnumHomeErrors(t *testing.T) {
	base := realTemp(t)
	write(t, filepath.Join(base, "file"), "x")
	for _, l := range [][2]string{
		{filepath.Join(base, "nowhere"), "dangling"},
		{"loop2", "loop1"},
		{"loop1", "loop2"},
	} {
		if err := os.Symlink(l[0], filepath.Join(base, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	for name, env := range map[string]string{
		"dangling symlink":     filepath.Join(base, "dangling"),
		"below dangling":       filepath.Join(base, "dangling", "home"),
		"symlink loop":         filepath.Join(base, "loop1"),
		"below a regular file": filepath.Join(base, "file", "home"), // ENOTDIR is not "does not exist"
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MAGNUM_HOME", env)
			if l, err := Resolve(); err == nil {
				t.Fatalf("want error, got %+v", l)
			}
		})
	}
}

const magnumGoMod = "module github.com/zhuravel/magnum\n\ngo 1.27\n"

func TestFindHomeAcceptsOnlyMagnumsGoMod(t *testing.T) {
	root := realTemp(t)
	cases := []struct {
		name   string
		goMod  string // go.mod at root/ (empty: none)
		config bool   // config.toml at root/
		wantOK bool
	}{
		{"magnum module", magnumGoMod, false, true},
		{"magnum module after a comment", "// header\n\nmodule github.com/zhuravel/magnum // trailing\ngo 1.27\n", false, true},
		{"quoted module path", "module \"github.com/zhuravel/magnum\"\n", false, true},
		{"another module", "module example.com/other\n\ngo 1.27\n", false, false},
		{"module that merely starts with magnum's", "module github.com/zhuravel/magnum-tools\n", false, false},
		{"empty go.mod", "\n", false, false},
		{"no module line", "go 1.27\n", false, false},
		{"no go.mod", "", false, false},
		{"config.toml alone", "", true, true},
		{"config.toml beside a foreign go.mod", "module example.com/other\n", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(root, c.name)
			start := filepath.Join(dir, "bin") // where the binary lives
			if err := os.MkdirAll(start, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.goMod != "" {
				write(t, filepath.Join(dir, "go.mod"), c.goMod)
			}
			if c.config {
				write(t, filepath.Join(dir, "config.toml"), "")
			}
			got, ok := findHome(start)
			if ok != c.wantOK {
				t.Fatalf("findHome(%q) = %q, %v; want ok=%v", start, got, ok, c.wantOK)
			}
			if ok && got != dir {
				t.Fatalf("home = %q, want %q", got, dir)
			}
		})
	}
}

// A foreign go.mod near the binary does not stop the walk: magnum's own
// go.mod further up still wins. The walk is bounded to four directories.
func TestFindHomeKeepsWalkingPastForeignModules(t *testing.T) {
	root := realTemp(t)
	write(t, filepath.Join(root, "go.mod"), magnumGoMod)
	write(t, filepath.Join(root, "vendorish", "go.mod"), "module example.com/other\n")
	start := filepath.Join(root, "vendorish")
	if got, ok := findHome(start); !ok || got != root {
		t.Fatalf("findHome = %q, %v; want %q", got, ok, root)
	}
	deep := filepath.Join(root, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := findHome(deep); ok {
		t.Fatalf("findHome(%q) = %q; the walk stops after four levels", deep, got)
	}
	if got, ok := findHome(filepath.Dir(deep)); !ok || got != root {
		t.Fatalf("findHome three levels deep = %q, %v; want %q", got, ok, root)
	}
}

// A binary reached through a symlink (~/bin/magnum -> checkout/bin/magnum)
// finds the checkout it lives in, not the directory holding the link.
func TestHomeFromExecutableFollowsSymlinks(t *testing.T) {
	base := realTemp(t)
	checkout := filepath.Join(base, "checkout")
	write(t, filepath.Join(checkout, "go.mod"), magnumGoMod)
	write(t, filepath.Join(checkout, "bin", "magnum"), "#!/bin/sh\n")
	// The link's own directory belongs to an unrelated Go module.
	write(t, filepath.Join(base, "other", "go.mod"), "module example.com/other\n")
	write(t, filepath.Join(base, "other", "config.toml"), "")
	link := filepath.Join(base, "other", "bin", "magnum")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "bin", "magnum"), link); err != nil {
		t.Fatal(err)
	}
	if got, ok := homeFromExecutable(link); !ok || got != checkout {
		t.Fatalf("homeFromExecutable(%q) = %q, %v; want %q", link, got, ok, checkout)
	}
	if _, ok := homeFromExecutable(filepath.Join(base, "missing")); ok {
		t.Fatal("a missing executable must not resolve")
	}
}

// MkdirAll applies 0700 only to directories it creates; EnsureDirs tightens
// the ones that already exist (an older install, a manual mkdir).
func TestEnsureDirsTightensExistingDirs(t *testing.T) {
	l := Layout{Home: realTemp(t)}
	for _, d := range []string{l.State(), l.Logs(), l.GhRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil { // MkdirAll honours the umask
			t.Fatal(err)
		}
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{l.State(), l.Logs(), l.Reviews(), l.GhRoot()} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %v, want 0700", d, got)
		}
	}
	if err := l.EnsureDirs(); err != nil { // idempotent
		t.Fatal(err)
	}
}

// An installed magnum keeps its data where XDG says; MAGNUM_HOME keeps the
// checkout layout; an install whose registry still sits in a checkout's
// state/ (the binary's checkout, else ~/Projects/magnum) keeps using it until
// `magnum migrate-home` moves it, rather than starting over empty.
func TestResolveLayouts(t *testing.T) {
	user := realTemp(t)
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	brewExe := filepath.Join(user, "homebrew", "Cellar", "magnum", "1.2.0", "bin", "magnum")
	write(t, brewExe, "")

	l, err := resolve(env(nil), brewExe, user)
	if err != nil || l.Home != "" || l.CheckoutLayout() || !l.Valid() {
		t.Fatalf("installed: %+v %v", l, err)
	}
	for got, want := range map[string]string{
		l.DB():                          filepath.Join(user, ".local/share/magnum/magnum.db"),
		l.ReviewDir("o", "r", 1, "abc"): filepath.Join(user, ".local/share/magnum/reviews/o/r/1/abc"),
		l.Notes():                       filepath.Join(user, ".local/share/magnum/notes"),
		l.Logs():                        filepath.Join(user, ".local/state/magnum/logs"),
		l.GhConfigDir("app"):            filepath.Join(user, ".local/state/magnum/gh/app"),
		l.UserConfig:                    filepath.Join(user, ".config/magnum/config.toml"),
		l.ConfigDir():                   filepath.Join(user, ".config/magnum"),
		l.Binary():                      filepath.Join(user, "homebrew", "bin", "magnum"), // not the versioned Cellar path
		l.Skill() + "|" + l.Config():    "|",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	x := realTemp(t)
	l, _ = resolve(env(map[string]string{"XDG_DATA_HOME": x + "/data", "XDG_STATE_HOME": x + "/state", "XDG_CONFIG_HOME": "relative"}), brewExe, user)
	if l.DB() != x+"/data/magnum/magnum.db" || l.Lock() != x+"/state/magnum/magnum.lock" || l.UserConfig != filepath.Join(user, ".config/magnum/config.toml") {
		t.Fatalf("XDG variables: %+v", l)
	}

	// The registry still in ~/Projects/magnum/state: that layout, until it moves.
	legacy := filepath.Join(user, "Projects", "magnum")
	write(t, filepath.Join(legacy, "state", "magnum.db"), "")
	if l, _ = resolve(env(nil), brewExe, user); l.Home != legacy || !l.CheckoutLayout() || l.DB() != legacy+"/state/magnum.db" {
		t.Fatalf("legacy registry: %+v", l)
	}
	write(t, filepath.Join(user, ".local/share/magnum/magnum.db"), "")
	if l, _ = resolve(env(nil), brewExe, user); l.CheckoutLayout() {
		t.Fatalf("both registries: the XDG one wins, got %+v", l)
	}

	// A binary in a checkout: Home for its sources, data in XDG.
	co := filepath.Join(realTemp(t), "magnum")
	write(t, filepath.Join(co, "go.mod"), "module "+modulePath+"\n")
	write(t, filepath.Join(co, "bin", "magnum"), "")
	if l, _ = resolve(env(nil), filepath.Join(co, "bin", "magnum"), user); l.Home != co || l.CheckoutLayout() || l.Binary() != filepath.Join(co, "bin", "magnum") || l.Skill() == "" {
		t.Fatalf("checkout binary: %+v", l)
	}

	// MAGNUM_HOME: the checkout layout whatever else exists.
	if l, _ = resolve(env(map[string]string{"MAGNUM_HOME": co}), brewExe, user); l.Home != co || !l.CheckoutLayout() || l.DB() != co+"/state/magnum.db" {
		t.Fatalf("MAGNUM_HOME: %+v", l)
	}
}
