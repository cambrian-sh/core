package config

import "testing"

// ENG-2170: execution.retrieval.superseded_tags was renamed superseded_markers (the
// markers are matched against a row's labels first, its tags only for older rows).
// An operator's value under the old name still loads, from a file, the durable store
// and the environment; the new name stated in the same layer wins.
func TestDeprecatedSupersededTags_LoadsAsMarkers(t *testing.T) {
	cases := map[string]struct {
		tuning string
		env    map[string]string
		store  mapStore
		want   []string
	}{
		"default":        {want: []string{"superseded"}},
		"old key, file":  {tuning: `{"schema_version": 4, "execution": {"retrieval": {"superseded_tags": ["retired"]}}}`, want: []string{"retired"}},
		"new key, file":  {tuning: `{"schema_version": 4, "execution": {"retrieval": {"superseded_markers": ["obsolete"]}}}`, want: []string{"obsolete"}},
		"both in a file": {tuning: `{"schema_version": 4, "execution": {"retrieval": {"superseded_tags": ["retired"], "superseded_markers": ["obsolete"]}}}`, want: []string{"obsolete"}},
		"old key, store": {store: mapStore{"execution.retrieval.superseded_tags": []any{"withdrawn"}}, want: []string{"withdrawn"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			if tc.tuning != "" {
				writeCfg(t, dir, "tuning.json", tc.tuning)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var store Store
			if tc.store != nil {
				store = tc.store
			}
			cfg, _ := loadFrom(t, dir, store)
			got := cfg.Execution.Retrieval.SupersededMarkers
			if len(got) != len(tc.want) {
				t.Fatalf("superseded_markers = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("superseded_markers = %v, want %v", got, tc.want)
				}
			}
			if err := cfg.Execution.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}
