package kernel

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	_ "modernc.org/sqlite"
)

// testOpener is a minimal in-package DBOpener for the internal kernel tests
// (the sqlite store module imports the kernel, so these tests can't use it).
type testOpener struct{}

func (testOpener) Open(ctx context.Context, cfg DBConfig) (DBConn, error) {
	const dsn = "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_defensive=1"

	c := &testConn{}

	var err error
	if c.conc, err = dbx.Open("sqlite", cfg.Path+dsn); err != nil {
		return nil, err
	}
	c.conc.DB().SetMaxOpenConns(cfg.MaxOpenConns)
	c.conc.DB().SetMaxIdleConns(cfg.MaxIdleConns)

	if c.nonconc, err = dbx.Open("sqlite", cfg.Path+dsn); err != nil {
		return nil, err
	}
	c.nonconc.DB().SetMaxOpenConns(1)
	c.nonconc.DB().SetMaxIdleConns(1)

	return c, nil
}

type testConn struct{ conc, nonconc *dbx.DB }

func (c *testConn) Concurrent() *dbx.DB                                   { return c.conc }
func (c *testConn) Nonconcurrent() *dbx.DB                                { return c.nonconc }
func (c *testConn) Close() error                                          { return errors.Join(c.conc.Close(), c.nonconc.Close()) }
func (c *testConn) Maintain(context.Context) error                        { return nil }
func (c *testConn) Optimize(context.Context, dbx.Builder) error           { return nil }
func (c *testConn) Checkpoint(context.Context, dbx.Builder) error         { return nil }
func (c *testConn) Vacuum(context.Context, dbx.Builder) error             { return nil }
func (c *testConn) VacuumInto(context.Context, dbx.Builder, string) error { return nil }
func (c *testConn) ErrorKind(err error) ErrKind {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrKindNotFound
	}
	return ErrKindOther
}
func (c *testConn) LockRetry(_ int, op func(int) error) error { return op(1) }
func (c *testConn) ExecLockRetry(time.Duration, int) dbx.ExecHookFunc {
	return func(q *dbx.Query, op func() error) error { return op() }
}
func (c *testConn) Route(concurrent, nonconcurrent dbx.Builder) dbx.Builder { return concurrent }
