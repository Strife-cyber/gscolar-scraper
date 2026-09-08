// Package db owns the SQLite store: schema, migrations and repositories.
//
// The database is the single source of truth for progress. Every page commit
// is one WAL transaction: the raw page HTML, the parsed papers (INSERT OR
// IGNORE on their content hash) and the task's page pointer are written
// together, so a crash mid-commit leaves a consistent state.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, no CGo
)

// Task statuses (the canonical values stored in tasks.status).
const (
	StatusPending    = "pending"
	StatusRunning    = "running"
	StatusCompleted  = "completed"
	StatusNeedsSplit = "needs_split"
	StatusError      = "error"
	// StatusIncomplete marks a task that reached Scholar's natural end of
	// results (no next page) but committed far fewer papers than its planned
	// TotalEstimate — Scholar under-delivering relative to its own "About N
	// results" count, not a crawl failure. Distinct from needs_split (which
	// means "never even started, still over the cap") and from error (which
	// means the crawl itself broke). -rerun-shortfall re-queues it to pending.
	StatusIncomplete = "incomplete"
)

// Open opens (creating if needed) the SQLite database at path and runs
// migrations. WAL mode and foreign keys are enabled for durability and
// crash-robustness.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(8000)&_pragma=synchronous(FULL)",
		path,
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1) // modernc: single writer is safest; the crawl is sequential anyway

	d := &DB{db: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// DB wraps the underlying *sql.DB with typed repositories.
type DB struct {
	db *sql.DB
}

// Close releases the underlying connection.
func (d *DB) Close() error { return d.db.Close() }

func (d *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS conferences (
    id    INTEGER PRIMARY KEY,
    name  TEXT UNIQUE NOT NULL,
    query TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id                     INTEGER PRIMARY KEY,
    conference_id          INTEGER NOT NULL REFERENCES conferences(id),
    query                  TEXT NOT NULL,
    year_from              INTEGER NOT NULL DEFAULT 0,
    year_to                INTEGER NOT NULL DEFAULT 0,
    keywords               TEXT NOT NULL DEFAULT '[]',   -- ordered subtractive chain (JSON)
    page                   INTEGER NOT NULL DEFAULT 1,   -- next page to scrape, 1-indexed
    status                 TEXT NOT NULL DEFAULT 'pending',
    total_results_estimate INTEGER NOT NULL DEFAULT 0,
    error                  TEXT NOT NULL DEFAULT '',
    UNIQUE(conference_id, query, year_from, year_to, keywords)
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_conf   ON tasks(conference_id);

CREATE TABLE IF NOT EXISTS papers (
    hash          TEXT PRIMARY KEY,             -- dedup key: sha256(title|year|first-author)
    title         TEXT NOT NULL,
    authors       TEXT NOT NULL DEFAULT '',
    year          INTEGER NOT NULL DEFAULT 0,
    snippet       TEXT NOT NULL DEFAULT '',
    citations     INTEGER,                      -- NULL when Scholar shows no count
    source_url    TEXT NOT NULL DEFAULT '',
    scholar_id    TEXT NOT NULL DEFAULT '',     -- Scholar result cluster id (data-cid)
    raw_html      TEXT NOT NULL DEFAULT '',     -- the parsed <div class="gs_r"> block
    task_id       INTEGER REFERENCES tasks(id),
    conference_id INTEGER REFERENCES conferences(id), -- how paper↔venue is known
    sourced_from  TEXT NOT NULL DEFAULT 'crawl',       -- 'crawl' or 'plan' (harvested)
    scraped_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_papers_task ON papers(task_id);

CREATE TABLE IF NOT EXISTS page_html (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     INTEGER NOT NULL REFERENCES tasks(id),
    page_number INTEGER NOT NULL,
    html        TEXT NOT NULL,                  -- entire raw results page
    scraped_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(task_id, page_number)
);

CREATE TABLE IF NOT EXISTS count_cache (
    query     TEXT NOT NULL,
    year_from INTEGER NOT NULL,
    year_to   INTEGER NOT NULL,
    count     INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (query, year_from, year_to)
);

CREATE TABLE IF NOT EXISTS keyword_rank (
    keyword     TEXT PRIMARY KEY,
    score       REAL NOT NULL,               -- rank position: higher = better splitter
    corpus_size INTEGER NOT NULL DEFAULT 0   -- paper count the ranking was mined from
);

CREATE TABLE IF NOT EXISTS plan_harvest_pages (
    query     TEXT NOT NULL,
    year_from INTEGER NOT NULL,
    year_to   INTEGER NOT NULL,
    html      TEXT NOT NULL,
    scraped_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (query, year_from, year_to)
);
`
	_, err := d.db.Exec(schema)
	if err != nil {
		return err
	}
	// Fresh DBs have the columns; pre-existing DBs need them added and the
	// papers backfilled with their paper→task→conference association.
	if err := d.ensurePapersColumns(); err != nil {
		return err
	}
	return d.backfillPaperConferences()
}

// ensurePapersColumns adds papers.conference_id and papers.sourced_from to a
// pre-existing database that predates them. Column existence is probed with a
// pragma; only missing ones are added.
func (d *DB) ensurePapersColumns() error {
	rows, err := d.db.Query(`PRAGMA table_info(papers)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	var cid int
	var name, ctype string
	var notnull, pk int
	var dflt any
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !cols["conference_id"] {
		if _, err := d.db.Exec(`ALTER TABLE papers ADD COLUMN conference_id INTEGER REFERENCES conferences(id)`); err != nil {
			return err
		}
	}
	if !cols["sourced_from"] {
		if _, err := d.db.Exec(`ALTER TABLE papers ADD COLUMN sourced_from TEXT NOT NULL DEFAULT 'crawl'`); err != nil {
			return err
		}
	}
	// idx_papers_conf depends on conference_id, so it is made here (after the
	// column is guaranteed) rather than in the one-shot schema batch.
	_, err = d.db.Exec(`CREATE INDEX IF NOT EXISTS idx_papers_conf ON papers(conference_id)`)
	return err
}

// backfillPaperConferences points every paper to its conference via its task's
// conference_id, filling the new column for records that predate it.
func (d *DB) backfillPaperConferences() error {
	_, err := d.db.Exec(`
		UPDATE papers
		SET conference_id = (SELECT conference_id FROM tasks WHERE tasks.id = papers.task_id)
		WHERE conference_id IS NULL AND task_id IS NOT NULL`)
	return err
}

// ---------------------------------------------------------------------------
// Conferences
// ---------------------------------------------------------------------------

// UpsertConference inserts a conference, returning its id (creating if the
// name is new, updating the query if it changed).
func (d *DB) UpsertConference(ctx context.Context, name, query string) (int64, error) {
	var id int64
	err := d.db.QueryRowContext(ctx, `
		INSERT INTO conferences (name, query) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET query = excluded.query
		RETURNING id`, name, query).Scan(&id)
	return id, err
}

// Conference is the db layer's view of a conference row.
type Conference struct {
	ID    int64
	Name  string
	Query string
}

// ListConferences returns all conferences ordered by id.
func (d *DB) ListConferences(ctx context.Context) ([]Conference, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, name, query FROM conferences ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conference
	for rows.Next() {
		var c Conference
		if err := rows.Scan(&c.ID, &c.Name, &c.Query); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

// Task is the db layer's view of a task row.
type Task struct {
	ID            int64
	ConferenceID  int64
	Query         string
	YearFrom      int
	YearTo        int
	Keywords      []string
	Page          int
	Status        string
	TotalEstimate int
	Error         string
}

// UpsertTask stores a task, updating it if the same (conference, query, year
// range, keywords) already exists. The planner decides the intended status
// (t.Status: 'pending' for a crawlable leaf, 'needs_split' for an over-cap
// TODO), and that status wins whenever the task is not already finished — only
// 'completed' and 'error' outcomes are preserved across re-planning. A stale
// 'needs_split' whose leaf now fits under the cap is thereby downgraded to
// 'pending' again, and a freshly over-cap leaf is never inserted crawlable.
func (d *DB) UpsertTask(ctx context.Context, t *Task) (int64, error) {
	if t.Status == "" {
		t.Status = StatusPending
	}
	const q = `
INSERT INTO tasks (conference_id, query, year_from, year_to, keywords, page, status, total_results_estimate, error)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, '')
ON CONFLICT(conference_id, query, year_from, year_to, keywords) DO UPDATE SET
    total_results_estimate = excluded.total_results_estimate,
    -- A task abandoned as needs_split is re-adopted by the planner only when its
    -- leaf now fits under the cap; its page pointer may have been run up to the
    -- cap by a stalled crawl, so reset it to 1 (papers re-collect via hash dedup).
    page = CASE
        WHEN tasks.status = 'needs_split' AND excluded.status = 'pending' THEN 1
        ELSE tasks.page
    END,
    status = CASE
        WHEN tasks.status IN ('completed','error') THEN tasks.status
        ELSE excluded.status
    END
RETURNING id`
	var id int64
	err := d.db.QueryRowContext(ctx, q,
		t.ConferenceID, t.Query, t.YearFrom, t.YearTo,
		jsonKeywords(t.Keywords), t.Status, t.TotalEstimate,
	).Scan(&id)
	return id, err
}

func jsonKeywords(kw []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, k := range kw {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(k, `"`, `\"`))
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String()
}

func parseKeywords(kw string) []string {
	kw = strings.TrimSpace(kw)
	if len(kw) < 2 || kw[0] != '[' || kw[len(kw)-1] != ']' {
		return nil
	}
	inner := strings.TrimSpace(kw[1 : len(kw)-1])
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Key returns the canonical identity string for a task — the same key
// ReplaceConferencePlan uses to compare tasks across plan runs.
func (t *Task) Key() string {
	return taskKey(t.Query, t.YearFrom, t.YearTo, jsonKeywords(t.Keywords))
}

// PendingTasks returns tasks in 'pending' status for a conference, oldest first.
func (d *DB) PendingTasks(ctx context.Context, conferenceID int64) ([]Task, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, conference_id, query, year_from, year_to, keywords, page, status, total_results_estimate, error
		FROM tasks WHERE conference_id = ? AND status = 'pending' ORDER BY id`,
		conferenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// AllTasks returns every task for a conference regardless of status.
func (d *DB) AllTasks(ctx context.Context, conferenceID int64) ([]Task, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, conference_id, query, year_from, year_to, keywords, page, status, total_results_estimate, error
		FROM tasks WHERE conference_id = ? ORDER BY id`,
		conferenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		var t Task
		var kw string
		if err := rows.Scan(&t.ID, &t.ConferenceID, &t.Query, &t.YearFrom,
			&t.YearTo, &kw, &t.Page, &t.Status, &t.TotalEstimate, &t.Error); err != nil {
			return nil, err
		}
		t.Keywords = parseKeywords(kw)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResetStaleRunning flips a conference's 'running' tasks back to 'pending'.
// A 'running' status only exists while the process is alive; seeing one at the
// start of a crawl means a previous run crashed mid-task, so it must be
// re-queued (its page pointer resumes where it stopped).
func (d *DB) ResetStaleRunning(ctx context.Context, conferenceID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'pending' WHERE conference_id = ? AND status = 'running'`,
		conferenceID)
	return err
}

// SetTaskStatus transitions a task and records an optional error message.
func (d *DB) SetTaskStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, error = ? WHERE id = ?`,
		status, errMsg, id)
	return err
}

// MarkTaskRunning flips a pending task to running.
func (d *DB) MarkTaskRunning(ctx context.Context, id int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'running' WHERE id = ?`, id)
	return err
}

// AdvanceTaskPage checkpoint: stores the next page to scrape.
func (d *DB) AdvanceTaskPage(ctx context.Context, id int64, nextPage int) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET page = ? WHERE id = ?`, nextPage, id)
	return err
}

// ReconcileTaskPage returns the next page number a task should resume from
// based on the pages already committed to page_html. If no pages have been
// saved it returns 1, so resuming always starts at the actually committed
// next page rather than a stale task pointer.
func (d *DB) ReconcileTaskPage(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(page_number), 0) + 1 FROM page_html WHERE task_id = ?`,
		taskID).Scan(&n)
	return n, err
}

// UpdateTaskPage overwrites a task's page pointer.
func (d *DB) UpdateTaskPage(ctx context.Context, id int64, page int) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET page = ? WHERE id = ?`, page, id)
	return err
}

// ---------------------------------------------------------------------------
// Papers
// ---------------------------------------------------------------------------

// Paper is the db layer's view of a paper row.
type Paper struct {
	Hash         string
	Title        string
	Authors      string
	Year         int
	Snippet      string
	Citations    int
	HasCitations bool
	SourceURL    string
	ScholarID    string
	RawHTML      string
	TaskID       int64
	ConferenceID int64  // how paper↔venue is known (crawled or plan-harvested)
	SourcedFrom  string // "crawl" from a task's page, "plan" harvested during planning
}

// SavePaper inserts a paper unless its hash already exists. Returns true if
// the paper was newly inserted (i.e. not a duplicate).
func (d *DB) SavePaper(ctx context.Context, p *Paper) (inserted bool, err error) {
	res, err := d.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO papers
			(hash, title, authors, year, snippet, citations, source_url, scholar_id, raw_html, task_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Hash, p.Title, p.Authors, p.Year, p.Snippet, citationsOrNull(p),
		p.SourceURL, p.ScholarID, p.RawHTML, taskIDOrNull(p.TaskID))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SavePapers upserts a batch of papers in one transaction, healing associations
// on conflict: a paper first harvested during planning (task_id NULL,
// sourced_from='plan') acquires its task_id and conference once a crawl saves
// it, and conference_id is derived from the task when not supplied. Returns the
// number of rows affected (inserts plus conflict-updates).
func (d *DB) SavePapers(ctx context.Context, papers []Paper) (int, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := savePapersTx(ctx, tx, papers)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// savePapersTx upserts a batch of papers within the caller's transaction,
// returning the number of rows affected.
func savePapersTx(ctx context.Context, tx *sql.Tx, papers []Paper) (int, error) {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO papers
			(hash, title, authors, year, snippet, citations, source_url, scholar_id, raw_html,
			 task_id, conference_id, sourced_from)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        COALESCE(?, (SELECT conference_id FROM tasks WHERE id = ?)), ?)
		ON CONFLICT(hash) DO UPDATE SET
			task_id       = COALESCE(excluded.task_id, papers.task_id),
			conference_id = COALESCE(excluded.conference_id, papers.conference_id),
			sourced_from  = CASE WHEN excluded.task_id IS NOT NULL THEN 'crawl' ELSE papers.sourced_from END`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	affected := 0
	for i := range papers {
		p := &papers[i]
		res, err := stmt.ExecContext(ctx,
			p.Hash, p.Title, p.Authors, p.Year, p.Snippet, citationsOrNull(p),
			p.SourceURL, p.ScholarID, p.RawHTML, taskIDOrNull(p.TaskID),
			conferenceOrNull(p), taskIDOrNull(p.TaskID), sourcedFromOf(p))
		if err != nil {
			return affected, err
		}
		n, _ := res.RowsAffected()
		affected += int(n)
	}
	return affected, nil
}

func conferenceOrNull(p *Paper) any {
	if p.ConferenceID != 0 {
		return p.ConferenceID
	}
	return nil
}

func sourcedFromOf(p *Paper) string {
	if p.SourcedFrom != "" {
		return p.SourcedFrom
	}
	if p.TaskID != 0 {
		return "crawl"
	}
	return "plan"
}

// PapersForConference returns the known papers for a conference, optionally
// restricted to a year range (0 bounds mean unbounded). Both crawled and
// plan-harvested papers are covered via the conference association.
func (d *DB) PapersForConference(ctx context.Context, conferenceID int64, yearFrom, yearTo int) ([]Paper, error) {
	var rows *sql.Rows
	var err error
	if yearFrom > 0 && yearTo >= yearFrom {
		rows, err = d.db.QueryContext(ctx, `
			SELECT hash, title, snippet, year, authors, raw_html, task_id, conference_id, sourced_from
			FROM papers WHERE conference_id = ? AND year BETWEEN ? AND ? ORDER BY year, hash`,
			conferenceID, yearFrom, yearTo)
	} else {
		rows, err = d.db.QueryContext(ctx, `
			SELECT hash, title, snippet, year, authors, raw_html, task_id, conference_id, sourced_from
			FROM papers WHERE conference_id = ? ORDER BY year, hash`, conferenceID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Paper
	for rows.Next() {
		var p Paper
		var taskID, confID *int64
		if err := rows.Scan(&p.Hash, &p.Title, &p.Snippet, &p.Year, &p.Authors,
			&p.RawHTML, &taskID, &confID, &p.SourcedFrom); err != nil {
			return nil, err
		}
		if taskID != nil {
			p.TaskID = *taskID
		}
		if confID != nil {
			p.ConferenceID = *confID
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PapersInYears returns every stored paper inside a year range, across ALL
// conferences — the broad corpus a year's split is partitioned over. Splitting
// a conference's year is not limited to that conference's own papers: known
// papers from other venues in the same year are equally valid evidence for
// which keywords divide the result space.
func (d *DB) PapersInYears(ctx context.Context, yearFrom, yearTo int) ([]Paper, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT hash, title, snippet, year, authors, raw_html, task_id, conference_id, sourced_from
		FROM papers WHERE year BETWEEN ? AND ? ORDER BY year, hash`, yearFrom, yearTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Paper
	for rows.Next() {
		var p Paper
		var taskID, confID *int64
		if err := rows.Scan(&p.Hash, &p.Title, &p.Snippet, &p.Year, &p.Authors,
			&p.RawHTML, &taskID, &confID, &p.SourcedFrom); err != nil {
			return nil, err
		}
		if taskID != nil {
			p.TaskID = *taskID
		}
		if confID != nil {
			p.ConferenceID = *confID
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllPaperTexts returns "title snippet" for every stored paper — the
// background corpus the TF-IDF keyword miner scores a conference's candidate
// splitters against.
func (d *DB) AllPaperTexts(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT title, snippet FROM papers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var title, snippet string
		if err := rows.Scan(&title, &snippet); err != nil {
			return nil, err
		}
		out = append(out, title+" "+snippet)
	}
	return out, rows.Err()
}

// PaperCount reports how many unique papers are stored.
func (d *DB) PaperCount(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM papers`).Scan(&n)
	return n, err
}

// CountPapersForTask reports how many papers are currently attributed to one
// task, used to detect Scholar under-delivering relative to TotalEstimate.
func (d *DB) CountPapersForTask(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM papers WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Page commits
// ---------------------------------------------------------------------------

// PageCommit is the atomic unit of work for one scraped page. All fields are
// written in a single WAL transaction.
type PageCommit struct {
	TaskID     int64
	PageNumber int
	HTML       string // raw page HTML
	Papers     []Paper
	NextPage   int    // new tasks.page pointer
	Final      bool   // if true, mark the task completed too
	ErrMsg     string // if non-empty, set status 'error' with this message
}

// CommitPage atomically saves a page: raw HTML, dedup'd papers and the task
// pointer. It is the sole checkpoint primitive; the crawler calls it once per
// page and everything durably lands together.
func (d *DB) CommitPage(ctx context.Context, pc PageCommit) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO page_html (task_id, page_number, html) VALUES (?, ?, ?)`,
		pc.TaskID, pc.PageNumber, pc.HTML); err != nil {
		return err
	}

	if _, err := savePapersTx(ctx, tx, pc.Papers); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET page = ? WHERE id = ?`, pc.NextPage, pc.TaskID); err != nil {
		return err
	}

	if pc.Final {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'completed' WHERE id = ?`, pc.TaskID); err != nil {
			return err
		}
	}
	if pc.ErrMsg != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'error', error = ? WHERE id = ?`, pc.ErrMsg, pc.TaskID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func citationsOrNull(p *Paper) any {
	if p.HasCitations {
		return p.Citations
	}
	return nil
}

func taskIDOrNull(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ReplaceConferencePlan removes stale tasks of a conference that are no longer
// part of the freshly computed plan, detaching their papers first. A task is
// stale when it is either an obsolete 'needs_split' TODO (the "tackle it
// later" step: re-running --plan with better keywords replaces the oversized
// leaves with new, finer ones) or a 'pending' task left over from a previous,
// differently-shaped plan (a whole-range task superseded by 2-year windows, a
// chain leaf that the empty-bucket optimization no longer emits, …). The plan
// is authoritative: anything not in keepKeys and not yet completed/errored is
// removed so the crawl never re-runs an obsolete query.
func (d *DB) ReplaceConferencePlan(ctx context.Context, conferenceID int64, keepKeys []string) error {
	keep := map[string]bool{}
	for _, k := range keepKeys {
		keep[k] = true
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, query, year_from, year_to, keywords FROM tasks
		 WHERE conference_id = ? AND status IN ('needs_split', 'pending')`, conferenceID)
	if err != nil {
		return err
	}
	type stale struct {
		id  int64
		key string
	}
	var stales []stale
	for rows.Next() {
		var id int64
		var q, kw string
		var yf, yt int
		if err := rows.Scan(&id, &q, &yf, &yt, &kw); err != nil {
			rows.Close()
			return err
		}
		key := taskKey(q, yf, yt, kw)
		if !keep[key] {
			stales = append(stales, stale{id: id, key: key})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range stales {
		if _, err := tx.ExecContext(ctx,
			`UPDATE papers SET task_id = NULL WHERE task_id = ?`, s.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tasks WHERE id = ?`, s.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func taskKey(query string, yearFrom, yearTo int, keywords string) string {
	return fmt.Sprintf("%s\x1f%d\x1f%d\x1f%s", query, yearFrom, yearTo, keywords)
}

// SetTaskNeedsSplit flags a task for re-splitting, with a reason.
func (d *DB) SetTaskNeedsSplit(ctx context.Context, id int64, reason string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'needs_split', error = ? WHERE id = ?`, reason, id)
	return err
}

// SetTaskIncomplete flags a task as having reached the natural end of results
// (no next page) while still far short of its TotalEstimate, with a reason.
// Unlike SetTaskNeedsSplit (never even attempted — over the cap) this records
// that the crawl ran to completion but Scholar itself under-delivered.
func (d *DB) SetTaskIncomplete(ctx context.Context, id int64, reason string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'incomplete', error = ? WHERE id = ?`, reason, id)
	return err
}

// RerunShortfallTasks re-queues 'incomplete' and 'error' tasks of a conference
// back to 'pending' so the next -crawl retries them. The page pointer is left
// untouched: crawlTask's existing resume logic re-searches and clicks Next
// forward to that pointer, so a shortfall retry naturally picks up wherever
// the previous run stopped rather than re-scraping from page 1. Returns how
// many tasks were re-queued.
func (d *DB) RerunShortfallTasks(ctx context.Context, conferenceID int64) (int, error) {
	res, err := d.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'pending', error = ''
		 WHERE conference_id = ? AND status IN ('incomplete', 'error')`,
		conferenceID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---------------------------------------------------------------------------
// Count cache (split planning)
// ---------------------------------------------------------------------------

// GetCachedCount returns a previously recorded "About N results" count for a
// (query, year range) so re-planning does not re-query Scholar.
func (d *DB) GetCachedCount(ctx context.Context, query string, yearFrom, yearTo int) (int, bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count FROM count_cache WHERE query=? AND year_from=? AND year_to=?`,
		query, yearFrom, yearTo).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// LoadKeywordRank returns the persisted mined-keyword ranking when it was
// computed over a corpus of exactly corpusSize papers. ok=false on a miss or
// when the corpus has grown since the ranking was mined.
func (d *DB) LoadKeywordRank(ctx context.Context, corpusSize int) ([]string, bool, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT keyword FROM keyword_rank WHERE corpus_size = ? ORDER BY score DESC`, corpusSize)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			return nil, false, err
		}
		out = append(out, kw)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, len(out) > 0, nil
}

// SaveKeywordRank replaces the persisted mined-keyword ranking. Older
// generations are deleted so the table never accumulates stale rankings; the
// stored score is the rank position, not the Balance×IDF product, so only the
// ordering round-trips.
func (d *DB) SaveKeywordRank(ctx context.Context, corpusSize int, kws []string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_rank`); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO keyword_rank (keyword, score, corpus_size) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i, kw := range kws {
		if _, err := stmt.ExecContext(ctx, kw, float64(len(kws)-i), corpusSize); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetCachedCount stores an "About N results" count.
func (d *DB) SetCachedCount(ctx context.Context, query string, yearFrom, yearTo, count int) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO count_cache (query, year_from, year_to, count) VALUES (?, ?, ?, ?)
		ON CONFLICT(query, year_from, year_to) DO UPDATE SET count = excluded.count,
			updated_at = CURRENT_TIMESTAMP`,
		query, yearFrom, yearTo, count)
	return err
}

// SavePlanHarvestPage stores the raw HTML from a planning count probe so it
// can be re-mined later without re-querying Scholar.
func (d *DB) SavePlanHarvestPage(ctx context.Context, query string, yearFrom, yearTo int, html string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO plan_harvest_pages (query, year_from, year_to, html) VALUES (?, ?, ?, ?)
		ON CONFLICT(query, year_from, year_to) DO UPDATE SET html = excluded.html,
			scraped_at = CURRENT_TIMESTAMP`,
		query, yearFrom, yearTo, html)
	return err
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Stats aggregates progress counters for the status display.
type Stats struct {
	Conferences int
	Pending     int
	Running     int
	Completed   int
	NeedsSplit  int
	Incomplete  int
	Papers      int
	Pages       int
}

// GetStats computes aggregate counters.
func (d *DB) GetStats(ctx context.Context) (Stats, error) {
	var s Stats
	count := func(q string, dst *int) error {
		return d.db.QueryRowContext(ctx, q).Scan(dst)
	}
	if err := count(`SELECT COUNT(*) FROM conferences`, &s.Conferences); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM tasks WHERE status='pending'`, &s.Pending); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM tasks WHERE status='running'`, &s.Running); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM tasks WHERE status='completed'`, &s.Completed); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM tasks WHERE status='needs_split'`, &s.NeedsSplit); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM tasks WHERE status='incomplete'`, &s.Incomplete); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM papers`, &s.Papers); err != nil {
		return s, err
	}
	if err := count(`SELECT COUNT(*) FROM page_html`, &s.Pages); err != nil {
		return s, err
	}
	return s, nil
}
