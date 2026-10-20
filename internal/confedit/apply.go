package confedit

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Change sets a key (Value non-nil) or removes it (nil: back to the
// default).
type Change struct {
	Key   Key
	Value any    // string, bool, int, int64, float64, []string, []any of scalars, map[string]any (an inline table)
	Note  string // non-empty: a dated note on the key's line (see the package comment)
}

// Apply returns the new file text; d is not modified. The changes apply in
// order, each to the text the one before it left (so a [[repo]] element one
// change creates takes the next change's key), and the result passes
// Verify against d. It fails when a change cannot be placed (an implicit
// table, a shape the editor refuses: ErrUnsupported) or names an element
// that does not exist; nothing is returned then. Removing a key the file
// does not set changes nothing.
func (d *Doc) Apply(now time.Time, changes ...Change) ([]byte, error) {
	cur := d
	cols := d.commentCols()
	for _, c := range changes {
		if len(c.Key.Path) == 0 || slices.Contains(c.Key.Path, "") || slices.Contains(c.Key.Table.Path, "") {
			return nil, fmt.Errorf("confedit: %s: an empty key", c.Key)
		}
		if strings.ContainsFunc(c.Note, unicode.IsControl) || !utf8.ValidString(c.Note) {
			return nil, fmt.Errorf("confedit: %s: a note must be one line of text", c.Key)
		}
		for _, part := range append(slices.Clone(c.Key.Table.Path), c.Key.Path...) {
			if !utf8.ValidString(part) || strings.ContainsFunc(part, unicode.IsControl) {
				return nil, fmt.Errorf("confedit: %s: a key must be text without control characters", c.Key)
			}
		}
		cur.cols = cols
		ed, err := cur.applyOne(now, c)
		if err != nil {
			return nil, fmt.Errorf("confedit: %s: %w", c.Key, err)
		}
		if !confined(cur.src, ed.out, ed.lo, ed.hi) {
			return nil, fmt.Errorf("%w: %s: the edit changed a line outside lines %d-%d", ErrEditor, c.Key, ed.lo, ed.hi-1)
		}
		next, err := Parse(ed.out)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: the edited file does not read back: %w", ErrEditor, c.Key, err)
		}
		cur = next
	}
	if err := Verify(d.src, cur.src, changes); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEditor, err)
	}
	return cur.Bytes(), nil
}

// edited is one change's new text and the old lines it may change: [lo,
// hi), 1-based; lo == hi for an insertion between lines.
type edited struct {
	out    []byte
	lo, hi int
}

// confined reports whether after differs from before only in before's lines
// [lo, hi) (1-based) and in lines added among them: every other line, its
// line ending included, is byte-identical and in place. It is the editor's
// text check, independent of Verify's decoding: a comment, a blank line or
// a space outside the edited key's lines never changes.
func confined(before, after []byte, lo, hi int) bool {
	a, b := splitLines(before), splitLines(after)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	q := 0
	for q < len(a)-p && q < len(b)-p && a[len(a)-1-q] == b[len(b)-1-q] {
		q++
	}
	first, last := p+1, len(a)-q // the changed old lines, 1-based, inclusive
	if first > last {
		return true // every old line in place: lines were only added (where, Verify and the tests check)
	}
	return lo <= first && last < hi
}

// splitLines splits text after each line feed, each line keeping its line
// ending (the last line may lack one); a text ending with a line feed has no
// empty line after it.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// entryID names a top-level entry across re-parses: its table (path and
// element) and its key.
func (d *Doc) entryID(i int) string {
	e := d.entries[i]
	t := d.tables[e.table]
	idx, _ := t.index()
	return strings.Join(t.path, "\x00") + "#" + fmt.Sprint(idx) + "#" + strings.Join(e.path, "\x00")
}

// commentCols are the comment columns of d's one-line entries that carry a
// trailing comment.
func (d *Doc) commentCols() map[string]int {
	out := map[string]int{}
	for i, e := range d.entries {
		if e.parent < 0 && e.commentStart >= 0 && !e.multiline {
			out[d.entryID(i)] = width(string(d.src[e.lineStart:e.commentStart]))
		}
	}
	return out
}

// lineOf is the 1-based line holding byte pos.
func (d *Doc) lineOf(pos int) int {
	return 1 + strings.Count(string(d.src[:min(pos, len(d.src))]), "\n")
}

// lines is the 1-based range [lo, hi) of the lines from byte from up to (not
// including) byte to, to > from.
func (d *Doc) lines(from, to int) (int, int) { return d.lineOf(from), d.lineOf(to-1) + 1 }

