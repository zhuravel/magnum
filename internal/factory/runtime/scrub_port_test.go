package runtime

// The improvement loop's evidence.sh shows the factory's texts (the liaison's
// notes, the holds' words) through a Python port of Scrub,
// .claude/skills/magnum-improve/scripts/scrub.py. The port may mask more than
// Scrub, never less: TestEvidenceScrubHoldsTheGoScrubsPatterns checks its
// pattern lists against the Go source, and the fuzz below feeds both the same
// text and fails when the port shows a byte Scrub masked. Run it with
//
//	go test -run '^$' -fuzz FuzzEvidenceScrubMasksEveryByteScrubMasks ./internal/factory/runtime
//
// It lives here, beside the scrub it holds the port to, since Go builds no
// package under .claude (a path element may not start with a dot).

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// scrubPortPath is the port, from this package's folder.
const scrubPortPath = "../../../.claude/skills/magnum-improve/scripts/scrub.py"

// execxSecretPatterns reads the patterns of execx's secretPattern, which execx
// does not export, from its source: the string literals of
// regexp.MustCompile(strings.Join([]string{...}, "|")). It fails when it finds
// none, or when the declaration takes another shape.
func execxSecretPatterns(tb testing.TB) []string {
	tb.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../../execx/execx.go", nil, 0)
	if err != nil {
		tb.Fatal(err)
	}
	unquote := func(e ast.Expr) string {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			tb.Fatalf("execx's secretPattern holds a pattern that is no string literal: %#v", e)
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			tb.Fatal(err)
		}
		return s
	}
	var pats []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "secretPattern" || len(vs.Values) != 1 {
				continue
			}
			compile, ok := vs.Values[0].(*ast.CallExpr)
			if !ok || len(compile.Args) != 1 {
				tb.Fatal("execx's secretPattern is no longer regexp.MustCompile(...)")
			}
			switch arg := compile.Args[0].(type) {
			case *ast.BasicLit:
				pats = append(pats, unquote(arg))
			case *ast.CallExpr:
				var list *ast.CompositeLit
				if len(arg.Args) == 2 {
					list, _ = arg.Args[0].(*ast.CompositeLit)
				}
				if list == nil || unquote(arg.Args[1]) != "|" {
					tb.Fatal(`execx's secretPattern is no longer regexp.MustCompile(strings.Join([]string{...}, "|"))`)
				}
				for _, e := range list.Elts {
					pats = append(pats, unquote(e))
				}
			default:
				tb.Fatalf("execx's secretPattern compiles what this test cannot read: %#v", arg)
			}
		}
	}
	if len(pats) == 0 {
		tb.Fatal("no pattern of execx's secretPattern found in internal/execx/execx.go")
	}
	return pats
}

// keptPiece is a piece of a replacement: a span of the text being replaced
// (kept) or a string put in.
type keptPiece struct {
	from, to int
	put      string
}

// scrubKept replays Scrub's passes over text with the origin of each byte, and
// returns Scrub's output and which bytes of text it kept. redact is execx's
// secretPattern, compiled from its source.
func scrubKept(redact *regexp.Regexp, text string) (string, []bool) {
	put := func(s string) func(string, []int) []keptPiece {
		return func(string, []int) []keptPiece { return []keptPiece{{put: s}} }
	}
	passes := []struct {
		re   *regexp.Regexp
		with func(cur string, m []int) []keptPiece
	}{
		{redact, func(cur string, m []int) []keptPiece {
			s := cur[m[0]:m[1]]
			if i := strings.IndexByte(s, ':'); i >= 0 && strings.HasPrefix(strings.ToLower(s), "authorization") {
				return []keptPiece{{from: m[0], to: m[0] + i + 1}, {put: " <redacted>"}}
			}
			return []keptPiece{{put: "<redacted>"}}
		}},
		{scrubTokens, put("<redacted>")},
		{scrubPairs, func(_ string, m []int) []keptPiece {
			return []keptPiece{{from: m[2], to: m[3]}, {from: m[4], to: m[5]}, {put: "<redacted>"}}
		}},
		{scrubEmail, put("<email>")},
		{scrubHost, put("<host>")},
	}
	cur, origin := text, make([]int, len(text))
	for i := range origin {
		origin[i] = i
	}
	for _, p := range passes {
		var b strings.Builder
		var next []int
		keep := func(from, to int) {
			b.WriteString(cur[from:to])
			next = append(next, origin[from:to]...)
		}
		last := 0
		for _, m := range p.re.FindAllStringSubmatchIndex(cur, -1) {
			keep(last, m[0])
			for _, piece := range p.with(cur, m) {
				if piece.put == "" {
					keep(piece.from, piece.to)
					continue
				}
				b.WriteString(piece.put)
				next = append(next, slices.Repeat([]int{-1}, len(piece.put))...)
			}
			last = m[1]
		}
		keep(last, len(cur))
		cur, origin = b.String(), next
	}
	kept := make([]bool, len(text))
	for _, o := range origin {
		if o >= 0 {
			kept[o] = true
		}
	}
	return cur, kept
}

