package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestReplaceConferencePlanSupersedesCrawledPending: a pending task with pages
// that the new plan drops is kept (its pages are collected work) but marked
// superseded, so the crawl stops picking it up; a later plan that emits the
// same leaf again re-adopts it as pending.
func TestReplaceConferencePlanSupersedesCrawledPending(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, _ := d.UpsertConference(ctx, "CONF", `"Conf"`)
	stale := &Task{ConferenceID: confID, Query: `"Conf" AND "neural"`, YearFrom: 2022, YearTo: 2022, Keywords: []string{"neural"}}
	id, err := d.UpsertTask(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitPage(ctx, PageCommit{TaskID: id, PageNumber: 1, HTML: "p", NextPage: 2}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceConferencePlan(ctx, confID, nil); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, d, id); got != StatusSuperseded {
		t.Fatalf("status %q, want superseded", got)
	}
	if pend, _ := d.PendingTasks(ctx, confID); len(pend) != 0 {
		t.Fatalf("superseded task still pending: %+v", pend)
	}

	stale.Status = StatusPending
	if _, err := d.UpsertTask(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got := taskStatus(t, d, id); got != StatusPending {
		t.Fatalf("re-planned leaf status %q, want pending", got)
	}
}

func taskStatus(t *testing.T, d *DB, id int64) string {
	t.Helper()
	var s string
	if err := d.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// openAt opens a DB at a known path so it can be handed to Merge.
func openAt(t *testing.T, name string) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// TestMerge: two machines scraped overlapping work with different row ids. The
// merge matches tasks by content, keeps the furthest-along status, remaps pages
// and papers to local ids, dedups papers by hash, and is idempotent.
func TestMerge(t *testing.T) {
	ctx := context.Background()
	local, localPath := openAt(t, "local.db")
	remote, remotePath := openAt(t, "remote.db")

	// Different insertion order so the same conference/task get different ids.
	lICML, _ := local.UpsertConference(ctx, "ICML", `"ICML"`)
	remote.UpsertConference(ctx, "CVPR", `"CVPR"`)
	rICML, _ := remote.UpsertConference(ctx, "ICML", `"ICML"`)

	shared := func(confID int64) *Task {
		return &Task{ConferenceID: confID, Query: `"ICML" AND "graph"`, YearFrom: 2020, YearTo: 2020,
			Keywords: []string{"graph"}, TotalEstimate: 400}
	}
	lShared, _ := local.UpsertTask(ctx, shared(lICML))
	remote.UpsertTask(ctx, &Task{ConferenceID: rICML, Query: `"ICML" -"graph"`, YearFrom: 2020, YearTo: 2020, Keywords: []string{"graph"}})
	rShared, _ := remote.UpsertTask(ctx, shared(rICML))

	// Local crawled page 1 of the shared task; remote crawled it to the end.
	if _, err := local.CommitPage(ctx, PageCommit{TaskID: lShared, PageNumber: 1, HTML: "L1", NextPage: 2,
		Papers: []Paper{{Hash: "h1", Title: "A", TaskID: lShared}}}); err != nil {
		t.Fatal(err)
	}
	for p := 1; p <= 3; p++ {
		if _, err := remote.CommitPage(ctx, PageCommit{TaskID: rShared, PageNumber: p, HTML: "R", NextPage: p + 1, Final: p == 3,
			Papers: []Paper{{Hash: []string{"h1", "h2", "h3"}[p-1], Title: "T", Snippet: "s", TaskID: rShared}}}); err != nil {
			t.Fatal(err)
		}
	}
	remote.SetCachedCount(ctx, `"ICML"`, 2020, 2020, 3520)
	remote.Close()

	st, err := local.Merge(ctx, localPath, remotePath)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if st.Conferences != 1 || st.TasksAdded != 1 || st.TasksUpdated != 1 || st.Pages != 2 || st.Papers != 2 || st.Counts != 1 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if got := taskStatus(t, local, lShared); got != StatusCompleted {
		t.Errorf("shared task status %q, want completed", got)
	}
	var page int
	local.db.QueryRow(`SELECT page FROM tasks WHERE id = ?`, lShared).Scan(&page)
	if page != 4 {
		t.Errorf("shared task page %d, want 4", page)
	}
	var html, snippet string
	local.db.QueryRow(`SELECT html FROM page_html WHERE task_id = ? AND page_number = 1`, lShared).Scan(&html)
	if html != "L1" {
		t.Errorf("local page 1 overwritten: %q", html)
	}
	local.db.QueryRow(`SELECT snippet FROM papers WHERE hash = 'h1'`).Scan(&snippet)
	if snippet != "s" {
		t.Errorf("empty local snippet not filled from remote: %q", snippet)
	}
	var orphan int
	local.db.QueryRow(`SELECT COUNT(*) FROM papers WHERE task_id IS NOT NULL AND task_id NOT IN (SELECT id FROM tasks)`).Scan(&orphan)
	if orphan != 0 {
		t.Errorf("%d papers point at missing tasks", orphan)
	}
	if n, ok, _ := local.GetCachedCount(ctx, `"ICML"`, 2020, 2020); !ok || n != 3520 {
		t.Errorf("count_cache not merged: %d %v", n, ok)
	}

	again, err := local.Merge(ctx, localPath, remotePath)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if again != (MergeStats{}) {
		t.Errorf("second merge not idempotent: %+v", again)
	}

	if _, err := local.Merge(ctx, localPath, localPath); err == nil {
		t.Error("merging a database into itself should fail")
	}
}