func (d *Doc) applyOne(now time.Time, c Change) (edited, error) {
	i, found := d.find(c.Key.full(), c.Key.Table)
	if c.Value == nil {
		if !found {
			return edited{out: d.Bytes(), lo: 1, hi: 1}, nil
		}
		return d.remove(i, c.Note, now)
	}
	lit, err := Literal(c.Value)
	if err != nil {
		return edited{}, err
	}
	if found {
		return d.replace(i, c.Value, lit, c.Note, now)
	}
	return d.insert(c.Key, lit, c.Note, now)
}

// splice is src with [from, to) replaced by text.
func (d *Doc) splice(from, to int, text string) []byte {
	out := make([]byte, 0, len(d.src)-(to-from)+len(text))
	out = append(out, d.src[:from]...)
	out = append(out, text...)
	return append(out, d.src[to:]...)
}

func width(s string) int { return utf8.RuneCountInString(s) }

func dated(now time.Time, note string) string { return now.Format("2006-01-02") + ": " + note }

// withNote is a trailing comment ("# ..." or "") with note put first.
func withNote(comment, note string, now time.Time) string {
	if note == "" {
		return comment
	}
	old := strings.TrimSpace(strings.TrimPrefix(comment, "#"))
	if old == "" {
		return "# " + dated(now, note)
	}
	return "# " + dated(now, note) + "; " + old
}

func (d *Doc) replace(i int, v any, lit, note string, now time.Time) (edited, error) {
	e := d.entries[i]
	ti := d.top(i)
	top := d.entries[ti]
	var val string
	switch {
	case top.multiline && i == ti && e.kind == kindArray && isList(v):
		var err error
		if val, err = d.reshapeArray(e, v); err != nil {
			return edited{}, err
		}
	case top.multiline:
		return edited{}, fmt.Errorf("%w: a value over several lines is replaced only as a whole array", ErrUnsupported)
	case i == ti:
		val = lit
	default:
		val = string(d.src[top.valStart:e.valStart]) + lit + string(d.src[e.valEnd:top.valEnd])
	}
	return d.rewrite(ti, val, note, now), nil
}

func isList(v any) bool {
	switch v.(type) {
	case []string, []any:
		return true
	}
	return false
}

// reshapeArray writes v in the shape of the multi-line array e: one element
// per line at the first element's indentation, the closing bracket at its
// own, and a trailing comma when e has one.
func (d *Doc) reshapeArray(e entry, v any) (string, error) {
	if e.comments || !e.onePerLine || len(e.elems) == 0 {
		return "", fmt.Errorf("%w: an array over several lines with comments inside, or with elements sharing lines, is edited by hand", ErrUnsupported)
	}
	var items []any
	switch x := v.(type) {
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	case []any:
		items = x
	}
	if len(items) == 0 {
		return "[]", nil
	}
	first := e.elems[0][0]
	indent := string(d.src[lineStart(d.src, first):first])
	closePos := e.valEnd - 1
	closeIndent := string(d.src[lineStart(d.src, closePos):closePos])
	if strings.TrimLeft(indent, " \t") != "" || strings.TrimLeft(closeIndent, " \t") != "" {
		return "", fmt.Errorf("%w: an array over several lines in a shape the editor does not keep", ErrUnsupported)
	}
	var b strings.Builder
	b.WriteString("[" + d.nl)
	for n, it := range items {
		lit, err := Literal(it)
		if err != nil {
			return "", err
		}
		b.WriteString(indent + lit)
		if n < len(items)-1 || e.trailingComma {
			b.WriteString(",")
		}
		b.WriteString(d.nl)
	}
	b.WriteString(closeIndent + "]")
	return b.String(), nil
}

func lineStart(src []byte, pos int) int {
	for pos > 0 && src[pos-1] != '\n' {
		pos--
	}
	return pos
}

