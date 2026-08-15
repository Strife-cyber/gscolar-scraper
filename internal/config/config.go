// Package config loads and validates the scraper's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the top-level configuration. All timing values are [min,max]
// millisecond ranges used to draw uniform-random delays (with jitter).
type Config struct {
	Browser          BrowserConfig    `json:"browser"`
	Conferences      []ConferenceCfg  `json:"conferences"`
	Timing           TimingConfig     `json:"timing"`
	AdaptiveThrottle ThrottleConfig   `json:"adaptive_throttle"`
	Keywords         []string         `json:"keywords"`
	Database         string           `json:"database"` // SQLite file path (default "scholar.db")
	StartYear        int              `json:"start_year"` // lower bound for "2000 to present" (default 2000)
	MaxResults       int              `json:"max_results"` // Scholar's cap (default 1000)
}

// BrowserConfig describes which browser to drive and where its profile lives.
type BrowserConfig struct {
	Kind       string `json:"kind"`       // "chrome" | "edge" | "" (auto-detect)
	Executable string `json:"executable"` // optional absolute path override
	ProfileDir string `json:"profile_dir"` // user data dir override; empty = auto-seeded copy of the real profile
	DebugPort  int    `json:"debug_port"`  // CDP port (default 9222)
	Headless   bool   `json:"headless"`
}

// ConferenceCfg is one line of the conference list.
type ConferenceCfg struct {
	Name  string `json:"name"`  // display label, e.g. "ICML"
	Query string `json:"query"` // search-box phrase, e.g. "\"International Conference on Machine Learning\""
}

// TimingConfig holds the [min,max] delay ranges in milliseconds.
type TimingConfig struct {
	BetweenPagesMS       []int `json:"between_pages_ms"`         // pause after each results page
	BetweenSearchesMS    []int `json:"between_searches_ms"`      // pause after each distinct search
	PlanBetweenSearchesMS []int `json:"plan_between_searches_ms"` // pause during the --plan phase
	TypingCharMS         []int `json:"typing_char_ms"`           // per-character typing delay
	AfterCaptchaMS       []int `json:"after_captcha_ms"`         // "human returning from break"
	ClickMoveStepMS      []int `json:"click_move_step_ms"`       // per mouse-move waypoint delay
	ScrollStepMS         []int `json:"scroll_step_ms"`           // per wheel-event delay
	ReadScrollProb       float64 `json:"read_scroll_prob"`       // chance of a "reading" scroll per page
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
	if c.Browser.DebugPort == 0 {
		c.Browser.DebugPort = 9222
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
	if c.StartYear < 0 || c.StartYear > 2100 {
		return fmt.Errorf("config: start_year out of range")
	}
	return nil
}
