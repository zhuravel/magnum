package prstatus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// sample is a status with every field set.
func sample() Status {
	return Status{
		UpdatedAt:   time.Date(2026, 11, 7, 14, 5, 9, 0, time.UTC),
		TaskID:      42,
		Jira:        Jira{Key: "ABC-123", Status: JiraInCodeReview},
		Plan:        Plan{QuestionsAsked: 3, QuestionsAnswered: 2},
		Code:        Code{Commits: 4, Proof: ProofRedToGreen, Specs: 2},
		LocalReview: LocalReview{Rounds: 2, Verdict: VerdictClean, NotesOpen: 1},
		PublishedAt: time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC),
		FixLoop:     FixLoop{State: FixRunning, Cycle: 1, Pushes: 1, ThreadsAnswered: 3},
		Staging:     Staging{Env: "staging-a", State: StagingDeployed},
		Stagings:    []Staging{{Env: "staging-a", State: StagingDeployed}, {Env: "beta", State: StagingDeploying}, {State: StagingFailed}},
		Now:         Now{Who: WhoReviewers, Stage: StagePublished},
	}
}

// zero reports whether s is the zero status, what Read gives for a file it
// refuses or does not find.
func zero(s Status) bool { return reflect.DeepEqual(s, Status{}) }

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	return paths.Layout{DataDir: t.TempDir(), StateDir: t.TempDir()}
}

// statusPath is the status file of example-org/example-repo#7 in a fresh layout.
func statusPath(t *testing.T) string {
	t.Helper()
	p, err := Path(testLayout(t), "example-org", "example-repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if p == "" {
		t.Fatal("Path gave no path for a valid PR")
	}
	return p
}

func mustWrite(t *testing.T, path string, s Status) {
	t.Helper()
	changed, err := Write(path, s)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first Write reported no change")
	}
}

func readOK(t *testing.T, path string) Status {
	t.Helper()
	s, ok, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Read found no status")
	}
	return s
}

