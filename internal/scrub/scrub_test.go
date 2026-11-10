package scrub

import (
	"math/rand"
	"regexp"
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

// R60: a password in a URL, a joined env name, an npm token and a command line's password. Secrets keeps a host and an
// email, so the cases use it.
func TestSecretsMasksThePasswordInAURLsUserinfo(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a database URL", "DATABASE_URL=postgres://app:S3cretPw@db.example.com/app", "DATABASE_URL=postgres://app:<redacted>@db.example.com/app"},
		{"an empty user", "redis://:pw@cache:6379", "redis://:<redacted>@cache:6379"},
		{"a password that holds an at sign", "postgres://app:p@ss@db/app", "postgres://app:<redacted>@db/app"},
		{"a password that holds a colon", "amqp://guest:a:b@mq:5672/", "amqp://guest:<redacted>@mq:5672/"},
		{"a clone URL in a log line", "Cloning into 'x' from https://ci:MADEUP-PW@git.example.com/org/repo.git ...", "Cloning into 'x' from https://ci:<redacted>@git.example.com/org/repo.git ..."},
		{"a quoted URL", `url: "mysql2://root:MADEUP@127.0.0.1:3306/app_test"`, `url: "mysql2://root:<redacted>@127.0.0.1:3306/app_test"`},
		{"two URLs on one line", "a=ftp://u:one@h1 b=http://v:two@h2/x", "a=ftp://u:<redacted>@h1 b=http://v:<redacted>@h2/x"},
		{"a Sentry DSN", "https://abc123def@o1.ingest.example.com/2", "https://<redacted>@o1.ingest.example.com/2"},
		{"a DSN in a setting", "SENTRY_DSN=https://0123456789abcdef0123456789abcdef@o4.ingest.example.com/17,", "SENTRY_DSN=https://<redacted>@o4.ingest.example.com/17,"},
		{"an older DSN keeps its public key and loses its secret", "http://pub1:sec2@sentry.example.com/9", "http://pub1:<redacted>@sentry.example.com/9"},
		{"a token as the user of a plain-http URL", "http://0123456789abcdef@registry.example.com/v2/", "http://<redacted>@registry.example.com/v2/"},
	} {
		got := Secrets(c.in)
		if got != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, got, c.want)
		}
	}
	// Text keeps no host either: the password is masked and the host after it too.
	if got, want := Text("DATABASE_URL=postgres://app:S3cretPw@db.example.com/app"), "DATABASE_URL=postgres://app:<redacted>@<host>/app"; got != want {
		t.Errorf("Text of a database URL = %q, want %q", got, want)
	}
	// An email address, a URL with no @ in its authority, and a user name that is no secret-bearing form stay.
	for _, in := range []string{
		"mail alice@example.com about it",
		"mailto:alice@example.com",
		"https://example.com/?to=alice@example.com",
		"https://example.com/a@b and http://localhost:3000/path",
		"http://localhost:3000 and https://example.com:8443/x",
		"jdbc:postgresql://localhost:5432/db?user=app",
		"redis://cache:6379",
		"ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git",
		"postgres://app@db/app",
		`<a href="https://example.com">alice@example.com</a>`,
		"[docs](https://example.com)alice@example.com",
		"see https://example.com, alice@example.com",
		"https://:@h/x",
	} {
		if got := Secrets(in); got != in {
			t.Errorf("a text with no password in a URL was changed:\n in:  %q\n got: %q", in, got)
		}
	}
}

