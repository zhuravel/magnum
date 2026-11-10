package scrub

import (
	"strings"
	"testing"
)

func TestTextDropsTokensEmailsAndHosts(t *testing.T) {
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
	clean := Text(in)
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
	if clean := Text("say hi to the writer"); clean != "say hi to the writer" {
		t.Errorf("Text of plain text = %q", clean)
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
		{"a quoted value that holds an unquoted pair: the pair is masked first, as before", `{"token":"x password: y"}`,
			`{"token":<redacted> password: <redacted>`},
		{"the AWS key names", `{"aws_secret_access_key":"MADEUP","access_key":"MADEUP2"}`, `{"aws_secret_access_key":<redacted>,"access_key":<redacted>}`},
		{"a header name that ends in a key word, after a hyphen", `{"X-Api-Key":"MADEUP","x-auth-token":"MADEUP2"}`,
			`{"X-Api-Key":<redacted>,"x-auth-token":<redacted>}`},
		{"a minified JSON over 2,000 characters", longJSON, strings.Replace(longJSON, `"password":"MADEUP-PW-123"`, `"password":<redacted>`, 1)},
	}
	for _, c := range cases {
		clean := Text(c.in)
		if clean != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, brief(c.in), brief(clean), brief(c.want))
		}
		if strings.Contains(clean, "MADEUP") {
			t.Errorf("%s: the scrubbed text still holds a made-up secret: %q", c.name, brief(clean))
		}
	}
}

// A quoted key is masked where the same key unquoted is, by one rule: the key word starts at a word boundary or after
// an underscore (a hyphen or a dot before it is a boundary, a letter or a digit is not) and ends the key.
func TestAQuotedKeyIsMaskedWhereTheUnquotedKeyIs(t *testing.T) {
	for _, c := range []struct {
		key    string
		masked bool
	}{
		{"password", true}, {"api-key", true}, {"x-api-key", true}, {"X_Api_Key", true}, {"app.secret", true},
		{"auth-token", true}, {"aws_secret_access_key", true}, {"db_password", true}, {"GITHUB_TOKEN", true},
		{"MY_APP.SECRET", true}, {"stripe-api-key", true}, {"__token", true}, {"db2_pwd", true},
		{"secret_key_base", true}, {"RAILS_MASTER_KEY", true}, {"signing_key", true}, {"encryption-key", true},
		{"DB_PASSWORD_FILE", false}, {"API_KEY_ID", false}, {"master_keys", false}, {"SECRET_KEY_BASE_DUMMY", false},
		{"password_hint", false}, {"tokens_used", false}, {"secrets", false}, {"mypassword", false}, {"secretary", false},
		{"token_count", false}, {"passwords", false}, {"keyword", false}, {"MAX_TOKENS", false}, {"2password", false},
	} {
		quoted := Text(`{"` + c.key + `": "MADEUP-QUOTED", "other": 1}`)
		if got := !strings.Contains(quoted, "MADEUP-QUOTED"); got != c.masked {
			t.Errorf(`"%s": a quoted key's value masked = %v, want %v: %q`, c.key, got, c.masked, quoted)
		}
		unquoted := Text(c.key + ": MADEUP-UNQUOTED")
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
		if clean := Text(in); clean != in {
			t.Errorf("a quoted word that is no key with a key word's name was changed:\n in:  %q\n got: %q", in, clean)
		}
	}
}

