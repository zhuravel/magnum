package config

import "testing"

// Slot names and paths are unique in the registry, so two pools that render the
// same one would share a slot: the second pool's provisioning finds the first
// pool's review1, returns it and never grows, with no error. Validation refuses
// the pair instead.
func TestValidatePoolsRejectsSharedSlotNamesAndPaths(t *testing.T) {
	pool := func(repo, name, path string, max int) Pool {
		return Pool{Repo: repo, MainClone: "/p/" + repo, SlotName: name, SlotPath: path, Max: max}
	}
	cases := []struct {
		name  string
		pools []Pool
		want  []string // fragments of the error; nil = valid
	}{
		{"distinct names and paths", []Pool{
			pool("acme/big", "big{n}", "/p/big.review{n}", 3),
			pool("acme/small", "small{n}", "/p/small.review{n}", 3),
		}, nil},
		{"the same slot_name template", []Pool{
			pool("acme/big", "review{n}", "/p/big.review{n}", 3),
			pool("acme/small", "review{n}", "/p/small.review{n}", 3),
		}, []string{"pool acme/small", `slot_name renders "review1"`, "pool acme/big"}},
		{"the same slot_path template", []Pool{
			pool("acme/big", "big{n}", "/p/review{n}", 3),
			pool("acme/small", "small{n}", "/p/review{n}", 3),
		}, []string{"pool acme/small", `slot_path renders "/p/review1"`, "pool acme/big"}},
		{"paths that differ only by a ./ segment", []Pool{
			pool("acme/big", "big{n}", "~/p/review{n}", 2),
			pool("acme/small", "small{n}", "~/p/./review{n}", 2),
		}, []string{"pool acme/small", "slot_path renders"}},
		{"templates that meet only at a later slot", []Pool{
			pool("acme/big", "rev{n}", "/p/big{n}", 12),
			pool("acme/small", "rev1{n}", "/p/small{n}", 3),
		}, []string{"pool acme/small", `slot_name renders "rev11"`}}, // rev{n} with n = 11, rev1{n} with n = 1
		{"templates that cannot meet inside the pools' sizes", []Pool{
			pool("acme/big", "rev{n}", "/p/big{n}", 9),
			pool("acme/small", "rev1{n}", "/p/small{n}", 9),
		}, nil}, // rev1..rev9 against rev11..rev19
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Watches[0].Include = []string{"*"}
			cfg.Pools = tc.pools
			err := cfg.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("valid: %v", err)
				}
				return
			}
			wantError(t, err, tc.want...)
		})
	}
}
