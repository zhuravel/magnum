package confedit

import (
	"errors"
	"strings"
	"testing"
)

func set(t Table, path string, v any) Change {
	return Change{Key: Key{Table: t, Path: strings.Split(path, ".")}, Value: v}
}

func noted(c Change, note string) Change { c.Note = note; return c }

func unset(t Table, path string) Change { return set(t, path, nil) }

// TestApplyEditsOnlyTheBytesOfTheEditedValue pins each edit rule byte for
// byte; every case also passes Verify (Apply runs it).
func TestApplyEditsOnlyTheBytesOfTheEditedValue(t *testing.T) {
	pipeline, daemon := Section("pipeline"), Section("daemon")
	cases := []struct {
		name, src, want string
		changes         []Change
	}{
		{
			name:    "a shorter value keeps the comment at its column",
			src:     "[pipeline]\nspend = \"medium\"   # the level\nfix_cycles = 3\n",
			changes: []Change{set(pipeline, "spend", "low")},
			want:    "[pipeline]\nspend = \"low\"      # the level\nfix_cycles = 3\n",
		},
		{
			name:    "a longer value pushes the comment right, one space at least",
			src:     "[pipeline]\nspend = \"low\" # the level\n",
			changes: []Change{set(pipeline, "spend", "medium")},
			want:    "[pipeline]\nspend = \"medium\" # the level\n",
		},
		{
			name:    "a note on a line without a comment aligns with the nearest comment",
			src:     "[daemon]\nmax_concurrent_reviews = 3        # rounds at once\npush_quiet_period = \"5m\"\n",
			changes: []Change{noted(set(daemon, "push_quiet_period", "15m"), `was "5m"`)},
			want:    "[daemon]\nmax_concurrent_reviews = 3        # rounds at once\npush_quiet_period = \"15m\"         # 2026-10-20: was \"5m\"\n",
		},
		{
			name:    "a note with no comment in the table takes two spaces",
			src:     "[pipeline]\nspend = \"high\"\n",
			changes: []Change{noted(set(pipeline, "spend", "burn"), `was "high"`)},
			want:    "[pipeline]\nspend = \"burn\"  # 2026-10-20: was \"high\"\n",
		},
		{
			name:    "a note goes before an old comment",
			src:     "[pipeline]\nspend = \"high\"   # 2026-10-14: medium -> high\n",
			changes: []Change{noted(set(pipeline, "spend", "low"), `was "high"`)},
			want:    "[pipeline]\nspend = \"low\"    # 2026-10-20: was \"high\"; 2026-10-14: medium -> high\n",
		},
		{
			name:    "a key of an inline table is replaced inside the braces",
			src:     "[spend.high]\nclaude-review = { model = \"fable\", effort = \"xhigh\" }   # 10-14\n",
			changes: []Change{set(Section("spend", "high"), "claude-review.model", "opus")},
			want:    "[spend.high]\nclaude-review = { model = \"opus\", effort = \"xhigh\" }    # 10-14\n",
		},
		{
			name:    "a missing key of an inline table goes before its closing brace",
			src:     "[spend.high]\nclaude-review = { model = \"opus\" }\n",
			changes: []Change{set(Section("spend", "high"), "claude-review.effort", "high")},
			want:    "[spend.high]\nclaude-review = { model = \"opus\", effort = \"high\" }\n",
		},
		{
			name:    "a key into an empty inline table",
			src:     "[spend.burn]\nclaude-review = {}\n",
			changes: []Change{set(Section("spend", "burn"), "claude-review.model", "opus")},
			want:    "[spend.burn]\nclaude-review = { model = \"opus\" }\n",
		},
		{
			name:    "a missing role of a level is a new inline-table line after the table's last key",
			src:     "[spend.burn]\ncodex-judge = { effort = \"xhigh\" }\n\n[pipeline]\nspend = \"high\"\n",
			changes: []Change{set(Section("spend", "burn"), "claude-simplify.model", "opus")},
			want:    "[spend.burn]\ncodex-judge = { effort = \"xhigh\" }\nclaude-simplify = { model = \"opus\" }\n\n[pipeline]\nspend = \"high\"\n",
		},
		{
			name:    "a key of a [[pool]] element goes above its [pool.env]",
			src:     "[[pool]]\nrepo = \"talkable/web\"\nmax = 2\n\n  [pool.env]\n  A = \"1\"\n\n[[pool]]\nrepo = \"talkable/api\"\n",
			changes: []Change{set(Table{Path: []string{"pool"}, Index: 0}, "min", int64(1))},
			want:    "[[pool]]\nrepo = \"talkable/web\"\nmax = 2\nmin = 1\n\n  [pool.env]\n  A = \"1\"\n\n[[pool]]\nrepo = \"talkable/api\"\n",
		},
		{
			name:    "a key of a subtable keeps the subtable's indentation",
			src:     "[[pool]]\nrepo = \"talkable/web\"\n\n  [pool.env]\n  A = \"1\"\n\n[[pool]]\nrepo = \"talkable/api\"\n",
			changes: []Change{set(Table{Path: []string{"pool", "env"}, Index: 0}, "B", "2")},
			want:    "[[pool]]\nrepo = \"talkable/web\"\n\n  [pool.env]\n  A = \"1\"\n  B = \"2\"\n\n[[pool]]\nrepo = \"talkable/api\"\n",
		},
		{
			name:    "a new key goes after the lines continuing the last key's comment",
			src:     "[daemon]\npush_quiet_period = \"15m\"   # a replay:\n                             # fewer rounds\n# request_debounce = \"5m\"   # tried\n",
			changes: []Change{set(daemon, "min_rereview_interval", "30m")},
			want:    "[daemon]\npush_quiet_period = \"15m\"   # a replay:\n                             # fewer rounds\nmin_rereview_interval = \"30m\"\n# request_debounce = \"5m\"   # tried\n",
		},
		{
			name:    "a key into a table with no key goes under its header",
			src:     "[usage]\n\n[pipeline]\nspend = \"high\"\n",
			changes: []Change{set(Section("usage"), "codex_soft", int64(90))},
			want:    "[usage]\ncodex_soft = 90\n\n[pipeline]\nspend = \"high\"\n",
		},
		{
			name:    "a new table goes after the last of its family, before the comments that introduce the next header",
			src:     "[kinds.codex]\nspend = \"burn\"\n# start = [\"--search\"]\n\n# The watches.\n[[watch]]\nowner = \"talkable\"\n",
			changes: []Change{set(Section("kinds", "claude"), "spend", "low")},
			want:    "[kinds.codex]\nspend = \"burn\"\n# start = [\"--search\"]\n\n[kinds.claude]\nspend = \"low\"\n\n# The watches.\n[[watch]]\nowner = \"talkable\"\n",
		},
		{
			name:    "a new table without a family goes before the first array of tables",
			src:     "[pipeline]\nspend = \"high\"\n# The watches.\n[[watch]]\nowner = \"talkable\"\n\n[notes]\nmax_bytes = 1\n",
			changes: []Change{noted(set(Section("usage"), "codex_soft", int64(90)), "was 80")},
			want:    "[pipeline]\nspend = \"high\"\n\n[usage]\ncodex_soft = 90  # 2026-10-20: was 80\n\n# The watches.\n[[watch]]\nowner = \"talkable\"\n\n[notes]\nmax_bytes = 1\n",
		},
		{
			name:    "a new table in a file without arrays of tables goes at the end",
			src:     "[pipeline]\nspend = \"high\"",
			changes: []Change{set(Section("usage"), "codex_hard", int64(98))},
			want:    "[pipeline]\nspend = \"high\"\n\n[usage]\ncodex_hard = 98\n",
		},
		{
			name: "a new [[role]] goes after the last [[role]]",
			src:  "[[role]]\nname = \"claude-review\"\nmodel = \"opus\"\n\n[pipeline]\nspend = \"high\"\n",
			changes: []Change{
				set(Table{Path: []string{"role"}, Index: 1}, "name", "claude-simplify"),
				set(Table{Path: []string{"role"}, Index: 1}, "model", "sonnet"),
			},
			want: "[[role]]\nname = \"claude-review\"\nmodel = \"opus\"\n\n[[role]]\nname = \"claude-simplify\"\nmodel = \"sonnet\"\n\n[pipeline]\nspend = \"high\"\n",
		},
		{
			name: "a new [[repo]] goes after the last [[repo]] and its subtables",
			src:  "[[repo]]\nrepo = \"talkable/web\"\n  [repo.env]\n  A = \"1\"\n[pipeline]\nspend = \"high\"\n",
			changes: []Change{
				set(Table{Path: []string{"repo"}, Index: 1}, "repo", "talkable/api"),
				set(Table{Path: []string{"repo"}, Index: 1}, "stakes", "the public API"),
			},
			want: "[[repo]]\nrepo = \"talkable/web\"\n  [repo.env]\n  A = \"1\"\n\n[[repo]]\nrepo = \"talkable/api\"\nstakes = \"the public API\"\n\n[pipeline]\nspend = \"high\"\n",
		},
		{
			name:    "a removed key keeps its comment, and the lines continuing it, as comment lines",
			src:     "[daemon]\npush_quiet_period = \"15m\"   # a replay:\n                             # fewer rounds\nquiet_hours = \"\"\n",
			changes: []Change{unset(daemon, "push_quiet_period")},
			want:    "[daemon]\n# a replay:\n                             # fewer rounds\nquiet_hours = \"\"\n",
		},
		{
			name:    "a removed key without a comment loses its line",
			src:     "[daemon]\npush_quiet_period = \"15m\"\nquiet_hours = \"\"\n",
			changes: []Change{unset(daemon, "push_quiet_period")},
			want:    "[daemon]\nquiet_hours = \"\"\n",
		},
		{
			name:    "a removal's note becomes the comment line",
			src:     "[pipeline]\n  spend = \"high\"   # 10-14: medium -> high\n",
			changes: []Change{noted(unset(pipeline, "spend"), `spend back to the default, was "high"`)},
			want:    "[pipeline]\n  # 2026-10-20: spend back to the default, was \"high\"; 10-14: medium -> high\n",
		},
		{
			name:    "removing an inline table's first key takes the comma after it",
			src:     "[spend.high]\nclaude-judge = { model = \"opus\", effort = \"xhigh\" }  # set\n",
			changes: []Change{unset(Section("spend", "high"), "claude-judge.model")},
			want:    "[spend.high]\nclaude-judge = { effort = \"xhigh\" }                  # set\n",
		},
		{
			name:    "removing an inline table's last key takes the comma before it",
			src:     "[spend.high]\nclaude-judge = { model = \"opus\", effort = \"xhigh\" }\n",
			changes: []Change{unset(Section("spend", "high"), "claude-judge.effort")},
			want:    "[spend.high]\nclaude-judge = { model = \"opus\" }\n",
		},
		{
			name:    "removing an inline table's only key removes its line",
			src:     "[spend.high]\nclaude-review = { model = \"opus\" }   # 10-20: fable -> opus\ncodex-judge = { effort = \"high\" }\n",
			changes: []Change{unset(Section("spend", "high"), "claude-review.model")},
			want:    "[spend.high]\n# 10-20: fable -> opus\ncodex-judge = { effort = \"high\" }\n",
		},
		{
			name:    "a multi-line array is replaced in its own shape",
			src:     "[[watch]]\nowner = \"talkable\"\ninclude = [\n  \"web\",\n  \"api\",\n]   # the repos\n",
			changes: []Change{set(Table{Path: []string{"watch"}, Index: 0}, "include", []string{"web", "docs", "api"})},
			want:    "[[watch]]\nowner = \"talkable\"\ninclude = [\n  \"web\",\n  \"docs\",\n  \"api\",\n]   # the repos\n",
		},
		{
			name:    "a list in a [[watch]] element is replaced on its line",
			src:     "[[watch]]\nowner = \"talkable\"\n\n[[watch]]\nowner = \"example\"\nauto_approve_authors = [\"alice\"]   # 10-20\n",
			changes: []Change{set(Table{Path: []string{"watch"}, Index: 1}, "auto_approve_authors", []string{"alice", "rev-ann"})},
			want:    "[[watch]]\nowner = \"talkable\"\n\n[[watch]]\nowner = \"example\"\nauto_approve_authors = [\"alice\", \"rev-ann\"] # 10-20\n",
		},
		{
			name:    "CRLF line endings are kept, inserted lines take them",
			src:     "[pipeline]\r\nspend = \"high\"   # set\r\n\r\n[usage]\r\ncodex_soft = 90\r\n",
			changes: []Change{set(pipeline, "spend", "low"), set(Section("usage"), "codex_hard", int64(98))},
			want:    "[pipeline]\r\nspend = \"low\"    # set\r\n\r\n[usage]\r\ncodex_soft = 90\r\ncodex_hard = 98\r\n",
		},
		{
			name:    "a file without a final line break gets the new key on a line of its own",
			src:     "[usage]\ncodex_soft = 90",
			changes: []Change{set(Section("usage"), "codex_hard", int64(98))},
			want:    "[usage]\ncodex_soft = 90\ncodex_hard = 98",
		},
		{
			name:    "a literal string holding # is a value, not a comment",
			src:     "[[repo]]\nrepo = \"talkable/api\"\nstakes = 'the #1 \"public\" API {v2}' # 10-15\n",
			changes: []Change{set(Table{Path: []string{"repo"}, Index: 0}, "stakes", "payments")},
			want:    "[[repo]]\nrepo = \"talkable/api\"\nstakes = \"payments\"                 # 10-15\n",
		},
		{
			name:    "quoted keys are found and written quoted",
			src:     "[board]\nbadges = { \"ops team\" = \"red\" }\n",
			changes: []Change{set(Section("board"), "badges.ops team", "blue"), set(Section("board"), "badges.docs.x", "D")},
			want:    "[board]\nbadges = { \"ops team\" = \"blue\", docs = { x = \"D\" } }\n",
		},
		{
			name: "two edits of one line keep its comment at the column it had",
			src:  "[spend.high]\nclaude-review = { model = \"opus\" }   # 10-20\n",
			changes: []Change{set(Section("spend", "high"), "claude-review.effort", "high"),
				unset(Section("spend", "high"), "claude-review.model")},
			want: "[spend.high]\nclaude-review = { effort = \"high\" }  # 10-20\n",
		},
		{
			name:    "a tab before the comment is kept as it is",
			src:     "[pipeline]\nspend = \"high\"\t# the level\n",
			changes: []Change{set(Section("pipeline"), "spend", "medium")},
			want:    "[pipeline]\nspend = \"medium\"\t# the level\n",
		},
		{
			name:    "a key beside dotted keys in an inline table stays dotted",
			src:     "[spend]\nhigh = { claude-review.model = \"opus\" }\n",
			changes: []Change{set(Section("spend"), "high.claude-review.effort", "high")},
			want:    "[spend]\nhigh = { claude-review.model = \"opus\", claude-review.effort = \"high\" }\n",
		},
		{
			name:    "a key beside dotted keys in a table stays dotted",
			src:     "[spend.high]\nclaude-review.model = \"opus\"\n",
			changes: []Change{set(Section("spend", "high"), "claude-review.effort", "high")},
			want:    "[spend.high]\nclaude-review.model = \"opus\"\nclaude-review.effort = \"high\"\n",
		},
		{
			name:    "removing a file's only key leaves it empty",
			src:     "x = 1\n",
			changes: []Change{unset(Section(), "x")},
			want:    "",
		},
		{
			name:    "removing a CRLF file's only key leaves it empty",
			src:     "x = 1\r\n",
			changes: []Change{unset(Section(), "x")},
			want:    "",
		},
		{
			name:    "a key removed and then set again holds the value set",
			src:     "[a]\nx = 1   # kept\ny = 2\n",
			changes: []Change{unset(Section("a"), "x"), set(Section("a"), "x", int64(3))},
			want:    "[a]\n# kept\ny = 2\nx = 3\n",
		},
		{
			name:    "what trails a value without a comment stays",
			src:     "[pipeline]\nfix_cycles = 3   \nspend = \"high\"\n",
			changes: []Change{set(Section("pipeline"), "fix_cycles", int64(5))},
			want:    "[pipeline]\nfix_cycles = 5   \nspend = \"high\"\n",
		},
		{
			name:    "a file's opening comment stays at the top when a table goes before the first array",
			src:     "# my config\n# mine\n[[watch]]\nowner = \"talkable\"\n",
			changes: []Change{set(Section("pipeline"), "spend", "high")},
			want:    "# my config\n# mine\n\n[pipeline]\nspend = \"high\"\n\n[[watch]]\nowner = \"talkable\"\n",
		},
		{
			name:    "removing a key the file does not set changes nothing",
			src:     "[pipeline]\nspend = \"high\"\n",
			changes: []Change{unset(pipeline, "fix_cycles")},
			want:    "[pipeline]\nspend = \"high\"\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Parse([]byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Apply(sweepNow, c.changes...)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
			if string(d.Bytes()) != c.src {
				t.Error("Apply changed the Doc")
			}
		})
	}
}

