package confedit

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

// FuzzApply makes one random edit to config.defaults.toml, the operator-like
// fixture (LF or CRLF) or a file the fuzzer writes (text itself): a value
// replaced by a string, an integer or a list, a key removed, a key inserted
// into a table or an inline table. The edit applies, reads back with
// exactly that change (Verify) and changes no line but the edited entry's
// (textCheck), or it is refused and returns nothing; it never fails the
// editor's own check (ErrEditor), and Parse never panics.
func FuzzApply(f *testing.F) {
	op := readFixture(f, "operator.toml")
	files := [][]byte{magnum.DefaultConfig, op, crlf(op)}
	f.Add(uint8(0), uint16(3), uint8(0), "high")
	f.Add(uint8(1), uint16(40), uint8(2), "")
	f.Add(uint8(1), uint16(77), uint8(4), "effort")
	f.Add(uint8(2), uint16(12), uint8(3), "new_key")
	f.Add(uint8(1), uint16(20), uint8(5), "alice,rev-ann")
	f.Add(uint8(3), uint16(0), uint8(0), "[a]\nb = 1 # c\n")
	f.Fuzz(func(t *testing.T, file uint8, at uint16, op uint8, text string) {
		var src []byte
		if int(file)%4 == 3 {
			src = []byte(text)
		} else {
			src = files[int(file)%4]
		}
		d, err := Parse(src)
		if err != nil {
			return
		}
		c, entry, ok := fuzzChange(d, int(at), op, text)
		if !ok {
			return
		}
		out, err := d.Apply(sweepNow, c)
		if errors.Is(err, ErrEditor) {
			t.Fatalf("Apply failed its own check: %v", err)
		}
		if err != nil {
			if out != nil {
				t.Fatalf("Apply failed and returned text: %v", err)
			}
			return
		}
		if !textCheck(d, entry, out) {
			t.Fatalf("the edit changed a line outside the entry's:\n%s", out)
		}
		if err := Verify(src, out, []Change{c}); err != nil {
			t.Fatalf("Apply returned text Verify refuses: %v", err)
		}
		if _, err := Parse(out); err != nil {
			t.Fatalf("the edited file does not parse: %v", err)
		}
		if !bytes.Equal(d.Bytes(), src) {
			t.Fatal("Apply changed the Doc")
		}
	})
}

// fuzzChange builds the edit op names on the at-th entry (or table), with
// the entry it edits (-1 for an insertion into a table).
func fuzzChange(d *Doc, at int, op uint8, text string) (Change, int, bool) {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	if name == "" {
		name = "zz"
	}
	if op%6 == 3 { // a key into a table
		if len(d.tables) < 2 {
			return Change{}, 0, false
		}
		tb := d.tables[1+at%(len(d.tables)-1)]
		idx, nested := tb.index()
		if nested {
			return Change{}, 0, false
		}
		k := Key{Table: Table{Path: tb.path, Index: idx}, Path: []string{name}}
		entry := -1 // an insertion, unless the table has the key: then its value is replaced
		if i, ok := d.find(k.full(), k.Table); ok {
			entry = i
		}
		return Change{Key: k, Value: int64(at), Note: name}, entry, true
	}
	if len(d.entries) == 0 {
		return Change{}, 0, false
	}
	i := at % len(d.entries)
	k, ok := d.keyOf(i)
	if !ok {
		return Change{}, 0, false
	}
	switch op % 6 {
	case 0:
		return Change{Key: k, Value: text}, i, true
	case 1:
		return Change{Key: k, Value: int64(at)}, i, true
	case 2:
		return Change{Key: k, Note: name}, i, true
	case 4:
		return Change{Key: Key{Table: k.Table, Path: append(slices.Clone(k.Path), name)}, Value: text}, i, true
	}
	return Change{Key: k, Value: strings.Split(text, ",")}, i, true
}
