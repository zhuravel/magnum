package scrub

// The improvement loop's evidence.sh shows the factory's texts (the liaison's
// notes, the holds' words) through a Python port of Text,
// .claude/skills/magnum-improve/scripts/scrub.py. The port may mask more than
// Text, never less. TestEvidenceScrubHoldsTheGoScrubsPatterns checks its
// patterns against the Go source, and the fuzz below feeds both the same text
// and wants the port's output to be Text's with the port's two extra masks.
// Run it with
//
//	go test -run '^$' -fuzz FuzzEvidenceScrubMasksEveryByteScrubMasks ./internal/scrub
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
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// The port, and the Go files of the scrub, from this package's folder.
const (
	scrubPortPath = "../../.claude/skills/magnum-improve/scripts/scrub.py"
	execxGoPath   = "../execx/execx.go"
	scrubGoPath   = "scrub.go"
)

// parseGo parses a Go file of the scrub.
func parseGo(tb testing.TB, path string) *ast.File {
	tb.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		tb.Fatal(err)
	}
	return file
}

// isCall says whether call calls x.fn, a package's function or a variable's
// method.
func isCall(call *ast.CallExpr, x, fn string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && (x == "" || id.Name == x)
}

// goPatterns reads the patterns of every regexp.MustCompile call in a Go file,
// by the package variable it is assigned to ("" for any other). A call's
// argument must be a string literal, string constants of the file and + of
// them, or strings.Join of a list of these with "|": anything else fails, so
// that no pattern escapes the reading.
func goPatterns(tb testing.TB, path string) map[string][]string {
	tb.Helper()
	file := parseGo(tb, path)
	names := map[*ast.CallExpr]string{}
	consts := map[string]ast.Expr{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.VAR && gen.Tok != token.CONST) {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, v := range vs.Values {
				if call, ok := v.(*ast.CallExpr); ok && gen.Tok == token.VAR && i < len(vs.Names) {
					names[call] = vs.Names[i].Name
				}
				if gen.Tok == token.CONST && i < len(vs.Names) {
					consts[vs.Names[i].Name] = v
				}
			}
		}
	}
	// unquote evaluates a string expression: a literal, a constant of the
	// file, or + of them.
	var unquote func(e ast.Expr) string
	unquote = func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				break
			}
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				tb.Fatal(err)
			}
			return s
		case *ast.Ident:
			if v, ok := consts[e.Name]; ok {
				return unquote(v)
			}
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return unquote(e.X) + unquote(e.Y)
			}
		case *ast.ParenExpr:
			return unquote(e.X)
		}
		tb.Fatalf("%s compiles a pattern that is no string literal, string constant or + of them: %#v", path, e)
		return ""
	}
	pats := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isCall(call, "regexp", "MustCompile") {
			return true
		}
		if len(call.Args) != 1 {
			tb.Fatalf("%s calls regexp.MustCompile with %d arguments", path, len(call.Args))
		}
		name := names[call]
		switch arg := call.Args[0].(type) {
		case *ast.BasicLit, *ast.Ident, *ast.BinaryExpr, *ast.ParenExpr:
			pats[name] = append(pats[name], unquote(arg))
		case *ast.CallExpr:
			var list *ast.CompositeLit
			if isCall(arg, "strings", "Join") && len(arg.Args) == 2 {
				list, _ = arg.Args[0].(*ast.CompositeLit)
			}
			if list == nil || unquote(arg.Args[1]) != "|" {
				tb.Fatalf(`%s's %s is no regexp.MustCompile(strings.Join([]string{...}, "|"))`, path, name)
			}
			for _, e := range list.Elts {
				pats[name] = append(pats[name], unquote(e))
			}
		default:
			tb.Fatalf("%s compiles what this test cannot read: %#v", path, arg)
		}
		return true
	})
	return pats
}