// rewrite replaces the line (or lines) of top-level entry ti up to its line
// end with the key as written, val, and the trailing comment: kept at its
// column (the one it had when Apply started) when neither the old nor the
// new value spans lines and the gap holds no tab, with note first; a note on
// a line without a comment becomes one, aligned with the comments around
// it.
func (d *Doc) rewrite(ti int, val, note string, now time.Time) edited {
	e := d.entries[ti]
	head := string(d.src[e.lineStart:e.valStart]) + val
	lastLine := head[strings.LastIndexByte(head, '\n')+1:]
	var text string
	switch {
	case e.commentStart < 0 && note == "":
		text = head + string(d.src[e.valEnd:e.lineEnd]) // what trails the value stays
	case e.commentStart < 0:
		text = head + d.gap(e.table, e.line, width(lastLine)) + "# " + dated(now, note)
	default:
		gap := string(d.src[e.valEnd:e.commentStart])
		if !e.multiline && !strings.Contains(val, "\n") && !strings.Contains(gap, "\t") {
			col, ok := d.cols[d.entryID(ti)]
			if !ok {
				col = width(string(d.src[e.lineStart:e.commentStart]))
			}
			gap = strings.Repeat(" ", max(1, col-width(head)))
		}
		text = head + gap + withNote(string(d.src[e.commentStart:e.lineEnd]), note, now)
	}
	lo, hi := d.lines(e.lineStart, max(e.lineEnd, e.lineStart+1))
	return edited{out: d.splice(e.lineStart, e.lineEnd, text), lo: lo, hi: hi}
}

// gap is the space before a new trailing comment on line line, w runes
// wide, in table ti: up to the column of the trailing comment nearest to
// that line in the table, else (or when the line reaches that column) two
// spaces.
func (d *Doc) gap(ti, line, w int) string {
	col, dist := -1, 0
	for _, i := range d.tables[ti].entries {
		e := d.entries[i]
		if e.commentStart < 0 {
			continue
		}
		if n := abs(e.line - line); col < 0 || n < dist {
			col, dist = width(string(d.src[lineStart(d.src, e.commentStart):e.commentStart])), n
		}
	}
	if col > w {
		return strings.Repeat(" ", col-w)
	}
	return "  "
}

func abs(n int) int { return max(n, -n) }

func (d *Doc) remove(i int, note string, now time.Time) (edited, error) {
	e := d.entries[i]
	if e.parent >= 0 {
		p := d.entries[e.parent]
		if len(p.children) == 1 {
			return d.remove(e.parent, note, now) // the inline table would be left empty
		}
		ti := d.top(i)
		top := d.entries[ti]
		if top.multiline {
			return edited{}, fmt.Errorf("%w: a key of an inline table over several lines", ErrUnsupported)
		}
		n := slices.Index(p.children, i)
		from, to := e.keyStart, 0
		if n < len(p.children)-1 {
			to = d.entries[p.children[n+1]].keyStart
		} else {
			from, to = d.entries[p.children[n-1]].valEnd, e.valEnd
		}
		val := string(d.src[top.valStart:from]) + string(d.src[to:top.valEnd])
		return d.rewrite(ti, val, note, now), nil
	}
	if e.comments {
		return edited{}, fmt.Errorf("%w: a value with comments inside is removed by hand", ErrUnsupported)
	}
	comment := ""
	if e.commentStart >= 0 {
		comment = string(d.src[e.commentStart:e.lineEnd])
	}
	comment = withNote(comment, note, now)
	lo, hi := d.lines(e.lineStart, max(e.contEnd, e.lineStart+1))
	if comment == "" {
		return edited{out: d.splice(e.lineStart, e.contEnd, ""), lo: lo, hi: hi}, nil
	}
	// The comment stays as a line of its own, with the lines continuing it.
	indent := string(d.src[e.lineStart:e.keyStart])
	return edited{out: d.splice(e.lineStart, e.lineEnd, indent+comment), lo: lo, hi: hi}, nil
}

// insert adds k = lit where the file does not set k: inside the inline
// table that holds a prefix of its path, else after the last key of its
// table (or of the deepest subtable header on its path), else in a new
// table.
func (d *Doc) insert(k Key, lit, note string, now time.Time) (edited, error) {
	full := k.full()
	for j := len(full) - 1; j >= 1; j-- {
		i, ok := d.find(full[:j], k.Table)
		if !ok {
			continue
		}
		e := d.entries[i]
		if e.kind != kindInline {
			return edited{}, fmt.Errorf("%w: %s is a value, not a table", ErrUnsupported, strings.Join(full[:j], "."))
		}
		return d.insertInline(i, full[j:], lit, note, now)
	}
	for j := len(full) - 1; j >= len(k.Table.Path); j-- {
		if ti, ok := d.header(full[:j], k.Table); ok {
			return d.insertLine(ti, full[j:], lit, note, now)
		}
	}
	if d.defines(k.Table) {
		return edited{}, fmt.Errorf("%w: %s exists only through dotted keys or subtables", ErrUnsupported, k.Table)
	}
	return d.createTable(k.Table, k.Path, lit, note, now)
}

