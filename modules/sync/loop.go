//go:build !no_sync

package sync

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/tools/hook"
)

// EnvPage is the number of changes per push/pull page (default 500).
const EnvPage = "TOKI_SYNC_PAGE"

// backend connects the client loop to the policies and hashing of the module.
type backend struct{ m *Module }

func (b backend) Policy(col *core.Collection) *client.PolicyView {
	p, err := b.m.pol.For(col)
	if err != nil || p == nil {
		return nil
	}
	return &client.PolicyView{Direction: p.Direction, Types: p.Types, Exclude: p.Exclude}
}

func (b backend) Rehash(tx kernel.App, rec *core.Record) error {
	p, err := b.m.pol.For(rec.Collection())
	if err != nil {
		return err
	}
	hash, err := RecordHash(rec, p)
	if err != nil {
		return err
	}
	_, err = tx.NonconcurrentDB().NewQuery("UPDATE _sync_meta SET hash={:h} WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"h": hash, "c": rec.Collection().Id, "r": rec.Id}).Execute()
	return err
}

// Client returns the running spoke client loop (nil when not started).
func (m *Module) Client() *client.Client { return m.loop.Load() }

// StartLoop starts the spoke client loop. It does nothing (and returns nil)
// when the node is not enrolled yet.
func (m *Module) StartLoop(ctx context.Context) error {
	if m.role != RoleSpoke || !m.ready.Load() {
		return errors.New("sync: the loop needs TOKI_SYNC_ROLE=spoke and an initialized module")
	}
	if m.loop.Load() != nil {
		return nil
	}
	cur, err := client.LoadCursor(m.app)
	if err != nil {
		return err
	}
	if cur == nil {
		m.app.Logger().Info("sync: this spoke is not enrolled, the client loop is not started (toki sync join <hub-url> <code>)")
		return nil
	}
	c, err := m.NewClient(func(o *client.Options) {
		o.Backend = backend{m}
		o.Logger = m.app.Logger()
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvPage))); err == nil && n > 0 {
			o.Page = n
		}
	})
	if err != nil {
		return err
	}
	m.loop.Store(c)
	c.Start(ctx)
	return nil
}

// StopLoop stops the client loop and waits for the current page.
func (m *Module) StopLoop(ctx context.Context) error {
	c := m.loop.Swap(nil)
	if c == nil {
		return nil
	}
	return c.Stop(ctx)
}

// bindLoop (spoke only) starts the loop OnServe, stops it OnTerminate and
// debounces the sync after local writes.
func (m *Module) bindLoop() {
	if m.role != RoleSpoke {
		return
	}
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "loop",
		Func: func(se *core.ServeEvent) error {
			if err := m.StartLoop(context.Background()); err != nil {
				se.App.Logger().Error("sync: failed to start the client loop", "error", err)
			}
			return se.Next()
		},
	})
	m.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId + "loop",
		Func: func(e *core.TerminateEvent) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = m.StopLoop(ctx)
			return e.Next()
		},
	})
	after := func(e *core.RecordEvent) error {
		err := e.Next()
		if c := m.loop.Load(); c != nil && kernel.SyncOriginFrom(e.Context) == nil {
			if p, _ := m.pol.For(e.Record.Collection()); p != nil {
				c.NotifyWrite()
			}
		}
		return err
	}
	for _, b := range []func(...string) *hook.TaggedHook[*core.RecordEvent]{
		m.app.OnRecordAfterCreateSuccess, m.app.OnRecordAfterUpdateSuccess, m.app.OnRecordAfterDeleteSuccess,
	} {
		b().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "write", Priority: 1000, Func: after})
	}
}
