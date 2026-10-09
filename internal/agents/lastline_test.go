package agents

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// countingReaderAt counts the bytes asked of it.
type countingReaderAt struct {
	r    io.ReaderAt
	read atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.read.Add(int64(len(p)))
	return c.r.ReadAt(p, off)
}

// refLine is a line of data: [start, end) and its text without the newline.
type refLine struct {
	start, end int64
	text       string
}

// refLines splits data[:size] the simple way, every line in order.
func refLines(data []byte, size int64) []refLine {
	var out []refLine
	for at := int64(0); at < size; {
		next := size
		if i := bytes.IndexByte(data[at:size], '\n'); i >= 0 {
			next = at + int64(i) + 1
		}
		out = append(out, refLine{at, next, string(bytes.TrimSuffix(data[at:next], []byte("\n")))})
		at = next
	}
	return out
}

// A transcript line can be megabytes (a tool result that printed a file):
// lastLine finds the lines before, in and after such lines, at the start of
// the file or in its middle, whatever the chunk boundaries, as reading every
// line would, and shows match each line's text, newest first.
func TestLastLineFindsLinesAroundLongLines(t *testing.T) {
	long := func(tag string, n int) string { return tag + strings.Repeat("x", n) }
	for name, lines := range map[string][]string{
		"long line in the middle":          {"a", "early", long("big", 3<<20), "tail", "last"},
		"long line first":                  {long("big", 2<<20+17), "after"},
		"long line last":                   {"a", "early", long("big", 1<<20+5)},
		"two long lines":                   {long("big", transcriptChunk), "", "early", long("big", 3*transcriptChunk-1), "z"},
		"only short lines":                 {"a", "early", "b"},
		"a short first line over a border": {"early" + strings.Repeat("e", 40), long("big", transcriptChunk-20)},
		"one chunk exactly":                {"early", long("big", transcriptChunk-10)},
	} {
		for _, final := range []string{"\n", ""} {
			data := []byte(strings.Join(lines, "\n") + final)
			for _, size := range []int64{int64(len(data)), int64(len(data)) - 1, int64(len(data)) / 2, transcriptChunk, transcriptChunk + 1} {
				size = min(size, int64(len(data)))
				all := refLines(data, size)
				for _, want := range []string{"big", "early", "tail", "a", "nothing"} {
					var seen, wantSeen []string
					match := func(line []byte) bool {
						seen = append(seen, string(line))
						return bytes.HasPrefix(line, []byte(want))
					}
					var ws, we int64
					wok := false
					for _, l := range slices.Backward(all) {
						wantSeen = append(wantSeen, l.text)
						if strings.HasPrefix(l.text, want) {
							ws, we, wok = l.start, l.end, true
							break
						}
					}
					s, e, ok, err := lastLine(bytes.NewReader(data), size, match)
					if err != nil || s != ws || e != we || ok != wok {
						t.Errorf("%s (final %q, size %d), %q: lastLine = %d, %d, %v, %v; want %d, %d, %v",
							name, final, size, want, s, e, ok, err, ws, we, wok)
					}
					if !slices.Equal(seen, wantSeen) {
						t.Errorf("%s (final %q, size %d), %q: match saw %d lines, other text than the %d lines it should",
							name, final, size, want, len(seen), len(wantSeen))
					}
				}
			}
		}
	}
}

// Looking back past one 8 MiB line costs about its size, read and kept
// once, not a copy of the partial line per 64 KiB chunk (about 512 MiB).
func TestLastLineReadsALongLineOnce(t *testing.T) {
	data := []byte(fmt.Sprintf("early\n%s\nlast\n", strings.Repeat("x", 8<<20)))
	r := &countingReaderAt{r: bytes.NewReader(data)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start, end, ok, err := lastLine(r, int64(len(data)), func(line []byte) bool { return string(line) == "early" })
	runtime.ReadMemStats(&after)
	if err != nil || !ok || start != 0 || end != 6 {
		t.Fatalf("lastLine = %d, %d, %v, %v; want the first line", start, end, ok, err)
	}
	if read := r.read.Load(); read > 2*int64(len(data))+transcriptChunk {
		t.Errorf("read %d bytes of %d", read, len(data))
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4*uint64(len(data)) {
		t.Errorf("allocated %d MiB for a %d MiB transcript", alloc>>20, len(data)>>20)
	}
}

// BenchmarkLastLineAcrossALongLine looks back past one 8 MiB line.
func BenchmarkLastLineAcrossALongLine(b *testing.B) {
	data := []byte(fmt.Sprintf("early\n%s\nlast\n", strings.Repeat("x", 8<<20)))
	match := func(line []byte) bool { return string(line) == "early" }
	for b.Loop() {
		if _, _, ok, err := lastLine(bytes.NewReader(data), int64(len(data)), match); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}
