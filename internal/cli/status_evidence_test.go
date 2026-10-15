package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// The card says how much of the PR its rounds examined: the evidence map's
// behaviours by state, and the files no round ever read (a stale entry
// counts as what it was); --json carries the counts. A PR without a map has
// no line.
func TestTheCardCountsTheEvidence(t *testing.T) {
	f, _, d, _ := statusFixture(t)
	card := func() (string, *statusDetail) {
		t.Helper()
		r, err := statusGather(context.Background(), d, statusOptions{Ref: "11920"})
		if err != nil || r.Detail == nil {
			t.Fatalf("gather: %v %+v", err, r.Detail)
		}
		var b bytes.Buffer
		statusRenderDetail(&b, *r.Detail, r.GeneratedAt)
		return b.String(), r.Detail
	}
	if out, det := card(); strings.Contains(out, "evidence:") || det.Evidence != nil {
		t.Fatalf("card without a map:\n%s", out)
	}

	entry := func(path, behaviour, state, was string) engine.EvidenceMapEntry {
		return engine.EvidenceMapEntry{EvidenceEntry: pipeline.EvidenceEntry{Path: path, Behaviour: behaviour, State: state}, Was: was, Head: "b1", Round: 1}
	}
	m := engine.EvidenceMap{PR: "talkable/talkable#11920", HeadSHA: "b2", Round: 2, Entries: []engine.EvidenceMapEntry{
		entry("app/a.rb", "a reward keeps its variant", pipeline.EvidenceProbed, ""),
		entry("app/a.rb", "a closed campaign is refused", pipeline.EvidenceProbed, ""),
		entry("app/b.rb", "the guard", pipeline.EvidenceRead, ""),
		entry("app/c.rb", "", pipeline.EvidenceUnknown, ""),
		entry("app/d.rb", "", pipeline.EvidenceUnknown, ""),
		entry("app/d.rb", "the retry", pipeline.EvidenceSkipped, ""),
		entry("app/e.rb", "the restore", pipeline.EvidenceStale, pipeline.EvidenceProbed),
	}}
	path := pipeline.EvidenceMapPath(f.Ctx.Layout, "talkable", "talkable", 11920)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, det := card()
	if want := "\n  evidence:  2 behaviours probed, 1 read, 1 skipped, 2 unknown, 1 stale (2 files never read)\n"; !strings.Contains(out, want) {
		t.Errorf("card does not say %q:\n%s", want, out)
	}
	want := engine.EvidenceCounts{Probed: 2, Read: 1, Skipped: 1, Unknown: 2, Stale: 1, FilesNeverRead: 2}
	if det.Evidence == nil || *det.Evidence != want {
		t.Errorf("Evidence = %+v, want %+v", det.Evidence, want)
	}

	if code := f.run("status", "11920", "--json"); code != 0 {
		t.Fatalf("status --json: exit %d: %s", code, f.Err)
	}
	var rep struct {
		Detail struct {
			Evidence *engine.EvidenceCounts `json:"evidence"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &rep); err != nil || rep.Detail.Evidence == nil || *rep.Detail.Evidence != want {
		t.Errorf("status --json evidence: %+v %v\n%s", rep.Detail.Evidence, err, f.Out)
	}
}