// execxSecretPatterns are execx's secretPattern, which execx does not export,
// read from its source.
func execxSecretPatterns(tb testing.TB) []string {
	tb.Helper()
	pats := goPatterns(tb, execxGoPath)["secretPattern"]
	if len(pats) == 0 {
		tb.Fatalf("no pattern of execx's secretPattern found in %s", execxGoPath)
	}
	return pats
}

// goPasses counts the passes a function of a Go file runs over its text: its
// calls of a ReplaceAll method, and for each call of another function in
// passesOf ("pkg.Func" for a package's, "Func" for the file's own), that one's
// passes.
func goPasses(tb testing.TB, path, fn string, passesOf map[string]int) int {
	tb.Helper()
	var body *ast.BlockStmt
	for _, decl := range parseGo(tb, path).Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
			body = fd.Body
		}
	}
	if body == nil {
		tb.Fatalf("no func %s in %s", fn, path)
	}
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if strings.HasPrefix(fun.Sel.Name, "ReplaceAll") {
				n++
			}
			if x, ok := fun.X.(*ast.Ident); ok {
				n += passesOf[x.Name+"."+fun.Sel.Name]
			}
		case *ast.Ident:
			n += passesOf[fun.Name]
		}
		return true
	})
	return n
}

// scrubPass is a pass of Text's replay: a pattern and ReplaceAllString's
// template, or with "" Redact's own function. A pass with run set is applied
// by that function of the package (maskJoinedEnv, which passes over the names
// of an allow list), and re and with are the pattern and the template it is
// built on, which the oracle of the older scrub follows.
type scrubPass struct {
	re   *regexp.Regexp
	with string
	run  func(string) string
}

// scrubPasses are Text's passes in its order (Secrets's, then the email and
// host passes); redact is execx's secretPattern, compiled from its source.
func scrubPasses(redact *regexp.Regexp) []scrubPass {
	return []scrubPass{
		{redact, "", nil},
		{scrubTokens, "<redacted>", nil},
		{scrubPairs, "$1$2<redacted>", nil},
		{scrubQuotedPairs, "$1$2<redacted>", nil},
		{scrubEnvPairs, "$1$2<redacted>", nil},
		{scrubURLPassword, "$1<redacted>@", nil},
		{scrubURLUser, "$1<redacted>@", nil},
		{scrubCurlUser, "$1$2<redacted>", nil},
		{scrubMySQLPassword, "$1$2<redacted>", nil},
		{scrubJoinedEnv, "$1$2<redacted>", maskJoinedEnv},
		{scrubEmail, "<email>", nil},
		{scrubUserinfoHost, "$1<host>", nil},
		{scrubHost, "<host>", nil},
	}
}

// scrubReplay replays Text's passes over text, and returns its text before
// the host pass, its last, and its output.
func scrubReplay(redact *regexp.Regexp, text string) (beforeHost, clean string) {
	clean = text
	for _, p := range scrubPasses(redact) {
		if p.re == scrubHost {
			beforeHost = clean
		}
		if p.run != nil {
			clean = p.run(clean)
			continue
		}
		if p.with != "" {
			clean = p.re.ReplaceAllString(clean, p.with)
			continue
		}
		clean = p.re.ReplaceAllStringFunc(clean, func(m string) string {
			if i := strings.IndexByte(m, ':'); i >= 0 && strings.HasPrefix(strings.ToLower(m), "authorization") {
				return m[:i+1] + " <redacted>"
			}
			return "<redacted>"
		})
	}
	return beforeHost, clean
}

var (
	// hostRun is a run of the port's host characters (HOST_RUN): the word
	// characters, dot, hyphen, and the long s and the Kelvin sign Go's (?i)
	// folds to.
	hostRun = regexp.MustCompile(`[A-Za-z0-9_.\x{17f}\x{212a}-]+`)
	// spaceRun is a run of non-space characters, as Go's \S and the port's
	// WORD count them.
	spaceRun = regexp.MustCompile(`\S+`)
)