func TestSecretsMasksAnAllCapsNameThatEndsInAKeyWord(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"PGPASSWORD", "PGPASSWORD=hunter2 psql -h db", "PGPASSWORD=<redacted> psql -h db"},
		{"REDISPASS", "REDISPASS=x", "REDISPASS=<redacted>"},
		{"MYSQL_PWD", "MYSQL_PWD=MADEUP mysqldump app", "MYSQL_PWD=<redacted> mysqldump app"},
		{"PASSWD", "export PGPASSWD=MADEUP", "export PGPASSWD=<redacted>"},
		{"TOKEN", "GITHUBTOKEN: MADEUP", "GITHUBTOKEN: <redacted>"},
		{"an underscore before PASS", "DB_PASS=MADEUP", "DB_PASS=<redacted>"},
		{"a docker flag", "docker run -e PGPASSWORD=MADEUP img", "docker run -e PGPASSWORD=<redacted> img"},
		{"a quoted value", `PGPASSWORD='MADEUP ONE' psql`, "PGPASSWORD=<redacted> psql"},
		{"digits in the name", "S3PASS=MADEUP", "S3PASS=<redacted>"},
		{"a name that holds an allowed word and goes on", "BYPASS_PASS=MADEUP", "BYPASS_PASS=<redacted>"},
		{"an allowed name with no value, then a secret on the next line", "BYPASS:\nPGPASSWORD=MADEUP", "BYPASS:\nPGPASSWORD=<redacted>"},
		{"an allowed name and a secret on one line", "COMPASS=north PGPASSWORD=MADEUP", "COMPASS=north PGPASSWORD=<redacted>"},
	} {
		got := Secrets(c.in)
		if got != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, got, c.want)
		}
	}
	// A lower-case word stays; the word must end the name; bare PASS (a test's verdict) is no name; and the English
	// words that end in PASS, and OLDPWD, are on the allow list.
	for _, in := range []string{
		"bypass=1", "compass: north", "Bypass=1", "redispass=x", "RedisPass=x", "pass=1", "Pass: ok",
		"PASS=1", "--- PASS: TestSomething (0.00s)", "PASS: spec/models/user_spec.rb", "ok  	example.com/pkg	0.1s", "PASSED=3",
		"BYPASS=1", "BYPASS: true", "MY_BYPASS=1", "COMPASS=north", "COMPASS_URL=http://compass.local/x", "ENCOMPASS: 1",
		"SURPASS=1", "TRESPASS=1", "IMPASS=1", "OVERPASS=1", "UNDERPASS=1", "OLDPWD=/home/runner/work",
		"PGPASSWORD_FILE=/run/secrets/pg", "REDISPASS_HINT=x", "TOKEN_LIMIT=5", "MAX_TOKENS=4096", "PASSWORDS=3",
		"2PASS=x",
	} {
		if got := Secrets(in); got != in {
			t.Errorf("a name that is no secret's was changed:\n in:  %q\n got: %q", in, got)
		}
	}
}

func TestSecretsMasksAnNPMToken(t *testing.T) {
	token := "npm_" + strings.Repeat("aB3", 12)
	for _, c := range []struct{ name, in, want string }{
		{"bare", "published with " + token + " today", "published with <redacted> today"},
		{"in a config line", "//registry.example.org/:_authToken=" + token, "//registry.example.org/:_authToken=<redacted>"},
		{"in a header", "authorization: bearer " + token, "authorization: <redacted>"},
		{"longer than 36", "x " + token + "Zz9 y", "x <redacted> y"},
		{"quoted", `"` + token + `"`, `"<redacted>"`},
	} {
		if got := Secrets(c.in); got != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, got, c.want)
		}
	}
	for _, in := range []string{
		"npm_" + strings.Repeat("a", 35), "npm_config_cache=/tmp/x", "npm_package_name=app", "my_npm_" + strings.Repeat("a", 36),
		"npm install", "npm_lifecycle_event=test",
	} {
		if got := Secrets(in); got != in {
			t.Errorf("a text with no npm token was changed:\n in:  %q\n got: %q", in, got)
		}
	}
}

