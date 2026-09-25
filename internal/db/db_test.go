package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestConferencesUpsert(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	id1, err := d.UpsertConference(ctx, "ICML", `"International Conference on Machine Learning"`)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := d.UpsertConference(ctx, "ICML", `"International Conference on Machine Learning"`)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("same-name upsert should return the same id, got %d then %d", id1, id2)
	}

	confs, err := d.ListConferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(confs) != 1 {
		t.Fatalf("want 1 conference, got %d", len(confs))
	}
	if confs[0].Name != "ICML" {
		t.Errorf("name = %q", confs[0].Name)
	}
}

func TestTaskUpsertAndDedup(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}

	mk := func(est int) *Task {
		return &Task{
			ConferenceID:  confID,
			Query:         `"ICML" AND "learning"`,
			YearFrom:      2020,
			YearTo:        2020,
			Keywords:      []string{"learning"},
			TotalEstimate: est,
		}
	}

	id1, err := d.UpsertTask(ctx, mk(500))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := d.UpsertTask(ctx, mk(600)) // same logical task, updated estimate
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("same task should share an id, got %d then %d", id1, id2)
	}

	// The page pointer must survive a re-upsert (crash-safe replan).
	if err := d.AdvanceTaskPage(ctx, id1, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertTask(ctx, mk(700)); err != nil {
		t.Fatal(err)
	}

	// Simulate an in-progress task that gets re-enqueued -> becomes pending.
	if err := d.MarkTaskRunning(ctx, id1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertTask(ctx, mk(700)); err != nil {
		t.Fatal(err)
	}

	pending, err := d.PendingTasks(ctx, confID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending task, got %d", len(pending))
	}
	if pending[0].Page != 5 {
		t.Errorf("page pointer lost after replan: %d", pending[0].Page)
	}
	if pending[0].TotalEstimate != 700 {
		t.Errorf("estimate not updated: %d", pending[0].TotalEstimate)
	}
}

// TestUpsertTaskFollowsPlanStatus pins the planner-status contract: the caller
// decides 'pending' vs 'needs_split'; a stale 'needs_split' whose leaf now fits
// is downgraded to 'pending' and its runaway page pointer reset to 1; completed
// and error outcomes are never reset by re-planning.
func TestUpsertTaskFollowsPlanStatus(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}

	mk := func(est int, status string) *Task {
		return &Task{
			ConferenceID:  confID,
			Query:         `"ICML" AND "neural"`,
			YearFrom:      2005,
			YearTo:        2005,
			Keywords:      []string{"neural"},
			TotalEstimate: est,
			Status:        status,
		}
	}

	// 1. An over-cap leaf is created as a needs_split TODO, not crawlable.
	id, err := d.UpsertTask(ctx, mk(545, StatusNeedsSplit))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.taskStatus(ctx, id); err != nil {
		t.Fatal(err)
	} else if got != StatusNeedsSplit {
		t.Fatalf("over-cap leaf status = %q, want needs_split", got)
	}
	if p, _ := d.PendingTasks(ctx, confID); len(p) != 0 {
		t.Fatalf("over-cap leaf must not be pending, got %d", len(p))
	}

	// 2. The same leaf, now under the cap: downgraded to pending and its page
	// pointer reset, so the crawl restarts from page 1 instead of resuming at a
	// pointer a stalled run ran up to the cap.
	if err := d.AdvanceTaskPage(ctx, id, 31); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertTask(ctx, mk(251, StatusPending)); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.taskStatus(ctx, id); got != StatusPending {
		t.Fatalf("under-cap leaf status = %q, want pending", got)
	}
	pending, err := d.PendingTasks(ctx, confID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending task, got %d", len(pending))
	}
	if pending[0].Page != 1 {
		t.Errorf("page pointer not reset on needs_split->pending: %d", pending[0].Page)
	}

	// 3. A completed task is preserved no matter what the planner now says.
	if err := d.SetTaskStatus(ctx, id, StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertTask(ctx, mk(999, StatusNeedsSplit)); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.taskStatus(ctx, id); got != StatusCompleted {
		t.Fatalf("completed task clobbered by replan: %q", got)
	}
}

func (d *DB) taskStatus(ctx context.Context, id int64) (string, error) {
	var s string
	err := d.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&s)
	return s, err
}