// keyText is a key part as written: bare when it can be, else quoted.
func keyText(part string) string {
	bare := part != ""
	for i := range len(part) {
		bare = bare && isBare(part[i])
	}
	if bare {
		return part
	}
	lit, _ := Literal(part)
	return lit
}

// dottedText is "a.b = lit": a key written beside dotted keys that already
// define its first part, which an inline table "a = { ... }" would define
// a second time.
func dottedText(path []string, lit string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = keyText(p)
	}
	return strings.Join(parts, ".") + " = " + lit
}

// dottedUnder reports whether one of paths (keys of one table or inline
// table, below the first base parts) is a dotted key through first: then a
// key under first must be dotted too.
func dottedUnder(paths [][]string, base int, first string) bool {
	for _, p := range paths {
		if len(p) > base+1 && p[base] == first {
			return true
		}
	}
	return false
}

// keyValueText is "a = lit", or "a = { b = lit }" for a longer path.
func keyValueText(path []string, lit string) string {
	val := lit
	for r := len(path) - 1; r >= 1; r-- {
		val = "{ " + keyText(path[r]) + " = " + val + " }"
	}
	return keyText(path[0]) + " = " + val
}

func (d *Doc) insertInline(i int, rest []string, lit, note string, now time.Time) (edited, error) {
	e := d.entries[i]
	ti := d.top(i)
	top := d.entries[ti]
	if top.multiline {
		return edited{}, fmt.Errorf("%w: an inline table over several lines", ErrUnsupported)
	}
	kv := keyValueText(rest, lit)
	var paths [][]string
	for _, c := range e.children {
		paths = append(paths, d.entries[c].path)
	}
	if len(rest) > 1 && dottedUnder(paths, len(e.path), rest[0]) {
		kv = dottedText(rest, lit)
	}
	var val string
	if len(e.children) > 0 {
		last := d.entries[e.children[len(e.children)-1]]
		val = string(d.src[top.valStart:last.valEnd]) + ", " + kv + string(d.src[last.valEnd:top.valEnd])
	} else {
		open, closing := e.valStart, e.valEnd-1
		if strings.TrimSpace(string(d.src[open+1:closing])) != "" {
			return edited{}, fmt.Errorf("%w: an inline table the editor cannot read", ErrUnsupported)
		}
		val = string(d.src[top.valStart:open]) + "{ " + kv + " }" + string(d.src[closing+1:top.valEnd])
	}
	return d.rewrite(ti, val, note, now), nil
}

// insertLine adds a key line after the last key of table ti (and the lines
// continuing its comment), indented as that key; right under the header
// when the table has no key.
func (d *Doc) insertLine(ti int, rest []string, lit, note string, now time.Time) (edited, error) {
	t := d.tables[ti]
	var pos int
	indent, near := "", t.line
	switch {
	case len(t.entries) > 0:
		last := d.entries[t.entries[len(t.entries)-1]]
		pos, indent, near = last.contEnd, string(d.src[last.lineStart:last.keyStart]), last.line
	case ti == 0:
		return edited{}, fmt.Errorf("%w: a key at the top of the file", ErrUnsupported)
	default:
		pos = d.afterLine(t.headerEnd)
	}
	kv := keyValueText(rest, lit)
	var paths [][]string
	for _, i := range t.entries {
		paths = append(paths, d.entries[i].path)
	}
	if len(rest) > 1 && dottedUnder(paths, 0, rest[0]) {
		kv = dottedText(rest, lit)
	}
	line := indent + kv
	if note != "" {
		line += d.gap(ti, near, width(line)) + "# " + dated(now, note)
	}
	at := d.lineOf(pos)
	if pos == len(d.src) && pos > 0 && d.src[pos-1] != '\n' {
		return edited{out: d.splice(pos, pos, d.nl+line), lo: at, hi: at + 1}, nil // the last line gains its line feed
	}
	return edited{out: d.splice(pos, pos, line+d.nl), lo: at, hi: at}, nil
}

// afterLine is the start of the line after the one ending at end.
func (d *Doc) afterLine(end int) int {
	if end < len(d.src) && d.src[end] == '\r' {
		end++
	}
	if end < len(d.src) {
		end++
	}
	return end
}

// contentEnd is where table ti's own text ends: after its last key and the
// lines continuing that key's comment, else after its header.
func (d *Doc) contentEnd(ti int) int {
	t := d.tables[ti]
	if len(t.entries) > 0 {
		return d.entries[t.entries[len(t.entries)-1]].contEnd
	}
	if ti == 0 {
		return 0
	}
	return d.afterLine(t.headerEnd)
}

