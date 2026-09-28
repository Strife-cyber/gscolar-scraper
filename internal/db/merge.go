package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// MergeStats reports what a Merge brought into the local database.
type MergeStats struct {
	Conferences  int // conferences new to the local DB
	TasksAdded   int // tasks the local DB did not have
	TasksUpdated int // shared tasks whose status/page/estimate changed
	Pages        int // page_html rows added
	Papers       int // papers new to the local DB
	Counts       int // count_cache rows added or refreshed
	HarvestPages int // plan_harvest_pages rows added or refreshed
}

// statusRank orders task statuses by how much a status knows, for merging two
// copies of the same task. The furthest-along outcome wins: a task completed on
// either machine is completed, a live over-cap flag (needs_split) beats a plan
// that still thought the leaf crawlable, and a superseded flag from a newer
// plan beats a stale 'pending' (a later -plan re-adopts it if still wanted).
// 'running' only exists while a process is alive, so it counts as 'pending'.
var statusRank = map[string]int{
	StatusPending:    1,
	StatusRunning:    1,
	StatusSuperseded: 2,
	StatusError:      3,
	StatusNeedsSplit: 4,
	StatusIncomplete: 5,
	StatusCompleted:  6,
}

// Merge folds the database at srcPath (a copy scraped on another machine) into
// this one, in a single transaction: either everything lands or nothing does.
//
// Row ids are local to each machine, so nothing is matched by id. Conferences
// match by name, tasks by (conference name, query, year range, keywords) — the
// same key the planner upserts on — and papers by their content hash. Pages and
// papers are re-pointed at the local task/conference ids. Shared rows combine:
//   - tasks keep the furthest-along status (statusRank), the larger page
//     pointer and a non-zero estimate;
//   - papers keep the local row, filling in a missing task/conference link,
//     empty text fields and a larger citation count from the other copy;
//   - page_html keeps the local page when both machines committed it;
//   - count_cache and plan_harvest_pages keep the most recently written row.
//
// keyword_rank is not merged: it is a cache keyed on the corpus size, which a
// merge changes, so the next plan re-mines it over the combined papers.
//
// Merge is idempotent — merging the same file twice adds nothing the second
// time — and symmetric enough that two machines can swap and merge each other's
// files. srcPath is opened (and migrated to the current schema) first, so it
// must not be in use by a running scraper.
func (d *DB) Merge(ctx context.Context, localPath, srcPath string) (MergeStats, error) {
	var st MergeStats
	if same, err := samePath(localPath, srcPath); err != nil {
		return st, err
	} else if same {
		return st, fmt.Errorf("merge: %s is the local database", srcPath)
	}

	// Bring the source up to the current schema (older copies lack columns the
	// INSERT ... SELECT below reads) and fold its WAL into the main file.
	src, err := Open(srcPath)
	if err != nil {
		return st, fmt.Errorf("merge: open %s: %w", srcPath, err)
	}
	if _, err := src.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		src.Close()
		return st, fmt.Errorf("merge: checkpoint %s: %w", srcPath, err)
	}
	if err := src.Close(); err != nil {
		return st, err
	}

	// ATTACH is per-connection and cannot run inside a transaction, so pin one
	// connection for the attach, the transaction and the detach.
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return st, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, srcPath); err != nil {
		return st, fmt.Errorf("merge: attach %s: %w", srcPath, err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE src`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	if err := mergeConferences(ctx, tx, &st); err != nil {
		return st, fmt.Errorf("merge conferences: %w", err)
	}
	if err := mergeTasks(ctx, tx, &st); err != nil {
		return st, fmt.Errorf("merge tasks: %w", err)
	}
	if err := mergePages(ctx, tx, &st); err != nil {
		return st, fmt.Errorf("merge pages: %w", err)
	}
	if err := mergePapers(ctx, tx, &st); err != nil {
		return st, fmt.Errorf("merge papers: %w", err)
	}
	if err := mergeCaches(ctx, tx, &st); err != nil {
		return st, fmt.Errorf("merge caches: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE temp.conf_map; DROP TABLE temp.task_map`); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

func samePath(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb)), nil
}

func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

