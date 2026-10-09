package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A negative duration means nothing any key wants: [[pool]]
// idle_remove_after = "-1h" loaded and removed every surplus slot at once.
// Every duration key of every section and block (watch, pool, repo, role)
// is refused when negative, with an error naming the key; a new duration
// key without a check fails here.
func TestEveryNegativeDurationIsRefused(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[repo]]
repo = "talkable/talkable"
ready_timeout = "5m"
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	var keys []string
	walkDurations(reflect.ValueOf(cfg).Elem(), "", func(where, key string, v reflect.Value) {
		keys = append(keys, where+key)
		old := reflect.ValueOf(v.Interface())
		neg := Duration{-time.Hour}
		if v.Kind() == reflect.Pointer {
			v.Set(reflect.ValueOf(&neg))
		} else {
			v.Set(reflect.ValueOf(neg))
		}
		defer v.Set(old)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s%s = -1h: Validate = %v, want an error naming %s", where, key, err, key)
		}
	})
	for _, want := range []string{"pool.idle_remove_after", "pool.ready_timeout", "repo.ready_timeout", "watch.related_lookback",
		"daemon.poll_interval", "pipeline.judge_fresh_after", "role.timeout", "learn.settle", "board.recent_closed"} {
		if !slices.Contains(keys, want) {
			t.Errorf("no %s among the duration keys walked: %v", want, keys)
		}
	}
}

// walkDurations calls visit with every settable Duration and *Duration in v,
// named by the TOML key of its section or block (where, "pool.") and its own
// (key); a list contributes its first element.
func walkDurations(v reflect.Value, where string, visit func(where, key string, v reflect.Value)) {
	durType := reflect.TypeFor[Duration]()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if !f.IsExported() || tag == "" || tag == "-" {
			continue
		}
		fv := v.Field(i)
		switch {
		case f.Type == durType || (f.Type.Kind() == reflect.Pointer && f.Type.Elem() == durType):
			visit(where, tag, fv)
		case f.Type.Kind() == reflect.Struct:
			walkDurations(fv, tag+".", visit)
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct && fv.Len() > 0:
			walkDurations(fv.Index(0), tag+".", visit)
		}
	}
}
