package settings

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/confedit"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
)

// DraftValue is one staged edit: Text for a scalar, List for a list field,
// Default to remove the key (back to the default; its comment stays as a
// comment line).
type DraftValue struct {
	Text    string
	List    []string
	Default bool
}

// Draft is the edits the screen staged, by target.
type Draft map[Target]DraftValue

// Hunk is one place a save changes: the table it is in ("[pipeline]",
// "[spend.burn]", "[[repo]] talkable/web", "[[watch]] 2 (talkable)"), its
// line in the old file (where the added lines go for an insertion), and the
// lines removed and added.
type Hunk struct {
	Table          string
	Line           int
	Removed, Added []string
}

// Plan is a save checked and ready to write: the old and new bytes, the
// hash of the file it was built on (Commit refuses another), the diff, the
// loader's warnings about the new file, how many settings it changes and
// the key paths it changes (for logs: never values).
type Plan struct {
	Path     string
	Old, New []byte
	Sum      [32]byte // of Old: the file the plan was built on
	NewSum   [32]byte
	Diff     string // a unified diff (no context lines), each hunk headed by its table
	Hunks    []Hunk
	Warnings []string
	Count    int
	Keys     []string

	history string // the config-history directory
}

// historyDir is where saves keep the files they replace.
func (s *Snapshot) historyDir() string { return filepath.Join(s.layout.Data(), "config-history") }

// edit is a draft entry that changes the file.
type edit struct {
	target  Target
	field   Field
	key     confedit.Key
	value   any // nil: remove
	old     confedit.Value
	had     bool
	creates *confedit.Change
}

// edits turns a draft into the edits that change the file, in screen
// order: each value checked (Check), typed, and dropped when the file
// already says it.
func (s *Snapshot) edits(d Draft) ([]edit, error) {
	targets := make([]Target, 0, len(d))
	for t := range d {
		targets = append(targets, t)
	}
	slices.SortFunc(targets, func(a, b Target) int {
		return cmp.Or(cmp.Compare(s.ordinal[a.Field], s.ordinal[b.Field]), cmp.Compare(a.Watch, b.Watch),
			cmp.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo)))
	})
	var out []edit
	for _, t := range targets {
		f, ok := s.byID[t.Field]
		if !ok {
			return nil, fmt.Errorf("settings: no setting %s", t.Field)
		}
		dv := d[t]
		if err := s.Check(t, dv); err != nil {
			return nil, fmt.Errorf("%s: %w", s.KeyText(t), err)
		}
		k, create, err := s.confKey(f, t)
		if err != nil {
			return nil, err
		}
		e := edit{target: t, field: f, key: k, creates: create}
		if create == nil {
			e.old, e.had = s.Doc.Get(k)
		}
		if !dv.Default {
			if e.value, err = typed(f, dv); err != nil {
				return nil, fmt.Errorf("%s: %w", s.KeyText(t), err)
			}
		}
		if e.value == nil && !e.had {
			continue // back to a default the file already leaves
		}
		if e.value != nil && e.had && sameValue(e.value, e.old.Data) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// typed is a draft value as the TOML value it is written as.
func typed(f Field, dv DraftValue) (any, error) {
	if f.Type.IsList() {
		return draftList(dv), nil
	}
	text := strings.TrimSpace(dv.Text)
	switch f.Type {
	case TypeBool:
		return text == "true", nil
	case TypeInt:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number", text)
		}
		return n, nil
	case TypePercent:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", text)
		}
		if v == math.Trunc(v) {
			return int64(v), nil
		}
		return v, nil
	case TypeText:
		return dv.Text, nil
	}
	return text, nil
}

// sameValue reports whether v, written, decodes to what the file has.
func sameValue(v, data any) bool {
	lit, err := confedit.Literal(v)
	if err != nil {
		return false
	}
	var m map[string]any
	if _, err := toml.Decode("v = "+lit, &m); err != nil {
		return false
	}
	return reflect.DeepEqual(m["v"], data)
}