func TestPaperDedup(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	p := Paper{
		Hash:         "abc",
		Title:        "A Paper",
		Authors:      "Jane Doe",
		Year:         2020,
		Snippet:      "snippet",
		Citations:    3,
		HasCitations: true,
	}
	ins, err := d.SavePaper(ctx, &p)
	if err != nil {
		t.Fatal(err)
	}
	if !ins {
		t.Fatal("first insert should report inserted")
	}
	ins, err = d.SavePaper(ctx, &p)
	if err != nil {
		t.Fatal(err)
	}
	if ins {
		t.Error("duplicate insert should be ignored")
	}
	n, err := d.PaperCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("paper count = %d, want 1", n)
	}
}

func TestCommitPageAtomic(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := d.UpsertTask(ctx, &Task{
		ConferenceID: confID,
		Query:        `"ICML"`,
		YearFrom:     2020, YearTo: 2020,
		TotalEstimate: 50,
	})
	if err != nil {
		t.Fatal(err)
	}

	pc := PageCommit{
		TaskID:     taskID,
		PageNumber: 1,
		HTML:       "<html>page one</html>",
		Papers: []Paper{
			{Hash: "h1", Title: "T1", Authors: "A", Year: 2020},
			{Hash: "h2", Title: "T2", Authors: "B", Year: 2020},
		},
		NextPage: 2,
	}
	if _, err := d.CommitPage(ctx, pc); err != nil {
		t.Fatal(err)
	}

	// Re-committing the same page must be idempotent (papers stay dedup'd,
	// page_html not duplicated).
	if _, err := d.CommitPage(ctx, pc); err != nil {
		t.Fatal(err)
	}

	tasks, err := d.AllTasks(ctx, confID)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Page != 2 {
		t.Errorf("task page = %d, want 2", tasks[0].Page)
	}
	n, _ := d.PaperCount(ctx)
	if n != 2 {
		t.Errorf("paper count = %d, want 2", n)
	}
}

func TestStats(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.UpsertConference(ctx, "A", `"A"`); err != nil {
		t.Fatal(err)
	}
	s, err := d.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Conferences != 1 {
		t.Errorf("conferences = %d, want 1", s.Conferences)
	}
}

func TestCountCache(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.SetCachedCount(ctx, `"ICML"`, 2000, 2020, 5000); err != nil {
		t.Fatal(err)
	}
	n, ok, err := d.GetCachedCount(ctx, `"ICML"`, 2000, 2020)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || n != 5000 {
		t.Errorf("cached count = (%d, %v), want (5000, true)", n, ok)
	}
	// A miss returns ok=false.
	if _, ok, _ := d.GetCachedCount(ctx, `"ICML"`, 2001, 2020); ok {
		t.Error("expected cache miss")
	}
}

