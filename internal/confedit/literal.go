package confedit

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// inlineKeyOrder is the order an inline table's keys are written in:
// config.SpendRole's fields first, then the rest by name.
var inlineKeyOrder = []string{"model", "effort", "rereview_effort", "max_subagents"}

// Literal is v as a one-line TOML value: BurntSushi's encoder for strings
// (a basic string, escaped), booleans, integers, floats and arrays of them,
// and "{ k = v, ... }" for a map (an inline table, its keys in
// config.SpendRole's order, then by name).
func Literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", fmt.Errorf("confedit: no value")
	case map[string]any:
		if len(x) == 0 {
			return "{}", nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			ia, ib := slices.Index(inlineKeyOrder, a), slices.Index(inlineKeyOrder, b)
			switch {
			case ia >= 0 && ib >= 0:
				return ia - ib
			case ia >= 0:
				return -1
			case ib >= 0:
				return 1
			}
			return strings.Compare(a, b)
		})
		parts := make([]string, len(keys))
		for i, k := range keys {
			if !utf8.ValidString(k) {
				return "", errors.New("confedit: a key must be UTF-8 text (TOML is)")
			}
			lit, err := Literal(x[k])
			if err != nil {
				return "", err
			}
			parts[i] = keyText(k) + " = " + lit
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	case []any:
		for _, el := range x {
			switch el.(type) {
			case map[string]any, []any, nil:
				return "", fmt.Errorf("confedit: an array of %T is not written", el)
			}
		}
	case string, bool, int, int64, float64, []string, []int, []int64, []float64, []bool:
	default:
		return "", fmt.Errorf("confedit: a value of type %T is not written", v)
	}
	if !validText(v) {
		return "", errors.New("confedit: a value must be UTF-8 text (TOML is)")
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.Len() == 0 {
		return "[]", nil // the encoder writes nothing for a nil slice
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]any{"v": v}); err != nil {
		return "", fmt.Errorf("confedit: %w", err)
	}
	out, ok := strings.CutPrefix(strings.TrimSuffix(buf.String(), "\n"), "v = ")
	if !ok || strings.ContainsAny(out, "\r\n") {
		return "", fmt.Errorf("confedit: a value of type %T does not fit on one line", v)
	}
	return out, nil
}

// validText reports whether every string in v is valid UTF-8.
func validText(v any) bool {
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x)
	case []string:
		for _, s := range x {
			if !utf8.ValidString(s) {
				return false
			}
		}
	case []any:
		for _, el := range x {
			if !validText(el) {
				return false
			}
		}
	}
	return true
}
