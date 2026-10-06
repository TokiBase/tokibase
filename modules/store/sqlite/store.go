// Package sqlite is the SQLite store module: driver import, connection setup,
// pool tuning, pragmas, WAL maintenance, lock retries, concurrent/nonconcurrent
// query routing and error classification.
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/kernel/validators"
)

func init() {
	// let the kernel validators detect the driver specific unique errors
	validators.SetUniqueErrorDetector(IsUniqueError)
}

const connMaxIdleTime = 3 * time.Minute

var (
	_ kernel.DBOpener = (*Opener)(nil)
	_ kernel.DBConn   = (*conn)(nil)
)

// Opener is the SQLite [kernel.DBOpener].
type Opener struct {
	connect kernel.DBConnectFunc
}

// NewOpener creates an Opener that opens the databases with the default
// modernc.org/sqlite driver setup ([DefaultConnect]).
func NewOpener() *Opener {
	return &Opener{connect: DefaultConnect}
}

// NewOpenerFunc creates an Opener that opens the databases with a custom connect
// function (the legacy BaseAppConfig.DBConnect option). The pool tuning,
// maintenance, retries and error classification stay the same.
func NewOpenerFunc(connect kernel.DBConnectFunc) *Opener {
	if connect == nil {
		connect = DefaultConnect
	}

	return &Opener{connect: connect}
}

// Open implements [kernel.DBOpener].
func (o *Opener) Open(ctx context.Context, cfg kernel.DBConfig) (kernel.DBConn, error) {
	connect := o.connect
	if connect == nil {
		connect = DefaultConnect
	}

	concurrentDB, err := connect(cfg.Path)
	if err != nil {
		return nil, err
	}
	concurrentDB.DB().SetMaxOpenConns(cfg.MaxOpenConns)
	concurrentDB.DB().SetMaxIdleConns(cfg.MaxIdleConns)
	concurrentDB.DB().SetConnMaxIdleTime(connMaxIdleTime)

	nonconcurrentDB, err := connect(cfg.Path)
	if err != nil {
		return nil, err
	}
	nonconcurrentDB.DB().SetMaxOpenConns(1)
	nonconcurrentDB.DB().SetMaxIdleConns(1)
	nonconcurrentDB.DB().SetConnMaxIdleTime(connMaxIdleTime)

	return &conn{
		cfg:             cfg,
		concurrentDB:    concurrentDB,
		nonconcurrentDB: nonconcurrentDB,
	}, nil
}

type conn struct {
	cfg             kernel.DBConfig
	concurrentDB    *dbx.DB
	nonconcurrentDB *dbx.DB
}

func (c *conn) Concurrent() *dbx.DB    { return c.concurrentDB }
func (c *conn) Nonconcurrent() *dbx.DB { return c.nonconcurrentDB }

func (c *conn) Close() error {
	var errs []error

	for _, db := range []*dbx.DB{c.concurrentDB, c.nonconcurrentDB} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (c *conn) Maintain(ctx context.Context) error {
	var errs []error

	if err := c.Checkpoint(ctx, c.nonconcurrentDB); err != nil {
		errs = append(errs, fmt.Errorf("wal_checkpoint: %w", err))
	}

	if c.cfg.OptimizeOnMaintain {
		if err := c.Optimize(ctx, c.nonconcurrentDB); err != nil {
			errs = append(errs, fmt.Errorf("optimize: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (c *conn) Optimize(ctx context.Context, db dbx.Builder) error {
	_, err := db.NewQuery("PRAGMA optimize").WithContext(ctx).Execute()

	return err
}

func (c *conn) Checkpoint(ctx context.Context, db dbx.Builder) error {
	_, err := db.NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").WithContext(ctx).Execute()

	return err
}

func (c *conn) Vacuum(ctx context.Context, db dbx.Builder) error {
	_, err := db.NewQuery("VACUUM").WithContext(ctx).Execute()

	return err
}

func (c *conn) VacuumInto(ctx context.Context, db dbx.Builder, path string) error {
	_, err := db.NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": path}).WithContext(ctx).Execute()

	return err
}

func (c *conn) ErrorKind(err error) kernel.ErrKind {
	return classifyError(err)
}

func (c *conn) LockRetry(maxRetries int, op func(attempt int) error) error {
	return baseLockRetry(op, maxRetries)
}

func (c *conn) ExecLockRetry(timeout time.Duration, maxRetries int) dbx.ExecHookFunc {
	return execLockRetry(timeout, maxRetries)
}

func (c *conn) Route(concurrent, nonconcurrent dbx.Builder) dbx.Builder {
	return &dualDBBuilder{concurrentDB: concurrent, nonconcurrentDB: nonconcurrent}
}
