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
}

// TestExplicitTuningValues: values supplied in JSON must survive Parse.
func TestExplicitTuningValues(t *testing.T) {
	raw := []byte(`{
		"conferences":[{"name":"ICML","query":"\"ICML\""}],
		"min_balance":0.4, "max_probes":3, "headroom":0.5,
		"min_papers_to_trust":25, "min_coverage":0.7, "max_probes_per_conf":40,
		"mine_bigrams":true, "max_mined_keywords":50
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
}

// TestTuningValidation: out-of-range tuning values are rejected.
func TestTuningValidation(t *testing.T) {
	cases := []string{
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_balance":1.5}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"headroom":1.1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_papers_to_trust":-3}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"min_coverage":1.1}`,
		`{"conferences":[{"name":"ICML","query":"\"ICML\""}],"max_mined_keywords":-1}`,
	}
	for _, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) accepted invalid config, want error", raw)
		}
	}
}
