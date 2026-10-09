package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum"
)

// Defaults() is what tests, tools and a base file without a section start
// from; config.defaults.toml is what the daemon runs on. They are two writings
// of the same values, so every key the committed file sets, in every section
// that is plain data, equals the Go literal: a default changed in one place
// only fails here instead of making a test pass on a configuration no
// installed binary has. A key the file only mentions in a comment keeps the
// Go value (that is how a documented default is spelled), so it is not
// compared. (Kinds and roles are compared in TestDefaultsEqualExplicitConfig,
// the lists of identities, watches, pools and repos are empty in both.)
func TestGoDefaultsEqualTheEmbeddedDefaultsFile(t *testing.T) {
	var fromFile Config
	md, err := toml.Decode(string(magnum.DefaultConfig), &fromFile)
	if err != nil {
		t.Fatal(err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		t.Fatalf("config.defaults.toml has unknown keys: %v", undecoded)
	}
	fromGo := Defaults()

	checked := 0
	cfgType := reflect.TypeOf(fromGo).Elem()
	for i := range cfgType.NumField() {
		field := cfgType.Field(i)
		section := field.Tag.Get("toml")
		if !field.IsExported() || field.Type.Kind() != reflect.Struct || section == "" || section == "-" {
			continue
		}
		goSection := reflect.ValueOf(fromGo).Elem().Field(i)
		fileSection := reflect.ValueOf(&fromFile).Elem().Field(i)
		for j := range field.Type.NumField() {
			key := field.Type.Field(j)
			name, _, _ := strings.Cut(key.Tag.Get("toml"), ",")
			if !key.IsExported() || name == "" || name == "-" || !md.IsDefined(section, name) {
				continue
			}
			checked++
			got, want := goSection.Field(j).Interface(), fileSection.Field(j).Interface()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("[%s] %s: Defaults() has %s, config.defaults.toml has %s", section, key.Name, show(got), show(want))
			}
		}
	}
	if checked < 40 { // the walk must reach the sections, not silently skip them
		t.Fatalf("compared only %d keys; the walk over the config sections is broken", checked)
	}
}

// show prints a value with a nil slice or map told from an empty one, so a
// difference like "" against "unicode" or nil against [] reads plainly.
func show(v any) string {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map {
		if rv.IsNil() {
			return "nil"
		}
	}
	return fmt.Sprintf("%#v", v)
}

// The terminal a bare Defaults() config opens panes in is the committed
// file's: Terminal.app, and the unicode icon set.
func TestDefaultTerminalIsTheCommittedOne(t *testing.T) {
	d := Defaults().Terminal
	if d.App != "Terminal" || d.Icons != "unicode" {
		t.Fatalf("Defaults().Terminal = app %q, icons %q; config.defaults.toml says Terminal and unicode", d.App, d.Icons)
	}
}