// portMasks are what the port puts where it masked: Scrub's three, and the
// [...] its last step puts for a run of non-space characters still over 2,000
// after the masks (a minified JSON whose quoted secret Scrub does not see).
var portMasks = []string{"<redacted>", "<email>", "<host>", "[...]"}

// portShown returns the first piece of the port's output that stands on no
// bytes Scrub kept, where the port masked less than Scrub, or "" and true. The
// output is the text with spans put out of sight behind the masks: its pieces
// between the masks must stand on kept bytes of the text, in order, the first at
// the text's start, the last at its end, and a mask between two over at least
// one byte. A piece is placed at its first fit, which leaves the most room to
// the rest. A space right before <redacted> goes with the mask: Redact puts one
// after "Authorization:", which a key's value can mask in turn.
func portShown(text string, kept []bool, out string) (string, bool) {
	var pieces []string
	for {
		at, n := -1, 0
		for _, mask := range portMasks {
			if i := strings.Index(out, mask); i >= 0 && (at < 0 || i < at) {
				at, n = i, len(mask)
			}
		}
		if at < 0 {
			pieces = append(pieces, out)
			break
		}
		piece := out[:at]
		if out[at:at+n] == "<redacted>" && strings.HasSuffix(piece, " ") {
			piece = piece[:len(piece)-1]
		}
		pieces = append(pieces, piece)
		out = out[at+n:]
	}
	fits := func(at int, piece string) bool {
		return at >= 0 && at+len(piece) <= len(text) && text[at:at+len(piece)] == piece &&
			!slices.Contains(kept[at:at+len(piece)], false)
	}
	first, last := pieces[0], pieces[len(pieces)-1]
	if len(pieces) == 1 {
		if len(first) != len(text) || !fits(0, first) {
			return first, false
		}
		return "", true
	}
	if !fits(0, first) {
		return first, false
	}
	end := len(first)
	for _, piece := range pieces[1 : len(pieces)-1] {
		at := end + 1
		for at+len(piece) <= len(text) && !fits(at, piece) {
			i := strings.Index(text[at+1:], piece)
			if i < 0 {
				return piece, false
			}
			at += 1 + i
		}
		if at+len(piece) > len(text) {
			return piece, false
		}
		end = at + len(piece)
	}
	if at := len(text) - len(last); at <= end || !fits(at, last) {
		return last, false
	}
	return "", true
}

// scrubPort is the port's process, which scrubs one text at a time: a 4-byte
// big-endian length and the text's bytes each way.
type scrubPort struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *bytes.Buffer
}

func startScrubPort(f *testing.F) *scrubPort {
	f.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		f.Skip("no python3 to run evidence.sh's scrub")
	}
	cmd := exec.CommandContext(f.Context(), python, "-I", scrubPortPath, "--serve")
	p := &scrubPort{cmd: cmd, stderr: new(bytes.Buffer)}
	cmd.Stderr = p.stderr
	if p.in, err = cmd.StdinPipe(); err != nil {
		f.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		f.Fatal(err)
	}
	p.out = bufio.NewReader(stdout)
	if err := cmd.Start(); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		_ = p.in.Close()
		_ = cmd.Wait()
	})
	return p
}

// ended waits for the port to end and returns what it wrote to its stderr (a
// traceback), which its pipe's goroutine writes until then.
func (p *scrubPort) ended() string {
	_ = p.in.Close()
	_ = p.cmd.Wait()
	return p.stderr.String()
}

