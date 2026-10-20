package confedit

// The scanner: a reader of the TOML magnum's config files are written in that
// records where every table header, key and value starts and ends, and
// where each trailing comment and its continuation lines are. It does not
// decode values (BurntSushi does, and Parse checks the scanner's keys
// against its MetaData.Keys()); it only has to find where each token ends,
// which is why strings, arrays and inline tables are scanned in full: a `#`
// or a `]` inside a string is not what it looks like.

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// table is one table of the file: the root (the keys above the first
// header) or one header's table, an element of an array of tables
// included.
type table struct {
	path  []string
	array bool // a [[path]] header
	// chain holds, for each prefix of path that is an array of tables, the
	// element the header belongs to: [pool.env] after the second [[pool]]
	// has chain {{1, 1}} (prefix length 1, element 1).
	chain []elemRef
	// header is the header line's start and end (before its line break);
	// -1, -1 for the root.
	headerStart, headerEnd int
	line                   int   // 1-based line of the header; 0 for the root
	entries                []int // the table's own key/values (top level), in order
}

type elemRef struct{ depth, index int }

// index is the element of the innermost array of tables the table belongs
// to, -1 when none; nested reports more than one array of tables in its
// path (no magnum file has them; such a table is not edited).
func (t table) index() (idx int, nested bool) {
	switch len(t.chain) {
	case 0:
		return -1, false
	case 1:
		return t.chain[0].index, false
	}
	return t.chain[len(t.chain)-1].index, true
}

// Value kinds of an entry.
const (
	kindOther  = 'o' // a number, a boolean, a date
	kindString = 's'
	kindArray  = 'a'
	kindInline = 't' // an inline table
)

// entry is one key/value: a top-level one (a line of its table) or a key
// inside an inline table that is itself an entry's value (never one inside
// an array: those are part of the array's value).
type entry struct {
	table  int      // index into Doc.tables
	path   []string // the key's path within its table (dotted keys, then the inner keys of inline tables)
	parent int      // the inline-table entry holding it; -1 for a top-level entry
	line   int      // 1-based line of the key

	lineStart        int // start of the key's line (top-level entries)
	keyStart         int
	valStart, valEnd int
	// commentStart is where the trailing comment on the value's last line
	// starts, -1 when none (top-level entries); lineEnd is the end of that
	// line before its line break; contEnd is the start of the line after
	// the entry's continuation comment lines (indented comment lines under
	// a trailing comment), or len(src).
	commentStart, lineEnd, contEnd int

	kind      byte
	multiline bool // the value spans lines
	comments  bool // a comment inside the value (an array or inline table over lines)
	children  []int
	// shape of an array value: each element's span, whether the elements
	// sit one per line with the brackets on lines of their own, and whether
	// the last one has a trailing comma.
	elems         [][2]int
	onePerLine    bool
	trailingComma bool
}

type scanner struct {
	src     []byte
	pos     int
	tables  []table
	entries []entry
	keys    [][]string // every key and header in document order (MetaData.Keys' order)

	cur     int            // the table the next key/value goes to
	count   map[string]int // elements seen per array of tables (by path), within the current parent element
	isArray map[string]bool
	breaks  []int // the offsets of the line feeds, for lineAt
}

type scanError struct {
	line int
	msg  string
}

func (e *scanError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// scan reads src into tables, entries and keys, or fails with a scanError
// at the first construct it does not model.
func scan(src []byte) (s *scanner, err error) {
	s = &scanner{src: src, count: map[string]int{}, isArray: map[string]bool{}}
	for i, c := range src {
		if c == '\n' {
			s.breaks = append(s.breaks, i)
		}
	}
	s.tables = []table{{headerStart: -1, headerEnd: -1}}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(*scanError)
			if !ok {
				panic(r)
			}
			s, err = nil, se
		}
	}()
	s.top()
	return s, nil
}

func (s *scanner) fail(format string, args ...any) {
	panic(&scanError{line: s.lineAt(s.pos), msg: fmt.Sprintf(format, args...)})
}

// lineAt is the 1-based line holding byte pos.
func (s *scanner) lineAt(pos int) int {
	n, _ := slices.BinarySearch(s.breaks, pos)
	return n + 1
}

func (s *scanner) eof() bool { return s.pos >= len(s.src) }

func (s *scanner) peek() byte {
	if s.eof() {
		return 0
	}
	return s.src[s.pos]
}

func (s *scanner) at(lit string) bool { return strings.HasPrefix(string(s.src[s.pos:]), lit) }

func (s *scanner) skipSpace() {
	for !s.eof() && (s.src[s.pos] == ' ' || s.src[s.pos] == '\t') {
		s.pos++
	}
}