func TestScrubMasksTheValueOfAnEnvStyleName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a password after a prefix and an underscore", "DB_PASSWORD=MADEUP", "DB_PASSWORD=<redacted>"},
		{"a token after a prefix, with a colon", "GITHUB_TOKEN: MADEUP", "GITHUB_TOKEN: <redacted>"},
		{"a secret after a dot, with spaces around the equals sign", "MY_APP.SECRET = MADEUP", "MY_APP.SECRET = <redacted>"},
		{"a quoted key", `"DB_PASSWORD": "MADEUP"`, `"DB_PASSWORD": <redacted>`},
		{"a single-quoted key with hyphens", `'stripe-api-key': MADEUP`, `'stripe-api-key': <redacted>`},
		{"an export line", "export OPENAI_API_KEY=MADEUP", "export OPENAI_API_KEY=<redacted>"},
		{"a quoted value of two words", `GOOGLE_CLIENT_SECRET="MADEUP ONE"`, "GOOGLE_CLIENT_SECRET=<redacted>"},
		{"two lines of an env file", "DB_PASSWORD=MADEUP\nREDIS_PASSWORD=MADEUP2\nDB_HOST=db", "DB_PASSWORD=<redacted>\nREDIS_PASSWORD=<redacted>\nDB_HOST=db"},
		{"a name with the AWS prefix after another prefix", "DEPLOY_AWS_SECRET_ACCESS_KEY=MADEUP", "DEPLOY_AWS_SECRET_ACCESS_KEY=<redacted>"},
		{"a JSON of env names", `{"DB_PASSWORD":"MADEUP","SLACK_BOT_TOKEN":"MADEUP2","DB_HOST":"db"}`, `{"DB_PASSWORD":<redacted>,"SLACK_BOT_TOKEN":<redacted>,"DB_HOST":"db"}`},
		{"a name that is masked and holds a limit", "MAX_TOKEN=4096", "MAX_TOKEN=<redacted>"},
		{"Rails: secret_key_base", "SECRET_KEY_BASE=MADEUP", "SECRET_KEY_BASE=<redacted>"},
		{"Rails: the master key, with a colon", "RAILS_MASTER_KEY: MADEUP", "RAILS_MASTER_KEY: <redacted>"},
		{"Rails: a signing key in JSON", `{"signing_key":"MADEUP","user":"alice"}`, `{"signing_key":<redacted>,"user":"alice"}`},
		{"Rails: an encryption key as a setting", "config.encryption_key = MADEUP", "config.encryption_key = <redacted>"},
		{"Rails: a key name in YAML with a hyphen", `'encryption-key': MADEUP`, `'encryption-key': <redacted>`},
	}
	for _, c := range cases {
		if clean := Text(c.in); clean != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, clean, c.want)
		}
	}
	for _, name := range []string{"DB_PASSWORD", "REDIS_PASSWORD", "POSTGRES_PASSWORD", "GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN",
		"SLACK_BOT_TOKEN", "CSRF_TOKEN", "OPENAI_API_KEY", "STRIPE_API_KEY", "STRIPE_SECRET_KEY", "GOOGLE_CLIENT_SECRET",
		"JWT_SECRET", "SESSION_SECRET", "DEPLOY_AWS_SECRET_ACCESS_KEY", "SECRET_KEY_BASE", "RAILS_MASTER_KEY", "SIGNING_KEY",
		"ENCRYPTION_KEY"} {
		for _, form := range []string{name + "=MADEUP", name + ": MADEUP", `"` + name + `": "MADEUP"`} {
			if clean := Text(form); strings.Contains(clean, "MADEUP") || !strings.Contains(clean, name) {
				t.Errorf("the common name %s is not masked with its name kept: %q -> %q", name, form, clean)
			}
		}
	}
	for _, in := range []string{"password_hint=MADEUP", "TOKENS_USED=3", "MAX_TOKENS=4096", "secretary: Jo", "TOKEN_LIMIT=5",
		`{"password_hint":"x","tokens_used":3,"DB_PASSWORD_FILE_HINT":"y"}`, "mypassword=MADEUP", "2password=MADEUP",
		"DB_PASSWORD_FILE=/run/secrets/db", "API_KEY_ID=MADEUP-ID", `{"api_key_id":"MADEUP-ID","secret_key_base_dummy":1}`} {
		if clean := Text(in); clean != in {
			t.Errorf("a name that only holds a key word was changed:\n in:  %q\n got: %q", in, clean)
		}
	}
}

// The value of a quoted key can hold a quote that a backslash escapes, as a JSON string and a YAML or TOML one do.
func TestScrubReadsTheEscapesOfAQuotedValue(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"an escaped quote", `{"password":"a\"b"}`, `{"password":<redacted>}`},
		{"two escaped quotes and the key after", `{"password":"ab\"cd\"ef","user":"alice"}`, `{"password":<redacted>,"user":"alice"}`},
		{"a single-quoted value", `{'password': 'it\'s fine'}`, `{'password': <redacted>}`},
		{"a value that ends in an escaped backslash", `{"password":"ab\\","user":"alice"}`, `{"password":<redacted>,"user":"alice"}`},
		{"an escaped quote after an escaped backslash", `{"password":"a\\\"b","user":"alice"}`, `{"password":<redacted>,"user":"alice"}`},
		{"an env-style name with an escaped quote", `DB_PASSWORD="a\"b c" next`, `DB_PASSWORD=<redacted> next`},
		{"a string that never ends", `{"password":"ab\"cd`, `{"password":<redacted>`},
	} {
		if clean := Text(c.in); clean != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, clean, c.want)
		}
	}
}

// An Authorization header written as JSON or YAML keeps its token today:
// execx.Redact's pattern needs the colon right after the word, and the key
// words had no authorization (R59). The quoted key's whole value goes, the
// scheme with it; a header with no scheme loses its raw token too.
func TestScrubMasksAQuotedAuthorizationHeader(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"Authorization":"Bearer MADEUP-OPAQUE-TOKEN-1"}`, `{"Authorization":<redacted>}`},
		{`headers: {'authorization': 'token MADEUP-TOKEN-2', 'accept': 'json'}`, `headers: {'authorization': <redacted>, 'accept': 'json'}`},
		{`"X-Authorization" = "MADEUP-RAW-3"`, `"X-Authorization" = <redacted>`},
		{`Authorization: MADEUP-RAW-4`, `Authorization: <redacted>`},
		{`Authorization: Bearer MADEUP-OPAQUE-5`, `Authorization: <redacted>`},
	} {
		if got := Secrets(tc.in); got != tc.want {
			t.Errorf("Secrets(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
