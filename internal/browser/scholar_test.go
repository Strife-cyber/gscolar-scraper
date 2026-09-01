package browser

import "testing"

func TestDecomposeAdvancedQuery(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		wantVenue   string
		wantAll     string
		wantWithout string
		wantOK      bool
	}{
		{
			name:      "quoted conference phrase -> venue",
			query:     `"International Conference on Machine Learning"`,
			wantVenue: "International Conference on Machine Learning",
			wantOK:    true,
		},
		{
			name:        "phrase with keyword chain",
			query:       `"International Conference on Machine Learning" AND "learning" -"neural"`,
			wantVenue:   "International Conference on Machine Learning",
			wantAll:     "learning",
			wantWithout: "neural",
			wantOK:      true,
		},
		{
			name:        "source: form",
			query:       `source:"NeurIPS" AND "attention" AND "graph" -"video" -"speech"`,
			wantVenue:   "NeurIPS",
			wantAll:     "attention graph",
			wantWithout: "video speech",
			wantOK:      true,
		},
		{
			name:    "bare text falls back to all words",
			query:   "foo bar",
			wantAll: "foo bar",
			wantOK:  true,
		},
		{
			name:   "empty query",
			query:  "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			venue, all, without, ok := decomposeAdvancedQuery(tt.query)
			if venue != tt.wantVenue || all != tt.wantAll || without != tt.wantWithout || ok != tt.wantOK {
				t.Errorf("decomposeAdvancedQuery(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tt.query, venue, all, without, ok,
					tt.wantVenue, tt.wantAll, tt.wantWithout, tt.wantOK)
			}
		})
	}
}
