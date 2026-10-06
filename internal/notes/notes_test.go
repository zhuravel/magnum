package notes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var limits = Limits{MaxBytes: 100, MaxLine: 20, MaxHarnessFiles: 2, MaxHarnessBytes: 10}

func testRepo(t *testing.T) Repo {
	t.Helper()
	return Repo{Root: filepath.Join(t.TempDir(), "notes"), Owner: "talkable", Name: "talkable"}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each limit is measured and named when passed; a missing notes file and
// harness measure zero.
func TestMeasureNamesEveryLimitPassed(t *testing.T) {
	r := testRepo(t)
	if s, files, err := Measure(r, limits); err != nil || s.Exists || len(files) != 0 || len(s.Over(limits)) != 0 {
		t.Fatalf("nothing on disk: %+v %v %v", s, files, err)
	}
	write(t, r.Notes(), "# Notes\n"+strings.Repeat("é", 21)+"\nshort\n")
	write(t, filepath.Join(r.Harness(), "run.sh"), "12345")
	s, _, err := Measure(r, limits)
	if err != nil {
		t.Fatal(err)
	}
	if s.Lines != 3 || s.LongLines != 1 || s.LongestLine != 21 || s.HarnessFiles != 1 || s.HarnessBytes != 5 {
		t.Errorf("size = %+v", s)
	}
	if got := s.Over(limits); !slices.Equal(got, []string{LimitLine}) {
		t.Errorf("over = %v, want max_line only (21 characters, not bytes)", got)
	}
	write(t, r.Notes(), strings.Repeat("x\n", 60))
	write(t, filepath.Join(r.Harness(), "qa", "smoke.rb"), "123456")
	write(t, filepath.Join(r.Harness(), "lint.sh"), "1")
	s, files, err := Measure(r, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Over(limits); !slices.Equal(got, []string{LimitBytes, LimitHarnessFiles, LimitHarnessBytes}) {
		t.Errorf("over = %v", got)
	}
	if names := []string{files[0].Name, files[1].Name, files[2].Name}; !slices.Equal(names, []string{"lint.sh", "qa/smoke.rb", "run.sh"}) {
		t.Errorf("listing = %v", names)
	}
}

// A symbolic link in the harness is neither followed nor listed.
func TestListSkipsSymbolicLinks(t *testing.T) {
	r := testRepo(t)
	write(t, filepath.Join(r.Harness(), "run.sh"), "x")
	outside := filepath.Join(t.TempDir(), "secret")
	write(t, outside, "token")
	if err := os.Symlink(outside, filepath.Join(r.Harness(), "link")); err != nil {
		t.Fatal(err)
	}
	files, err := List(r.Harness())
	if err != nil || len(files) != 1 || files[0].Name != "run.sh" {
		t.Fatalf("List = %+v, %v", files, err)
	}
}

// WriteState replaces the notes and the whole harness; ReadState reads back
// the same state.
func TestWriteStateReplacesNotesAndHarness(t *testing.T) {
	r := testRepo(t)
	write(t, r.Notes(), "old\n")
	write(t, filepath.Join(r.Harness(), "gone.sh"), "x")
	s := State{Exists: true, Notes: []byte("new\n"), Files: []Blob{{Path: "qa/smoke.rb", Body: []byte("ok")}, {Path: "run.sh", Body: []byte("bin/rspec")}}}
	for i := range s.Files {
		s.Files[i].SHA256 = TextSHA(s.Files[i].Body)
	}
	if err := WriteState(r, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Same(s) || string(got.Notes) != "new\n" {
		t.Fatalf("read back %+v", got)
	}
	if _, err := os.Stat(filepath.Join(r.Harness(), "gone.sh")); !os.IsNotExist(err) {
		t.Errorf("the old harness file is still there: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(r.Notes())); len(ents) != 2 { // talkable.md and talkable/
		t.Errorf("leftovers next to the notes: %v", ents)
	}
	if err := WriteState(r, State{Files: []Blob{{Path: "../escape", Body: []byte("x")}}}); err == nil {
		t.Error("a harness path outside the directory was written")
	}
}

// Lock is the judges' mkdir lock: a second holder waits and gives up with
// ErrBusy; a lock older than ten minutes is taken over.
func TestLockFollowsTheJudgesProtocol(t *testing.T) {
	r := testRepo(t)
	lockSleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	t.Cleanup(func() {
		lockSleep = func(ctx context.Context, d time.Duration) error { time.Sleep(d); return ctx.Err() }
	})
	unlock, err := Lock(context.Background(), r.Lock(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(context.Background(), r.Lock(), 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Lock = %v, want ErrBusy", err)
	}
	unlock()
	if _, err := os.Stat(r.Lock()); !os.IsNotExist(err) {
		t.Fatalf("unlock left %s: %v", r.Lock(), err)
	}
	if err := os.Mkdir(r.Lock(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(r.Lock(), old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err = Lock(context.Background(), r.Lock(), 0)
	if err != nil {
		t.Fatalf("a stale lock was not taken over: %v", err)
	}
	unlock()
}

// proposal builds a scratch directory holding base and a curator's output.
func proposal(t *testing.T, base State, notes string, files map[string]string, changes Changes) Proposal {
	t.Helper()
	s, err := PrepareScratch(filepath.Join(t.TempDir(), "c1"), base, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.Harness()); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		write(t, filepath.Join(s.Harness(), filepath.FromSlash(name)), body)
	}
	write(t, s.Proposal(), notes)
	b, _ := json.Marshal(changes)
	write(t, s.Changes(), string(b))
	p, problems := ReadProposal(s)
	if len(problems) > 0 {
		t.Fatalf("ReadProposal: %v", problems)
	}
	return p
}

func baseState() State {
	s := State{Exists: true, Notes: []byte("# Notes for talkable/talkable\n\n## Tests\nRun bin/rspec.\n"), Files: []Blob{
		{Path: "campaign_snapshot_probes_spec.rb", Body: []byte("probe")},
		{Path: "run_spec.sh", Body: []byte("bin/rspec \"$@\"\n")},
	}}
	for i := range s.Files {
		s.Files[i].SHA256 = TextSHA(s.Files[i].Body)
	}
	return s
}

const goodNotes = "# Notes for talkable/talkable (updated 2026-10-06)\n\n## Tests\nRun `run_spec.sh <spec>`.\n\n## QA\nSmoke a change with `qa/smoke.rb <path>`.\n"

func goodChanges() Changes {
	return Changes{
		Sections: []Change{
			{Name: "Tests", Action: ActionKept, Reason: "every review runs the specs of the files it reads"},
			{Name: "QA", Action: ActionAdded, Reason: "a generic smoke check any change can use"},
		},
		Files: []Change{
			{Name: "run_spec.sh", Action: ActionKept, Reason: "runs any spec with the checkout's Ruby"},
			{Name: "qa/smoke.rb", Action: ActionAdded, Reason: "parameterized smoke check for any model"},
			{Name: "campaign_snapshot_probes_spec.rb", Action: ActionMerged, Into: "qa/smoke.rb", Reason: "its general part is the smoke check"},
		},
	}
}

func goodFiles() map[string]string {
	return map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "qa/smoke.rb": "puts ARGV.first\n"}
}

// A proposal that keeps only general knowledge, accounts for every section
// and file with a reason and names every harness file is valid, whatever
// its size.
func TestValidateAcceptsAGeneralProposalOfAnySize(t *testing.T) {
	big := goodNotes + strings.Repeat("- a durable pitfall that helps every future review of this repository\n", 400)
	p := proposal(t, baseState(), big, goodFiles(), goodChanges())
	if problems := Validate(p, Check{Base: baseState(), PRNumbers: []int{11920}, Branches: []string{"feature/coupon-expiry"}}); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

// Validation rejects a proposal whose kept items lack reasons, or that names
// a pull request, a branch or one pull request's probe, or carries a secret
// or a home directory path, or leaves a harness file unnamed.
func TestValidateRejectsOnePullRequestsContent(t *testing.T) {
	check := Check{Base: baseState(), PRNumbers: []int{11920}, Branches: []string{"feature/coupon-expiry", "main"}}
	cases := []struct {
		name  string
		edit  func(notes *string, files map[string]string, ch *Changes)
		wants string
	}{
		{"a kept file without a reason", func(_ *string, _ map[string]string, ch *Changes) { ch.Files[0].Reason = "" }, `file "run_spec.sh" in changes.json has no reason`},
		{"a kept section without a reason", func(_ *string, _ map[string]string, ch *Changes) { ch.Sections[0].Reason = " " }, `section "Tests" in changes.json has no reason`},
		{"a section missing from changes", func(_ *string, _ map[string]string, ch *Changes) { ch.Sections = ch.Sections[1:] }, `section "Tests" of proposal.md is not in changes.json's sections`},
		{"a reason on two lines", func(_ *string, _ map[string]string, ch *Changes) { ch.Files[1].Reason = "one\ntwo" }, "the reason is not one line"},
		{"a pull request number", func(n *string, _ map[string]string, _ *Changes) { *n += "- see #11920 for the fix\n" }, "proposal.md names a pull request or issue number"},
		{"PR 123 in a reason", func(_ *string, _ map[string]string, ch *Changes) { ch.Files[0].Reason = "added for PR 4521" }, "a reason in changes.json names a pull request"},
		{"a branch name", func(n *string, _ map[string]string, _ *Changes) { *n += "- check out feature/coupon-expiry first\n" }, "names a pull request's branch"},
		{"a probe kept", func(n *string, f map[string]string, ch *Changes) {
			f["brand_review_probes_spec.rb"] = "x"
			*n += "- `brand_review_probes_spec.rb` probes brands\n"
			ch.Files = append(ch.Files, Change{Name: "brand_review_probes_spec.rb", Action: ActionAdded, Reason: "probes"})
		}, "harness file brand_review_probes_spec.rb is one pull request's probe"},
		{"a file named after a PR number", func(n *string, f map[string]string, ch *Changes) {
			f["check_11920.sh"] = "x"
			*n += "- `check_11920.sh` checks\n"
			ch.Files = append(ch.Files, Change{Name: "check_11920.sh", Action: ActionAdded, Reason: "checks"})
		}, "harness file check_11920.sh is one pull request's probe"},
		{"the deleted probe still named", func(n *string, _ map[string]string, _ *Changes) {
			*n += "- campaign_snapshot_probes_spec.rb is gone\n"
		}, "names a probe file"},
		{"an unnamed harness file", func(n *string, _ map[string]string, _ *Changes) {
			*n = strings.Replace(*n, "`qa/smoke.rb <path>`", "the smoke script", 1)
		}, "harness file qa/smoke.rb is not named in proposal.md"},
		{"a current file unaccounted for", func(_ *string, _ map[string]string, ch *Changes) { ch.Files = ch.Files[:2] }, "current harness file campaign_snapshot_probes_spec.rb is not in changes.json"},
		{"a secret in a script", func(_ *string, f map[string]string, _ *Changes) {
			f["run_spec.sh"] = "GH_TOKEN=ghp_" + strings.Repeat("a", 36) + " bin/rspec\n"
		}, "harness/run_spec.sh holds what looks like a secret"},
		{"a home directory path", func(n *string, _ map[string]string, _ *Changes) {
			*n += "- Ruby: env PATH=/Users/someone/.local/share/mise/installs/ruby/3.3/bin:$PATH\n"
		}, "proposal.md names a home directory path"},
		{"no change at all", nil, "the proposal changes nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notes, files, ch := goodNotes, goodFiles(), goodChanges()
			if tc.edit == nil {
				base := baseState()
				notes = string(base.Notes)
				files = map[string]string{}
				for _, b := range base.Files {
					files[b.Path] = string(b.Body)
				}
			} else {
				tc.edit(&notes, files, &ch)
			}
			problems := Validate(proposal(t, baseState(), notes, files, ch), check)
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.wants) }) {
				t.Errorf("problems %q lack %q", problems, tc.wants)
			}
		})
	}
}

// Only a file name or a path with "probe" in it names a probe file: a
// browser-probe technique's JavaScript global or a constant that says
// PROBE is no file, while a probe script by its name or its path still is.
func TestValidateTellsProbeFilesFromProbeIdentifiers(t *testing.T) {
	check := Check{Base: baseState(), PRNumbers: []int{11920}}
	identifiers := goodNotes + "\n## Browser QA\nSet `window.PROBE_SELECTOR` to the element under test; raise `PROBE_TIMEOUT` " +
		"(or `MY_PROBE_TIMEOUT`, `window.PROBE`) for a slow page, and `probe.enabled` turns it on.\n"
	ch := goodChanges()
	ch.Sections = append(ch.Sections, Change{Name: "Browser QA", Action: ActionAdded, Reason: "a generic way to probe any page"})
	if problems := Validate(proposal(t, baseState(), identifiers, goodFiles(), ch), check); len(problems) != 0 {
		t.Errorf("identifiers taken for probe files: %q", problems)
	}
	for _, name := range []string{"qa/probe_coupons.rb", "coupon_probe.sh", "spec/probes/coupon_spec.rb", "probes/"} {
		text := goodNotes + "\n## Browser QA\nRun `" + name + "` first.\n"
		problems := Validate(proposal(t, baseState(), text, goodFiles(), ch), check)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "proposal.md names a probe file") }) {
			t.Errorf("%s: problems %q lack the probe file", name, problems)
		}
	}
}

// What the curator did not write, or wrote outside plain files, is a
// problem of the proposal.
func TestReadProposalNamesWhatIsMissing(t *testing.T) {
	s, err := PrepareScratch(filepath.Join(t.TempDir(), "c1"), baseState(), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(s.Harness(), "passwd")); err != nil {
		t.Fatal(err)
	}
	_, problems := ReadProposal(s)
	for _, want := range []string{"proposal.md was not written", "changes.json was not written", "harness/passwd is not a regular file"} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
			t.Errorf("problems %q lack %q", problems, want)
		}
	}
	// The scratch holds the current state twice: read only, and as the
	// harness the curator edits.
	if b, err := os.ReadFile(filepath.Join(s.CurrentHarness(), "run_spec.sh")); err != nil || string(b) != "bin/rspec \"$@\"\n" {
		t.Errorf("current/run_spec.sh = %q, %v", b, err)
	}
	if b, err := os.ReadFile(s.Current()); err != nil || !strings.HasPrefix(string(b), "# Notes for") {
		t.Errorf("current.md = %q, %v", b, err)
	}
}
