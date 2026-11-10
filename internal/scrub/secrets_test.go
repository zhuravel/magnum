package scrub

// Secrets is Text without the email and host passes, for text where a host or an address is what the reader needs
// (a failing test's output in a CI log). It must mask what Text masks of secrets, and nothing less.

import (
	"regexp"
	"strings"
	"testing"
)

func TestSecretsKeepsAHostAndAnEmailAndMasksTheSecrets(t *testing.T) {
	in := "connect db1.internal.example.com (10.0.4.12) as alice@example.com\n" +
		"DB_PASSWORD=MADEUP-PW\nRAILS_MASTER_KEY: MADEUP-KEY\n" +
		`{"token":"MADEUP-TOKEN"} ` + "ghp_abcdefghijklmnopqrstuvwxyz0123456789 AKIAABCDEFGHIJKLMNOP\n"
	secrets := Secrets(in)
	for _, keep := range []string{"db1.internal.example.com", "10.0.4.12", "alice@example.com"} {
		if !strings.Contains(secrets, keep) {
			t.Errorf("Secrets took %q out:\n%s", keep, secrets)
		}
	}
	for _, gone := range []string{"MADEUP-PW", "MADEUP-KEY", "MADEUP-TOKEN", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "AKIAABCDEFGHIJKLMNOP"} {
		if strings.Contains(secrets, gone) {
			t.Errorf("Secrets still holds %q:\n%s", gone, secrets)
		}
	}
	text := Text(in)
	for _, gone := range []string{"db1.internal.example.com", "10.0.4.12", "alice@example.com"} {
		if strings.Contains(text, gone) {
			t.Errorf("Text still holds %q:\n%s", gone, text)
		}
	}
	for _, mark := range []string{"<host>", "<email>"} {
		if !strings.Contains(text, mark) || strings.Contains(secrets, mark) {
			t.Errorf("%s: Text has it = %v, Secrets has it = %v, want true and false", mark, strings.Contains(text, mark), strings.Contains(secrets, mark))
		}
	}
}

// secretsPassesOf are the replay's passes up to the email pass: Secrets's.
func secretsPassesOf(redact *regexp.Regexp) []scrubPass {
	var passes []scrubPass
	for _, p := range scrubPasses(redact) {
		if p.re == scrubEmail {
			break
		}
		passes = append(passes, p)
	}
	return passes
}

// replayOf runs passes over text as the scrub does.
func replayOf(passes []scrubPass, text string) string {
	for _, p := range passes {
		if p.run != nil {
			text = p.run(text)
			continue
		}
		if p.with != "" {
			text = p.re.ReplaceAllString(text, p.with)
			continue
		}
		text = p.re.ReplaceAllStringFunc(text, func(m string) string {
			if i := strings.IndexByte(m, ':'); i >= 0 && strings.HasPrefix(strings.ToLower(m), "authorization") {
				return m[:i+1] + " <redacted>"
			}
			return "<redacted>"
		})
	}
	return text
}

// secretsMaskWhatTextMasksOfSecrets checks Secrets on a text: it is the replay of the passes before the email pass;
// Text is Secrets followed by the email and host passes; Secrets and Text differ only where those two passes find
// something; and Secrets leaves no byte of text that the scrub before R58 masked by its secret passes (Redact, the
// tokens and the first key-value pass).
func secretsMaskWhatTextMasksOfSecrets(t *testing.T, redact *regexp.Regexp, text string) {
	t.Helper()
	secrets, full := Secrets(text), Text(text)
	if replayed := replayOf(secretsPassesOf(redact), text); replayed != secrets {
		t.Fatalf("the replay of Secrets's passes parted from Secrets on %q:\nreplay:  %q\nSecrets: %q", brief(text), brief(replayed), brief(secrets))
	}
	afterEmail := scrubUserinfoHost.ReplaceAllString(scrubEmail.ReplaceAllString(secrets, "<email>"), "$1<host>")
	if want := scrubHost.ReplaceAllString(afterEmail, "<host>"); full != want {
		t.Fatalf("Text is not Secrets and then the email, userinfo host and host passes on %q:\nText: %q\nwant: %q", brief(text), brief(full), brief(want))
	}
	if secrets != full && !scrubEmail.MatchString(secrets) && !scrubUserinfoHost.MatchString(secrets) && !scrubHost.MatchString(secrets) {
		t.Fatalf("Secrets and Text differ where no email or host is: %q\nSecrets: %q\nText:    %q", brief(text), brief(secrets), brief(full))
	}
	before := scrubBeforeR58(redact)
	secretsBefore := survivors(text, before[:3])
	now := survivors(text, tracked(secretsPassesOf(redact)))
	for i := range text {
		if now[i] && !secretsBefore[i] {
			t.Fatalf("Secrets shows a byte (offset %d) that the scrub before R58 masked:\ntext:   %q\nbefore: %q\nnow:    %q",
				i, brief(text), brief(shown(text, secretsBefore)), brief(shown(text, now)))
		}
	}
}

func TestSecretsMasksEveryByteTextMasksExceptEmailsAndHosts(t *testing.T) {
	redact := regexp.MustCompile(strings.Join(execxSecretPatterns(t), "|"))
	for _, seed := range scrubSeeds {
		secretsMaskWhatTextMasksOfSecrets(t, redact, scrubText(seed.head, seed.fill, seed.n, seed.tail))
	}
	for _, text := range grammarTexts() {
		secretsMaskWhatTextMasksOfSecrets(t, redact, text)
	}
}
