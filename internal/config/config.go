// Package config loads and validates the scraper's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the top-level configuration. All timing values are [min,max]
// millisecond ranges used to draw uniform-random delays (with jitter).
type Config struct {
	Browser            BrowserConfig   `json:"browser"`
	Conferences        []ConferenceCfg `json:"conferences"`
	Timing             TimingConfig    `json:"timing"`
	AdaptiveThrottle   ThrottleConfig  `json:"adaptive_throttle"`
	Keywords           []string        `json:"keywords"`
	MaxKeywords        int             `json:"max_keywords"`         // max keywords the split chain may use per query (default 5)
	Database           string          `json:"database"`             // SQLite file path (default "scholar.db")
	StartYear          int             `json:"start_year"`           // lower bound for "2000 to present" (default 2000)
	EndYear            int             `json:"end_year"`             // upper bound of the planning range; 0 = the current year (see EffectiveEndYear)
	MaxResults         int             `json:"max_results"`          // Scholar's cap (default 1000)
	PlanWindowYears    int             `json:"plan_window_years"`    // base year-window width the planner walks: 1 = one task per year, 2 = chronological pairs (default 2)
	IncludeCitations   bool            `json:"include_citations"`    // keep Scholar's citation-only records (entries with no document, known only from being cited). Default false: they inflate every count, can push a window that holds ~900 real articles over the 1000 cap and trigger a pointless keyword split, and they land in the papers table as rows that are not articles
	MinBalance         float64         `json:"min_balance"`          // splitter: min balance score for a keyword to be used (default 0.15)
	MaxProbes          int             `json:"max_probes"`           // splitter: max count() probes per node (default 8)
	Headroom           float64         `json:"headroom"`             // target bucket capacity as a fraction of MaxResults (default 0.8)
	MinPapersToTrust   int             `json:"min_papers_to_trust"`  // if a year's known-paper corpus is smaller than this, fall back to the count-probe chain (default 10)
	MinCoverage        float64         `json:"min_coverage"`         // known/true fraction required before offline partitioning is trusted (default 0.5)
	MaxProbesPerConf   int             `json:"max_probes_per_conf"`  // hard cap on real count searches (browser round-trips) per conference plan (default 60)
	MineBigrams        bool            `json:"mine_bigrams"`         // legacy: mine two-word phrase candidates (default false) — subsumed by mine_max_ngram
	MineMaxNGram       int             `json:"mine_max_ngram"`       // mine contiguous phrase candidates up to this many tokens (default 3)
	MinePhraseBoost    float64         `json:"mine_phrase_boost"`    // multiplies a multi-word candidate's ranking score so phrases are preferred as splitters over comparably-scored single words (default 1.5, 1.0 = no effect)
	MaxMinedKeywords   int             `json:"max_mined_keywords"`   // cap on mined keyword candidates fed to the planner (default 200)
	MinCompletionRatio float64         `json:"min_completion_ratio"` // a task reaching the natural end of results with fewer than this fraction of TotalEstimate papers is flagged incomplete instead of completed (default 0.5)
}

// BrowserConfig describes which browser to drive and where its profile lives.
type BrowserConfig struct {
	Kind       string `json:"kind"`        // "chrome" | "edge" | "" (auto-detect)
	Executable string `json:"executable"`  // optional absolute path override
	ProfileDir string `json:"profile_dir"` // user data dir override; empty = auto-seeded copy of the real profile
	DebugPort  int    `json:"debug_port"`  // CDP port (default 9222)
	Headless   bool   `json:"headless"`

	// Stealth applies go-rod/stealth's automation-telltale patches to every new
	// page. Default OFF, deliberately: patching window.chrome/navigator makes
	// Scholar's own browser sniffer fail, and Scholar then serves a DEGRADED
	// page — unstable result counts, no pagination. A real un-patched browser on
	// a real profile is the better disguise here. Only turn this on if you have
	// measured that it helps.
	Stealth bool `json:"stealth"`

	// Locale drives the UI language Scholar serves (the hl= parameter), via
	// --lang, --accept-lang, the Accept-Language header and Google's PREF
	// cookie. Keep "en-US": under hl=fr Scholar treats "and" as a mandatory
	// query word instead of a stopword, which slashes the result count of every
	// venue whose name contains it ("Robotics and Automation", "Knowledge
	// Discovery and Data Mining", …). Default "en-US".
	Locale string `json:"locale"`
}

// ConferenceCfg is one line of the conference list.
type ConferenceCfg struct {
	Name         string `json:"name"`           // display label, e.g. "ICML"
	Query        string `json:"query"`          // search-box phrase, e.g. "\"International Conference on Machine Learning\""
	UseShortName bool   `json:"use_short_name"` // when true, the conference name is used as the query instead of query
}