// TestSavePapersPlanThenCrawlHeal: a paper harvested during planning (task_id
// NULL, conference known, sourced_from='plan') gains its task_id and conference
// association when a crawl later saves the same hash.
func TestSavePapersPlanThenCrawlHeal(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: `"ICML"`, YearFrom: 2020, YearTo: 2020})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Plan harvest: conference known, no task yet.
	planPaper := Paper{
		Hash: "abc", Title: "A Paper", Authors: "Jane Doe", Year: 2020,
		ConferenceID: confID, SourcedFrom: "plan",
	}
	ins, err := d.SavePapers(ctx, []Paper{planPaper})
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 {
		t.Errorf("inserted %d, want 1", ins)
	}

	// 2. Crawl saves the same hash with a task id.
	crawlPaper := Paper{
		Hash: "abc", Title: "A Paper", Authors: "Jane Doe", Year: 2020,
		TaskID: taskID, SourcedFrom: "crawl",
	}
	ins, err = d.SavePapers(ctx, []Paper{crawlPaper})
	if err != nil {
		t.Fatal(err)
	}
	// An UPSERT conflict-UPDATE reports one affected row; the meaningful checks
	// are the healed column values below, not this counter.
	if ins != 1 {
		t.Errorf("conflict-update affected %d rows, want 1", ins)
	}

	// 3. The row now points at the task and the conference is still known.
	var gotTask, gotConf *int64
	var gotSrc string
	err = d.db.QueryRow(`SELECT task_id, conference_id, sourced_from FROM papers WHERE hash='abc'`).
		Scan(&gotTask, &gotConf, &gotSrc)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask == nil || *gotTask != taskID {
		t.Errorf("task_id = %v, want %d", gotTask, taskID)
	}
	if gotConf == nil || *gotConf != confID {
		t.Errorf("conference_id = %v, want %d", gotConf, confID)
	}
	if gotSrc != "crawl" {
		t.Errorf("sourced_from = %q, want 'crawl'", gotSrc)
	}
}

// TestSavePapersDerivesConferenceFromTask: a crawled paper carries no explicit
// conference; the upsert derives it from the task.
func TestSavePapersDerivesConferenceFromTask(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: `"ICML"`, YearFrom: 2020, YearTo: 2020})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SavePapers(ctx, []Paper{{Hash: "x", Title: "X", Year: 2020, TaskID: taskID, SourcedFrom: "crawl"}}); err != nil {
		t.Fatal(err)
	}
	var gotConf *int64
	err = d.db.QueryRow(`SELECT conference_id FROM papers WHERE hash='x'`).Scan(&gotConf)
	if err != nil {
		t.Fatal(err)
	}
	if gotConf == nil || *gotConf != confID {
		t.Errorf("conference_id = %v, want %d", gotConf, confID)
	}
}