// hostRuns runs Text's host pass on each run of host characters of text, as
// the port does, with long, a run of more than 253 characters that holds a dot
// as <host> (the port's first extra mask).
func hostRuns(text string, long bool) string {
	return hostRun.ReplaceAllStringFunc(text, func(run string) string {
		if long && strings.Contains(run, ".") && utf8.RuneCountInString(run) > 253 {
			return "<host>"
		}
		return scrubHost.ReplaceAllString(run, "<host>")
	})
}

// capRuns puts [...] for each run of non-space characters of text over 2,000
// characters (the port's second extra mask). An invalid byte counts as one,
// as the port's surrogateescape decoding counts it.
func capRuns(text string) string {
	return spaceRun.ReplaceAllStringFunc(text, func(run string) string {
		if utf8.RuneCountInString(run) > 2000 {
			return "[...]"
		}
		return run
	})
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

func TestEvidenceScrubHoldsTheGoScrubsPatterns(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3: the drift of evidence.sh's scrub from the Go scrub is not checked")
	}
	out, err := exec.CommandContext(t.Context(), python, "-I", scrubPortPath, "--patterns").Output()
	if err != nil {
		t.Fatalf("scrub.py --patterns: %v", err)
	}
	var port map[string][]string
	if err := json.Unmarshal(out, &port); err != nil {
		t.Fatalf("scrub.py --patterns printed no JSON: %v\n%s", err, out)
	}
	execxPats, scrubPats := goPatterns(t, execxGoPath), goPatterns(t, scrubGoPath)

	// Every pattern of every regexp.MustCompile in the two Go files is in
	// scrub.py's lists, and each of theirs is one of these.
	set := func(lists ...map[string][]string) map[string]bool {
		s := map[string]bool{}
		for _, l := range lists {
			for _, pats := range l {
				for _, p := range pats {
					s[p] = true
				}
			}
		}
		return s
	}
	inGo, inPort := set(execxPats, scrubPats), set(port)
	if len(inGo) == 0 || !maps.Equal(inGo, inPort) {
		var onlyGo, onlyPort []string
		for p := range inGo {
			if !inPort[p] {
				onlyGo = append(onlyGo, p)
			}
		}
		for p := range inPort {
			if !inGo[p] {
				onlyPort = append(onlyPort, p)
			}
		}
		t.Errorf("scrub.py's patterns are not the Go scrub's (%d in Go):\nonly in Go:       %q\nonly in scrub.py: %q",
			len(inGo), onlyGo, onlyPort)
	}

	// In their lists, in their order.
	for list, inGo := range map[string][]string{
		"redact":        execxSecretPatterns(t),
		"tokens":        scrubPats["scrubTokens"],
		"pairs":         scrubPats["scrubPairs"],
		"quoted":        scrubPats["scrubQuotedPairs"],
		"env":           scrubPats["scrubEnvPairs"],
		"url_pass":      scrubPats["scrubURLPassword"],
		"url_user":      scrubPats["scrubURLUser"],
		"curl":          scrubPats["scrubCurlUser"],
		"mysql":         scrubPats["scrubMySQLPassword"],
		"joined":        scrubPats["scrubJoinedEnv"],
		"joined_kept":   scrubPats["scrubJoinedKept"],
		"email":         scrubPats["scrubEmail"],
		"userinfo_host": scrubPats["scrubUserinfoHost"],
		"host":          scrubPats["scrubHost"],
	} {
		if !slices.Equal(port[list], inGo) {
			t.Errorf("scrub.py's %s patterns are not the Go scrub's, word for word:\nscrub.py: %q\nGo:       %q",
				list, port[list], inGo)
		}
	}

	// Text runs as many passes as its replay here, which the fuzz compares
	// the port with: Secrets's, then the email and host passes. Secrets runs
	// as many as the replay's up to the email pass.
	// maskJoinedEnv is one pass (scrubJoinedEnv's, with its allow list) and has no ReplaceAll of its own to count.
	passesOf := map[string]int{"execx.Redact": goPasses(t, execxGoPath, "Redact", nil), "maskJoinedEnv": 1}
	secrets := goPasses(t, scrubGoPath, "Secrets", passesOf)
	if want := len(secretsPassesOf(nil)); secrets != want {
		t.Errorf("Secrets runs %d passes and its replay here %d: add the new pass to scrubPasses and to scrub.py", secrets, want)
	}
	if got, want := goPasses(t, scrubGoPath, "Text", map[string]int{"Secrets": secrets}), len(scrubPasses(nil)); got != want {
		t.Errorf("Text runs %d passes and its replay here %d: add the new pass to scrubPasses and to scrub.py", got, want)
	}
}