// TestApplyRefusesWhatItCannotPlaceUnambiguously pins the refusals: each
// fails with ErrUnsupported and returns nothing.
func TestApplyRefusesWhatItCannotPlaceUnambiguously(t *testing.T) {
	watch0 := Table{Path: []string{"watch"}, Index: 0}
	cases := []struct {
		name, src string
		change    Change
	}{
		{"a multi-line array with comments inside", "[[watch]]\ntrackers = [\n  \"A-\",  # main\n  \"B-\",\n]\n",
			set(watch0, "trackers", []string{"A-"})},
		{"removing a multi-line array with comments inside", "[[watch]]\ntrackers = [\n  \"A-\",  # main\n]\n",
			unset(watch0, "trackers")},
		{"a multi-line array whose elements share lines", "[[watch]]\ninclude = [\"web\",\n  \"api\"]\n",
			set(watch0, "include", []string{"web"})},
		{"a multi-line string", "[[watch]]\nstakes = '''\nlong\n'''\n", set(watch0, "stakes", "short")},
		{"a key of an inline table over several lines", "[spend.high]\nclaude-review = {\n  model = \"opus\",\n}\n",
			set(Section("spend", "high"), "claude-review.model", "sonnet")},
		{"a table only dotted keys define", "[spend]\nburn.claude-review = { model = \"opus\" }\n",
			set(Section("spend", "burn"), "codex-judge.model", "x")},
		{"a table only a subtable defines", "[spend.burn.claude-review]\nmodel = \"opus\"\n",
			set(Section("spend", "burn"), "codex-judge.model", "x")},
		{"a new subtable of an array element", "[[pool]]\nrepo = \"talkable/web\"\n",
			set(Table{Path: []string{"pool", "env"}, Index: 0}, "A", "1")},
		{"a key through a value", "[pipeline]\nspend = \"high\"\n", set(Section("pipeline"), "spend.level", "x")},
		{"a key at the top of a file without top-level keys", "[pipeline]\nspend = \"high\"\n", set(Section(), "x", "y")},
		{"removing an array whose nested array holds a comment", "[[watch]]\nx = [ [1, # one\n 2], 3 ]\n", unset(watch0, "x")},
		{"replacing an array whose nested array holds a comment", "[[watch]]\nx = [\n  [1, # one\n  2],\n  3,\n]\n",
			set(watch0, "x", []any{int64(3)})},
		{"removing an inline table whose array holds a comment", "[[watch]]\na = { b = [1, # one\n 2] }\n", unset(watch0, "a")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Parse([]byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Apply(sweepNow, c.change)
			if !errors.Is(err, ErrUnsupported) || got != nil {
				t.Errorf("Apply = %q, %v; want ErrUnsupported", got, err)
			}
		})
	}
}

