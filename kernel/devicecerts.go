package kernel

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrDeviceCertNotFound is returned by [DeviceCertProvider.Lookup] and
// [DeviceCertProvider.Revoke] for an unknown serial or name.
var ErrDeviceCertNotFound = errors.New("device certificate not found")

// DeviceCertKind tells the key usage of a device certificate.
type DeviceCertKind string

// Kinds of device certificates.
const (
	DeviceCertClient DeviceCertKind = "client"
	DeviceCertServer DeviceCertKind = "server"
)

// DeviceCertRequest asks for a certificate.
type DeviceCertRequest struct {
	Name string
	Kind DeviceCertKind
	// Node is the sync node the certificate belongs to ("" for a LAN peer).
	Node string
	// SANs are DNS names and IP addresses.
	SANs []string
	// Days is the lifetime (0 = the module default).
	Days int
	// SPKI is the DER public key to certify. When empty the provider generates
	// a key pair and returns the private key in the result.
	SPKI []byte
	// RouteScope limits the routes a client certificate may call ("" = none).
	RouteScope string
}

// DeviceCert is an issued certificate.
type DeviceCert struct {
	Serial    string
	Name      string
	Kind      DeviceCertKind
	Node      string
	NotAfter  time.Time
	RevokedAt time.Time // zero when valid
	CertPEM   []byte
	// KeyPEM is set only by Issue when the provider generated the key.
	KeyPEM []byte
}

// DeviceCertProvider is implemented by modules/devicecert. modules/sync uses it
// (through [DeviceCertsOf]) to issue the node leaf and to revoke it when a node
// is revoked, without importing the module.
type DeviceCertProvider interface {
	Issue(ctx context.Context, req DeviceCertRequest) (*DeviceCert, error)
	// Lookup finds a certificate by serial or name.
	Lookup(ctx context.Context, serialOrName string) (*DeviceCert, error)
	// Revoke marks a certificate by serial or name as revoked.
	Revoke(ctx context.Context, serialOrName string) error
}

var deviceCerts sync.Map // App -> DeviceCertProvider

// SetDeviceCerts registers the provider of app (nil removes it).
func SetDeviceCerts(app App, p DeviceCertProvider) {
	if app == nil {
		return
	}
	if p == nil {
		deviceCerts.Delete(app)
		return
	}
	deviceCerts.Store(app, p)
}

// DeviceCertsOf returns the provider of app, or nil when the devicecert module
// is off or compiled out.
func DeviceCertsOf(app App) DeviceCertProvider {
	if app == nil {
		return nil
	}
	v, _ := deviceCerts.Load(app)
	p, _ := v.(DeviceCertProvider)
	return p
}

// ReleaseEdgeProviders drops the node identity, sync status and device cert
// providers of app (called on terminate).
func ReleaseEdgeProviders(app App) {
	if app == nil {
		return
	}
	nodeIdentities.Delete(app)
	syncStatusProviders.Delete(app)
	deviceCerts.Delete(app)
}