// TestPapersForConference: returns only the papers associated to a conference,
// honoring the year range.
func TestPapersForConference(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "ICML", `"ICML"`)
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := d.UpsertConference(ctx, "NeurIPS", `"NeurIPS"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SavePapers(ctx, []Paper{
		{Hash: "a", Title: "A", Year: 2020, ConferenceID: confID, SourcedFrom: "plan"},
		{Hash: "b", Title: "B", Year: 2021, ConferenceID: confID, SourcedFrom: "plan"},
		{Hash: "c", Title: "C", Year: 2022, ConferenceID: confID, SourcedFrom: "plan"},
		{Hash: "d", Title: "D", Year: 2020, ConferenceID: otherID, SourcedFrom: "plan"},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := d.PapersForConference(ctx, confID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("got %d papers for conference, want 3", len(all))
	}
	window, err := d.PapersForConference(ctx, confID, 2020, 2021)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 2 {
		t.Errorf("got %d papers in 2020-2021 window, want 2", len(window))
	}
	for _, p := range window {
		if p.Year < 2020 || p.Year > 2021 {
			t.Errorf("paper %q year %d outside window", p.Hash, p.Year)
		}
	}
	empty, err := d.PapersForConference(ctx, confID, 2030, 2031)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d papers in empty window, want 0", len(empty))
	}
}

// TestMigrationOnPreExistingDB: a database with the OLD papers schema (no
// conference_id / sourced_from columns) gains them on Open, and existing papers
// are backfilled to their conference via the tasks table.
func TestMigrationOnPreExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// 1. Create a legacy DB with the OLD papers schema so Open() has not
	//    migrated it yet; it carries one paper 'z' at task 1 / conference 5.
	if err := OpenLegacySQLite(path); err != nil {
		t.Fatal(err)
	}

	// 2. Reopen through the app's Open(), which must run the migration.
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	// The column must now exist and the legacy paper 'z' (task 1 -> conference 5)
	// must have been backfilled to conference 5.
	var conf *int64
	if err := d.db.QueryRow(`SELECT conference_id FROM papers WHERE hash='z'`).Scan(&conf); err != nil {
		t.Fatal(err)
	}
	if conf == nil {
		t.Error("migrated paper has NULL conference_id")
	} else if *conf != 5 {
		t.Errorf("backfilled conference_id = %d, want 5", *conf)
	}

	// The backfilled conference is visible through the repository query.
	ps, err := d.PapersForConference(context.Background(), 5, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Hash != "z" {
		t.Errorf("PapersForConference(5) = %+v, want the backfilled paper 'z'", ps)
	}
}

// OpenLegacySQLite builds a legacy-schema (pre-column) database at path so the
// app's migration can be tested against a real old file. It closes the scratch
// connection before returning so the app's Open() may take over the file.
func OpenLegacySQLite(path string) error {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
CREATE TABLE conferences (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, query TEXT NOT NULL);
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY, conference_id INTEGER NOT NULL REFERENCES conferences(id),
	query TEXT NOT NULL, year_from INTEGER NOT NULL DEFAULT 0, year_to INTEGER NOT NULL DEFAULT 0,
	keywords TEXT NOT NULL DEFAULT '[]', page INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'pending',
	total_results_estimate INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
	UNIQUE(conference_id, query, year_from, year_to, keywords));
CREATE TABLE papers (
	hash TEXT PRIMARY KEY, title TEXT NOT NULL, authors TEXT NOT NULL DEFAULT '',
	year INTEGER NOT NULL DEFAULT 0, snippet TEXT NOT NULL DEFAULT '',
	citations INTEGER, source_url TEXT NOT NULL DEFAULT '', scholar_id TEXT NOT NULL DEFAULT '',
	raw_html TEXT NOT NULL DEFAULT '', task_id INTEGER REFERENCES tasks(id),
	scraped_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP);
INSERT INTO conferences (id, name, query) VALUES (5, 'ICML', '"ICML"');
INSERT INTO tasks (id, conference_id, query, year_from, year_to) VALUES (1, 5, '"ICML"', 2020, 2020);
INSERT INTO papers (hash, title, year, task_id) VALUES ('z', 'Z', 2020, 1);`)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// TestCitationsStoredAsZeroNotNull: an article Scholar shows no "Cited by"
// link for must land in the table as 0, not NULL, so callers can sum, average
// and order on the column without special-casing absence.
func TestCitationsStoredAsZeroNotNull(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	papers := []Paper{
		{Hash: "uncited", Title: "Nobody cited this", Year: 2024},                            // HasCitations false
		{Hash: "cited", Title: "This one was cited", Year: 2024, Citations: 42, HasCitations: true},
	}
	if _, err := d.SavePapers(ctx, papers); err != nil {
		t.Fatalf("SavePapers: %v", err)
	}

	var nulls int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM papers WHERE citations IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 0 {
		t.Errorf("%d rows have a NULL citation count, want 0", nulls)
	}

	for _, tc := range []struct {
		hash string
		want int
	}{{"uncited", 0}, {"cited", 42}} {
		var got int
		if err := d.db.QueryRow(`SELECT citations FROM papers WHERE hash = ?`, tc.hash).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.hash, err)
		}
		if got != tc.want {
			t.Errorf("%s: citations = %d, want %d", tc.hash, got, tc.want)
		}
	}
}