// atNewline reports whether a line break (LF or CRLF) starts at pos.
func (s *scanner) atNewline() bool {
	return s.peek() == '\n' || (s.peek() == '\r' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '\n')
}

func (s *scanner) newline() {
	if s.peek() == '\r' {
		s.pos++
	}
	s.pos++
}

// skipComment skips a comment to the end of its line (not the line break).
func (s *scanner) skipComment() {
	for !s.eof() && !s.atNewline() {
		if s.src[s.pos] == '\r' {
			s.fail("a carriage return without a line feed")
		}
		s.pos++
	}
}

// endOfLine expects the rest of the line to be blank or a comment and
// returns where the comment starts (-1 when none) and where the line ends.
func (s *scanner) endOfLine() (commentStart, lineEnd int) {
	s.skipSpace()
	commentStart = -1
	if s.peek() == '#' {
		commentStart = s.pos
		s.skipComment()
	}
	if !s.eof() && !s.atNewline() {
		s.fail("unexpected %q after a value or header", s.peek())
	}
	return commentStart, s.pos
}

func (s *scanner) top() {
	if strings.HasPrefix(string(s.src), "\uFEFF") {
		s.fail("a byte order mark")
	}
	for !s.eof() {
		s.skipSpace()
		switch {
		case s.eof():
		case s.atNewline():
			s.newline()
		case s.peek() == '#':
			s.skipComment()
		case s.peek() == '[':
			s.header()
		default:
			s.keyValue()
		}
	}
}

func (s *scanner) lineStartOf(pos int) int {
	for pos > 0 && s.src[pos-1] != '\n' {
		pos--
	}
	return pos
}

func pathKey(p []string) string { return strings.Join(p, "\x00") }

func (s *scanner) header() {
	start := s.lineStartOf(s.pos)
	if strings.TrimLeft(string(s.src[start:s.pos]), " \t") != "" {
		s.fail("a header after other text on its line")
	}
	array := s.at("[[")
	if array {
		s.pos += 2
	} else {
		s.pos++
	}
	s.skipSpace()
	path := s.keyPath()
	s.skipSpace()
	if array {
		if !s.at("]]") {
			s.fail("an array-of-tables header not closed by ]]")
		}
		s.pos += 2
	} else {
		if s.peek() != ']' {
			s.fail("a table header not closed by ]")
		}
		s.pos++
	}
	_, end := s.endOfLine()
	k := pathKey(path)
	if array {
		s.isArray[k] = true
		s.count[k]++
		for other := range s.count { // the deeper arrays belong to the previous element
			if strings.HasPrefix(other, k+"\x00") {
				delete(s.count, other)
			}
		}
	}
	var chain []elemRef
	for i := 1; i <= len(path); i++ {
		if pk := pathKey(path[:i]); s.isArray[pk] {
			chain = append(chain, elemRef{depth: i, index: s.count[pk] - 1})
		}
	}
	s.tables = append(s.tables, table{path: path, array: array, chain: chain, headerStart: start, headerEnd: end, line: s.lineAt(start)})
	s.cur = len(s.tables) - 1
	s.keys = append(s.keys, path)
}

// keyPath reads a dotted key (whitespace allowed around the dots).
func (s *scanner) keyPath() []string {
	var path []string
	for {
		path = append(path, s.keyPart())
		s.skipSpace()
		if s.peek() != '.' {
			return path
		}
		s.pos++
		s.skipSpace()
	}
}