// scrubSeed is a fuzz text: head, then fill repeated n times (a long word, when it has no space), then tail. The
// secrets in the seeds are made up, of the right shape.
type scrubSeed struct {
	head, fill string
	n          uint16
	tail       string
}

// text is the seed's text, as the fuzz builds it from its arguments.
func scrubText(head, fill string, n uint16, tail string) string {
	if fill == "" {
		return head + tail
	}
	return head + strings.Repeat(fill, min(int(n), maxFill/len(fill))) + tail
}

// scrubSeeds are the fuzz's seeds, which the test of the scrub before R58 reads too.
var scrubSeeds = []scrubSeed{
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
	// R58: the value of a key in quotes, as JSON, YAML and TOML write it.
	{`{"password":"MADEUP-PW-123","user":"alice"}`, "", 0, `, "api_key": "MADEUP", 'secret': 'x y', "token" = MADEUP-WORD,"n":1}`},
	{`{"X-Api-Key":"MADEUP","x_auth-token": 12345, "client_secret":["MADEUP"]}`, "", 0, `["pwd": MADEUP]`},
	{`{"password": "MADEUP-LINE-ONE`, "", 0, "\nMADEUP-LINE-TWO\", \"user\": \"alice\"}\n\"private_key\":\n\"MADEUP\""},
	// R58 round 1b: env-style names, a key word after an underscore, unquoted and quoted; the names that only hold a key
	// word, and a digit or a letter before the key word, stay.
	{"DB_PASSWORD=MADEUP\nGITHUB_TOKEN: MADEUP\nMY_APP.SECRET = MADEUP\nexport OPENAI_API_KEY=\"MADEUP ONE\"\n", "", 0,
		"DEPLOY_AWS_SECRET_ACCESS_KEY=MADEUP\nMAX_TOKENS=4096 TOKEN_LIMIT=5 mypassword=x 2password=y secretary: Jo\n__token: x"},
	{`{"DB_PASSWORD": "MADEUP", 'stripe-api-key': x, "__token":1, "db2_pwd":2, "2password": "x", "MAX_TOKENS": 4096}`, "", 0,
		`{"SLACK_BOT_TOKEN":"MADEUP","DB_HOST":"db1.internal.example.com","DB_PASSWORD_HINT":"x"}`},
	{"dB_pa\u017f\u017fword=LONG-S\nGITHUB_TO\u212aEN: KELVIN\n\"x\u017f_token\": 1, \"\u212a_\u017fecret\": 2", "", 0, ""},
	{"DB_PASSWORD=db1.internal.example.com and ", "_", 1900, "token: x, \"_"},
	{"- ", "_", 5000, "password=NO-BOUNDARY-NEEDED \"_token\": MADEUP"},
	// R58 round 1c: the Rails key words, each unquoted and quoted; the file and the id of a key stay.
	{"SECRET_KEY_BASE=MADEUP\n", "", 0, "secret_key_base: MADEUP \"secret_key_base\": \"MADEUP\" SECRET_KEY_BASE_DUMMY=1\n"},
	{"RAILS_MASTER_KEY=MADEUP\n", "", 0, "master-key: MADEUP {\"master_key\":\"MADEUP\",\"master_keys\":\"x\"}\n"},
	{"signing_key = MADEUP\n", "", 0, "{\"signing_key\": \"MADEUP ONE\", 'Signing-Key': 'MADEUP'} DB_PASSWORD_FILE=/run/secrets/db\n"},
	{"config.encryption_key: MADEUP\n", "", 0, "{\"encryption_key\":\"MADEUP\"} API_KEY_ID=MADEUP-ID\n"},
	// R58 fix round 2: a key with no value, then the next key (the first key-value pass runs first, on the text the
	// scrub before R58 saw), and a quoted value that reads escapes.
	{"SESSION_SECRET:\napi_key: hunter2\nDB_PASSWORD=\npassword: hunter2\nsigning_key:\n  password: hunter2\n", "", 0,
		"\"DB_PASSWORD\":\n  client_secret: hunter2\nGITHUB_TOKEN=x,password: hunter2\n\"secret\": password: hunter2"},
	{`{"password":"a\"b"}` + "\n", "", 0, `{"password":"ab\"cd\"ef","user":"alice"} {'password': 'it\'s fine'} {"password":"ab\\","user":"alice"}`},
	{`DB_PASSWORD="a\"b c" next password="a\"b c" more `, "", 0, `{"token":"a\\\"b","user":"alice"} {"password":"ab\"cd`},
	{`{"password":"`, `\"`, 3000, `x", "token": "MADEUP"}`},
	{`{"password":"`, `\\`, 3000, `", "token": "MADEUP"} DB_PASSWORD='ab\'`},
	// A quoted value that holds an unquoted pair: the first key-value pass masks the pair, then the quoted pass the rest.
	{`{"token":"x password: y"}`, "", 0, ` {'secret': "a token=b c", "pwd" : 'it''s'}`},
	// A key word before its separator with no closing quote is no quoted key (the unquoted pair masks one word), and a
	// prefix has no space or quote in it.
	{`"pwd: 'MADEUP ONE TWO' and "token: "MADEUP THREE"`, "", 0, ` {"my password": "MADEUP-SPACE", "a'password": "MADEUP-QUOTE"}`},
	// A key that only holds a key word, and a quoted JSON key inside a JSON string, are left as they are.
	{`{"password_hint":"the dog","tokens_used":3,"mypassword":"MADEUP","user":"password","note":"token"}`, "", 0,
		`{\"password\":\"ESCAPED-JSON\"} "password" is a word, and so is 'token' too`},
	// R58: Go's (?i) folds s and k in a quoted key too, and a prefix that ends in the long s or the Kelvin sign is
	// one of non-word characters.
	{"{\"pa\u017f\u017fword\":\"LONG-S\",\"to\u212aen\":\"KELVIN\",\"a\u017f-api_\u212aey\":\"MADEUP\"}", "", 0,
		"\"a\u212apassword\": \"MADEUP\", \"\u017fpwd\" = x"},
	// R58: a minified JSON over 2,000 characters with a password in it: Text masks the value, and the port's cap
	// shows the run as [...].
	{`{"items":[`, `{"id":1,"name":"widget"},`, 400, `{"password":"MADEUP-PW-123","user":"alice"}]}`},
	{`"`, `,`, 1900, `password": "MADEUP", "token":"MADEUP-2"`},
	{`"`, `,`, 5000, `password": "MADEUP", "token":"MADEUP-2"`},
	{`"`, `a`, 5000, `password": "MADEUP"`},
	// The port's skips: a JWT after an eyJ that fails, an email that starts where the last one ended, and Go's $ (the
	// text's end, not a line break before it).
	{"- eyJshort.eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "", 0,
		" and a@b.co%x@d.org"},
	{"-----BEGIN EC PRIVATE KEY-----\nMIIEtruncat\n", "", 0, ""},
	// The texts Python's engine backtracks on: an eyJ repeated, a dotted line, a long first part of an address.
	{"- ", "eyJ", 2000, ""},
	{"- ", "a.", 3000, " and token: DOTTED-NEXT"},
	{"- ", "a", 5000, "@x"},
	// R59: an Authorization header under a quoted key, with a scheme and without one.
	{`{"Authorization":"Bearer `, "t", 30, `","Accept":"json"}`},
	{`'x-authorization': `, "r", 30, ` , next`},
	// R60: the password in a URL's userinfo, the user of an http URL (a Sentry DSN's key), and the URLs that stay: an
	// email address, no @ in the authority, another scheme's user, markup next to a URL.
	{"DATABASE_URL=postgres://app:MADEUP-PW@db1.internal.example.com/app\nREDIS_URL=redis://:MADEUP@cache:6379\n", "", 0,
		"dsn https://abc123def@o1.ingest.example.com/2 and ssh://git@github.com/org/repo mail alice@example.com\n"},
	{"postgres://app:p@ss@db/app?x=a@b.co and amqp://guest:a:b@mq:5672/ ", "", 0,
		`<a href="https://example.com">a@b.co</a> [x](https://example.com)a@b.co http://host:8080/@x https://:@h/x x://u:p@h,y://v:q@h2`},
	{"clone https://x-access-token:ghs_abcDEF123_x@github.com/org/repo.git and https://ci:MADEUP@git.example.com\n", "", 0,
		"mailto:a@b.co https://example.com/?to=a@b.co 'postgres://u:pw@h' \"mysql2://root:pw@127.0.0.1/db\" (http://k@h)"},
	{"- ", "://", 2000, "u:p@h"},
	{"- x://", ":", 5000, "@h"},
	{"- x://u:", "p", 5000, "@h"},
	{"- https://", "a", 5000, "@h"},
	{"- ", "https://", 700, "k@h"},
	// R60: an all-caps name that ends in a key word, the names of the allow list, the verdict of a test, and the
	// lower-case, longer and glued names that stay.
	{"PGPASSWORD=MADEUP psql\nREDISPASS: MADEUP\nexport MYSQL_PWD='MADEUP ONE'\nGITHUBTOKEN=\"MADEUP\" DB_PASS=x\n", "", 0,
		"BYPASS=1 COMPASS_URL=http://x SKIP_BYPASS: true OLDPWD=/home/runner --- PASS: TestX (0.00s) PASS=ok\n"},
	{"BYPASS:\nPGPASSWORD=MADEUP\nCOMPASS=north PGPASSWORD=MADEUP2 BYPASS_PASS=MADEUP3\n", "", 0,
		"bypass=1 redispass=x PGPASSWORD_FILE=/f TOKEN_LIMIT=5 2PASS=x \u00e9PGPASSWORD=x PGPASSWORD\u00e9=x SURPASS=1 TRESPASS=2\n"},
	{"PGPA\u017f\u017fWORD=LONG-S GITHUB\u00feTOKEN=x TO\u212aEN=KELVIN PGPASSWORD=MADEUP\n", "", 0, "xPGPASSWORD=nope PGPASSWORD\nSECOND=line"},
	{"- ", "A", 5000, "PGPASSWORD=MADEUP"},
	{"- ", "PASS", 1500, "=MADEUP PGPASSWORD=MADEUP2"},
	{"- ", "A_", 3000, "TOKEN: MADEUP"},
	{"- ", "BYPASS ", 800, "BYPASS: PGPASSWORD=MADEUP"},
	// R60: npm tokens (36 letters or digits after npm_, or more), and the words that start the same way.
	{"//registry.example.org/:_authToken=npm_abcdefghijABCDEFGHIJ0123456789abcdef\n", "", 0,
		"npm_abcdefghijABCDEFGHIJ0123456789abcdefXYZ npm_abcdefghijABCDEFGHIJ0123456789abcde npm_config_cache=/x my_npm_abcdefghijABCDEFGHIJ0123456789abcdef\n"},
	// R60: curl -u and --user, mysql and mysqldump's -p<password>, --password=<password> and --password <password>,
	// and the options of other tools that stay (a port, a user and group, a database).
	{"curl -u admin:MADEUP https://api.example.com/x\ncurl --user 'admin:MADEUP 2' x\ncurl -uadmin:MADEUP3 x\n", "", 0,
		`curl -sS -X POST "https://x/?a=1&b=2" -H 'Accept: json' -u "admin:MADEUP4" -o out` + "\ncurl -sS \\\n  -u admin:MADEUP5 \\\n  https://x\n"},
	{"mysql -u root -pMADEUP app\nmysqldump -h db --password=MADEUP2 app\nmysql --password MADEUP3 app\n", "", 0,
		"mysql -u root -p'MADEUP 4' app and mysql --password=\"MADEUP 5\" app and gunzip < d.gz | mysql -pMADEUP6 app\n"},
	{"ssh -p 22 h; ssh -p22 h; docker run -p 8080:80 -u 1000:1000 img; psql -p 5432 db\n", "", 0,
		"mysql -u root -p app\nmysql -P3306 -h db\ncurl x && docker run -u 1000:1000 i\ncurl x | tar --user a:b\ncurl x ; ssh -p22 h\nmysql2 -p22\n"},
	{"curl -s x\ndocker run -u 1000:1000 img\nmysql\nssh -p22 h\ncurl -u admin\ncurl -u $CREDS x\ncurl -u :MADEUP x\n", "", 0, ""},
	{"PGPASSWORD=\ncurl -u admin:MADEUP x\nREDISPASS:\nredis://:MADEUP2@cache\nPGTOKEN=\nmysql -pMADEUP3\nDB_PASSWORD=\npostgres://u:MADEUP4@h/x\n", "", 0, ""},
	{"- ", "curl ", 1200, "-u a:b"},
	{"- curl ", "-x ", 2000, ""},
	{"- ", "mysql ", 1000, "-pMADEUP"},
	{"- curl -u ", "a", 5000, ":MADEUP"},
	{"- curl -u a:", "p", 5000, ""},
	{"- mysql --password ", "p", 5000, ""},
	{"- curl ", "&", 3000, " -u a:b"},
	{"- curl ", "\\\n", 1500, " -u a:b"},
	{"curl -\u017fu a:b -u\u00e9x:y\n", "", 0, "mysql\u212a -pMADEUP \u017fmysql -pMADEUP2 curl\u00e9 -u a:b\n"},
}