func TestSecretsMasksThePasswordOfACurlOrMySQLCommandLine(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"curl -u", "curl -u admin:MADEUP https://api.example.com/x", "curl -u admin:<redacted> https://api.example.com/x"},
		{"curl --user", "curl --user admin:MADEUP https://api.example.com/x", "curl --user admin:<redacted> https://api.example.com/x"},
		{"curl -u after the URL and other options", "+ curl -sS -X POST https://api.example.com/x -H 'Accept: json' -u admin:MADEUP -o out",
			"+ curl -sS -X POST https://api.example.com/x -H 'Accept: json' -u admin:<redacted> -o out"},
		{"curl -u attached", "curl -uadmin:MADEUP https://api.example.com/x", "curl -uadmin:<redacted> https://api.example.com/x"},
		{"curl -u quoted", `curl -u "admin:MADEUP" https://api.example.com/x`, `curl -u "admin:<redacted>" https://api.example.com/x`},
		{"curl -u single-quoted", `curl --user 'admin:MADEUP' https://api.example.com/x`, `curl --user 'admin:<redacted>' https://api.example.com/x`},
		{"curl -u with a colon in the password", "curl -u admin:ab:cd https://api.example.com/x", "curl -u admin:<redacted> https://api.example.com/x"},
		{"curl -u an empty user", "curl -u :MADEUP https://api.example.com/x", "curl -u :<redacted> https://api.example.com/x"},
		{"curl by its path", "/usr/bin/curl -u admin:MADEUP x", "/usr/bin/curl -u admin:<redacted> x"},
		{"curl -u on the next line of a continued command", "curl -sS \\\n  -u admin:MADEUP \\\n  https://api.example.com/x", "curl -sS \\\n  -u admin:<redacted> \\\n  https://api.example.com/x"},
		{"curl after an earlier command", "cd app && curl -u admin:MADEUP x", "cd app && curl -u admin:<redacted> x"},
		{"mysql -p attached", "mysql -u root -pMADEUP app", "mysql -u root -p<redacted> app"},
		{"mysqldump -p attached", "mysqldump -h db -uroot -pMADEUP app > dump.sql", "mysqldump -h db -uroot -p<redacted> app > dump.sql"},
		{"mysql -p quoted", `mysql -u root -p'MA DE UP!' app`, `mysql -u root -p<redacted> app`},
		{"mysql --password=", "mysql --user=root --password=MADEUP app", "mysql --user=root --password=<redacted> app"},
		{"mysql --password and a space", "mysql --user=root --password MADEUP app", "mysql --user=root --password <redacted> app"},
		{"mysqldump --password quoted", `mysqldump --password="MADEUP ONE" app`, `mysqldump --password=<redacted> app`},
		{"mysql by its path", "/usr/local/mysql/bin/mysql -pMADEUP", "/usr/local/mysql/bin/mysql -p<redacted>"},
		{"mysql -p after a pipe's other side", "gunzip < d.sql.gz | mysql -pMADEUP app", "gunzip < d.sql.gz | mysql -p<redacted> app"},
	} {
		got := Secrets(c.in)
		if got != c.want {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, got, c.want)
		}
	}
	// -p, -u and --user of other tools are ports, user names and groups: they stay. So does what a masked command
	// line is not: mysql's -p with no password attached (it asks for one), curl -u with no password, and a command
	// that is no curl or mysql.
	for _, in := range []string{
		"ssh -p 22 deploy@host.example.com", "ssh -p22 deploy@host.example.com", "docker run -p 8080:80 nginx", "docker run -p8080:80 nginx",
		"psql -h db -p 5432 -U app", "psql -p5432 app", "scp -P 22 a b", "nc -p 4000 host",
		"docker run -u 1000:1000 img", "docker exec --user 1000:1000 c ls", "chown --user a:b f",
		"mysql -u root -p app", "mysql -u root -p", "mysql -P 3306 -h db", "mysql -P3306", "mysqldump --port 3306 app", "mysql --protocol=tcp",
		"curl -u admin https://api.example.com/x", "curl -u $CREDS https://api.example.com/x", "curl --url https://api.example.com/x",
		"curl https://api.example.com/x && docker run -u 1000:1000 img", "curl x ; docker run -u 1000:1000 img",
		"curl x | docker run -u 1000:1000 img", "curl x || docker run -u 1000:1000 img", "curl -s x\ndocker run -u 1000:1000 img",
		"mysql -e 'select 1' | ssh -p 22 h", "mysql x && ssh -p22 h", "mysql2 -p22", "echo mysql\nssh -p22 h",
	} {
		if got := Secrets(in); got != in {
			t.Errorf("a command line with no password to mask was changed:\n in:  %q\n got: %q", in, got)
		}
	}
}

