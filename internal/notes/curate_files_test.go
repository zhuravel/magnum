package notes

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sparse makes path a file of size bytes without writing them.
func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	write(t, path, "")
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// readProposalSoon is ReadProposal that fails the test when it blocks.
func readProposalSoon(t *testing.T, s Scratch) (Proposal, []string) {
	t.Helper()
	done := make(chan struct{})
	var (
		p        Proposal
		problems []string
	)
	go func() { defer close(done); p, problems = ReadProposal(s) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReadProposal blocked")
	}
	return p, problems
}

func hasProblem(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) })
}

// What the curator leaves is held to the caps a recorded state has: a
// proposal.md or changes.json past MaxFileBytes is a problem and none of it
// is read, however much of it is valid.
func TestReadProposalRefusesFilesPastMaxFileBytes(t *testing.T) {
	s, err := PrepareScratch(filepath.Join(t.TempDir(), "c1"), baseState(), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	notes := "# Notes for talkable/talkable\n\n## Tests\n" + strings.Repeat("Run bin/rspec.\n", MaxFileBytes/15+1)
	write(t, s.Proposal(), notes)
	changes := `{"sections":[],"files":[]}` + strings.Repeat(" ", MaxFileBytes)
	write(t, s.Changes(), changes)
	p, problems := readProposalSoon(t, s)
	tooLarge := " is larger than " + strconv.Itoa(MaxFileBytes) + " bytes"
	for _, want := range []string{scratchProposal + tooLarge, scratchChanges + tooLarge} {
		if !hasProblem(problems, want) {
			t.Errorf("problems %q lack %q", problems, want)
		}
	}
	if p.State.Exists || len(p.State.Notes) != 0 || p.ChangesJSON != nil {
		t.Errorf("an oversized file was read: %d notes bytes, %d changes bytes", len(p.State.Notes), len(p.ChangesJSON))
	}
}

// plantHarness puts kind in s's harness: a file past MaxFileBytes, files
// that reach MaxStateBytes (with the base harness, past it), or a FIFO.
func plantHarness(t *testing.T, kind string, s Scratch) {
	t.Helper()
	switch kind {
	case "a file past MaxFileBytes":
		sparse(t, filepath.Join(s.Harness(), "big.rb"), MaxFileBytes+1)
	case "files past MaxStateBytes":
		for i := range MaxStateBytes / MaxFileBytes {
			sparse(t, filepath.Join(s.Harness(), "part"+strconv.Itoa(i)+".rb"), MaxFileBytes)
		}
	case "a fifo":
		if err := syscall.Mkfifo(filepath.Join(s.Harness(), "pipe"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
}

// A harness file past MaxFileBytes, or files past MaxStateBytes together
// with proposal.md, make the harness too large; a FIFO is a problem, never
// a wait.
func TestReadProposalCapsTheHarness(t *testing.T) {
	tooLarge := "harness/ cannot be read: " + ErrTooLarge.Error()
	for kind, want := range map[string]string{
		"a file past MaxFileBytes": tooLarge,
		"files past MaxStateBytes": tooLarge,
		"a fifo":                   "harness/pipe is not a regular file",
	} {
		t.Run(kind, func(t *testing.T) {
			s, err := PrepareScratch(filepath.Join(t.TempDir(), "c1"), baseState(), []byte("{}"))
			if err != nil {
				t.Fatal(err)
			}
			write(t, s.Proposal(), "# Notes for talkable/talkable\n\n## Tests\nRun bin/rspec.\n")
			write(t, s.Changes(), `{"sections":[],"files":[]}`)
			plantHarness(t, kind, s)
			p, problems := readProposalSoon(t, s)
			if !hasProblem(problems, want) {
				t.Fatalf("problems %q lack %q", problems, want)
			}
			if !p.State.Exists {
				t.Errorf("proposal.md was not read: %q", problems)
			}
		})
	}
}
