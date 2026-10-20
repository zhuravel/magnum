// Package confedit edits a TOML config file in place: a value replaced, a
// key or a table added, a key removed, and every other byte (comments,
// alignment, blank lines, line endings) kept as it was. magnum's config
// files carry a dated comment on most lines that explains a choice, which a
// TOML encoder would drop, so this package never re-encodes a file: a small
// scanner (scan.go) records where each key, value and trailing comment is,
// Parse cross-checks it against BurntSushi's own reading of the file
// (MetaData.Keys, in document order), and Apply splices only the bytes an
// edit needs, then Verify decodes the old and the new file and requires them
// to differ in exactly the edited leaves. Whatever it does not model fails
// with ErrUnsupported: the file is then edited by hand.
//
// The rules (DECISIONS "The settings screen edits config.toml in place"):
//   - A replaced value keeps its trailing comment at its column (the gap
//     before it takes up a length change, at least one space) and the line
//     ending. A value over several lines is replaced only as a whole array
//     without comments inside, one element per line, and keeps that shape.
//   - A missing key goes after the last key of its table (and the indented
//     comment lines that continue that key's comment), so before any
//     subtable of it: inserting into a [[pool]] element puts the key above
//     its [pool.env]. A key of an inline table goes inside its braces.
//   - A missing table goes after the last table of its family (the same
//     first name: the last [spend.*], the last [[repo]] element with its
//     subtables), else before the first array of tables ([[watch]],
//     [[repo]], ...) so a plain table stays above them, else at the end of
//     the file; in each case before the comment lines that introduce the
//     header after it, with a blank line on each side.
//   - A removed key (back to its default) loses its line or lines; its
//     trailing comment stays as a comment line of its own, with the lines
//     that continue it.
//   - A note (Change.Note) becomes the trailing comment "# YYYY-MM-DD: note"
//     on a line without one, and goes first on a line with one: "#
//     YYYY-MM-DD: note; <the old comment>".
//   - A key only dotted keys or subtables define (an implicit table), a
//     table inside a nested array of tables, a multi-line inline table and
//     anything else ambiguous fail with ErrUnsupported.
package confedit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrUnsupported is the error of a file, or an edit, the editor does not
// model: nothing is written and the file is edited by hand.
var ErrUnsupported = errors.New("confedit: construct not supported; edit the file by hand")

// ErrEditor is the error of an edit whose result fails the editor's own
// checks: it does not read back, it changes a line outside the edited key's,
// or it decodes to another change than the one asked for (Verify). It is a
// bug in the editor, never a shape it refuses; nothing is returned.
var ErrEditor = errors.New("confedit: the edit failed the editor's own check (a bug in the editor); nothing was written")

// Doc is a scanned file: every table header and key/value with its byte
// spans, checked against BurntSushi's MetaData.Keys() order.
type Doc struct {
	src     []byte
	sum     [32]byte
	tables  []table
	entries []entry
	data    map[string]any // the file as BurntSushi decodes it
	nl      string         // the line break inserted lines take: the file's first one
	// cols are the comment columns of the lines of the file Apply started
	// from, by entry (entryID): a line two changes of one Apply rewrite keeps
	// its comment at the column it had, not at the one the first moved it to.
	cols map[string]int
}

// Parse scans src and cross-checks it with toml.Decode: a file BurntSushi
// refuses is that error, and a file whose keys the scanner reads in another
// order or number than MetaData.Keys() (or with a construct it does not
// model) is ErrUnsupported.
func Parse(src []byte) (*Doc, error) {
	data := map[string]any{}
	md, err := toml.Decode(string(src), &data)
	if err != nil {
		return nil, fmt.Errorf("confedit: %w", err)
	}
	s, err := scan(src)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if err := crossCheck(s.keys, md.Keys()); err != nil {
		return nil, err
	}
	nl := "\n"
	if i := bytes.IndexByte(src, '\n'); i > 0 && src[i-1] == '\r' {
		nl = "\r\n"
	}
	return &Doc{src: slices.Clone(src), sum: sha256.Sum256(src), tables: s.tables, entries: s.entries, data: data, nl: nl}, nil
}

