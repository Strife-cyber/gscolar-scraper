// Package model defines the domain types shared across the scraper.
package model

// Conference is one entry from the config's conference list. Name is a
// display label; Query is the exact text typed into the Scholar search box
// (typically a quoted phrase, e.g. `"International Conference on Machine Learning"`).
type Conference struct {
	ID    int64
	Name  string
	Query string
}

// Task is a single crawlable unit: a Scholar query restricted to a year
// range, optionally further partitioned by the sequential keyword chain.
//
// Query is the effective search-box string (already includes the conference
// phrase, any positive keyword and all negative exclusions), so the crawler
// never has to reason about split structure.
//
// Keywords holds the ordered subtractive chain used to derive this task
// (JSON-serialized), purely for provenance, debugging and re-splitting.
type Task struct {
	ID             int64
	ConferenceID   int64
	Query          string
	YearFrom       int // 0 means "default lower bound" (see config.StartYear)
	YearTo         int // 0 means "present"
	Keywords       []string
	Page           int // next page to scrape, 1-indexed
	Status         string
	TotalEstimate  int // "About X results" read at planning time
	Error          string
}

// Task statuses.
const (
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusCompleted  = "completed"
	StatusNeedsSplit = "needs_split"
	StatusError      = "error"
)

// ResultItem is one parsed paper entry from a Scholar results page.
// Citations is only meaningful when HasCitations is true; Scholar does not
// display a citation count for every entry (notably [CITATION] items).
type ResultItem struct {
	Title        string
	URL          string
	Authors      string
	Venue        string
	Year         int
	Snippet      string
	Citations    int
	HasCitations bool
	ScholarID    string // the result cluster id (data-cid attribute)
	CitationOnly bool   // [CITATION] entries have no link out
	RawHTML      string // outer HTML of the .gs_r block (stored verbatim)
}

// Paper is a unique paper as stored in the papers table. Hash is the primary
// key computed by the hash package (normalized title + year + first author).
type Paper struct {
	Hash       string
	Title      string
	Authors    string
	Year       int
	Snippet    string
	Citations  int
	HasCitations bool
	SourceURL  string
	ScholarID  string
	RawHTML    string // the <div class="gs_r ..."> block this paper was parsed from
	TaskID     int64
}

// PageRecord is one raw results page (for later debugging / re-parsing).
type PageRecord struct {
	TaskID     int64
	PageNumber int
	HTML       string
}