// mergeConferences adds the source's unknown conferences and builds
// temp.conf_map (source id -> local id), matched by name.
func mergeConferences(ctx context.Context, tx *sql.Tx, st *MergeStats) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO main.conferences (name, query)
		SELECT name, query FROM src.conferences WHERE true
		ON CONFLICT(name) DO NOTHING`)
	if err != nil {
		return err
	}
	st.Conferences = affected(res)
	_, err = tx.ExecContext(ctx, `
		CREATE TEMP TABLE conf_map AS
		SELECT s.id AS src_id, m.id AS dst_id
		FROM src.conferences s JOIN main.conferences m ON m.name = s.name`)
	return err
}

// mergeTasks inserts the source's unknown tasks, combines shared ones, and
// builds temp.task_map (source id -> local id).
func mergeTasks(ctx context.Context, tx *sql.Tx, st *MergeStats) error {
	type srcTask struct {
		id, confID              int64
		query, keywords, status string
		errMsg                  string
		yearFrom, yearTo, page  int
		estimate                int
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, cm.dst_id, t.query, t.year_from, t.year_to, t.keywords,
		       t.page, t.status, t.total_results_estimate, t.error
		FROM src.tasks t JOIN temp.conf_map cm ON cm.src_id = t.conference_id`)
	if err != nil {
		return err
	}
	var tasks []srcTask
	for rows.Next() {
		var t srcTask
		if err := rows.Scan(&t.id, &t.confID, &t.query, &t.yearFrom, &t.yearTo, &t.keywords,
			&t.page, &t.status, &t.estimate, &t.errMsg); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE task_map (src_id INTEGER PRIMARY KEY, dst_id INTEGER NOT NULL)`); err != nil {
		return err
	}
	for _, t := range tasks {
		if t.status == StatusRunning {
			t.status = StatusPending
		}
		var (
			id             int64
			page, estimate int
			status, errMsg string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT id, page, status, total_results_estimate, error FROM main.tasks
			WHERE conference_id = ? AND query = ? AND year_from = ? AND year_to = ? AND keywords = ?`,
			t.confID, t.query, t.yearFrom, t.yearTo, t.keywords).Scan(&id, &page, &status, &estimate, &errMsg)
		switch {
		case err == sql.ErrNoRows:
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO main.tasks (conference_id, query, year_from, year_to, keywords,
				                        page, status, total_results_estimate, error)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				t.confID, t.query, t.yearFrom, t.yearTo, t.keywords,
				t.page, t.status, t.estimate, t.errMsg).Scan(&id); err != nil {
				return err
			}
			st.TasksAdded++
		case err != nil:
			return err
		default:
			newStatus, newErr := status, errMsg
			if statusRank[t.status] > statusRank[status] {
				newStatus, newErr = t.status, t.errMsg
			}
			newPage := max(page, t.page)
			newEstimate := estimate
			if newEstimate <= 0 {
				newEstimate = t.estimate
			}
			if newStatus != status || newPage != page || newEstimate != estimate {
				if _, err := tx.ExecContext(ctx, `
					UPDATE main.tasks SET status = ?, error = ?, page = ?, total_results_estimate = ?
					WHERE id = ?`, newStatus, newErr, newPage, newEstimate, id); err != nil {
					return err
				}
				st.TasksUpdated++
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO temp.task_map (src_id, dst_id) VALUES (?, ?)`, t.id, id); err != nil {
			return err
		}
	}
	return nil
}

// mergePages copies the source's committed pages under the local task ids.
func mergePages(ctx context.Context, tx *sql.Tx, st *MergeStats) error {
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO main.page_html (task_id, page_number, html, scraped_at)
		SELECT tm.dst_id, h.page_number, h.html, h.scraped_at
		FROM src.page_html h JOIN temp.task_map tm ON tm.src_id = h.task_id`)
	if err != nil {
		return err
	}
	st.Pages = affected(res)
	return nil
}

// mergePapers inserts the source's unknown papers and heals shared ones.
func mergePapers(ctx context.Context, tx *sql.Tx, st *MergeStats) error {
	var before, after int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.papers`).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO main.papers
			(hash, title, authors, year, snippet, citations, source_url, scholar_id, raw_html,
			 task_id, conference_id, sourced_from, scraped_at)
		SELECT p.hash, p.title, p.authors, p.year, p.snippet, COALESCE(p.citations, 0),
		       p.source_url, p.scholar_id, p.raw_html,
		       tm.dst_id, cm.dst_id,
		       CASE WHEN tm.dst_id IS NULL AND p.task_id IS NOT NULL THEN 'plan' ELSE p.sourced_from END,
		       p.scraped_at
		FROM src.papers p
		LEFT JOIN temp.task_map tm ON tm.src_id = p.task_id
		LEFT JOIN temp.conf_map cm ON cm.src_id = p.conference_id
		WHERE true
		ON CONFLICT(hash) DO UPDATE SET
			task_id       = COALESCE(papers.task_id, excluded.task_id),
			conference_id = COALESCE(papers.conference_id, excluded.conference_id),
			sourced_from  = CASE WHEN papers.task_id IS NULL AND excluded.task_id IS NOT NULL
			                     THEN 'crawl' ELSE papers.sourced_from END,
			citations     = MAX(COALESCE(papers.citations, 0), excluded.citations),
			authors       = CASE WHEN papers.authors = ''    THEN excluded.authors    ELSE papers.authors END,
			snippet       = CASE WHEN papers.snippet = ''    THEN excluded.snippet    ELSE papers.snippet END,
			source_url    = CASE WHEN papers.source_url = '' THEN excluded.source_url ELSE papers.source_url END,
			scholar_id    = CASE WHEN papers.scholar_id = '' THEN excluded.scholar_id ELSE papers.scholar_id END,
			raw_html      = CASE WHEN papers.raw_html = ''   THEN excluded.raw_html   ELSE papers.raw_html END`); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.papers`).Scan(&after); err != nil {
		return err
	}
	st.Papers = after - before
	return nil
}

// mergeCaches folds in the planning caches, newest row winning.
func mergeCaches(ctx context.Context, tx *sql.Tx, st *MergeStats) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO main.count_cache (query, year_from, year_to, count, updated_at)
		SELECT query, year_from, year_to, count, updated_at FROM src.count_cache WHERE true
		ON CONFLICT(query, year_from, year_to) DO UPDATE SET
			count = excluded.count, updated_at = excluded.updated_at
		WHERE excluded.updated_at > count_cache.updated_at`)
	if err != nil {
		return err
	}
	st.Counts = affected(res)
	res, err = tx.ExecContext(ctx, `
		INSERT INTO main.plan_harvest_pages (query, year_from, year_to, html, scraped_at)
		SELECT query, year_from, year_to, html, scraped_at FROM src.plan_harvest_pages WHERE true
		ON CONFLICT(query, year_from, year_to) DO UPDATE SET
			html = excluded.html, scraped_at = excluded.scraped_at
		WHERE excluded.scraped_at > plan_harvest_pages.scraped_at`)
	if err != nil {
		return err
	}
	st.HarvestPages = affected(res)
	return nil
}
