package config

import "testing"

// TestDefaults pins the splitting/mine tuning defaults so a bare config (one
// conference, no tuning fields) still gets sensible behavior.
func TestDefaults(t *testing.T) {
	raw := []byte(`{"conferences":[{"name":"ICML","query":"\"ICML\""}]}`)
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.MinBalance != 0.15 {
		t.Errorf("min_balance default = %v, want 0.15", c.MinBalance)
	}
	if c.MaxProbes != 8 {
		t.Errorf("max_probes default = %d, want 8", c.MaxProbes)
	}
	if c.Headroom != 0.8 {
		t.Errorf("headroom default = %v, want 0.8", c.Headroom)
	}
	if c.MinPapersToTrust != 10 {
		t.Errorf("min_papers_to_trust default = %d, want 10", c.MinPapersToTrust)
	}
	if c.MinCoverage != 0.5 {
		t.Errorf("min_coverage default = %v, want 0.5", c.MinCoverage)
	}
	if c.MaxProbesPerConf != 60 {
		t.Errorf("max_probes_per_conf default = %d, want 60", c.MaxProbesPerConf)
	}
	if c.MineBigrams {
		t.Error("mine_bigrams default = true, want false")
	}
	if c.MaxMinedKeywords != 200 {
		t.Errorf("max_mined_keywords default = %d, want 200", c.MaxMinedKeywords)
	}
	if c.MinCompletionRatio != 0.5 {
		t.Errorf("min_completion_ratio default = %v, want 0.5", c.MinCompletionRatio)
	}
}

// TestExplicitTuningValues: values supplied in JSON must survive Parse.
func TestExplicitTuningValues(t *testing.T) {
	raw := []byte(`{
		"conferences":[{"name":"ICML","query":"\"ICML\""}],
		"min_balance":0.4, "max_probes":3, "headroom":0.5,
		"min_papers_to_trust":25, "min_coverage":0.7, "max_probes_per_conf":40,
		"mine_bigrams":true, "max_mined_keywords":50, "min_completion_ratio":0.3
	}`)
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.MinBalance != 0.4 || c.MaxProbes != 3 || c.Headroom != 0.5 {
		t.Errorf("tuning not honored: %+v", c)
	}
	if c.MinPapersToTrust != 25 || !c.MineBigrams || c.MaxMinedKeywords != 50 {
		t.Errorf("tuning not honored: %+v", c)
	}
	if c.MinCoverage != 0.7 || c.MaxProbesPerConf != 40 {
		t.Errorf("tuning not honored: %+v", c)
	}
	if c.MinCompletionRatio != 0.3 {
		t.Errorf("min_completion_ratio not honored: %+v", c)
	}
}

// TestTuningValidation: out-of-range tuning values are rejected.
func TestTuningValidation(t *testing.T) {
	cases := []string{
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_balance":1.5}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"headroom":1.1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_papers_to_trust":-3}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_coverage":1.1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"max_mined_keywords":-1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_completion_ratio":1.5}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_completion_ratio":-0.1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"max_side_fraction":0.4}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"max_side_fraction":1}`,
	}
	for _, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) accepted invalid config, want error", raw)
		}
	}
}

// TestUseShortName verifies that use_short_name replaces the full query with
// the conference's short name, quoted so it is routed to the publication field.
func TestUseShortName(t *testing.T) {
	raw := []byte(`{"conferences":[{"name":"AAAI","query":"\"AAAI Conference on Artificial Intelligence\"","use_short_name":true}]}`)
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Conferences[0].Query != `"AAAI"` {
		t.Errorf("use_short_name query = %q, want %q", c.Conferences[0].Query, `"AAAI"`)
	}
}

// TestVenueTypeAndAliases: type defaults to conference, only conference and
// journal are accepted, and aliases are loaded as given.
func TestVenueTypeAndAliases(t *testing.T) {
	c, err := Parse([]byte(`{"conferences":[
		{"name":"ICML","query":"\"ICML\""},
		{"name":"TCYB","query":"\"IEEE Transactions on Cybernetics\"","type":"journal",
		 "aliases":["\"IEEE Transactions on Systems, Man, and Cybernetics, Part B\""]}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Conferences[0].Type; got != "conference" {
		t.Errorf("default type = %q, want conference", got)
	}
	tcyb := c.Conferences[1]
	if tcyb.Type != "journal" || len(tcyb.Aliases) != 1 {
		t.Errorf("TCYB = %+v, want a journal with one alias", tcyb)
	}

	for _, bad := range []string{
		`{"conferences":[{"name":"X","query":"\"X\"","type":"magazine"}]}`,
		`{"conferences":[{"name":"X","query":"\"X\"","aliases":[" "]}]}`,
		`{"conferences":[{"name":"X","query":"\"X\"","aliases":["\"X\""]}]}`,
		`{"conferences":[{"name":"X","query":"\"X\"","aliases":["\"Y\"","\"Y\""]}]}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) accepted an invalid venue", bad)
		}
	}
}

// TestShippedConfigLoads: the repository's config.json must stay valid.
func TestShippedConfigLoads(t *testing.T) {
	if _, err := Load("../../config.json"); err != nil {
		t.Fatalf("config.json: %v", err)
	}
}