// load decodes the file as the object it holds, numbers kept as written.
func load(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// sum is the digest rule, restated: the hex sha256 of the object's JSON as
// encoding/json writes a map (keys sorted, no spaces) without updated_at and
// digest.
func sum(t *testing.T, m map[string]any) string {
	t.Helper()
	c := map[string]any{}
	for k, v := range m {
		if k != "updated_at" && k != "digest" {
			c[k] = v
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// store writes m as the file, its digest made right first when fix says so.
func store(t *testing.T, path string, m map[string]any, fix bool) {
	t.Helper()
	if fix {
		m["digest"] = sum(t, m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// reversed encodes v with every object's keys in reverse order.
func reversed(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	var enc func(v any)
	enc = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
			return
		}
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range m {
				if !yield(k) {
					return
				}
			}
		})
		slices.Reverse(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			enc(m[k])
		}
		buf.WriteByte('}')
	}
	enc(v)
	return buf.Bytes()
}

func TestWriteThenReadGivesTheStatusBack(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	// Times are kept in UTC to the second, whatever zone and precision the
	// writer hands over.
	s.UpdatedAt = time.Date(2026, 11, 7, 16, 5, 9, 123456789, time.FixedZone("UTC+2", 2*3600))
	s.PublishedAt = time.Date(2026, 11, 7, 15, 0, 0, 999, time.FixedZone("UTC+2", 2*3600))
	mustWrite(t, path, s)
	got := readOK(t, path)
	want := sample()
	want.V = Version
	want.Digest = got.Digest
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read gave\n%+v\nwant\n%+v", got, want)
	}
	if d := sum(t, load(t, path)); got.Digest != d {
		t.Errorf("digest = %q, want the rule's %q", got.Digest, d)
	}
}

func TestATaskWithoutAJiraIssueHasTheZeroJira(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	s.Jira = Jira{}
	mustWrite(t, path, s)
	if got := readOK(t, path).Jira; got != (Jira{}) {
		t.Errorf("Jira = %+v, want the zero Jira", got)
	}
}

// A Jira issue's status maps to the file's words by Jira's category of it:
// new is to_do and done is done; indeterminate is in_code_review when the
// status's name says review, in any case, in_development when it says
// development or progress (devWords), and other for the rest of the work
// in between (QA, a deploy, a block), which "in development" would tell
// the reviewer falsely; any other category, an unknown one ("") included,
// is other. Every word it gives is
// one Write takes.
func TestJiraStatusOfMapsTheCategoryAndTheName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		category, name string
		want           JiraStatus
	}{
		{"new", "To Do", JiraToDo},
		{"new", "Backlog", JiraToDo},
		{"new", "Ready for review", JiraToDo},
		{"indeterminate", "In Progress", JiraInDevelopment},
		{"indeterminate", "In Development", JiraInDevelopment},
		{"indeterminate", "In Review", JiraInCodeReview},
		{"indeterminate", "CODE REVIEW", JiraInCodeReview},
		{"indeterminate", "Peer-reviewing", JiraInCodeReview},
		{"indeterminate", "Doing", JiraInDevelopment},
		{"indeterminate", "In Dev", JiraInDevelopment},
		{"indeterminate", "Implementing", JiraInDevelopment},
		{"indeterminate", "Work in progress", JiraInDevelopment},
		{"indeterminate", "In QA", JiraOther},
		{"indeterminate", "Testing", JiraOther},
		{"indeterminate", "Ready to Deploy", JiraOther},
		{"indeterminate", "Blocked", JiraOther},
		{"indeterminate", "On Hold", JiraOther},
		{"done", "Done", JiraDone},
		{"done", "Won't do", JiraDone},
		{"done", "Reviewed", JiraDone},
		{"undefined", "Triage", JiraOther},
		{"", "In Review", JiraOther},
		{"Indeterminate", "In Progress", JiraOther},
	} {
		got := JiraStatusOf(tc.category, tc.name)
		if got != tc.want {
			t.Errorf("JiraStatusOf(%q, %q) = %q, want %q", tc.category, tc.name, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("JiraStatusOf(%q, %q) = %q, a word Write refuses", tc.category, tc.name, got)
		}
	}
}

func TestTheDigestIgnoresKeyOrderAndUpdatedAt(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	first := readOK(t, path).Digest

	// The same object with its keys backwards at every level reads, with the
	// same digest.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	back := reversed(t, load(t, path))
	if bytes.Equal(bytes.TrimSpace(before), back) {
		t.Fatal("the reversed file is the file itself")
	}
	if err := os.WriteFile(path, back, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readOK(t, path).Digest; got != first {
		t.Errorf("digest after reordering the keys = %q, want %q", got, first)
	}

	later := sample()
	later.UpdatedAt = later.UpdatedAt.Add(time.Hour)
	other := statusPath(t)
	mustWrite(t, other, later)
	if got := readOK(t, other).Digest; got != first {
		t.Errorf("digest of another updated_at = %q, want %q", got, first)
	}

	moved := sample()
	moved.Code.Commits++
	third := statusPath(t)
	mustWrite(t, third, moved)
	if got := readOK(t, third).Digest; got == first {
		t.Errorf("a changed commit count kept the digest %q", got)
	}
}

func TestAnUnchangedStatusIsNotWrittenAgain(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	mustWrite(t, path, s)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	s.UpdatedAt = s.UpdatedAt.Add(10 * time.Minute)
	changed, err := Write(path, s)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("Write of the same content reported a change")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the file changed:\n%s\nwas\n%s", after, before)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if !st.ModTime().Equal(old) {
		t.Errorf("the file was written again (mtime %v)", st.ModTime())
	}

	s.FixLoop.Pushes++
	if changed, err := Write(path, s); err != nil || !changed {
		t.Fatalf("Write of new content = %v, %v; want true, nil", changed, err)
	}
	got := readOK(t, path)
	if got.FixLoop.Pushes != s.FixLoop.Pushes || !got.UpdatedAt.Equal(s.UpdatedAt) {
		t.Errorf("Read gave pushes %d at %v, want %d at %v", got.FixLoop.Pushes, got.UpdatedAt, s.FixLoop.Pushes, s.UpdatedAt)
	}
}

func TestWriteStampsAZeroUpdatedAtWithTheTimeOfTheWrite(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	s.UpdatedAt = time.Time{}
	before := time.Now().UTC().Truncate(time.Second)
	mustWrite(t, path, s)
	after := time.Now().UTC()
	got := readOK(t, path).UpdatedAt
	if got.Before(before) || got.After(after) || got.Location() != time.UTC {
		t.Errorf("updated_at = %v, want the write's time in UTC, between %v and %v", got, before, after)
	}
}

func TestWriteReplacesAFileReadRefuses(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	if err := os.WriteFile(path, []byte("{\"v\":"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, sample())
	readOK(t, path)
}

// The config takes any staging name that is not blank; one that is not a
// slug goes into the file as "", and the status is still written, its
// staging state with it.
func TestAStagingNameThatIsNotASlugIsWrittenWithoutAName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"alpha":                 "alpha",
		"staging-a":             "staging-a",
		strings.Repeat("a", 32): strings.Repeat("a", 32),
		"Alpha":                 "",
		"staging_1":             "",
		"qa env":                "",
		" alpha":                "",
		strings.Repeat("a", 33): "",
		"":                      "",
	} {
		if got := StagingEnv(name); got != want {
			t.Errorf("StagingEnv(%q) = %q, want %q", name, got, want)
		}
		path := statusPath(t)
		s := sample()
		s.Staging = Staging{Env: StagingEnv(name), State: StagingDeploying}
		mustWrite(t, path, s)
		if got := readOK(t, path).Staging; got != s.Staging {
			t.Errorf("staging of %q = %+v, want %+v", name, got, s.Staging)
		}
	}
}