// changes are the confedit changes of edits: a [[role]] or [[repo]] block
// a first edit creates (one per role or repository), then each key, with
// its dated note when notes is on.
func (s *Snapshot) changes(edits []edit, notes bool) []confedit.Change {
	var out []confedit.Change
	created := map[string]confedit.Table{}
	next := map[string]int{}
	gridNote, groups := gridNotes(edits), map[string][]int{}
	for i, e := range edits {
		if e.field.Scope == ScopeSpendLevel {
			groups[gridID(e)] = append(groups[gridID(e)], i)
		}
	}
	done := map[string]bool{}
	for _, e := range edits {
		if e.field.Scope == ScopeSpendLevel {
			id := gridID(e)
			if done[id] {
				continue
			}
			done[id] = true
			for _, c := range s.gridChanges(edits, groups[id]) {
				if notes && c.Note == "-" {
					c.Note = gridNote[id]
				}
				if c.Note == "-" {
					c.Note = ""
				}
				out = append(out, c)
			}
			continue
		}
		k := e.key
		if e.creates != nil {
			array := e.creates.Key.Table.Path[0]
			id := array + "\x00" + strings.ToLower(fmt.Sprint(e.creates.Value))
			tbl, ok := created[id]
			if !ok {
				tbl = confedit.Table{Path: []string{array}, Index: s.Doc.Elements(array) + next[array]}
				next[array]++
				created[id] = tbl
				out = append(out, confedit.Change{Key: confedit.Key{Table: tbl, Path: e.creates.Key.Path}, Value: e.creates.Value})
			}
			k.Table = tbl
		}
		c := confedit.Change{Key: k, Value: e.value}
		if notes {
			c.Note = noteFor(e)
		}
		out = append(out, c)
	}
	return out
}

func gridID(e edit) string { return e.field.Level + "\x00" + e.field.Role }

