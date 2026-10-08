//go:build !no_sync

package sync

import (
	"context"
	"errors"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// revokeNodeCerts revokes the edge certificates of a revoked node through the
// devicecert provider (a no-op without the module). The provider revokes every
// unrevoked certificate named after the node id, so the deny list reaches the
// edges with the next pull.
func revokeNodeCerts(app core.App, nodeID string) {
	p := kernel.DeviceCertsOf(app)
	if p == nil || nodeID == "" {
		return
	}
	if err := p.Revoke(context.Background(), nodeID); err != nil && !errors.Is(err, kernel.ErrDeviceCertNotFound) {
		app.Logger().Warn("sync: failed to revoke the edge certificates of a revoked node", "node", nodeID, "error", err)
	}
}
