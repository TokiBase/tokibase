package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/store/sqlite"
)

func TestErrorKind(t *testing.T) {
	t.Parallel()

	c := openTestConn(t, true)

	scenarios := []struct {
		err      error
		expected kernel.ErrKind
	}{
		{nil, kernel.ErrKindOther},
		{errors.New("test"), kernel.ErrKindOther},
		{fmt.Errorf("wrap: %w", sql.ErrNoRows), kernel.ErrKindNotFound},
		{errors.New("database is locked"), kernel.ErrKindLocked},
		{errors.New("table is locked"), kernel.ErrKindLocked},
		{errors.New("constraint failed: UNIQUE constraint failed: a.b (2067)"), kernel.ErrKindUnique},
		{errors.New("constraint failed: NOT NULL constraint failed: a.b (1299)"), kernel.ErrKindConstraint},
	}

	for i, s := range scenarios {
		if k := c.ErrorKind(s.err); k != s.expected {
			t.Errorf("[%d] expected %d, got %d", i, s.expected, k)
		}
	}
}

func TestOpenPoolPragmasAndMaintenance(t *testing.T) {
	t.Parallel()

	c := openTestConn(t, true)
	ctx := context.Background()

	var mode string
	if err := c.Nonconcurrent().NewQuery("PRAGMA journal_mode").Row(&mode); err != nil || mode != "wal" {
		t.Fatalf("expected wal journal mode, got %q (%v)", mode, err)
	}

	var fk int
	if err := c.Nonconcurrent().NewQuery("PRAGMA foreign_keys").Row(&fk); err != nil || fk != 1 {
		t.Fatalf("expected foreign_keys=1, got %d (%v)", fk, err)
	}

	if v := c.Concurrent().DB().Stats().MaxOpenConnections; v != 7 {
		t.Fatalf("expected concurrent max open 7, got %d", v)
	}
	if v := c.Nonconcurrent().DB().Stats().MaxOpenConnections; v != 1 {
		t.Fatalf("expected nonconcurrent max open 1, got %d", v)
	}

	if err := c.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Vacuum(ctx, c.Nonconcurrent()); err != nil {
		t.Fatal(err)
	}
	if err := c.VacuumInto(ctx, c.Concurrent(), filepath.Join(t.TempDir(), "copy.db")); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func openTestConn(t *testing.T, optimize bool) kernel.DBConn {
	t.Helper()

	c, err := sqlite.NewOpener().Open(context.Background(), kernel.DBConfig{
		Path:               filepath.Join(t.TempDir(), "test.db"),
		MaxOpenConns:       7,
		MaxIdleConns:       3,
		OptimizeOnMaintain: optimize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}