// crossCheck compares the keys and headers the scanner read with the ones
// BurntSushi's MetaData.Keys() lists, in document order: any difference in
// number, order or name is ErrUnsupported.
func crossCheck(scanned [][]string, keys []toml.Key) error {
	if len(keys) != len(scanned) {
		return fmt.Errorf("%w: the scanner read %d keys and headers, the TOML decoder %d", ErrUnsupported, len(scanned), len(keys))
	}
	for i, k := range keys {
		if !slices.Equal([]string(k), scanned[i]) {
			return fmt.Errorf("%w: key %d: the scanner read %s, the TOML decoder %s", ErrUnsupported, i+1,
				strings.Join(scanned[i], "."), k.String())
		}
	}
	return nil
}

// Bytes is the file's text.
func (d *Doc) Bytes() []byte { return slices.Clone(d.src) }

// Sum is the SHA-256 of the file's text.
func (d *Doc) Sum() [32]byte { return d.sum }

// Table names a table: a [section], an element of an [[array]], or a
// subtable of one.
type Table struct {
	Path []string // {"pipeline"}, {"spend", "burn"}, {"kinds", "codex"}, {"repo"}; nil = the keys above the first header
	// Index is the element of the array of tables in Path ({"watch"} 1 =
	// the second [[watch]]; {"pool", "env"} 1 = the [pool.env] of the
	// second [[pool]]); -1 for a plain table.
	Index int
}

// Section is the plain table at path.
func Section(path ...string) Table { return Table{Path: path, Index: -1} }

func (t Table) String() string {
	name := strings.Join(t.Path, ".")
	switch {
	case len(t.Path) == 0:
		return "the top of the file"
	case t.Index < 0:
		return "[" + name + "]"
	}
	return fmt.Sprintf("[[%s]] #%d", name, t.Index+1)
}

// Elements is how many [[path]] elements the file has (an array of tables
// at the top level).
func (d *Doc) Elements(path ...string) int {
	n := 0
	for _, t := range d.tables {
		if t.array && slices.Equal(t.path, path) && len(t.chain) == 1 {
			n++
		}
	}
	return n
}

// Watch is the i-th [[watch]] (0-based).
func (d *Doc) Watch(i int) (Table, bool) {
	if i < 0 || i >= d.Elements("watch") {
		return Table{}, false
	}
	return Table{Path: []string{"watch"}, Index: i}, true
}

// Repo is the [[repo]] whose repo is name, compared as validateRepos does
// (any case); config refuses a repository configured twice, so it is
// unique.
func (d *Doc) Repo(name string) (Table, bool) {
	return d.elementWith("repo", "repo", func(v string) bool { return strings.EqualFold(v, name) })
}

// Role is the [[role]] whose name is name.
func (d *Doc) Role(name string) (Table, bool) {
	return d.elementWith("role", "name", func(v string) bool { return v == name })
}

func (d *Doc) elementWith(array, key string, match func(string) bool) (Table, bool) {
	for i, el := range tablesOf(d.data[array]) {
		if v, ok := el[key].(string); ok && match(v) {
			return Table{Path: []string{array}, Index: i}, true
		}
	}
	return Table{}, false
}

// tablesOf is a decoded array of tables (nil for anything else).
func tablesOf(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, el := range x {
			m, ok := el.(map[string]any)
			if !ok {
				return nil
			}
			out = append(out, m)
		}
		return out
	}
	return nil
}

// HeaderLine is the 1-based line of t's header; false when the file has no
// header of t (0 and true for the top of the file).
func (d *Doc) HeaderLine(t Table) (int, bool) {
	ti, ok := d.header(t.Path, t)
	if !ok {
		return 0, false
	}
	return d.tables[ti].line, true
}

