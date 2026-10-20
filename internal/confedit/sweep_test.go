package confedit

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

var sweepNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

// sweepResult counts what a sweep over one file did: each edit either
// applied (Apply verifies the result) or was refused with ErrUnsupported;
// anything else is a failure.
type sweepResult struct {
	replaced, inserted, removed, refused int
	failures                             []string
}

// keyOf is the Key of entry i as a caller names it.
func (d *Doc) keyOf(i int) (Key, bool) {
	e := d.entries[i]
	t := d.tables[e.table]
	idx, nested := t.index()
	if nested {
		return Key{}, false
	}
	return Key{Table: Table{Path: t.path, Index: idx}, Path: e.path}, true
}

// sweepValue is a new value of the same type as v (nil: none to try).
func sweepValue(v any) any {
	switch x := v.(type) {
	case string:
		return `zz edited "q" # not a comment`
	case bool:
		return !x
	case int64:
		return x + 1
	case float64:
		return x + 0.5
	case []any:
		out := slices.Clone(x)
		for _, el := range x {
			if _, ok := el.(string); !ok {
				return nil
			}
		}
		return append(out, "zz # added")
	case map[string]any:
		return map[string]any{"zz": "edited"}
	}
	return nil
}

func (r *sweepResult) record(what string, err error, ok *int) {
	switch {
	case err == nil:
		*ok++
	case errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrEditor):
		r.refused++
	default:
		r.failures = append(r.failures, fmt.Sprintf("%s: %v", what, err))
	}
}

// entryLines is the 1-based range [lo, hi) of the lines of entry i's
// top-level entry, its continuation comment lines included: the only old
// lines an edit of it may change.
func (d *Doc) entryLines(i int) (int, int) {
	e := d.entries[d.top(i)]
	return d.lines(e.lineStart, max(e.contEnd, e.lineStart+1))
}

// textCheck is the sweep's and the fuzz test's own check of an edit's text,
// apart from Verify's decoding: an edit of entry i (or, i < 0, an insertion)
// changes no line but that entry's, the last line gaining a line feed
// allowed for an insertion at the end of a file without one.
func textCheck(d *Doc, i int, out []byte) bool {
	lo, hi := 1, 1
	if i >= 0 {
		lo, hi = d.entryLines(i)
	} else if n := len(d.src); n > 0 && d.src[n-1] != '\n' {
		lo, hi = d.lineOf(n), d.lineOf(n)+1
	}
	return confined(d.src, out, lo, hi)
}

// sweep replaces every value, removes every key and inserts a key into
// every table and inline table of src, one edit at a time.
func sweep(tb testing.TB, src []byte) sweepResult {
	tb.Helper()
	d, err := Parse(src)
	if err != nil {
		tb.Fatal(err)
	}
	var r sweepResult
	for i := range d.entries {
		k, ok := d.keyOf(i)
		if !ok {
			continue
		}
		v, ok := d.Get(k)
		if !ok {
			r.failures = append(r.failures, fmt.Sprintf("Get %s: not found", k))
			continue
		}
		check := func(what string, out []byte, err error) error {
			if err == nil && !textCheck(d, i, out) {
				return fmt.Errorf("%s changed a line outside the entry's", what)
			}
			return err
		}
		if nv := sweepValue(v.Data); nv != nil {
			out, err := d.Apply(sweepNow, Change{Key: k, Value: nv})
			r.record("replace "+k.String(), check("replace", out, err), &r.replaced)
		}
		out, err := d.Apply(sweepNow, Change{Key: k, Note: "back to the default"})
		r.record("remove "+k.String(), check("remove", out, err), &r.removed)
		if d.entries[i].kind == kindInline {
			ik := Key{Table: k.Table, Path: append(slices.Clone(k.Path), "zz_new")}
			out, err := d.Apply(sweepNow, Change{Key: ik, Value: "v"})
			r.record("insert "+ik.String(), check("insert", out, err), &r.inserted)
		}
	}
	for ti, tb := range d.tables {
		idx, nested := tb.index()
		if ti == 0 || nested {
			continue
		}
		k := Key{Table: Table{Path: tb.path, Index: idx}, Path: []string{"zz_new"}}
		out, err := d.Apply(sweepNow, Change{Key: k, Value: int64(7), Note: "added"})
		if err == nil && !textCheck(d, -1, out) {
			err = fmt.Errorf("an insertion changed a line")
		}
		r.record("insert "+k.String(), err, &r.inserted)
	}
	return r
}

func TestEveryValueOfTheFixturesCanBeReplacedRemovedOrInsertedAndNothingElseChanges(t *testing.T) {
	for name, src := range fixtures(t) {
		r := sweep(t, src)
		for _, f := range r.failures {
			t.Errorf("%s: %s", name, f)
		}
		if r.replaced == 0 || r.inserted == 0 || r.removed == 0 {
			t.Errorf("%s: replaced %d, inserted %d, removed %d", name, r.replaced, r.inserted, r.removed)
		}
		t.Logf("%s: replaced %d, inserted %d, removed %d, refused %d", name, r.replaced, r.inserted, r.removed, r.refused)
	}
}
