package fixwork

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The owner rows (KVPRFixOwner) magnum's engine writes with this build, by
// FixOwner.Value, which the factory decodes across processes with the same
// type: testdata/owner holds each row as this build writes it, never written
// again, so a changed JSON name, omitempty or omitzero fails here before a
// factory reads a row it cannot tell from no row.
func TestOwnerRowsWrittenByThisBuildDecodeInFixwork(t *testing.T) {
	flip := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	rows := map[string]FixOwner{
		"factory.json":       {Owner: OwnerFactory, At: flip, By: "magnum fix --handover"},
		"factory-no-by.json": {Owner: OwnerFactory, At: flip},
		"returning.json":     {Owner: OwnerReturning, At: flip.Add(75 * time.Minute), By: "magnum fix --handover back"},
	}
	files, err := filepath.Glob(filepath.Join("testdata", "owner", "*.json"))
	if err != nil || len(files) != len(rows) {
		t.Fatalf("testdata/owner holds %d files (%v), want %d", len(files), err, len(rows))
	}
	for name, want := range rows {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "owner", name))
			if err != nil {
				t.Fatal(err)
			}
			row := strings.TrimSuffix(string(raw), "\n")
			got, err := want.Value()
			if err != nil || got != row {
				t.Errorf("this build writes %s (%v), the fixture holds %s", got, err, row)
			}
			dec := json.NewDecoder(bytes.NewReader([]byte(row)))
			dec.DisallowUnknownFields()
			var back FixOwner
			if err := dec.Decode(&back); err != nil || back != want {
				t.Errorf("decoded %+v (%v), want %+v", back, err, want)
			}
			if o, err := ParseFixOwner(row); err != nil || o != want || o.Factory() != (want.Owner == OwnerFactory) {
				t.Errorf("ParseFixOwner = %+v, %v, want %+v", o, err, want)
			}
		})
	}
}

// A row that is no owner is a failed read, never no row: no JSON, an owner
// this build does not know (a newer one's), none, or no time.
func TestARowThatIsNoOwnerIsAFailedRead(t *testing.T) {
	for _, raw := range []string{"", "factory", "{", `{"owner":"magnum","at":"2026-11-13T09:30:00Z"}`, `{"at":"2026-11-13T09:30:00Z"}`,
		`{"owner":"factory"}`} {
		if o, err := ParseFixOwner(raw); err == nil {
			t.Errorf("ParseFixOwner(%q) = %+v, want an error", raw, o)
		}
	}
	if _, err := (FixOwner{Owner: "magnum", At: time.Now()}).Value(); err == nil {
		t.Error("Value wrote an owner this build does not know")
	}
}