// gridChanges are the changes of one role's cells in one level (members,
// indexes into edits), one per cell, each value edited in place inside the
// role's braces (a missing key added before the closing brace; confedit
// keeps the line's comment at the column it had): the cells that are set
// before the ones removed, so a removal never takes the line a set still
// needs, and the last carries the group's note (Note "-").
func (s *Snapshot) gridChanges(edits []edit, members []int) []confedit.Change {
	ordered := slices.Clone(members)
	slices.SortStableFunc(ordered, func(a, b int) int {
		return cmp.Compare(boolInt(edits[a].value == nil), boolInt(edits[b].value == nil))
	})
	out := make([]confedit.Change, 0, len(ordered))
	for n, j := range ordered {
		c := confedit.Change{Key: edits[j].key, Value: edits[j].value}
		if n == len(ordered)-1 {
			c.Note = "-"
		}
		out = append(out, c)
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// noteFor is a non-grid edit's dated note: "was <old>", "was the default",
// or "<key> back to the default, was <old>" (the comment line a removed key
// leaves).
func noteFor(e edit) string {
	switch {
	case e.value == nil:
		return strings.Join(e.key.Path, ".") + " back to the default, was " + oldText(e.old.Raw)
	case e.had:
		return "was " + oldText(e.old.Raw)
	}
	return "was the default"
}

// gridNotes are the notes of a level table's edits, by role and level
// (gridID): the cells of one role share a line, so one note names each
// ("was model \"sonnet\", effort unset").
func gridNotes(edits []edit) map[string]string {
	out := map[string]string{}
	last := map[string]int{}
	parts, cells, olds := map[string][]string{}, map[string][]string{}, map[string][]string{}
	removals := map[string]bool{}
	for i, e := range edits {
		if e.field.Scope != ScopeSpendLevel {
			continue
		}
		id := gridID(e)
		if _, ok := last[id]; !ok {
			removals[id] = true
		}
		last[id] = i
		old := "unset"
		if e.had {
			old = oldText(e.old.Raw)
		}
		parts[id] = append(parts[id], e.field.Cell+" "+old)
		cells[id] = append(cells[id], e.field.Cell)
		olds[id] = append(olds[id], old)
		removals[id] = removals[id] && e.value == nil
	}
	for id, i := range last {
		if removals[id] {
			out[id] = edits[i].field.Role + " " + strings.Join(cells[id], " and ") + " back to the level's default, was " +
				strings.Join(olds[id], ", ")
		} else {
			out[id] = "was " + strings.Join(parts[id], ", ")
		}
	}
	return out
}

// oldText is a value as written, on one line and at most 80 characters.
func oldText(raw string) string {
	s := strings.Join(strings.Fields(raw), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// candidate applies a draft to the file as loaded.
func (s *Snapshot) candidate(d Draft, notes bool, now time.Time) ([]byte, []edit, error) {
	edits, err := s.edits(d)
	if err != nil {
		return nil, nil, err
	}
	if len(edits) == 0 {
		return s.Doc.Bytes(), nil, nil
	}
	out, err := s.Doc.Apply(now, s.changes(edits, notes)...)
	if err != nil {
		return nil, nil, fmt.Errorf("settings: %w", err)
	}
	return out, edits, nil
}

// Preview is the roles after a restart on the draft, resolved by magnum's
// own loader from the edited file: a draft that does not load is its
// error.
func (s *Snapshot) Preview(d Draft) ([]RoleRow, error) {
	out, _, err := s.candidate(d, false, time.Now())
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(s.layout, s.base, out)
	if err != nil {
		return nil, err
	}
	return roleRows(cfg), nil
}

// Saved is the roles as the file on disk resolves them.
func (s *Snapshot) Saved() []RoleRow { return roleRows(s.Config) }

func roleRows(cfg *config.Config) []RoleRow {
	var out []RoleRow
	for _, r := range cfg.AllRoles() {
		kind := r.AgentKind()
		k, _ := cfg.KindSpec(kind)
		out = append(out, RoleRow{Role: r.Name, Kind: kind, Model: cmp.Or(r.Model, k.DefaultModel), Effort: r.Effort,
			RereviewEffort: r.RereviewEffort, Spend: cfg.SpendLevel(kind)})
	}
	return out
}

// Plan builds the save of d: the file re-read and refused when it changed
// since Load (ErrChanged), the edits applied in place (with "was <old>"
// notes when notes is on) and verified, the new file checked as `magnum
// config` checks one (the loader, then every prompt rendered with this
// build), and the diff. Nothing is written.
func (s *Snapshot) Plan(d Draft, notes bool, now time.Time) (*Plan, error) {
	if err := checkWritable(s.Path); err != nil {
		return nil, err
	}
	cur, _, err := readConfig(s.Path)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(cur) != s.Sum {
		return nil, ErrChanged
	}
	out, edits, err := s.candidate(d, notes, now)
	if err != nil {
		return nil, err
	}
	p, err := s.plan(out)
	if err != nil {
		return nil, err
	}
	p.Count = len(edits)
	p.Warnings = append(p.Warnings, s.unusual(edits)...)
	return p, nil
}

// plan checks new bytes and describes the change from the loaded file.
func (s *Snapshot) plan(out []byte) (*Plan, error) {
	cfg, err := loadConfig(s.layout, s.base, out)
	if err != nil {
		return nil, fmt.Errorf("settings: magnum would not load the new config: %w", err)
	}
	if _, err := engine.CheckPrompts(cfg); err != nil {
		return nil, fmt.Errorf("settings: the prompts do not render with the new config:\n%w", err)
	}
	keys, err := confedit.Changed(s.Bytes, out)
	if err != nil {
		return nil, err
	}
	p := &Plan{Path: s.Path, Old: s.Bytes, New: out, Sum: s.Sum, NewSum: sha256.Sum256(out), Warnings: cfg.Warnings(),
		Keys: keys, history: s.historyDir()}
	p.Hunks = hunks(s.Doc, out)
	p.Diff = unified(s.Path, p.Hunks)
	return p, nil
}

// unusual warns about a model or an effort the configuration names nowhere
// else: config accepts any, and a typo fails every launch of the role.
func (s *Snapshot) unusual(edits []edit) []string {
	var out []string
	for _, e := range edits {
		v, ok := e.value.(string)
		if !ok {
			continue
		}
		switch e.field.Type {
		case TypeModel:
			if !slices.Contains(s.models(), v) {
				out = append(out, fmt.Sprintf("%s: model %q is named nowhere else in the config; a typo fails every launch of %s",
					s.KeyText(e.target), v, e.field.Role))
			}
		case TypeEffort:
			if !slices.Contains(s.efforts(), v) {
				out = append(out, fmt.Sprintf("%s: effort %q is not one of %s; the agent CLI may refuse it",
					s.KeyText(e.target), v, strings.Join(Efforts, ", ")))
			}
		}
	}
	return out
}

// hunks diffs the loaded file against out line by line, each change with
// the table it lies in.
func hunks(old *confedit.Doc, out []byte) []Hunk {
	a, b := lines(old.Bytes()), lines(out)
	newDoc, _ := confedit.Parse(out)
	var hs []Hunk
	for _, op := range diffLines(a, b) {
		h := Hunk{Line: op.aStart + 1, Removed: a[op.aStart:op.aEnd], Added: b[op.bStart:op.bEnd]}
		switch {
		case len(h.Removed) > 0 || newDoc == nil:
			h.Table = tableLabel(old, old.TableAt(max(1, op.aStart+1)))
		default:
			first := 0 // a new table's block starts with a blank line: label it by its header
			for first < len(h.Added)-1 && strings.TrimSpace(h.Added[first]) == "" {
				first++
			}
			h.Table = tableLabel(newDoc, newDoc.TableAt(op.bStart+1+first))
		}
		hs = append(hs, h)
	}
	return hs
}

// lines splits a file into lines without their line breaks.
func lines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// tableLabel names a table for a hunk: "[pipeline]", "[[repo]]
// talkable/web", "[[watch]] 2 (talkable)", "[[role]] claude-review".
func tableLabel(d *confedit.Doc, t confedit.Table) string {
	if len(t.Path) == 0 {
		return "the top of the file"
	}
	name := strings.Join(t.Path, ".")
	if t.Index < 0 {
		return "[" + name + "]"
	}
	get := func(key string) string {
		v, _ := d.Get(confedit.Key{Table: confedit.Table{Path: t.Path[:1], Index: t.Index}, Path: []string{key}})
		s, _ := v.Data.(string)
		return s
	}
	label := "[[" + t.Path[0] + "]]"
	switch t.Path[0] {
	case "watch":
		label += " " + strconv.Itoa(t.Index+1)
		if o := get("owner"); o != "" {
			label += " (" + o + ")"
		}
	case "repo", "pool":
		label += " " + cmp.Or(get("repo"), strconv.Itoa(t.Index+1))
	case "role", "identity":
		label += " " + cmp.Or(get("name"), strconv.Itoa(t.Index+1))
	default:
		label += " " + strconv.Itoa(t.Index+1)
	}
	if len(t.Path) > 1 {
		label += " [" + name + "]"
	}
	return label
}

// unified is the hunks as a unified diff without context lines.
func unified(path string, hs []Hunk) string {
	if len(hs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s (new)\n", path, path)
	span := func(start, n int) string {
		if n == 1 {
			return strconv.Itoa(start)
		}
		if n == 0 {
			start--
		}
		return strconv.Itoa(start) + "," + strconv.Itoa(n)
	}
	added := 0
	for _, h := range hs {
		newStart := h.Line + added
		fmt.Fprintf(&b, "@@ -%s +%s @@ %s\n", span(h.Line, len(h.Removed)), span(newStart, len(h.Added)), h.Table)
		for _, l := range h.Removed {
			b.WriteString("-" + l + "\n")
		}
		for _, l := range h.Added {
			b.WriteString("+" + l + "\n")
		}
		added += len(h.Added) - len(h.Removed)
	}
	return b.String()
}

// lineOp is a run of lines a[aStart:aEnd] replaced by b[bStart:bEnd].
type lineOp struct{ aStart, aEnd, bStart, bEnd int }

// diffLines is a line diff: the common prefix and suffix skipped, the rest
// by a longest common subsequence (a large rest, as a restore can have, is
// one replacement).
func diffLines(a, b []string) []lineOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}
	ma, mb := a[p:len(a)-q], b[p:len(b)-q]
	if len(ma) == 0 && len(mb) == 0 {
		return nil
	}
	if len(ma)*len(mb) > 4_000_000 {
		return []lineOp{{p, p + len(ma), p, p + len(mb)}}
	}
	// lcs[i][j]: the longest common subsequence of ma[i:] and mb[j:].
	lcs := make([][]int32, len(ma)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(mb)+1)
	}
	for i, x := range slices.Backward(ma) {
		for j, y := range slices.Backward(mb) {
			if x == y {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	cur := lineOp{-1, -1, -1, -1}
	flush := func() {
		if cur.aStart >= 0 {
			ops = append(ops, cur)
			cur = lineOp{-1, -1, -1, -1}
		}
	}
	open := func() {
		if cur.aStart < 0 {
			cur = lineOp{p + i, p + i, p + j, p + j}
		}
	}
	for i < len(ma) || j < len(mb) {
		switch {
		case i < len(ma) && j < len(mb) && ma[i] == mb[j]:
			flush()
			i, j = i+1, j+1
		case j < len(mb) && (i == len(ma) || lcs[i][j+1] >= lcs[i+1][j]):
			open()
			j++
			cur.bEnd = p + j
		default:
			open()
			i++
			cur.aEnd = p + i
		}
	}
	flush()
	return ops
}
