package db

import (
	"context"
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
	if err := d.CommitPage(ctx, pc); err != nil {
		t.Fatal(err)
	}

	// Re-committing the same page must be idempotent (papers stay dedup'd,
	// page_html not duplicated).
	if err := d.CommitPage(ctx, pc); err != nil {
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