// Stagings are the change's deploy to each staging environment, in the
// config's order, under v=1 beside Staging: they come back as written, and
// a status without them writes no stagings key, so the file of a writer
// before them keeps its digest and is not written again.
func TestStagingsComeBackInOrderAndAStatusWithoutThemWritesNoKey(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	mustWrite(t, path, s)
	if got := readOK(t, path).Stagings; !slices.Equal(got, s.Stagings) {
		t.Errorf("stagings = %+v, want %+v in their order", got, s.Stagings)
	}
	if _, ok := load(t, path)["stagings"]; !ok {
		t.Error("the file has no stagings key")
	}

	path = statusPath(t)
	s.Stagings = nil
	mustWrite(t, path, s)
	if _, ok := load(t, path)["stagings"]; ok {
		t.Error("a status without stagings wrote the key: an older writer's file would change its digest")
	}
	if got := readOK(t, path); got.Stagings != nil {
		t.Errorf("stagings = %+v, want none", got.Stagings)
	}
}

// A reader from before Stagings takes a file that has them: it ignores the
// field, the digest it computes covers it as it covers every key, and the
// staging row it renders is the first environment's, in a word it knows.
func TestAReaderBeforeStagingsTakesAFileWithThem(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var old struct { // the fields such a reader checks here, by its own rules
		V       int     `json:"v"`
		Digest  string  `json:"digest"`
		Staging Staging `json:"staging"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("the old fields do not decode: %v", err)
	}
	if old.V != Version || old.Digest != sum(t, load(t, path)) {
		t.Errorf("v %d, digest %q: an old reader refuses the file (want v %d and the rule's digest)", old.V, old.Digest, Version)
	}
	if first := sample().Stagings[0]; old.Staging != first || !old.Staging.State.Valid() {
		t.Errorf("staging = %+v, want the first environment's %+v", old.Staging, first)
	}
}

func TestWriteRefusesAnInvalidStatusAndWritesNothing(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	s := sample()
	s.Code.Proof = "nonsense"
	changed, err := Write(path, s)
	if !errors.Is(err, ErrInvalid) || changed {
		t.Errorf("Write = %v, %v; want false, ErrInvalid", changed, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Write made the folder of an invalid status (%v)", err)
	}
}

func TestReadRefusesAFileOver64KiB(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Spaces after the object keep it valid JSON: only the size differs.
	full := slices.Concat(b, bytes.Repeat([]byte(" "), MaxSize-len(b)))
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	readOK(t, path)
	if err := os.WriteFile(path, append(full, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Read(path)
	if !errors.Is(err, ErrTooBig) || ok || !zero(s) {
		t.Errorf("Read of %d bytes = %v, %v; want ErrTooBig", MaxSize+1, ok, err)
	}
}

func TestReadRefusesACutShortFile(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range [][]byte{b[:len(b)/2], b[:1], {}} {
		if err := os.WriteFile(path, cut, 0o600); err != nil {
			t.Fatal(err)
		}
		s, ok, err := Read(path)
		if !errors.Is(err, ErrCutShort) || ok || !zero(s) {
			t.Errorf("Read of %d of %d bytes = %v, %v; want ErrCutShort", len(cut), len(b), ok, err)
		}
	}
}

func TestReadRefusesAFileThatIsNotAJSONObject(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	for _, text := range []string{"not json", "[1]", `"v"`, "null", `{"v":1} {"v":1}`, `{"v":1} x`} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		s, ok, err := Read(path)
		if !errors.Is(err, ErrNotJSON) || ok || !zero(s) {
			t.Errorf("Read of %q = %v, %v; want ErrNotJSON", text, ok, err)
		}
	}
}

func TestReadRefusesAnUnknownVersion(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	for _, v := range []any{json.Number("2"), json.Number("0"), "1", nil} {
		m := load(t, path)
		if v == nil {
			delete(m, "v")
		} else {
			m["v"] = v
		}
		store(t, path, m, true)
		s, ok, err := Read(path)
		if !errors.Is(err, ErrVersion) || ok || !zero(s) {
			t.Errorf("Read of v %v = %v, %v; want ErrVersion", v, ok, err)
		}
		mustWrite(t, path, sample())
	}
}

func TestReadRefusesAWordOutsideItsEnum(t *testing.T) {
	t.Parallel()
	const word = "nonsense-word"
	for _, tc := range []struct {
		field string // the error names it
		set   func(m map[string]any)
	}{
		{"jira.status", func(m map[string]any) { sub(m, "jira")["status"] = word }},
		{"code.proof", func(m map[string]any) { sub(m, "code")["proof"] = word }},
		{"local_review.verdict", func(m map[string]any) { sub(m, "local_review")["verdict"] = word }},
		{"fix_loop.state", func(m map[string]any) { sub(m, "fix_loop")["state"] = word }},
		{"staging.state", func(m map[string]any) { sub(m, "staging")["state"] = word }},
		{"now.who", func(m map[string]any) { sub(m, "now")["who"] = word }},
		{"now.stage", func(m map[string]any) { sub(m, "now")["stage"] = word }},
		{"now.stage", func(m map[string]any) { sub(m, "now")["stage"] = "" }},
		{"jira.key", func(m map[string]any) { sub(m, "jira")["key"] = "abc-123" }},
		{"jira.key", func(m map[string]any) { sub(m, "jira")["key"] = "ABC" }},
		{"jira.status", func(m map[string]any) { sub(m, "jira")["status"] = "" }},
		{"jira.status", func(m map[string]any) { sub(m, "jira")["key"] = "" }},
		{"staging.env", func(m map[string]any) { sub(m, "staging")["env"] = "Staging A" }},
		{"staging.env", func(m map[string]any) { sub(m, "staging")["env"] = strings.Repeat("a", 33) }},
		{"stagings[1].state", func(m map[string]any) { staging(m, 1)["state"] = word }},
		{"stagings[2].state", func(m map[string]any) { staging(m, 2)["state"] = "" }},
		{"stagings[0].env", func(m map[string]any) { staging(m, 0)["env"] = "Staging A" }},
		{"plan.questions_asked", func(m map[string]any) { sub(m, "plan")["questions_asked"] = json.Number("-1") }},
		{"plan.questions_answered", func(m map[string]any) { sub(m, "plan")["questions_answered"] = json.Number("-1") }},
		{"code.commits", func(m map[string]any) { sub(m, "code")["commits"] = json.Number("-1") }},
		{"code.specs", func(m map[string]any) { sub(m, "code")["specs"] = json.Number("-1") }},
		{"local_review.rounds", func(m map[string]any) { sub(m, "local_review")["rounds"] = json.Number("-1") }},
		{"local_review.notes_open", func(m map[string]any) { sub(m, "local_review")["notes_open"] = json.Number("-1") }},
		{"fix_loop.cycle", func(m map[string]any) { sub(m, "fix_loop")["cycle"] = json.Number("-1") }},
		{"fix_loop.pushes", func(m map[string]any) { sub(m, "fix_loop")["pushes"] = json.Number("-1") }},
		{"fix_loop.threads_answered", func(m map[string]any) { sub(m, "fix_loop")["threads_answered"] = json.Number("-1") }},
		{"code.commits", func(m map[string]any) { sub(m, "code")["commits"] = word }},
		{"task_id", func(m map[string]any) { m["task_id"] = json.Number("0") }},
		{"published_at", func(m map[string]any) { m["published_at"] = "0001-01-01T00:00:00Z" }},
		// updated_at is outside the digest, so only validate can refuse a file
		// without it, which would say "updated 00:00 UTC" of year 1.
		{"updated_at", func(m map[string]any) { delete(m, "updated_at") }},
		{"updated_at", func(m map[string]any) { m["updated_at"] = nil }},
		{"updated_at", func(m map[string]any) { m["updated_at"] = "0001-01-01T00:00:00Z" }},
	} {
		path := statusPath(t)
		mustWrite(t, path, sample())
		m := load(t, path)
		tc.set(m)
		store(t, path, m, true)
		s, ok, err := Read(path)
		if !errors.Is(err, ErrInvalid) || ok || !zero(s) {
			t.Errorf("%s: Read = %v, %v; want ErrInvalid", tc.field, ok, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the error %q does not name the field", tc.field, err)
		}
		if strings.Contains(err.Error(), word) {
			t.Errorf("%s: the error %q repeats the file's word", tc.field, err)
		}
	}
}

func sub(m map[string]any, key string) map[string]any { return m[key].(map[string]any) }

// staging is entry i of the file's stagings.
func staging(m map[string]any, i int) map[string]any {
	return m["stagings"].([]any)[i].(map[string]any)
}

func TestEveryEnumAcceptsItsWordsOnly(t *testing.T) {
	t.Parallel()
	type valid interface{ Valid() bool }
	words := map[string][]valid{
		"JiraStatus":   {JiraToDo, JiraInDevelopment, JiraInCodeReview, JiraDone, JiraOther, JiraUnknown},
		"Proof":        {ProofNone, ProofRed, ProofRedToGreen, ProofGreen},
		"Verdict":      {VerdictNone, VerdictClean, VerdictFindings, VerdictFailed},
		"FixState":     {FixNone, FixRunning, FixNeedsYou, FixDone, FixOff},
		"StagingState": {StagingNone, StagingDeploying, StagingDeployed, StagingFailed},
		"Who":          {WhoYou, WhoMagnum, WhoReviewers, WhoCI, WhoFactory, WhoNone},
		"Stage":        {StagePlan, StageCode, StageReview, StageReady, StagePublished, StageDone, StageStopped, StageFailed},
	}
	others := map[string][]valid{
		"JiraStatus":   {JiraStatus(""), JiraStatus("To Do"), JiraStatus("blocked")},
		"Proof":        {Proof(""), Proof("RED"), Proof("green_to_red")},
		"Verdict":      {Verdict(""), Verdict("approved")},
		"FixState":     {FixState(""), FixState("paused")},
		"StagingState": {StagingState(""), StagingState("up")},
		"Who":          {Who(""), Who("someone")},
		"Stage":        {Stage(""), Stage("investigate"), Stage("coding")},
	}
	for name, ws := range words {
		for _, w := range ws {
			if !w.Valid() {
				t.Errorf("%s %q is not Valid", name, w)
			}
		}
		for _, w := range others[name] {
			if w.Valid() {
				t.Errorf("%s %q is Valid", name, w)
			}
		}
	}
}

func TestReadRefusesADigestThatDoesNotMatch(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(m map[string]any){
		"a count changed": func(m map[string]any) { sub(m, "code")["commits"] = json.Number("99") },
		"no digest":       func(m map[string]any) { delete(m, "digest") },
		"a new field":     func(m map[string]any) { m["later_field"] = json.Number("1") },
	} {
		path := statusPath(t)
		mustWrite(t, path, sample())
		m := load(t, path)
		change(m)
		store(t, path, m, false)
		s, ok, err := Read(path)
		if !errors.Is(err, ErrDigest) || ok || !zero(s) {
			t.Errorf("%s: Read = %v, %v; want ErrDigest", name, ok, err)
		}
	}
}

func TestReadIgnoresFieldsItDoesNotKnow(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	mustWrite(t, path, sample())
	want := readOK(t, path)
	m := load(t, path)
	m["later_field"] = map[string]any{"count": json.Number("3")}
	sub(m, "code")["later_count"] = json.Number("5")
	store(t, path, m, true)
	got := readOK(t, path)
	want.Digest = got.Digest
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read gave\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadOfAMissingFileIsAbsent(t *testing.T) {
	t.Parallel()
	path := statusPath(t)
	for _, p := range []string{path, filepath.Join(filepath.Dir(path), "8.json")} {
		s, ok, err := Read(p)
		if err != nil || ok || !zero(s) {
			t.Errorf("Read of a missing file = %+v, %v, %v; want the zero status, false, nil", s, ok, err)
		}
	}
	mustWrite(t, path, sample())
	s, ok, err := Read(filepath.Join(filepath.Dir(path), "8.json"))
	if err != nil || ok || !zero(s) {
		t.Errorf("Read of another PR's missing file = %+v, %v, %v; want absent", s, ok, err)
	}
}

func TestPathNamesTheFileOfAPullRequest(t *testing.T) {
	t.Parallel()
	l := testLayout(t)
	want := filepath.Join(l.FactoryPRStatus(), "example-org", "example.repo_1", "7.json")
	for _, name := range [][2]string{{"example-org", "example.repo_1"}, {"Example-Org", "Example.Repo_1"}} {
		got, err := Path(l, name[0], name[1], 7)
		if err != nil || got != want {
			t.Errorf("Path(%q, %q, 7) = %q, %v; want %q (GitHub names fold case)", name[0], name[1], got, err, want)
		}
	}
}

func TestPathRefusesWhatIsNotAGitHubRepoOrAPRNumber(t *testing.T) {
	t.Parallel()
	l := testLayout(t)
	for _, tc := range []struct {
		owner, repo string
		number      int
	}{
		{"..", "example-repo", 7},
		{"example-org", "..", 7},
		{".", "example-repo", 7},
		{"example-org", ".", 7},
		{"a/b", "example-repo", 7},
		{"example-org", "a/b", 7},
		{`a\b`, "example-repo", 7},
		{"", "example-repo", 7},
		{"example-org", "", 7},
		{"-example", "example-repo", 7},
		{"example org", "example-repo", 7},
		{strings.Repeat("a", 40), "example-repo", 7},
		{"example-org", strings.Repeat("a", 101), 7},
		{"example-org", "example-repo", 0},
		{"example-org", "example-repo", -1},
	} {
		got, err := Path(l, tc.owner, tc.repo, tc.number)
		if err == nil || got != "" {
			t.Errorf("Path(%q, %q, %d) = %q, %v; want a refusal", tc.owner, tc.repo, tc.number, got, err)
		}
	}
}

// TestWriteKeepsTheFileAndItsFoldersPrivate runs alone: it sets the
// process's umask to 0, so the modes come from Write and not from it.
func TestWriteKeepsTheFileAndItsFoldersPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	l := testLayout(t)
	path, err := Path(l, "example-org", "example-repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, sample())
	s := sample()
	s.Code.Specs++
	if changed, err := Write(path, s); err != nil || !changed {
		t.Fatalf("Write = %v, %v", changed, err)
	}
	if m := perm(t, path); m != 0o600 {
		t.Errorf("file mode = %v, want 0600", m)
	}
	for _, dir := range []string{l.FactoryPRStatus(), filepath.Dir(filepath.Dir(path)), filepath.Dir(path)} {
		if m := perm(t, dir); m != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, m)
		}
	}
	es, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name() != "7.json" {
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Errorf("the PR's folder holds %v, want only 7.json", names)
	}
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
