package runtime

import (
	"strings"
	"testing"
)

func TestScrubDropsTokensEmailsHostsAndLongPastes(t *testing.T) {
	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"AKIAABCDEFGHIJKLMNOP",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"hunter2",
		"alice@example.com",
		"db1.internal.example.com",
		"10.0.4.12",
		"xoxb-123456789012-abcdefghijkl",
		"sk-abcdefghijklmnopqrstuvwxyz123456",
		"glpat-abcdefghij0123456789",
	}
	in := "push with " + secrets[0] + " and " + secrets[1] + ", aws_secret_access_key=" + secrets[2] +
		" password=" + secrets[3] + "\nmail " + secrets[4] + " about " + secrets[5] + " and " + secrets[6] +
		", token " + secrets[7] + " and " + secrets[8] + " and " + secrets[9] +
		"\nkeep spec/models/user_spec.rb:12, main.go, config/database.yml and v1.2.3"
	clean, kept := Scrub(in)
	if !kept {
		t.Fatalf("a short message was dropped: %q", clean)
	}
	for _, s := range secrets {
		if strings.Contains(clean, s) {
			t.Errorf("the scrubbed text still holds %q:\n%s", s, clean)
		}
	}
	for _, keep := range []string{"password=", "aws_secret_access_key=", "spec/models/user_spec.rb:12", "main.go",
		"config/database.yml", "v1.2.3", "push with", "keep"} {
		if !strings.Contains(clean, keep) {
			t.Errorf("the scrub took %q out:\n%s", keep, clean)
		}
	}

	if clean, kept := Scrub(strings.Repeat("a line of a pasted log\n", 60)); kept {
		t.Errorf("a 60-line paste was kept: %d bytes", len(clean))
	}
	if _, kept := Scrub(strings.Repeat("x", 5000)); kept {
		t.Error("a 5000-byte paste was kept")
	}
	if clean, kept := Scrub("say hi to the writer"); !kept || clean != "say hi to the writer" {
		t.Errorf("Scrub of plain text = %q, %v", clean, kept)
	}
}

func TestScrubMasksTheValueOfAQuotedSecretKey(t *testing.T) {
	longJSON := `{"items":[` + strings.Repeat(`{"id":12345,"name":"widget","tags":["a","b"]},`, 50) + `{"id":1}],` +
		`"password":"MADEUP-PW-123","user":"alice","note":"` + strings.Repeat("n", 100) + `"}`
	if len(longJSON) <= 2000 {
		t.Fatalf("the minified JSON of the test is %d bytes, not over 2,000", len(longJSON))
	}
	cases := []struct{ name, in, want string }{
		{"JSON, double quotes, no space", `{"password":"MADEUP-PW-123","user":"alice"}`, `{"password":<redacted>,"user":"alice"}`},
		{"JSON, a space after the colon", `"api_key": "MADEUP"`, `"api_key": <redacted>`},
		{"single quotes around the key and a value of two words", `'secret': 'x y'`, `'secret': <redacted>`},
		{"TOML or YAML, a quoted key and an equals sign", `"token" = "MADEUP-TOKEN"`, `"token" = <redacted>`},
		{"spaces before the separator", `"password"   :   "MADEUP"`, `"password"   :   <redacted>`},
		{"a word value ends at a comma", `{"client_secret": MADEUP-WORD, "n": 1}`, `{"client_secret": <redacted>, "n": 1}`},
		{"a number value ends at a brace", `{"token":12345}`, `{"token":<redacted>}`},
		{"a word value ends at a bracket", `["pwd": MADEUP]`, `["pwd": <redacted>]`},
		{"the key's case is any", `{"Password":"MADEUP","API_KEY":"MADEUP2"}`, `{"Password":<redacted>,"API_KEY":<redacted>}`},
		{"a value that spans lines in its quotes is masked whole", "{\"password\": \"MADEUP-LINE-ONE\nMADEUP-LINE-TWO\", \"user\": \"alice\"}",
			`{"password": <redacted>, "user": "alice"}`},
		{"the value on the next line", "\"private_key\":\n\"MADEUP\"", "\"private_key\":\n<redacted>"},
		{"a key of an object inside an object", `{"db":{"password":"MADEUP","host":"localhost"},"token":"MADEUP2"}`,
			`{"db":{"password":<redacted>,"host":"localhost"},"token":<redacted>}`},
		{"a quoted value that holds an unquoted pair is masked whole", `{"token":"x password: y"}`, `{"token":<redacted>}`},
		{"the AWS key names", `{"aws_secret_access_key":"MADEUP","access_key":"MADEUP2"}`, `{"aws_secret_access_key":<redacted>,"access_key":<redacted>}`},
		{"a header name that ends in a key word, after a hyphen", `{"X-Api-Key":"MADEUP","x-auth-token":"MADEUP2"}`,
			`{"X-Api-Key":<redacted>,"x-auth-token":<redacted>}`},
		{"a minified JSON over 2,000 characters", longJSON, strings.Replace(longJSON, `"password":"MADEUP-PW-123"`, `"password":<redacted>`, 1)},
	}
	for _, c := range cases {
		clean, _ := Scrub(c.in)
		if clean != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, brief(c.in), brief(clean), brief(c.want))
		}
		if strings.Contains(clean, "MADEUP") {
			t.Errorf("%s: the scrubbed text still holds a made-up secret: %q", c.name, brief(clean))
		}
	}
}

// A quoted key is masked where the same key unquoted is, by scrubPairs' rules: the key word starts at a word
// boundary (a hyphen or a dot before it is one, an underscore or a letter is not) and ends the key.
func TestAQuotedKeyIsMaskedWhereTheUnquotedKeyIs(t *testing.T) {
	for _, c := range []struct {
		key    string
		masked bool
	}{
		{"password", true}, {"api-key", true}, {"x-api-key", true}, {"X_Api_Key", false}, {"app.secret", true},
		{"auth-token", true}, {"aws_secret_access_key", true},
		{"password_hint", false}, {"tokens_used", false}, {"secrets", false}, {"mypassword", false}, {"db_password", false},
		{"token_count", false}, {"passwords", false}, {"keyword", false},
	} {
		quoted, _ := Scrub(`{"` + c.key + `": "MADEUP-QUOTED", "other": 1}`)
		if got := !strings.Contains(quoted, "MADEUP-QUOTED"); got != c.masked {
			t.Errorf(`"%s": a quoted key's value masked = %v, want %v: %q`, c.key, got, c.masked, quoted)
		}
		unquoted, _ := Scrub(c.key + ": MADEUP-UNQUOTED")
		if got := !strings.Contains(unquoted, "MADEUP-UNQUOTED"); got != c.masked {
			t.Errorf("%s: an unquoted key's value masked = %v, want %v (the quoted key must follow it): %q", c.key, got, c.masked, unquoted)
		}
	}
	for _, in := range []string{
		`{"password_hint":"the name of my dog","tokens_used":3}`,
		`{"passwords":["a","b"],"secrets_count":2}`,
		`{"user":"password","note":"token"}`,
		`"password" is a word, and so is 'token' too`,
	} {
		if clean, _ := Scrub(in); clean != in {
			t.Errorf("a quoted word that is no key with a key word's name was changed:\n in:  %q\n got: %q", in, clean)
		}
	}
}