// TimingConfig holds the [min,max] delay ranges in milliseconds.
type TimingConfig struct {
	BetweenPagesMS        []int   `json:"between_pages_ms"`         // pause after each results page
	BetweenSearchesMS     []int   `json:"between_searches_ms"`      // pause after each distinct search
	PlanBetweenSearchesMS []int   `json:"plan_between_searches_ms"` // pause during the --plan phase
	TypingCharMS          []int   `json:"typing_char_ms"`           // per-character typing delay
	AfterCaptchaMS        []int   `json:"after_captcha_ms"`         // "human returning from break"
	ClickMoveStepMS       []int   `json:"click_move_step_ms"`       // per mouse-move waypoint delay
	ScrollStepMS          []int   `json:"scroll_step_ms"`           // per wheel-event delay
	ReadScrollProb        float64 `json:"read_scroll_prob"`         // chance of a "reading" scroll per page
	CaptchaPollIntervalMS int     `json:"captcha_poll_interval_ms"` // polling interval while waiting for a CAPTCHA to clear (ms, default 10000)
	CaptchaTimeoutMS      int     `json:"captcha_timeout_ms"`       // max time to wait for CAPTCHA resolution; 0 = forever
	LoginTimeoutMS        int     `json:"login_timeout_ms"`         // max time to wait for login resolution; 0 = forever
}

// ThrottleConfig implements the adaptive throttle: after every CAPTCHA the
// delay multiplier grows by Step (capped at Max), and is reset on restart.
type ThrottleConfig struct {
	Enabled bool    `json:"enabled"`
	Step    float64 `json:"step"`
	Max     float64 `json:"max"`
}

