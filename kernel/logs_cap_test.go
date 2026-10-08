package kernel_test

import (
	"context"
	"testing"

	"github.com/tokibase/tokibase/tests"
)

func TestPruneLogsBySize(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	_, err := app.AuxNonconcurrentDB().NewQuery(`
		INSERT INTO {{_logs}} ([[id]], [[level]], [[message]], [[data]], [[created]])
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 6000)
		SELECT 'cap'||x, 0, 'GET /api/x/'||x, json_object('pad', hex(randomblob(150))), strftime('%Y-%m-%d %H:%M:%fZ') FROM c
	`).Execute()
	if err != nil {
		t.Fatal(err)
	}

	live := func() int64 {
		var n int64
		err := app.AuxConcurrentDB().NewQuery(
			"SELECT ((SELECT page_count FROM pragma_page_count) - (SELECT freelist_count FROM pragma_freelist_count)) * (SELECT page_size FROM pragma_page_size)",
		).Row(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	count := func(where string) int64 {
		var n int64
		if err := app.AuxConcurrentDB().NewQuery("SELECT count(*) FROM {{_logs}} WHERE " + where).Row(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	ctx := context.Background()
	before := live()
	rowsBefore := count("1=1")

	// disabled or under the cap: nothing happens
	for _, max := range []int64{0, -1, before * 2} {
		if n, err := app.PruneLogsBySize(ctx, max); err != nil || n != 0 {
			t.Fatalf("max %d: expected no deletes, got %d (%v)", max, n, err)
		}
	}

	max := before / 2
	deleted, err := app.PruneLogsBySize(ctx, max)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == 0 || count("1=1") != rowsBefore-deleted {
		t.Fatalf("expected deleted rows, got %d (rows %d -> %d)", deleted, rowsBefore, count("1=1"))
	}
	if after := live(); after > max {
		t.Fatalf("expected the live size under %d, got %d (before %d)", max, after, before)
	}
	if count("[[id]] = 'cap1'") != 0 || count("[[id]] = 'cap6000'") != 1 {
		t.Fatal("expected the oldest log to be pruned and the newest kept")
	}
}