func TestApplyRefusesAnElementThatDoesNotFollowTheLast(t *testing.T) {
	d, err := Parse([]byte("[[repo]]\nrepo = \"talkable/web\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Apply(sweepNow, set(Table{Path: []string{"repo"}, Index: 2}, "repo", "x")); err == nil {
		t.Error("an element two past the last was created")
	}
	if _, err := d.Apply(sweepNow, noted(set(Section("pipeline"), "spend", "low"), "two\nlines")); err == nil {
		t.Error("a note over two lines was written")
	}
}

func TestGetReadsTheValueItsCommentAndLine(t *testing.T) {
	d, err := Parse(readFixture(t, "operator.toml"))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := d.Get(Key{Table: Section("daemon"), Path: []string{"push_quiet_period"}})
	if !ok || v.Raw != `"15m"` || v.Data != "15m" || v.Line != 9 || v.Multiline ||
		v.Comment != "# 2026-10-06: a replay of three days of pushes:\n                                  # fewer rounds for ten more minutes" {
		t.Errorf("push_quiet_period = %+v, %v", v, ok)
	}
	v, ok = d.Get(Key{Table: Section("spend", "high"), Path: []string{"claude-review", "model"}})
	if !ok || v.Raw != `"opus"` || v.Comment != "# 2026-10-20: fable -> opus" {
		t.Errorf("[spend.high] claude-review.model = %+v, %v", v, ok)
	}
	w, ok := d.Watch(0)
	if !ok {
		t.Fatal("no first watch")
	}
	if v, ok := d.Get(Key{Table: w, Path: []string{"include"}}); !ok || !v.Multiline {
		t.Errorf("watch 0 include = %+v, %v", v, ok)
	}
	if _, ok := d.Watch(2); ok {
		t.Error("a third watch")
	}
	r, ok := d.Repo("TALKABLE/api")
	if !ok || r.Index != 1 {
		t.Errorf("Repo(talkable/api) = %v, %v", r, ok)
	}
	if v, ok := d.Get(Key{Table: r, Path: []string{"stakes"}}); !ok || v.Data != `the "public" API {v2}` {
		t.Errorf("repo stakes = %+v", v)
	}
	if ro, ok := d.Role("claude-simplify"); !ok || ro.Index != 1 {
		t.Errorf("Role(claude-simplify) = %v, %v", ro, ok)
	}
	if _, ok := d.Get(Key{Table: Section("pipeline"), Path: []string{"autofix"}}); ok {
		t.Error("autofix is not in the file")
	}
	pool1 := Table{Path: []string{"pool"}, Index: 1}
	if v, ok := d.Get(Key{Table: pool1, Path: []string{"env", "NODE_ENV"}}); !ok || v.Data != "test" {
		t.Errorf("the second pool's env = %+v, %v", v, ok)
	}
	if _, ok := d.Get(Key{Table: pool1, Path: []string{"env", "RAILS_ENV"}}); ok {
		t.Error("the first pool's env key read as the second's")
	}
}

func TestVerifyRefusesAnyOtherDifference(t *testing.T) {
	old := []byte("[pipeline]\nspend = \"high\"\nfix_cycles = 3\n")
	spend := set(Section("pipeline"), "spend", "low")
	for name, c := range map[string]struct {
		new     string
		changes []Change
	}{
		"another key changed":          {"[pipeline]\nspend = \"low\"\nfix_cycles = 4\n", []Change{spend}},
		"a key lost":                   {"[pipeline]\nspend = \"low\"\n", []Change{spend}},
		"the value is not the one set": {"[pipeline]\nspend = \"medium\"\nfix_cycles = 3\n", []Change{spend}},
		"a removed key still set":      {string(old), []Change{unset(Section("pipeline"), "spend")}},
		"an empty table added":         {"[pipeline]\nspend = \"low\"\nfix_cycles = 3\n[usage]\n", []Change{spend}},
	} {
		if err := Verify(old, []byte(c.new), c.changes); err == nil {
			t.Errorf("%s: Verify passed", name)
		}
	}
	if err := Verify(old, []byte("[pipeline]\nspend = \"low\" # x\nfix_cycles = 3\n"), []Change{spend}); err != nil {
		t.Errorf("Verify of the one change: %v", err)
	}
}

func TestLiteralWritesOneLineValues(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{"a \"q\" \\ b", `"a \"q\" \\ b"`},
		{"two\nlines", `"two\nlines"`},
		{true, "true"},
		{int64(95), "95"},
		{80.5, "80.5"},
		{[]string{"alice", "rev-ann"}, `["alice", "rev-ann"]`},
		{[]string{}, "[]"},
		{[]string(nil), "[]"},
		{map[string]any{"effort": "high", "model": "opus", "zz": int64(1)}, `{ model = "opus", effort = "high", zz = 1 }`},
		{map[string]any{"a b": "x"}, `{ "a b" = "x" }`},
	} {
		got, err := Literal(c.v)
		if err != nil || got != c.want {
			t.Errorf("Literal(%#v) = %s, %v; want %s", c.v, got, err, c.want)
		}
	}
	for _, v := range []any{nil, struct{}{}, []any{map[string]any{}}} {
		if _, err := Literal(v); err == nil {
			t.Errorf("Literal(%#v) passed", v)
		}
	}
}