// Load reads and validates a JSON config file, filling in defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse decodes raw JSON and applies defaults + validation.
func Parse(raw []byte) (*Config, error) {
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// EffectiveEndYear returns the upper bound of the planning range: EndYear when
// it is set, otherwise the current year ("2000 to present"). Resolving it here
// rather than in applyDefaults keeps the fallback working for a Config built as
// a struct literal, and lets a long-running process roll over on New Year's Day.
func (c *Config) EffectiveEndYear() int {
	if c.EndYear > 0 {
		return c.EndYear
	}
	return time.Now().Year()
}

// EffectivePlanWindowYears returns the planner's base year-window width,
// falling back to 2 when unset. Like EffectiveEndYear, resolving it here keeps
// a Config built as a struct literal (which never runs applyDefaults) valid.
func (c *Config) EffectivePlanWindowYears() int {
	if c.PlanWindowYears > 0 {
		return c.PlanWindowYears
	}
	return 2
}

func (c *Config) applyDefaults() {
	if c.Database == "" {
		c.Database = "scholar.db"
	}
	if c.StartYear == 0 {
		c.StartYear = 2000
	}
	if c.MaxResults == 0 {
		c.MaxResults = 1000
	}
	if c.PlanWindowYears == 0 {
		c.PlanWindowYears = 2
	}
	if c.MaxKeywords == 0 {
		c.MaxKeywords = 5
	}
	if c.MinBalance == 0 {
		c.MinBalance = 0.15
	}
	if c.MaxProbes == 0 {
		c.MaxProbes = 8
	}
	if c.Headroom == 0 {
		c.Headroom = 0.8
	}
	if c.MinPapersToTrust == 0 {
		c.MinPapersToTrust = 10
	}
	if c.MinCoverage == 0 {
		c.MinCoverage = 0.5
	}
	if c.MaxProbesPerConf == 0 {
		c.MaxProbesPerConf = 60
	}
	if c.MaxMinedKeywords == 0 {
		c.MaxMinedKeywords = 200
	}
	if c.MineMaxNGram == 0 {
		c.MineMaxNGram = 3
	}
	if c.MinePhraseBoost == 0 {
		c.MinePhraseBoost = 1.5
	}
	if c.MinCompletionRatio == 0 {
		c.MinCompletionRatio = 0.5
	}
	for i := range c.Conferences {
		if c.Conferences[i].UseShortName {
			c.Conferences[i].Query = fmt.Sprintf("\"%s\"", c.Conferences[i].Name)
		}
	}
	if c.Browser.DebugPort == 0 {
		c.Browser.DebugPort = 9222
	}
	if c.Browser.Locale == "" {
		c.Browser.Locale = "en-US"
	}
	if c.Timing.BetweenPagesMS == nil {
		c.Timing.BetweenPagesMS = []int{30000, 60000}
	}
	if c.Timing.BetweenSearchesMS == nil {
		c.Timing.BetweenSearchesMS = []int{60000, 120000}
	}
	if c.Timing.PlanBetweenSearchesMS == nil {
		c.Timing.PlanBetweenSearchesMS = []int{15000, 30000}
	}
	if c.Timing.TypingCharMS == nil {
		c.Timing.TypingCharMS = []int{80, 250}
	}
	if c.Timing.AfterCaptchaMS == nil {
		c.Timing.AfterCaptchaMS = []int{30000, 90000}
	}
	if c.Timing.ClickMoveStepMS == nil {
		c.Timing.ClickMoveStepMS = []int{12, 40}
	}
	if c.Timing.ScrollStepMS == nil {
		c.Timing.ScrollStepMS = []int{100, 350}
	}
	if c.Timing.ReadScrollProb == 0 {
		c.Timing.ReadScrollProb = 0.5
	}
	if c.Timing.CaptchaPollIntervalMS == 0 {
		c.Timing.CaptchaPollIntervalMS = 10000
	}
	if c.Timing.CaptchaTimeoutMS == 0 {
		c.Timing.CaptchaTimeoutMS = 0
	}
	if c.Timing.LoginTimeoutMS == 0 {
		c.Timing.LoginTimeoutMS = 0
	}
	if c.AdaptiveThrottle.Step == 0 {
		c.AdaptiveThrottle.Step = 0.2
	}
	if c.AdaptiveThrottle.Max == 0 {
		c.AdaptiveThrottle.Max = 2.5
	}
	if len(c.Keywords) == 0 {
		// Sensible high-frequency academic terms, used in order by the
		// sequential-subtraction splitter. Order matters: a paper is assigned
		// to the first keyword it contains.
		c.Keywords = []string{
			"learning", "neural", "network", "model", "data", "deep",
			"training", "image", "language", "system", "method", "optimization",
			"algorithm", "approach", "classification", "detection", "generation",
			"reinforcement", "representation", "attention", "graph", "clustering",
			"robot", "speech", "recognition", "prediction", "text", "video",
		}
	}
}

func (c *Config) validate() error {
	if len(c.Conferences) == 0 {
		return fmt.Errorf("config: at least one conference is required")
	}
	seen := map[string]bool{}
	for _, conf := range c.Conferences {
		if conf.Name == "" || conf.Query == "" {
			return fmt.Errorf("config: every conference needs a name and a query")
		}
		if seen[conf.Name] {
			return fmt.Errorf("config: duplicate conference name %q", conf.Name)
		}
		seen[conf.Name] = true
	}
	if c.Browser.Kind != "" && c.Browser.Kind != "chrome" && c.Browser.Kind != "edge" {
		return fmt.Errorf("config: browser.kind must be \"chrome\", \"edge\" or empty")
	}
	for _, rng := range [][]int{
		c.Timing.BetweenPagesMS, c.Timing.BetweenSearchesMS,
		c.Timing.PlanBetweenSearchesMS, c.Timing.TypingCharMS,
		c.Timing.AfterCaptchaMS, c.Timing.ClickMoveStepMS, c.Timing.ScrollStepMS,
	} {
		if len(rng) != 2 || rng[0] <= 0 || rng[1] < rng[0] {
			return fmt.Errorf("config: each timing range must be [min,max] with min>0")
		}
	}
	if c.Timing.CaptchaPollIntervalMS < 0 {
		return fmt.Errorf("config: captcha_poll_interval_ms must be >= 0")
	}
	if c.Timing.CaptchaTimeoutMS < 0 {
		return fmt.Errorf("config: captcha_timeout_ms must be >= 0")
	}
	if c.Timing.LoginTimeoutMS < 0 {
		return fmt.Errorf("config: login_timeout_ms must be >= 0")
	}
	if c.StartYear < 0 || c.StartYear > 2100 {
		return fmt.Errorf("config: start_year out of range")
	}
	if c.EndYear < 0 || c.EndYear > 2100 {
		return fmt.Errorf("config: end_year out of range")
	}
	if c.EndYear > 0 && c.EndYear < c.StartYear {
		return fmt.Errorf("config: end_year (%d) must be >= start_year (%d)", c.EndYear, c.StartYear)
	}
	if c.PlanWindowYears < 1 {
		return fmt.Errorf("config: plan_window_years must be >= 1")
	}
	if c.MaxKeywords < 0 {
		return fmt.Errorf("config: max_keywords must be >= 0")
	}
	if c.MinBalance < 0 || c.MinBalance > 1 {
		return fmt.Errorf("config: min_balance must be in [0,1]")
	}
	if c.MaxProbes < 1 {
		return fmt.Errorf("config: max_probes must be >= 1")
	}
	if c.Headroom <= 0 || c.Headroom > 1 {
		return fmt.Errorf("config: headroom must be in (0,1]")
	}
	if c.MinPapersToTrust < 0 {
		return fmt.Errorf("config: min_papers_to_trust must be >= 0")
	}
	if c.MinCoverage <= 0 || c.MinCoverage > 1 {
		return fmt.Errorf("config: min_coverage must be in (0,1]")
	}
	if c.MaxProbesPerConf < 1 {
		return fmt.Errorf("config: max_probes_per_conf must be >= 1")
	}
	if c.MaxMinedKeywords < 0 {
		return fmt.Errorf("config: max_mined_keywords must be >= 0")
	}
	if c.MinePhraseBoost <= 0 {
		return fmt.Errorf("config: mine_phrase_boost must be > 0")
	}
	if c.MinCompletionRatio <= 0 || c.MinCompletionRatio > 1 {
		return fmt.Errorf("config: min_completion_ratio must be in (0,1]")
	}
	return nil
}
