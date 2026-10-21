package confedit

import "testing"

// Each save of the settings screen puts a dated "was" note in front of the
// line's comment, and the notes of the earlier saves piled up behind it
// ("# 2026-10-21: was "xhigh"; 2026-10-20: was "high"; 2026-10-19: ..."). A
// line keeps the newest only; a comment of the user's, even a dated one that
// says "was", stays.
func TestANoteReplacesTheEarlierWasNotesOfItsLine(t *testing.T) {
	pipeline := Section("pipeline")
	cases := []struct {
		name, src, want string
		change          Change
	}{
		{
			name:   "the note of the last save goes",
			src:    "[pipeline]\nspend = \"low\"   # 2026-10-19: was \"high\"; the level\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"; the level\n",
		},
		{
			name:   "so do all the notes that piled up",
			src:    "[pipeline]\nspend = \"low\"   # 2026-10-19: was \"high\"; 2026-10-18: was \"xhigh\"; 2026-10-17: was the default; the level\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"; the level\n",
		},
		{
			name:   "a line with no other comment keeps the new note alone",
			src:    "[pipeline]\nspend = \"low\"  # 2026-10-19: was \"high\"\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"\n",
		},
		{
			name:   "a semicolon inside the old value does not end the note",
			src:    "[pipeline]\nspend = \"low\"  # 2026-10-19: was \"a; b\"; the level\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"; the level\n",
		},
		{
			name:   "the note of a level table's cells goes",
			src:    "[spend.high]\nclaude-simplify = { model = \"opus\" }  # 2026-10-19: was model \"sonnet\", effort unset; why\n",
			change: noted(set(Section("spend", "high"), "claude-simplify.model", "fable"), `was model "opus"`),
			want:   "[spend.high]\nclaude-simplify = { model = \"fable\" } # 2026-10-20: was model \"opus\"; why\n",
		},
		{
			name:   "a removal drops it too and writes its own",
			src:    "[pipeline]\nspend = \"low\"  # 2026-10-19: was \"high\"; the level\n",
			change: noted(unset(pipeline, "spend"), `spend back to the default, was "low"`),
			want:   "[pipeline]\n# 2026-10-20: spend back to the default, was \"low\"; the level\n",
		},
		{
			name:   "a dated comment of the user's that says was, in words, stays",
			src:    "[pipeline]\nspend = \"low\"  # 2026-10-02: was 3, raised after the queue grew\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"; 2026-10-02: was 3, raised after the queue grew\n",
		},
		{
			name:   "a comment that only starts with a date stays",
			src:    "[pipeline]\nspend = \"low\"  # 2026-10-14: medium -> high\n",
			change: noted(set(pipeline, "spend", "medium"), `was "low"`),
			want:   "[pipeline]\nspend = \"medium\" # 2026-10-20: was \"low\"; 2026-10-14: medium -> high\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Parse([]byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Apply(sweepNow, c.change)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
