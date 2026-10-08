//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/tools/hook"
)

// registerProviders publishes the node identity and the sync status to the
// kernel (kernel.NodeIdentityOf / kernel.SyncStatusOf), so edge modules can use
// them without importing this package. They are dropped on terminate.
func (m *Module) registerProviders() {
	kernel.SetNodeIdentity(m.app, nodeIdent{m})
	kernel.SetSyncStatusProvider(m.app, m.kernelStatus)
	m.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId + "/providers",
		Func: func(e *core.TerminateEvent) error {
			kernel.ReleaseEdgeProviders(m.app)
			return e.Next()
		},
	})
}

type nodeIdent struct{ m *Module }

func (n nodeIdent) NodeID() string { return n.m.NodeID() }

func (n nodeIdent) HubID() string {
	if id := n.m.HubID(); id != "" {
		return id
	}
	if cur, _ := client.LoadCursor(n.m.app); cur != nil {
		return cur.HubID
	}
	return ""
}

func (n nodeIdent) HubPub() ed25519.PublicKey {
	if p := n.m.HubPub(); p != nil {
		return p
	}
	if cur, _ := client.LoadCursor(n.m.app); cur != nil {
		if b, err := base64.StdEncoding.DecodeString(cur.HubPub); err == nil && len(b) == ed25519.PublicKeySize {
			return ed25519.PublicKey(b)
		}
	}
	return nil
}

func (n nodeIdent) Cert() string {
	if n.m.role != RoleSpoke {
		return ""
	}
	if cur, _ := client.LoadCursor(n.m.app); cur != nil {
		return cur.Cert
	}
	return ""
}

func (n nodeIdent) Sign(msg []byte) ([]byte, error) {
	if sp := n.m.spoke; sp != nil && len(sp.Ed) == ed25519.PrivateKeySize {
		return ed25519.Sign(sp.Ed, msg), nil
	}
	if h := n.m.hub; h != nil && len(h.priv) == ed25519.PrivateKeySize {
		return ed25519.Sign(h.priv, msg), nil
	}
	return nil, errors.New("sync: node key not available")
}

func (m *Module) kernelStatus() kernel.SyncStatus {
	if m.role == RoleHub {
		return kernel.SyncStatus{State: kernel.SyncStateHub, HubReachable: true}
	}
	c := m.Client()
	if c == nil {
		return kernel.SyncStatus{State: kernel.SyncStateOffline}
	}
	st := c.Status()
	out := kernel.SyncStatus{HubReachable: st.Online && st.Failures == 0 && !st.LastOK.IsZero(), Pending: st.Pending, LastSync: st.LastOK}
	switch {
	case st.Paused:
		out.State = kernel.SyncStatePaused
	case out.HubReachable:
		out.State = kernel.SyncStateOnline
	default:
		out.State = kernel.SyncStateOffline
	}
	return out
}
