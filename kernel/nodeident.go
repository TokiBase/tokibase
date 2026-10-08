package kernel

import (
	"crypto/ed25519"
	"sync"
)

// NodeIdentity is the identity of this process in a sync deployment. It is
// implemented and registered by modules/sync; other modules (devicecert,
// kiosk, printer) read it through [NodeIdentityOf] without importing sync.
// No private key ever leaves the provider: callers only get [NodeIdentity.Sign].
type NodeIdentity interface {
	// NodeID is the id of this node ("" before the module is initialized).
	NodeID() string
	// HubID is the id of the hub this node belongs to (the hub's own id on a hub).
	HubID() string
	// HubPub is the hub public key (nil when unknown).
	HubPub() ed25519.PublicKey
	// Cert is the device certificate issued by the hub (compact JWS), "" on a
	// hub or on a node that is not enrolled yet.
	Cert() string
	// Sign signs msg with the node key (Ed25519). It fails while the node has
	// no key yet.
	Sign(msg []byte) ([]byte, error)
}

var nodeIdentities sync.Map // App -> NodeIdentity

// SetNodeIdentity registers the identity provider of app. A nil provider
// removes the registration.
func SetNodeIdentity(app App, id NodeIdentity) {
	if app == nil {
		return
	}
	if id == nil {
		nodeIdentities.Delete(app)
		return
	}
	nodeIdentities.Store(app, id)
}

// NodeIdentityOf returns the identity provider of app, or nil when none is
// registered (sync is off or compiled out). Providers of other apps in the same
// process are never returned.
func NodeIdentityOf(app App) NodeIdentity {
	if app == nil {
		return nil
	}
	v, _ := nodeIdentities.Load(app)
	id, _ := v.(NodeIdentity)
	return id
}