// before is where a table inserted in front of table ti's header starts:
// at the comment lines right above the header, which introduce it, but
// after the previous table's own text. Comment lines that run up to the top
// of the file are the file's own opening comment: the table goes under
// them, right above the header.
func (d *Doc) before(ti int) int {
	floor := d.contentEnd(ti - 1)
	start := d.tables[ti].headerStart
	pos := start
	for pos > floor {
		prev := lineStart(d.src, pos-1)
		if !strings.HasPrefix(strings.TrimLeft(string(d.src[prev:pos]), " \t"), "#") || prev < floor {
			break
		}
		pos = prev
	}
	if pos == 0 && pos < start {
		return start
	}
	return pos
}

// createTable adds the table t with its first key: a new [[...]] element
// after the last element of its array (and that element's subtables), or a
// plain table after the last table of its family, else before the first
// array of tables, else at the end of the file.
func (d *Doc) createTable(t Table, rel []string, lit, note string, now time.Time) (edited, error) {
	parts := make([]string, len(t.Path))
	for i, p := range t.Path {
		parts[i] = keyText(p)
	}
	name := strings.Join(parts, ".")
	pos := len(d.src)
	var header string
	if t.Index >= 0 {
		if len(t.Path) != 1 {
			return edited{}, fmt.Errorf("%w: a new element of a nested array of tables", ErrUnsupported)
		}
		n := d.Elements(t.Path...)
		if t.Index != n {
			return edited{}, fmt.Errorf("confedit: %s does not exist (the file has %d)", t, n)
		}
		header = "[[" + name + "]]"
		if last := d.lastTable(func(x table) bool {
			return x.path[0] == t.Path[0] && len(x.chain) > 0 && x.chain[0].index == n-1
		}); n > 0 && last > 0 {
			pos = d.nextHeaderStart(last)
		}
	} else {
		for i := 1; i < len(t.Path); i++ {
			for _, x := range d.tables {
				if x.array && slices.Equal(x.path, t.Path[:i]) {
					return edited{}, fmt.Errorf("%w: a new subtable of an array of tables", ErrUnsupported)
				}
			}
		}
		header = "[" + name + "]"
		if last := d.lastTable(func(x table) bool { return !x.array && len(x.chain) == 0 && x.path[0] == t.Path[0] }); last > 0 {
			pos = d.nextHeaderStart(last)
		} else if first := slices.IndexFunc(d.tables, func(x table) bool { return x.array }); first > 0 {
			pos = d.before(first)
		}
	}
	line := keyValueText(rel, lit)
	if note != "" {
		line += "  # " + dated(now, note)
	}
	at := d.lineOf(pos)
	ed := edited{out: d.insertBlock(pos, header+d.nl+line+d.nl), lo: at, hi: at}
	if pos == len(d.src) && pos > 0 && d.src[pos-1] != '\n' {
		ed.hi = at + 1 // the last line gains its line feed
	}
	return ed, nil
}

// lastTable is the last table (not the root) match accepts; 0 when none.
func (d *Doc) lastTable(match func(table) bool) int {
	for i := len(d.tables) - 1; i > 0; i-- {
		if match(d.tables[i]) {
			return i
		}
	}
	return 0
}

// nextHeaderStart is where a table inserted after table ti goes: before the
// next header (and the comments that introduce it), or the end of the file.
func (d *Doc) nextHeaderStart(ti int) int {
	if ti+1 < len(d.tables) {
		return d.before(ti + 1)
	}
	return len(d.src)
}

// insertBlock puts block (whole lines) at pos, a line start or the end of
// the file, with a blank line before and after it unless one is there.
func (d *Doc) insertBlock(pos int, block string) []byte {
	head := string(d.src[:pos])
	if head != "" && !strings.HasSuffix(head, "\n") {
		head += d.nl
	}
	if head != "" {
		body := strings.TrimSuffix(strings.TrimSuffix(head, "\n"), "\r")
		if strings.TrimSpace(body[strings.LastIndexByte(body, '\n')+1:]) != "" {
			head += d.nl // a blank line before the new table
		}
	}
	tail := string(d.src[pos:])
	next, _, _ := strings.Cut(tail, "\n")
	if strings.TrimSpace(next) != "" {
		block += d.nl // and after it
	}
	return []byte(head + block + tail)
}
