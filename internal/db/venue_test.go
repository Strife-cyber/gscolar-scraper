package db

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// TestConferenceTypeMigratesLegacyDB: a database from before conferences.type
// gains the column, with every existing venue defaulting to 'conference'.
func TestConferenceTypeMigratesLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	if err := OpenLegacySQLite(path); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	confs, err := d.ListConferences(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(confs) != 1 || confs[0].Type != TypeConference {
		t.Fatalf("migrated conferences = %+v, want ICML typed 'conference'", confs)
	}
}

// TestConferenceTypeAndAliases: type and aliases round-trip through
// ListConferences, Names puts the main query first, and re-syncing the
// aliases replaces the list instead of appending to it.
func TestConferenceTypeAndAliases(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	id, err := d.UpsertConference(ctx, "TCYB", `"IEEE Transactions on Cybernetics"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetConferenceType(ctx, id, TypeJournal); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConferenceType(ctx, id, "magazine"); err == nil {
		t.Error("type 'magazine' was accepted; the CHECK constraint should refuse it")
	}
	if err := d.SetConferenceAliases(ctx, id, []string{`"Old A"`, `"Old B"`}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConferenceAliases(ctx, id, []string{`"Old B"`}); err != nil {
		t.Fatal(err)
	}

	confs, err := d.ListConferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(confs) != 1 {
		t.Fatalf("conferences = %d, want 1", len(confs))
	}
	c := confs[0]
	if c.Type != TypeJournal {
		t.Errorf("type = %q, want journal", c.Type)
	}
	want := []string{`"IEEE Transactions on Cybernetics"`, `"Old B"`}
	if got := c.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// TestTaskHasBase: a task belongs to a name when its query is the name or the
// name plus keyword terms, and a shorter name never claims a longer one's
// tasks just because it is a prefix of it.
func TestTaskHasBase(t *testing.T) {
	short := `"IEEE Transactions on Neural Networks"`
	long := `"IEEE Transactions on Neural Networks and Learning Systems"`
	cases := []struct {
		query, base string
		want        bool
	}{
		{short, short, true},
		{short + ` AND "deep"`, short, true},
		{short + ` -"deep"`, short, true},
		{long, short, false},
		{long + ` AND "deep"`, short, false},
		{long + ` AND "deep"`, long, true},
	}
	for _, tc := range cases {
		if got := TaskHasBase(tc.query, tc.base); got != tc.want {
			t.Errorf("TaskHasBase(%s, %s) = %v, want %v", tc.query, tc.base, got, tc.want)
		}
	}
}

// TestReplaceNamePlanLeavesOtherNamesAlone: planning one name of a venue
// prunes that name's stale tasks and tasks of names the venue no longer has,
// but never the tasks of its other current names.
func TestReplaceNamePlanLeavesOtherNamesAlone(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "TNNLS", `"New"`)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(q string) int64 {
		id, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: q, YearFrom: 2010, YearTo: 2011, Status: StatusPending})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mainKept := mk(`"New"`)
	mainStale := mk(`"New" AND "stale"`)
	alias := mk(`"Old"`)
	aliasSplit := mk(`"Old" AND "deep"`)
	gone := mk(`"Renamed away"`)

	if err := d.ReplaceNamePlan(ctx, confID,
		[]string{taskKey(`"New"`, 2010, 2011, "[]")}, []string{`"Old"`}); err != nil {
		t.Fatalf("ReplaceNamePlan: %v", err)
	}

	exists := func(id int64) bool {
		var n int
		if err := d.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	for _, tc := range []struct {
		name string
		id   int64
		want bool
	}{
		{"main name, still planned", mainKept, true},
		{"main name, stale", mainStale, false},
		{"alias", alias, true},
		{"alias bucket", aliasSplit, true},
		{"name the venue no longer has", gone, false},
	} {
		if got := exists(tc.id); got != tc.want {
			t.Errorf("%s: exists = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGetVenueStats: progress is reported per venue and per name, with a
// paper served under two names counted under each but once for the venue.
func TestGetVenueStats(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	confID, err := d.UpsertConference(ctx, "TNNLS", `"New"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetConferenceType(ctx, confID, TypeJournal); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConferenceAliases(ctx, confID, []string{`"Old"`}); err != nil {
		t.Fatal(err)
	}
	newTask, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: `"New" AND "x"`, YearFrom: 2020, YearTo: 2020, Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	oldTask, err := d.UpsertTask(ctx, &Task{ConferenceID: confID, Query: `"Old"`, YearFrom: 2010, YearTo: 2010, Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	shared := Paper{Hash: "shared", Title: "Shared", Year: 2010, ConferenceID: confID}
	if _, err := d.CommitPage(ctx, PageCommit{TaskID: newTask, PageNumber: 1, HTML: "<p/>", NextPage: 2,
		Papers: []Paper{shared, {Hash: "only-new", Title: "Only new", Year: 2020, ConferenceID: confID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitPage(ctx, PageCommit{TaskID: oldTask, PageNumber: 1, HTML: "<p/>", NextPage: 2, Final: true,
		Papers: []Paper{shared}}); err != nil {
		t.Fatal(err)
	}

	vs, err := d.GetVenueStats(ctx)
	if err != nil {
		t.Fatalf("GetVenueStats: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("venues = %+v, want 1", vs)
	}
	v := vs[0]
	if v.Name != "TNNLS" || v.Type != TypeJournal || v.Papers != 2 || v.Tasks != 2 || v.Completed != 1 {
		t.Errorf("venue = %+v, want TNNLS journal with 2 papers and 1/2 tasks done", v)
	}
	want := []NameStats{
		{Query: `"New"`, Tasks: 1, Completed: 0, Papers: 2},
		{Query: `"Old"`, IsAlias: true, Tasks: 1, Completed: 1, Papers: 1},
	}
	if !reflect.DeepEqual(v.Names, want) {
		t.Errorf("names = %+v, want %+v", v.Names, want)
	}
}