// TestBackfillCitationZero: a database written by an older version still holds
// NULLs in papers.citations. SQLite cannot add NOT NULL to a column in place,
// so such a database keeps the nullable column forever — opening it must
// normalize the values instead. The legacy shape is rebuilt here because the
// current schema rejects the NULL outright (which is itself the proof that new
// databases can no longer grow one).
func TestBackfillCitationZero(t *testing.T) {
	d := openTest(t)

	if _, err := d.db.Exec(`DROP TABLE papers`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := d.db.Exec(`CREATE TABLE papers (
		hash      TEXT PRIMARY KEY,
		title     TEXT NOT NULL DEFAULT '',
		citations INTEGER
	)`); err != nil {
		t.Fatalf("recreate legacy papers: %v", err)
	}
	if _, err := d.db.Exec(
		`INSERT INTO papers (hash, title, citations) VALUES ('legacy', 'Old row', NULL), ('kept', 'Cited', 7)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := d.backfillCitationZero(); err != nil {
		t.Fatalf("backfillCitationZero: %v", err)
	}

	for _, tc := range []struct {
		hash string
		want int
	}{{"legacy", 0}, {"kept", 7}} {
		var got int
		if err := d.db.QueryRow(`SELECT citations FROM papers WHERE hash = ?`, tc.hash).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.hash, err)
		}
		if got != tc.want {
			t.Errorf("%s: citations = %d, want %d", tc.hash, got, tc.want)
		}
	}
}

// TestCommitPageReportsNewPapersForThisTask: the count CommitPage returns is
// what tells the crawler a result set is exhausted, so it must be per-task.
// A paper already in the table from the planning harvest is still new to the
// task that just crawled it; counting it as a duplicate would end a crawl early.
func TestCommitPageReportsNewPapersForThisTask(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "CONF", `"Conf"`)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: `"Conf"`, YearFrom: 2020, YearTo: 2020, Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	// Already known from planning: same hash, no task attached.
	if _, err := d.SavePapers(ctx, []Paper{{Hash: "h1", Title: "Harvested", Year: 2020}}); err != nil {
		t.Fatal(err)
	}

	page := func(n int, hashes ...string) int {
		t.Helper()
		var ps []Paper
		for _, h := range hashes {
			ps = append(ps, Paper{Hash: h, Title: h, Year: 2020, TaskID: taskID})
		}
		added, err := d.CommitPage(ctx, PageCommit{TaskID: taskID, PageNumber: n, HTML: "<html>", Papers: ps, NextPage: n + 1})
		if err != nil {
			t.Fatalf("CommitPage %d: %v", n, err)
		}
		return added
	}

	if got := page(1, "h1", "h2"); got != 2 {
		t.Errorf("page 1 added %d, want 2 (the harvested paper counts — it is new to this task)", got)
	}
	if got := page(2, "h3"); got != 1 {
		t.Errorf("page 2 added %d, want 1", got)
	}
	// Scholar recycling papers it already served on this task.
	if got := page(3, "h1", "h2", "h3"); got != 0 {
		t.Errorf("page 3 added %d, want 0 (all already collected by this task)", got)
	}
}

// TestReplaceConferencePlanKeepsCrawledTasks: a task carrying committed pages
// must survive a re-plan even when the new plan no longer names it. Deleting it
// trips page_html's foreign key (no ON DELETE CASCADE, foreign_keys is on),
// which rolls back the whole prune and fails the conference's plan outright —
// and it would throw away collected work in any case.
func TestReplaceConferencePlanKeepsCrawledTasks(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "CONF", `"Conf"`)
	if err != nil {
		t.Fatal(err)
	}

	mk := func(q, status string) int64 {
		id, err := d.UpsertTask(ctx, &Task{
			ConferenceID: confID, Query: q, YearFrom: 2024, YearTo: 2024, Status: status,
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", q, err)
		}
		return id
	}
	crawled := mk(`"Conf" AND "walked"`, StatusNeedsSplit) // flagged AFTER being crawled
	untouched := mk(`"Conf" AND "never"`, StatusNeedsSplit)

	if _, err := d.CommitPage(ctx, PageCommit{
		TaskID: crawled, PageNumber: 1, HTML: "<html>page</html>", NextPage: 2,
	}); err != nil {
		t.Fatalf("CommitPage: %v", err)
	}

	// A new plan that names neither task.
	if err := d.ReplaceConferencePlan(ctx, confID, []string{
		taskKey(`"Conf" AND "fresh"`, 2024, 2024, "[]"),
	}); err != nil {
		t.Fatalf("ReplaceConferencePlan: %v", err)
	}

	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, crawled).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the crawled task was pruned; its pages are collected work and its deletion breaks the foreign key")
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, untouched).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the never-crawled task should still be pruned")
	}
}
