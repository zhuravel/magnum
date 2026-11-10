package execx

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// A CI job's log ends where it failed, so the CI reader keeps the end of
// gh's output: KeepTail keeps the last bytes of stdout, drops the start,
// and says so (Truncated) without the prefix capture's marker; an output
// that fits is kept whole.
func TestKeepTailKeepsTheLastBytesAndSaysTruncated(t *testing.T) {
	r := &Real{}
	script := `printf 'the start\n'; i=0; while [ $i -lt 3000 ]; do printf 'line %04d\n' $i; i=$((i+1)); done; printf 'the end\n'`
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", script}, KeepTail: 64})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 64 || !bytes.HasSuffix(res.Stdout, []byte("line 2999\nthe end\n")) || !res.Truncated {
		t.Fatalf("stdout = %q (%d bytes), truncated %v; want the last 64 bytes", res.Stdout, len(res.Stdout), res.Truncated)
	}
	if bytes.Contains(res.Stdout, []byte("the start")) || strings.Contains(string(res.Stdout), strings.TrimSpace(TruncationMarker)) {
		t.Errorf("stdout = %q: the start and the prefix marker are dropped", res.Stdout)
	}
	short, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "printf 'one\\ntwo\\n'"}, KeepTail: 64})
	if err != nil || string(short.Stdout) != "one\ntwo\n" || short.Truncated {
		t.Fatalf("a short output = %q, %v, truncated %v; want it whole", short.Stdout, err, short.Truncated)
	}
}

// The tail ring keeps the last bytes across writes of any size.
func TestTheTailCaptureKeepsTheLastBytesOfManyWrites(t *testing.T) {
	w := &capWriter{limit: 10, tail: true}
	var all strings.Builder
	for i := range 50 {
		s := fmt.Sprintf("%d,", i)
		all.WriteString(s)
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 25) + "END")); err != nil {
		t.Fatal(err)
	}
	all.WriteString(strings.Repeat("x", 25) + "END")
	want := all.String()[all.Len()-10:]
	if got := string(w.bytes()); got != want || !w.truncated {
		t.Fatalf("tail = %q, truncated %v; want %q", got, w.truncated, want)
	}
}
