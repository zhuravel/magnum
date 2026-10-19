package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The docs name the config's keys and count its built-in roles in prose, and
// both drifted before (a key renamed in a table, "four built-in roles" after
// the fix and whiteboard roles joined the list). These tests read the docs
// from the checkout, so a doc that disagrees with the loader fails the gate.

func readDoc(t *testing.T, elems ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, elems...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// Every ```toml block of README.md and prompts/README.md is a configuration
// the loader's decoder reads with no key it does not know: a key renamed in
// the code and not in the docs fails here. (Decoding only: a block may name
// an identity or a role declared elsewhere, which validation would refuse.)
func TestEveryTomlBlockInTheDocsDecodesWithoutUnknownKeys(t *testing.T) {
	blocks := 0
	for _, doc := range []string{"README.md", filepath.Join("prompts", "README.md")} {
		for i, m := range tomlBlock.FindAllStringSubmatch(readDoc(t, doc), -1) {
			blocks++
			var cfg Config
			md, err := toml.Decode(m[1], &cfg)
			if err != nil {
				t.Errorf("%s: toml block %d does not decode: %v", doc, i+1, err)
				continue
			}
			if undecoded := md.Undecoded(); len(undecoded) > 0 {
				t.Errorf("%s: toml block %d has keys the config does not know: %v", doc, i+1, undecoded)
			}
		}
	}
	if blocks < 10 { // the pattern must find the blocks, not silently skip them
		t.Fatalf("found only %d toml blocks in the docs; the pattern is broken", blocks)
	}
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

// The docs write the count of built-in roles as a word ("the seven built-in
// roles", "the seven [[role]] blocks below"); it is len(DefaultRoles()).
func TestTheDocsCountTheBuiltInRoles(t *testing.T) {
	n := len(DefaultRoles())
	if n >= len(numberWords) {
		t.Fatalf("DefaultRoles has %d roles; numberWords stops at %d", n, len(numberWords)-1)
	}
	want := numberWords[n]
	words := strings.Join(numberWords[1:], "|")
	count := regexp.MustCompile(`(?i)\b(` + words + `) (built-in roles|\[\[role\]\] blocks below)|the (` + words + `) roles below are the built-in`)
	seen := 0
	for _, doc := range [][]string{{"README.md"}, {"prompts", "README.md"}, {"config.defaults.toml"}, {"config.full.example.toml"}, {"config.example.toml"}} {
		// A comment wraps lines, so a phrase may stand across a line break.
		text := strings.Join(strings.Fields(readDoc(t, doc...)), " ")
		for _, m := range count.FindAllStringSubmatch(text, -1) {
			seen++
			if got := strings.ToLower(m[1] + m[3]); got != want {
				t.Errorf("%s says %q, but DefaultRoles has %d (%s) roles", filepath.Join(doc...), m[0], n, want)
			}
		}
	}
	if seen < 4 { // the pattern must find the phrases, not silently skip them
		t.Fatalf("found only %d counts of the built-in roles in the docs; the pattern is broken", seen)
	}
}

// repoKeyGroups sorts a [[repo]] block's keys by Repo.worktreeKeys: the keys a
// block for a pooled repository may set, and the per-PR worktree keys it may
// not. Each key is set alone on an empty Repo, so a key the method counts is
// banned and any other (but repo, which names the block) is allowed; a field
// added to Repo or to worktreeKeys moves the lists with it.
func repoKeyGroups(t *testing.T) (allowed, banned []string) {
	t.Helper()
	typ := reflect.TypeFor[Repo]()
	for i := range typ.NumField() {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("toml"), ",")
		if key == "" || key == "repo" {
			continue
		}
		var r Repo
		f := reflect.ValueOf(&r).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int:
			f.SetInt(1)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
			f.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf("v"))
		case reflect.Struct: // Duration
			f.Field(0).SetInt(1)
		default:
			t.Fatalf("Repo.%s (%s) has a kind this test cannot set", typ.Field(i).Name, f.Kind())
		}
		if r.worktreeKeys() {
			banned = append(banned, key)
		} else {
			allowed = append(allowed, key)
		}
	}
	slices.Sort(allowed)
	slices.Sort(banned)
	if len(allowed) < 11 || len(banned) < 6 { // the derivation must find the keys, not silently skip them
		t.Fatalf("derived allowed %v and banned %v", allowed, banned)
	}
	return allowed, banned
}

var keyName = regexp.MustCompile(`[a-z][a-z_]*[a-z]`)

// keysNamed returns the sorted key names a list of prose names ("a, b and c").
func keysNamed(list string) []string {
	var out []string
	for _, w := range keyName.FindAllString(list, -1) {
		if w != "and" {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return out
}

// A [[repo]] block for a repository with a [[pool]] may set every key but the
// per-PR worktree ones (Repo.worktreeKeys). Three texts list them: the error
// that refuses a worktree key there, and the comments of config.defaults.toml
// and config.full.example.toml; the error omitted three allowed keys and the
// comments two and five. They must name exactly the keys the code allows.
func TestTheTextsOnAPooledReposKeysNameEveryKeyTheCodeAllows(t *testing.T) {
	allowed, banned := repoKeyGroups(t)

	cfg := validPipelineConfig()
	cfg.Watches = []Watch{{Owner: "example", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
	cfg.Pools = []Pool{{Repo: "example/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}}
	cfg.Repos = []Repo{{Repo: "example/big", Setup: []string{"bin/setup"}}}
	err := cfg.Validate()
	wantError(t, err, "repo example/big", "has a [[pool]]")
	m := regexp.MustCompile(`only (.+?) apply to it \((.+?) configure per-PR worktrees`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the error does not list the keys that apply and the ones that do not: %v", err)
	}
	if got := keysNamed(m[1]); !slices.Equal(got, allowed) {
		t.Errorf("the error names %v as the keys that apply to a pooled repository, but the code allows %v", got, allowed)
	}
	if got := keysNamed(m[2]); !slices.Equal(got, banned) {
		t.Errorf("the error names %v as per-PR worktree keys, but the code bans %v", got, banned)
	}

	// The comments of both files carry the sentence from "Its no_findings_event"
	// to "[[pool]] repo too."; a comment wraps lines, so read it joined.
	for _, doc := range []string{"config.defaults.toml", "config.full.example.toml"} {
		text := strings.Join(strings.Fields(strings.ReplaceAll(readDoc(t, doc), "\n#", "\n")), " ")
		i := strings.Index(text, "Its no_findings_event")
		j := strings.Index(text, "[[pool]] repo too.")
		if i < 0 || j < i {
			t.Fatalf("%s: the sentence on the keys a pooled repository may set is gone", doc)
		}
		sentence := text[i : j+len("[[pool]] repo too.")]
		for _, key := range allowed {
			if !regexp.MustCompile(`\b` + key + `\b`).MatchString(sentence) {
				t.Errorf("%s: the sentence omits %s, which a [[repo]] block for a pooled repository may set: %s", doc, key, sentence)
			}
		}
		for _, key := range banned {
			if regexp.MustCompile(`\b` + key + `\b`).MatchString(sentence) {
				t.Errorf("%s: the sentence names %s, which a [[repo]] block for a pooled repository may not set: %s", doc, key, sentence)
			}
		}
	}
}