// A joined name with no value takes the next line's text for its value (the separator crosses a line break), as the
// older key-value passes do. The pass of joined names runs last, after the passes whose commands and URLs it would
// take the first word of, so that no secret shows for it.
func TestAKeyWithNoValueLeavesTheNextR60SecretMasked(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a joined name, then curl", "PGPASSWORD=\ncurl -u admin:MADEUP x", "PGPASSWORD=\n<redacted> -u admin:<redacted> x"},
		{"a joined name, then a URL", "REDISPASS:\nredis://:MADEUP@cache", "REDISPASS:\n<redacted>"},
		{"a joined name, then mysql", "PGTOKEN=\nmysql -pMADEUP", "PGTOKEN=\n<redacted> -p<redacted>"},
		{"an env-style name, then a URL", "DB_PASSWORD=\npostgres://app:MADEUP@db/x", "DB_PASSWORD=\n<redacted>"},
	} {
		got := Secrets(c.in)
		if got != c.want || strings.Contains(got, "MADEUP") {
			t.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", c.name, c.in, got, c.want)
		}
	}
}

// The oracle's grammar (grammarTexts) has no URL, command line or joined name, so R60 holds the new passes to the same two
// checks on grammars of their own: no byte the scrub before R58 masked shows, and Secrets is the replay of its passes.
// 3,000 texts of 3 to 16 pieces each, the same every run. A command's -p<password> keeps its -p, which an email address
// that follows (-pa@b.co) took in whole before, so the grammar of commands and names has no domain in it, and the grammar
// of URLs, which has domains, has no -p.
func TestTheR60PassesMaskNoLessThanTheScrubBeforeR58(t *testing.T) {
	redact := regexp.MustCompile(strings.Join(execxSecretPatterns(t), "|"))
	common := []string{":", "=", " ", "\n", "/", "?", "#", `"`, `'`, "@", "|", ";", "&&", "&", "\\", "\\\n", "MADEUP", "x", "é", "a b",
		"password", "token", "api_key", "SESSION_SECRET", "DB_PASSWORD", "secret_key_base", "Authorization: Bearer ", "PGPASSWORD", "BYPASS"}
	for name, frags := range map[string][]string{
		"commands and names": append([]string{"curl ", "mysql ", "mysqldump ", "-u ", "-uadmin:", "--user ", "-p", "-pMADEUP", "--password",
			"--password=", "-P 3306", "REDISPASS", "MYSQL_PWD", "GITHUBTOKEN", "DB_PASS", "COMPASS", "OLDPWD", "PASS", "TOKEN", "PWD",
			"npm_aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3aB3", "npm_", "https://", "redis://:", "u:p@", "k@"}, common...),
		"URLs and domains": append([]string{"://", "https://", "http://", "redis://:", "ssh://", "u:p@", "k@", "a@b.co", "db.example.com", "h.co", ".", "-",
			"user:pw@", "npm_", "PGPASSWORD="}, common...),
	} {
		t.Run(name, func(t *testing.T) {
			r := rand.New(rand.NewSource(60))
			for range 3000 {
				var sb strings.Builder
				for range 3 + r.Intn(14) {
					sb.WriteString(frags[r.Intn(len(frags))])
				}
				text := sb.String()
				notMaskedNow(t, redact, text)
				secretsMaskWhatTextMasksOfSecrets(t, redact, text)
			}
		})
	}
}
