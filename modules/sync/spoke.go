//go:build !no_sync

package sync

import (
	"context"
	"errors"

	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// NewClient returns the transport client of this spoke (it must be enrolled).
// StartLoop wraps it with the sync loop (PR3).
func (m *Module) NewClient(extra ...func(*client.Options)) (*client.Client, error) {
	if m.role != RoleSpoke || m.spoke == nil {
		return nil, errors.New("sync: not a spoke (TOKI_SYNC_ROLE=spoke)")
	}
	o := client.Options{App: m.app, Identity: m.spoke, Clock: m.Clock(), Now: m.now}
	for _, f := range extra {
		f(&o)
	}
	return client.New(o)
}

// actorClient is the running loop client, or a fresh transport client.
func (m *Module) actorClient() (*client.Client, error) {
	if c := m.Client(); c != nil {
		return c, nil
	}
	return m.NewClient(func(o *client.Options) { o.Backend = backend{m} })
}

// AddActor asks the hub for an actor grant for the user behind hubToken and
// stores it (docs/SYNC_DESIGN.md §1.6). The local writes of that user are then
// captured with their grant id.
func (m *Module) AddActor(ctx context.Context, hubToken string) (*proto.ActorResponse, error) {
	c, err := m.actorClient()
	if err != nil {
		return nil, err
	}
	return c.AddActor(ctx, hubToken)
}

// RemoveActor revokes a grant on the hub and forgets it locally.
func (m *Module) RemoveActor(ctx context.Context, aid string) error {
	c, err := m.actorClient()
	if err != nil {
		return err
	}
	return c.RemoveActor(ctx, aid)
}

// LocalToken mints a local auth token for the user of a grant.
func (m *Module) LocalToken(aid string) (string, error) {
	return client.LocalToken(m.app, aid, m.Clock().WallNow())
}