// FuzzEvidenceScrubMasksEveryByteScrubMasks wants the port's output of a text
// to be Text's with the port's two extra masks: a run of host characters over
// 253 that holds a dot as <host>, then a run of non-space characters still
// over 2,000 as [...]. Both only mask more, so the port shows no byte Text
// masks. The port runs the host pattern on each run of host characters; that
// this is Text's host pass on the whole text is checked on each text too.
func FuzzEvidenceScrubMasksEveryByteScrubMasks(f *testing.F) {
	port := startScrubPort(f)
	redact := regexp.MustCompile(strings.Join(execxSecretPatterns(f), "|"))
	for _, seed := range scrubSeeds {
		f.Add(seed.head, seed.fill, seed.n, seed.tail)
	}
	f.Fuzz(func(t *testing.T, head, fill string, n uint16, tail string) {
		text := scrubText(head, fill, n, tail)
		notMaskedNow(t, redact, text)
		beforeHost, clean := scrubReplay(redact, text)
		if want := Text(text); clean != want {
			t.Fatalf("the replay of Text parted from Text:\nreplay: %q\nText:   %q", brief(clean), brief(want))
		}
		if runs := hostRuns(beforeHost, false); runs != clean {
			t.Fatalf("Text's host pass on each run of host characters is not its pass on the whole text:\nruns:  %q\nText:  %q",
				brief(runs), brief(clean))
		}
		want := capRuns(hostRuns(beforeHost, true))
		if out := port.scrub(t, text); out != want {
			t.Fatalf("evidence.sh's scrub is not Text with its two extra masks\ntext:  %q\nText:  %q\nwant:  %q\nport:  %q",
				brief(text), brief(clean), brief(want), brief(out))
		}
	})
}
