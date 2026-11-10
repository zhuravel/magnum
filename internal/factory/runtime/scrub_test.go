package runtime

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/scrub"
)

func TestScrubDropsTokensEmailsHostsAndLongPastes(t *testing.T) {
	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"hunter2",
		"RAILS-MASTER-MADEUP",
		"alice@example.com",
		"db1.internal.example.com",
		"10.0.4.12",
	}
	in := "push with " + secrets[0] + ", password=" + secrets[1] + " RAILS_MASTER_KEY=" + secrets[2] +
		"\nmail " + secrets[3] + " about " + secrets[4] + " and " + secrets[5] +
		"\nkeep spec/models/user_spec.rb:12, main.go, config/database.yml and v1.2.3"
	clean, kept := Scrub(in)
	if !kept {
		t.Fatalf("a short message was dropped: %q", clean)
	}
	if want := scrub.Text(in); clean != want {
		t.Errorf("Scrub does not mask as scrub.Text does:\n got:  %q\n want: %q", clean, want)
	}
	for _, s := range secrets {
		if strings.Contains(clean, s) {
			t.Errorf("the scrubbed text still holds %q:\n%s", s, clean)
		}
	}
	for _, keep := range []string{"password=", "RAILS_MASTER_KEY=", "spec/models/user_spec.rb:12", "main.go",
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

// The limits sit on the edge: 40 lines and 4000 bytes are kept, one more is a paste.
func TestScrubKeepsAMessageOfExactlyTheLimitsAndDropsOneMore(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		kept bool
	}{
		{"40 lines", strings.Repeat("a line\n", 40), true},
		{"40 lines without the last line break", strings.TrimSuffix(strings.Repeat("a line\n", 40), "\n"), true},
		{"41 lines", strings.Repeat("a line\n", 41), false},
		{"4000 bytes", strings.Repeat("x", 4000), true},
		{"4001 bytes", strings.Repeat("x", 4001), false},
	} {
		if _, kept := Scrub(c.in); kept != c.kept {
			t.Errorf("%s: kept = %v, want %v", c.name, kept, c.kept)
		}
	}
}
