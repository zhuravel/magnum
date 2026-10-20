package confedit

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Verify decodes before and after and reports an error unless they differ
// in exactly the leaves the changes name (array elements by index): every
// leaf that differs lies at or under a change's key, and the changes'
// final effect holds in after, in their order: each key holds the value its
// last change wrote, a removed key is absent, and a change that a later one
// on the same key (or one holding it, or one under it) overrides is not
// checked on its own. It is the check that catches
// an editor mistake that would change another key; nothing is written when
// it fails. Its errors name keys, never values.
func Verify(before, after []byte, changes []Change) error {
	var a, b map[string]any
	if _, err := toml.Decode(string(before), &a); err != nil {
		return fmt.Errorf("confedit: the old file: %w", err)
	}
	if _, err := toml.Decode(string(after), &b); err != nil {
		return fmt.Errorf("confedit: the new file: %w", err)
	}
	paths := make([][]any, len(changes))
	for i, c := range changes {
		p, err := changePath(c.Key.Table, c.Key.Path, b, a)
		if err != nil {
			return fmt.Errorf("confedit: %s: %w", c.Key, err)
		}
		paths[i] = p
	}
	var diffs [][]any
	diff(a, b, nil, &diffs)
	for _, d := range diffs {
		if !slices.ContainsFunc(paths, func(p []any) bool { return hasPrefix(d, p) }) {
			return fmt.Errorf("confedit: the edit changed %s, which no change names", pathString(d))
		}
	}
	for i, c := range changes {
		if slices.ContainsFunc(paths[i+1:], func(p []any) bool { return hasPrefix(paths[i], p) || hasPrefix(p, paths[i]) }) {
			continue // a later change sets or removes the same key, one holding it or one under it: its effect is the final one
		}
		got, ok := lookup(b, paths[i])
		if c.Value == nil {
			if ok {
				return fmt.Errorf("confedit: %s is still set after its removal", c.Key)
			}
			continue
		}
		want, err := normalize(c.Value)
		if err != nil {
			return err
		}
		if !ok || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("confedit: %s does not read back as the value written", c.Key)
		}
	}
	return nil
}

// Changed lists the leaves where before and after differ, as dotted key
// paths with array elements by index ("pipeline.spend",
// "watch[1].roles[2]"), in key order: what a save changed, named without
// its values.
func Changed(before, after []byte) ([]string, error) {
	var a, b map[string]any
	if _, err := toml.Decode(string(before), &a); err != nil {
		return nil, fmt.Errorf("confedit: the old file: %w", err)
	}
	if _, err := toml.Decode(string(after), &b); err != nil {
		return nil, fmt.Errorf("confedit: the new file: %w", err)
	}
	var diffs [][]any
	diff(a, b, nil, &diffs)
	out := make([]string, len(diffs))
	for i, d := range diffs {
		out[i] = pathString(d)
	}
	return out, nil
}

// normalize is v as BurntSushi decodes its literal.
func normalize(v any) (any, error) {
	lit, err := Literal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if _, err := toml.Decode("v = "+lit, &m); err != nil {
		return nil, fmt.Errorf("confedit: a written value does not decode: %w", err)
	}
	return m["v"], nil
}

// changePath is the decoded path of a key: its table's path with the
// element index after the array of tables in it (found in the first of
// docs that has that prefix), then the key's path.
func changePath(t Table, rel []string, docs ...map[string]any) ([]any, error) {
	var p []any
	used := t.Index < 0
	for i, part := range t.Path {
		p = append(p, part)
		if used {
			continue
		}
		for _, doc := range docs {
			v, ok := lookup(doc, p)
			if !ok {
				continue
			}
			if tablesOf(v) != nil || isArrayOfTables(v) {
				p, used = append(p, t.Index), true
			}
			break
		}
		if !used && i == len(t.Path)-1 {
			return nil, fmt.Errorf("no array of tables at %s", strings.Join(t.Path, "."))
		}
	}
	for _, part := range rel {
		p = append(p, part)
	}
	return p, nil
}

func isArrayOfTables(v any) bool {
	_, ok := v.([]map[string]any)
	return ok
}

// lookup walks doc along p (strings into tables, ints into arrays).
func lookup(doc map[string]any, p []any) (any, bool) {
	var cur any = doc
	for _, step := range p {
		switch s := step.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[s]; !ok {
				return nil, false
			}
		case int:
			arr := asSlice(cur)
			if arr == nil || s < 0 || s >= len(arr) {
				return nil, false
			}
			cur = arr[s]
		}
	}
	return cur, true
}

// asSlice is an array value as []any (nil for anything else).
func asSlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out
	}
	return nil
}

// diff appends the paths of the leaves where a and b differ: a scalar that
// changed, a key on one side only (each leaf under it, or the key itself
// for an empty table or array), an array element by index.
func diff(a, b any, path []any, out *[][]any) {
	am, aMap := a.(map[string]any)
	bm, bMap := b.(map[string]any)
	if aMap && bMap {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			av, aok := am[k]
			bv, bok := bm[k]
			p := append(slices.Clone(path), k)
			switch {
			case !aok:
				leaves(bv, p, out)
			case !bok:
				leaves(av, p, out)
			default:
				diff(av, bv, p, out)
			}
		}
		return
	}
	as, bs := asSlice(a), asSlice(b)
	if as != nil && bs != nil {
		for i := range max(len(as), len(bs)) {
			p := append(slices.Clone(path), i)
			switch {
			case i >= len(as):
				leaves(bs[i], p, out)
			case i >= len(bs):
				leaves(as[i], p, out)
			default:
				diff(as[i], bs[i], p, out)
			}
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}

// leaves appends every leaf path under v (v's own path for a scalar or an
// empty table or array).
func leaves(v any, path []any, out *[][]any) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			leaves(m[k], append(slices.Clone(path), k), out)
		}
		return
	}
	if s := asSlice(v); len(s) > 0 {
		for i, x := range s {
			leaves(x, append(slices.Clone(path), i), out)
		}
		return
	}
	*out = append(*out, path)
}

func hasPrefix(p, prefix []any) bool {
	return len(prefix) <= len(p) && reflect.DeepEqual(p[:len(prefix)], prefix)
}

func pathString(p []any) string {
	var b strings.Builder
	for _, step := range p {
		switch s := step.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s)
		case int:
			b.WriteString("[" + strconv.Itoa(s) + "]")
		}
	}
	return b.String()
}