func (p *scrubPort) scrub(t *testing.T, text string) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	head := binary.BigEndian.AppendUint32(nil, uint32(len(text)))
	if _, err := p.in.Write(append(head, text...)); err != nil {
		t.Fatalf("the port took no text: %v\n%s", err, p.ended())
	}
	if _, err := io.ReadFull(p.out, head); err != nil {
		t.Fatalf("the port answered nothing: %v\n%s", err, p.ended())
	}
	out := make([]byte, binary.BigEndian.Uint32(head))
	if _, err := io.ReadFull(p.out, out); err != nil {
		t.Fatalf("the port's answer was cut: %v\n%s", err, p.ended())
	}
	return string(out)
}

// brief is s for a failure's message: its start and end around a cut.
func brief(s string) string {
	if len(s) > 600 {
		return s[:300] + "[...cut...]" + s[len(s)-300:]
	}
	return s
}

// maxFill bounds the repeated part of a fuzz text: past evidence.sh's old
// 2,000-character words, and small enough for many texts a second.
const maxFill = 6000

var (
	hostRun  = regexp.MustCompile(`[A-Za-z0-9_.\x{17f}\x{212a}-]+`)
	spaceRun = regexp.MustCompile(`\S+`)
)

// portLongRun says whether the port may mask more of text than Scrub, whose
// output is clean: text has a run of host characters over 253, which the port
// masks whole when it holds a dot, or clean has a run of non-space characters
// over 2,000 (masks can make a run longer), which the port's last step drops.
// Else the port's output is Scrub's.
func portLongRun(text, clean string) bool {
	for _, run := range []struct {
		re   *regexp.Regexp
		in   string
		over int
	}{{hostRun, text, 253}, {spaceRun, clean, 2000}} {
		for _, at := range run.re.FindAllStringIndex(run.in, -1) {
			if at[1]-at[0] > run.over {
				return true
			}
		}
	}
	return false
}

func TestEvidenceScrubHoldsTheGoScrubsPatterns(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 to run evidence.sh's scrub")
	}
	out, err := exec.CommandContext(t.Context(), python, "-I", scrubPortPath, "--patterns").Output()
	if err != nil {
		t.Fatalf("scrub.py --patterns: %v", err)
	}
	var port map[string][]string
	if err := json.Unmarshal(out, &port); err != nil {
		t.Fatalf("scrub.py --patterns printed no JSON: %v\n%s", err, out)
	}
	for _, want := range []struct {
		list string
		inGo []string
	}{
		{"redact", execxSecretPatterns(t)},
		{"tokens", []string{scrubTokens.String()}},
		{"pairs", []string{scrubPairs.String()}},
		{"email", []string{scrubEmail.String()}},
		{"host", []string{scrubHost.String()}},
	} {
		got := port[want.list]
		if want.list == "tokens" {
			got = []string{strings.Join(got, "|")}
		}
		if !slices.Equal(got, want.inGo) {
			t.Errorf("scrub.py's %s patterns are not the Go scrub's, word for word:\nscrub.py: %q\nGo:       %q",
				want.list, got, want.inGo)
		}
	}
}

