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