// TableAt is the table a 1-based line lies in: the one whose header is the
// last at or above it (the top of the file above the first header).
func (d *Doc) TableAt(line int) Table {
	at := 0
	for i, t := range d.tables {
		if i > 0 && t.line <= line {
			at = i
		}
	}
	t := d.tables[at]
	idx, _ := t.index()
	return Table{Path: slices.Clone(t.path), Index: idx}
}

// Key is a key inside a table; Path has more than one element for a key in
// an inline table ({"claude-review", "model"} in [spend.burn]) or a dotted
// key.
type Key struct {
	Table Table
	Path  []string
}

func (k Key) String() string {
	return k.Table.String() + " " + strings.Join(k.Path, ".")
}

func (k Key) full() []string { return append(slices.Clone(k.Table.Path), k.Path...) }

// Value is what the file says for a key.
type Value struct {
	Raw string // the value's text as written
	// Comment is the trailing comment of the key's line (for a key inside an
	// inline table, of the line holding it) with its continuation lines; ""
	// if none.
	Comment   string
	Line      int // 1-based line of the key
	Multiline bool
	// Data is the value as BurntSushi decodes it: string, int64, float64,
	// bool, a date, []any or map[string]any.
	Data any
}

// Get reads k; false when the file does not set it (the default applies).
func (d *Doc) Get(k Key) (Value, bool) {
	i, ok := d.find(k.full(), k.Table)
	if !ok {
		return Value{}, false
	}
	e := d.entries[i]
	top := d.entries[d.top(i)]
	v := Value{Raw: string(d.src[e.valStart:e.valEnd]), Line: e.line, Multiline: e.multiline}
	if top.commentStart >= 0 {
		v.Comment = strings.TrimRight(string(d.src[top.commentStart:top.contEnd]), "\r\n")
	}
	v.Data, _ = d.decoded(k)
	return v, true
}

// decoded walks the decoded file to k.
func (d *Doc) decoded(k Key) (any, bool) {
	p, err := changePath(k.Table, k.Path, d.data)
	if err != nil {
		return nil, false
	}
	return lookup(d.data, p)
}

// top is the top-level entry that holds entry i (i itself when it is one).
func (d *Doc) top(i int) int {
	for d.entries[i].parent >= 0 {
		i = d.entries[i].parent
	}
	return i
}

// tableMatches reports whether table t is (or lies in) the element t names:
// the same element of the same array of tables, or a plain table.
func (d *Doc) tableMatches(ti int, want Table) bool {
	t := d.tables[ti]
	idx, nested := t.index()
	if nested || idx != want.Index {
		return false
	}
	return idx < 0 || t.chain[0].depth <= len(want.Path)
}

// find is the entry whose full path is full, in a table matching want.
func (d *Doc) find(full []string, want Table) (int, bool) {
	for i, e := range d.entries {
		t := d.tables[e.table]
		if len(t.path)+len(e.path) != len(full) || !d.tableMatches(e.table, want) {
			continue
		}
		if slices.Equal(t.path, full[:len(t.path)]) && slices.Equal(e.path, full[len(t.path):]) {
			return i, true
		}
	}
	return 0, false
}

// header is the table whose header path is path, matching want.
func (d *Doc) header(path []string, want Table) (int, bool) {
	if len(path) == 0 {
		return 0, true // the root
	}
	for i, t := range d.tables {
		if i > 0 && slices.Equal(t.path, path) && d.tableMatches(i, want) {
			return i, true
		}
	}
	return 0, false
}

// defines reports whether the file defines anything under want's path
// without a header of it: a subtable header or a dotted key.
func (d *Doc) defines(want Table) bool {
	under := func(p []string) bool { return len(p) > len(want.Path) && slices.Equal(p[:len(want.Path)], want.Path) }
	for i, t := range d.tables {
		if i > 0 && under(t.path) && d.tableMatches(i, want) {
			return true
		}
	}
	for _, e := range d.entries {
		if under(append(slices.Clone(d.tables[e.table].path), e.path...)) && d.tableMatches(e.table, want) {
			return true
		}
	}
	return false
}
