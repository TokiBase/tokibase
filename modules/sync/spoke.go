//go:build !no_sync

package sync

import (
	"errors"

	"github.com/tokibase/tokibase/modules/sync/client"
)

// NewClient returns the transport client of this spoke (it must be enrolled).
// PR2 has no loop yet; callers run Handshake/Ping themselves.
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