func isBare(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (s *scanner) keyPart() string {
	switch c := s.peek(); {
	case c == '"':
		if s.at(`"""`) {
			s.fail("a multi-line string as a key")
		}
		start := s.pos
		s.basicString()
		v, err := unquoteBasic(string(s.src[start+1 : s.pos-1]))
		if err != nil {
			s.fail("a quoted key: %v", err)
		}
		return v
	case c == '\'':
		if s.at("'''") {
			s.fail("a multi-line string as a key")
		}
		start := s.pos
		s.literalString()
		return string(s.src[start+1 : s.pos-1])
	case isBare(c):
		start := s.pos
		for !s.eof() && isBare(s.src[s.pos]) {
			s.pos++
		}
		return string(s.src[start:s.pos])
	}
	s.fail("a key expected, found %q", s.peek())
	return ""
}

func (s *scanner) keyValue() {
	lineStart := s.lineStartOf(s.pos)
	e := entry{table: s.cur, parent: -1, lineStart: lineStart, keyStart: s.pos, line: s.lineAt(s.pos)}
	e.path = s.keyPath()
	s.keys = append(s.keys, s.fullPath(e.path))
	s.skipSpace()
	if s.peek() != '=' {
		s.fail("= expected after a key")
	}
	s.pos++
	s.skipSpace()
	idx := len(s.entries)
	s.entries = append(s.entries, e)
	s.value(idx, e.path)
	e = s.entries[idx]
	e.commentStart, e.lineEnd = s.endOfLine()
	e.multiline = s.lineAt(e.valEnd) != e.line
	e.contEnd = s.continuation(e)
	s.entries[idx] = e
	s.tables[s.cur].entries = append(s.tables[s.cur].entries, idx)
}

// continuation returns the start of the line after e's continuation comment
// lines: the indented comment lines right under a trailing comment, which
// continue it (config.defaults.toml's style). A blank line or a comment at
// column 0 ends them: it belongs to what follows.
func (s *scanner) continuation(e entry) int {
	pos := e.lineEnd
	if pos < len(s.src) {
		if s.src[pos] == '\r' {
			pos++
		}
		pos++ // the line feed
	}
	if e.commentStart < 0 {
		return pos
	}
	for pos < len(s.src) {
		i := pos
		for i < len(s.src) && (s.src[i] == ' ' || s.src[i] == '\t') {
			i++
		}
		if i == pos || i >= len(s.src) || s.src[i] != '#' {
			break
		}
		for i < len(s.src) && s.src[i] != '\n' {
			i++
		}
		if i < len(s.src) {
			i++
		}
		pos = i
	}
	return pos
}

// fullPath is a key's path from the document root, as MetaData.Keys gives
// it.
func (s *scanner) fullPath(rel []string) []string {
	return append(append([]string(nil), s.tables[s.cur].path...), rel...)
}

// value scans the value of entries[idx] (path is its key's path in its
// table), filling its span, kind and, for an inline table, its children.
func (s *scanner) value(idx int, path []string) {
	start := s.pos
	kind, children, comments, elems, onePerLine, trailing := s.anyValue(path, idx, true)
	e := &s.entries[idx]
	e.valStart, e.valEnd, e.kind, e.comments = start, s.pos, kind, comments
	e.children, e.elems, e.onePerLine, e.trailingComma = children, elems, onePerLine, trailing
}

// anyValue scans one value. record says whether keys inside an inline table
// become entries (a value of a key; not one inside an array). path is the
// value's key path in its table; parent the entry the value belongs to.
func (s *scanner) anyValue(path []string, parent int, record bool) (kind byte, children []int, comments bool, elems [][2]int, onePerLine, trailing bool) {
	switch s.peek() {
	case '"':
		if s.at(`"""`) {
			s.multiString('"')
		} else {
			s.basicString()
		}
		return kindString, nil, false, nil, false, false
	case '\'':
		if s.at("'''") {
			s.multiString('\'')
		} else {
			s.literalString()
		}
		return kindString, nil, false, nil, false, false
	case '[':
		comments, elems, onePerLine, trailing = s.array(path)
		return kindArray, nil, comments, elems, onePerLine, trailing
	case '{':
		children, comments = s.inlineTable(path, parent, record)
		return kindInline, children, comments, nil, false, false
	case 0, '\n', '\r', '#', ',', ']', '}':
		s.fail("a value expected")
	}
	s.bare()
	return kindOther, nil, false, nil, false, false
}

// bare scans a number, boolean or date, a date-time written with a space
// between date and time included.
func (s *scanner) bare() {
	start := s.pos
	stop := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ']' || c == '}' || c == '#'
	}
	for !s.eof() && !stop(s.src[s.pos]) {
		s.pos++
	}
	tok := string(s.src[start:s.pos])
	if isDate(tok) && s.peek() == ' ' && s.pos+2 < len(s.src) && isDigit(s.src[s.pos+1]) && isDigit(s.src[s.pos+2]) {
		s.pos++
		for !s.eof() && !stop(s.src[s.pos]) {
			s.pos++
		}
	}
	if s.pos == start {
		s.fail("a value expected")
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDate(tok string) bool {
	if len(tok) != 10 || tok[4] != '-' || tok[7] != '-' {
		return false
	}
	for i, c := range []byte(tok) {
		if i != 4 && i != 7 && !isDigit(c) {
			return false
		}
	}
	return true
}

func (s *scanner) basicString() {
	s.pos++ // "
	for {
		if s.eof() || s.atNewline() || s.peek() == '\r' {
			s.fail("an unterminated string")
		}
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case '"':
			s.pos++
			return
		default:
			s.pos++
		}
	}
}

func (s *scanner) literalString() {
	s.pos++ // '
	for {
		if s.eof() || s.atNewline() || s.peek() == '\r' {
			s.fail("an unterminated literal string")
		}
		if s.src[s.pos] == '\'' {
			s.pos++
			return
		}
		s.pos++
	}
}

// multiString scans a multi-line string opened by three quotes q; it ends
// at the first three closing quotes, which may follow up to two quotes that
// are part of the string.
func (s *scanner) multiString(q byte) {
	s.pos += 3
	for {
		if s.eof() {
			s.fail("an unterminated multi-line string")
		}
		c := s.src[s.pos]
		if c == '\\' && q == '"' {
			s.pos += 2
			continue
		}
		if c == q && s.pos+2 < len(s.src) && s.src[s.pos+1] == q && s.src[s.pos+2] == q {
			n := 3
			for n < 5 && s.pos+n < len(s.src) && s.src[s.pos+n] == q {
				n++
			}
			s.pos += n
			return
		}
		s.pos++
	}
}

// skipBlank skips whitespace, line breaks and comments inside an array or
// an inline table and reports whether it skipped a comment.
func (s *scanner) skipBlank() (comment bool) {
	for {
		s.skipSpace()
		switch {
		case s.atNewline():
			s.newline()
		case s.peek() == '#':
			s.skipComment()
			comment = true
		default:
			return comment
		}
	}
}

func (s *scanner) array(path []string) (comments bool, elems [][2]int, onePerLine, trailing bool) {
	open := s.pos
	s.pos++ // [
	onePerLine = true
	lastLine := s.lineAt(open)
	for {
		if s.skipBlank() {
			comments = true
		}
		if s.eof() {
			s.fail("an unterminated array")
		}
		if s.peek() == ']' {
			if s.lineAt(s.pos) == lastLine {
				onePerLine = false // the closing bracket shares a line with an element or with [
			}
			s.pos++
			return comments, elems, onePerLine, trailing
		}
		start := s.pos
		if s.lineAt(start) == lastLine {
			onePerLine = false
		}
		if _, _, nested, _, _, _ := s.anyValue(path, -1, false); nested {
			comments = true // a comment inside an element is the array's too
		}
		elems = append(elems, [2]int{start, s.pos})
		lastLine = s.lineAt(s.pos)
		trailing = false
		if s.skipBlank() {
			comments = true
		}
		switch s.peek() {
		case ',':
			s.pos++
			trailing = true
		case ']':
		default:
			s.fail("an array element not followed by , or ]")
		}
	}
}

// inlineTable scans { ... } (TOML 1.1: line breaks, comments and a trailing
// comma allowed). Its keys are listed for the cross-check and, when record,
// become entries under parent.
func (s *scanner) inlineTable(path []string, parent int, record bool) (children []int, comments bool) {
	s.pos++ // {
	for {
		if s.skipBlank() {
			comments = true
		}
		if s.eof() {
			s.fail("an unterminated inline table")
		}
		if s.peek() == '}' {
			s.pos++
			return children, comments
		}
		keyStart := s.pos
		rel := s.keyPath()
		inner := append(append([]string(nil), path...), rel...)
		s.keys = append(s.keys, s.fullPath(inner))
		s.skipSpace()
		if s.peek() != '=' {
			s.fail("= expected after a key in an inline table")
		}
		s.pos++
		s.skipSpace()
		if record {
			idx := len(s.entries)
			s.entries = append(s.entries, entry{table: s.cur, path: inner, parent: parent, keyStart: keyStart,
				line: s.lineAt(keyStart), commentStart: -1})
			s.value(idx, inner)
			s.entries[idx].multiline = s.lineAt(s.entries[idx].valEnd) != s.entries[idx].line
			children = append(children, idx)
			comments = comments || s.entries[idx].comments
		} else if _, _, nested, _, _, _ := s.anyValue(inner, -1, false); nested {
			comments = true
		}
		if s.skipBlank() {
			comments = true
		}
		switch s.peek() {
		case ',':
			s.pos++
		case '}':
		default:
			s.fail("an inline-table value not followed by , or }")
		}
	}
}

// unquoteBasic resolves the escapes of a basic string's body as TOML 1.1
// does (\b \t \n \f \r \e \" \\ \xHH \uXXXX \UXXXXXXXX).
func unquoteBasic(body string) (string, error) {
	if !strings.Contains(body, `\`) {
		return body, nil
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", errors.New("a trailing backslash")
		}
		switch body[i] {
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'e':
			b.WriteByte(0x1b)
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[body[i]]
			if i+1+n > len(body) {
				return "", errors.New("a short escape")
			}
			r, err := strconv.ParseInt(body[i+1:i+1+n], 16, 32)
			if err != nil || r < 0 || r > utf8.MaxRune || !utf8.ValidRune(rune(r)) {
				return "", errors.New("an invalid escape")
			}
			b.WriteRune(rune(r))
			i += n
		default:
			return "", fmt.Errorf("an unknown escape \\%c", body[i])
		}
	}
	return b.String(), nil
}
