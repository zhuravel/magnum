package config

import (
	"strings"
	"testing"
)

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		glob, name string
		want       bool
	}{
		// "dir/**": everything below dir, at any depth.
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"docs/**", "doc/a.md", false},
		{"docs/**", "docsx/a.md", false},
		{"docs/**", "src/docs/a.md", false},
		{"docs/**", "docs", true}, // ** matches zero segments
		// "**/pattern": the pattern at any depth.
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/README.md", true},
		{"**/*.md", "a/b.go", false},
		{"**/*.md", "a.md/b.go", false},
		// "*" never crosses "/".
		{"*.md", "README.md", true},
		{"*.md", "docs/x.md", false},
		{"docs/*.md", "docs/x.md", true},
		{"docs/*.md", "docs/a/x.md", false},
		{"*", "a/b", false},
		{"*", "a", true},
		// "**" in the middle.
		{"src/**/test/*.go", "src/test/a.go", true},
		{"src/**/test/*.go", "src/a/test/a.go", true},
		{"src/**/test/*.go", "src/a/b/test/a.go", true},
		{"src/**/test/*.go", "src/a/b/a.go", false},
		{"src/**/test/*.go", "src/a/test/b/a.go", false},
		{"src/**/test/*.go", "lib/a/test/a.go", false},
		// Several "**" and a leading/trailing one.
		{"**/a/**/b/**", "a/b", true},
		{"**/a/**/b/**", "x/a/y/z/b/c/d", true},
		{"**/a/**/b/**", "x/b/a", false},
		{"**", "any/thing/at/all", true},
		{"**/**", "x", true},
		// Exact names, case-sensitive.
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
		{"go.mod", "go.sum", false},
		{"README.md", "readme.md", false},
		{"Docs/**", "docs/a.md", false},
		{"cmd/magnum/main.go", "cmd/magnum/main.go", true},
		// Character classes and "?" stay inside one segment.
		{"v?/[ab]*.txt", "v1/alpha.txt", true},
		{"v?/[ab]*.txt", "v12/alpha.txt", false},
		{"v?/[ab]*.txt", "v1/zeta.txt", false},
		// Degenerate input never matches.
		{"", "a", false},
		{"a", "", false},
		{"", "", false},
		{"[", "a", false},
		{"docs/[", "docs/a", false},
	} {
		if got := MatchPath(tc.glob, tc.name); got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.glob, tc.name, got, tc.want)
		}
	}
}

func TestMatchPathManyStarsStaysFast(t *testing.T) {
	glob := strings.Repeat("**/a/", 20) + "z"
	name := strings.Repeat("a/", 200) + "b"
	if MatchPath(glob, name) {
		t.Fatal("matched")
	}
}

func TestValidatePathGlob(t *testing.T) {
	for _, ok := range []string{"docs/**", "**/*.md", "*.md", "src/**/test/*.go", "go.mod", "a/[bc]?/*", "**", `a\*b`} {
		if err := ValidatePathGlob(ok); err != nil {
			t.Errorf("ValidatePathGlob(%q) = %v", ok, err)
		}
	}
	for _, tc := range []struct{ glob, want string }{
		{"", "empty"},
		{"  ", "empty"},
		{"[", "syntax error"},
		{"docs/[a-", "syntax error"},
		{`a\`, "syntax error"},
		{"a**b", "whole path segment"},
		{"docs/**.md", "whole path segment"},
		{"***/x", "whole path segment"},
		{"/docs/**", "empty path segment"},
		{"docs/", "empty path segment"},
		{"docs//a", "empty path segment"},
	} {
		err := ValidatePathGlob(tc.glob)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidatePathGlob(%q) = %v, want an error containing %q", tc.glob, err, tc.want)
		}
	}
}

func TestPathsSkipped(t *testing.T) {
	docs := Watch{SkipPaths: []string{"docs/**", "**/*.md"}}
	for _, tc := range []struct {
		name  string
		w     Watch
		files []string
		want  bool
	}{
		{"no globs", Watch{}, []string{"docs/a.md"}, false},
		{"empty globs list", Watch{SkipPaths: []string{}}, []string{"docs/a.md"}, false},
		{"blank glob matches nothing", Watch{SkipPaths: []string{""}}, []string{"docs/a.md"}, false},
		{"no files", docs, nil, false},
		{"empty files", docs, []string{}, false},
		{"all match", docs, []string{"docs/a/b.png", "README.md", "src/NOTES.md"}, true},
		{"one globs each", docs, []string{"docs/x", "y.md"}, true},
		{"one outside", docs, []string{"docs/a.md", "src/main.go"}, false},
		{"only outside", docs, []string{"src/main.go"}, false},
		{"rename out of src", docs, []string{"docs/main.go", "src/main.go"}, false},
		{"blank among real globs", Watch{SkipPaths: []string{"", "docs/**"}}, []string{"docs/a"}, true},
	} {
		if got := tc.w.PathsSkipped(tc.files); got != tc.want {
			t.Errorf("%s: PathsSkipped(%q) = %v, want %v", tc.name, tc.files, got, tc.want)
		}
	}
}

func TestValidateSkipPaths(t *testing.T) {
	for _, bad := range []string{"", "[", "a**b", "docs/"} {
		cfg := validPipelineConfig()
		cfg.Watches[0].SkipPaths = []string{"docs/**", bad}
		err := cfg.Validate()
		wantError(t, err, "watch acme", "skip_paths", `"`+bad+`"`)
		if n := len(validateErrs(err)); n != 1 {
			t.Errorf("%q: %d errors, want only the bad glob: %v", bad, n, err)
		}
	}
	cfg := validPipelineConfig()
	cfg.Watches[0].SkipPaths = []string{"docs/**", "**/*.md", "src/**/test/*.go"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSkipPathsLoadsFromTOML(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + "skip_paths = [\"docs/**\", \"**/*.md\"]\n"})
	w := cfg.Watches[0]
	if got := strings.Join(w.SkipPaths, ","); got != "docs/**,**/*.md" {
		t.Fatalf("SkipPaths = %q", got)
	}
	if !w.PathsSkipped([]string{"docs/a.png", "README.md"}) || w.PathsSkipped([]string{"main.go"}) {
		t.Error("PathsSkipped on the loaded watch")
	}
	// A bad glob makes the load fail and names the watch and the glob.
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "skip_paths = [\"docs/**\", \"a**b\"]\n"})
	wantError(t, err, "watch acme", "skip_paths", `"a**b"`)
	// The overlay can declare it too.
	_, err = loadFiles(t, t.TempDir(), map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[watch]]\nowner = \"acme\"\ninclude = [\"*\"]\nidentity = \"z\"\nskip_paths = [\"[\"]\n",
	})
	wantError(t, err, "skip_paths", `"["`)
}