func FuzzEvidenceScrubMasksEveryByteScrubMasks(f *testing.F) {
	port := startScrubPort(f)
	redact := regexp.MustCompile(strings.Join(execxSecretPatterns(f), "|"))
	// A text is head, then fill repeated n times (a long word, when it has no
	// space), then tail. The secrets are made up, of the right shape.
	for _, seed := range []struct {
		head, fill string
		n          uint16
		tail       string
	}{
		// R54: a quote inside a word over 2,000 characters opens a value that goes on past it.
		{`password: "`, "x", 2100, ` QUOTE-TAIL"`},
		{"- ", "x", 2100, `.password="QUOTED-START QUOTED-TAIL"`},
		// R54: a word over 2,000 characters ends in -----BEGIN, the key's body follows.
		{"- ", "x", 2100, "-----BEGIN RSA PRIVATE KEY----- KEY-BODY -----END RSA PRIVATE KEY-----"},
		// The key's header glued to a long body that goes on past the word.
		{"-----BEGIN PRIVATE KEY-----", "M", 2100, " BODY-SECOND -----END PRIVATE KEY-----"},
		// R54: a long word ends in a key, its separator after a space.
		{"- ", "x", 2100, ".password : SPACED-COLON"},
		{"- ", "x", 2100, ".token = SPACED-EQUALS"},
		// R54: authorization:Bearer glued, at the end of a long word.
		{"- ", "x", 2100, "authorization:Bearer GLUED-BEARER"},
		// Go's Authorization pattern needs no word boundary before it.
		{"- ", "x", 2100, "authorization: Bearer NO-BOUNDARY"},
		// A GitHub token takes in the Authorization: a long word ends with, and the key-value pass masks what follows.
		{"ghp_", "x", 2100, "authorization: bearer password= TAKEN-IN"},
		// Go's \s has no vertical tab, so Go's value goes on past one.
		{"password=a", "", 0, "\vVERTICAL-TAB"},
		{"Authorization: Bearer a", "", 0, "\vVT-BEARER"},
		// Go's (?i) folds s to the long s and k to the Kelvin sign.
		{"pa\u017f\u017fword=", "", 0, "LONG-S"},
		{"to\u212aen: ", "", 0, "KELVIN"},
		{"mail db1.internal.example.\u017fite and ", "", 0, "db2.example.u\u212a"},
		// A sample of every pattern.
		{"- token ghs_abcDEF123_x here, and a JWT eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." +
			"dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U\n", "", 0,
			"- Authorization: Bearer opaqueTOKEN123, then Authorization: Basic dXNlcjpodW50ZXIy\n"},
		{"- AKIAABCDEFGHIJKLMNOP and aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n", "", 0,
			"- xoxb-123456789012-abcdefghijkl sk-abcdefghijklmnopqrstuvwxyz123456 sk-" + "ant-abcdefghijklmnopqrstuv " +
				"glpat-abcdefghij0123456789 rk_" + "live_abcdefghij0123456789 AI" + "zaSyA-abcdefghijklmnopqrstuvwxyz01234\n"},
		{`- password=hunter3 client_secret: "s3cr3t value" api_key:` + "\n  NEXT-LINE\n", "", 0,
			"- mail bob@example.org about db1.internal.example.com and 10.0.4.12\n"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEsecretbody\n-----END RSA PRIVATE KEY-----\n- the cut key follows\n", "", 0,
			"-----BEGIN EC PRIVATE KEY-----\nMIIEtruncat"},
		// The port's skips: a JWT after an eyJ that fails, an email that starts where the last one ended, and Go's $ (the
		// text's end, not a line break before it).
		{"- eyJshort.eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "", 0,
			" and a@b.co%x@d.org"},
		{"-----BEGIN EC PRIVATE KEY-----\nMIIEtruncat\n", "", 0, ""},
		// The texts Python's engine backtracks on: an eyJ repeated, a dotted line, a long first part of an address.
		{"- ", "eyJ", 2000, ""},
		{"- ", "a.", 3000, " and token: DOTTED-NEXT"},
		{"- ", "a", 5000, "@x"},
	} {
		f.Add(seed.head, seed.fill, seed.n, seed.tail)
	}
	f.Fuzz(func(t *testing.T, head, fill string, n uint16, tail string) {
		text := head + tail
		if fill != "" {
			text = head + strings.Repeat(fill, min(int(n), maxFill/len(fill))) + tail
		}
		clean, kept := scrubKept(redact, text)
		if want, _ := Scrub(text); clean != want {
			t.Fatalf("the replay of Scrub parted from Scrub:\nreplay: %q\nScrub:  %q", brief(clean), brief(want))
		}
		out := port.scrub(t, text)
		if !portLongRun(text, clean) {
			if out != clean {
				t.Fatalf("evidence.sh's scrub is not Scrub on a text with no long run\ntext:  %q\nScrub: %q\nport:  %q",
					brief(text), brief(clean), brief(out))
			}
			return
		}
		if piece, ok := portShown(text, kept, out); !ok {
			t.Fatalf("evidence.sh's scrub shows %q where Scrub masks it\ntext:  %q\nScrub: %q\nport:  %q",
				brief(piece), brief(text), brief(clean), brief(out))
		}
	})
}
