package kernel

import (
	"context"
	"crypto/ed25519"
	"testing"
)

type fakeIdent struct{ id string }

func (f fakeIdent) NodeID() string                { return f.id }
func (f fakeIdent) HubID() string                 { return "h" }
func (f fakeIdent) HubPub() ed25519.PublicKey     { return nil }
func (f fakeIdent) Cert() string                  { return "" }
func (f fakeIdent) Sign(m []byte) ([]byte, error) { return m, nil }

type fakeCerts struct{}

func (fakeCerts) Issue(context.Context, DeviceCertRequest) (*DeviceCert, error) {
	return &DeviceCert{Serial: "1"}, nil
}
func (fakeCerts) Lookup(context.Context, string) (*DeviceCert, error) {
	return nil, ErrDeviceCertNotFound
}
func (fakeCerts) Revoke(context.Context, string) error { return nil }

// two distinct comparable App values
type appA struct{ App }
type appB struct{ App }

func TestEdgeProvidersNilSafe(t *testing.T) {
	if NodeIdentityOf(nil) != nil || DeviceCertsOf(nil) != nil {
		t.Fatal("nil app must give nil")
	}
	if _, ok := SyncStatusOf(nil); ok {
		t.Fatal("nil app must give !ok")
	}
	SetNodeIdentity(nil, fakeIdent{})
	SetSyncStatusProvider(nil, func() SyncStatus { return SyncStatus{} })
	SetDeviceCerts(nil, fakeCerts{})
	ReleaseEdgeProviders(nil)
	a := &appA{}
	if NodeIdentityOf(a) != nil || DeviceCertsOf(a) != nil {
		t.Fatal("unregistered app must give nil")
	}
	if _, ok := SyncStatusOf(a); ok {
		t.Fatal("unregistered must give !ok")
	}
}

func TestEdgeProvidersPerApp(t *testing.T) {
	a, b := &appA{}, &appB{}
	SetNodeIdentity(a, fakeIdent{"na"})
	SetNodeIdentity(b, fakeIdent{"nb"})
	SetSyncStatusProvider(a, func() SyncStatus { return SyncStatus{Pending: 3, HubReachable: true} })
	SetDeviceCerts(a, fakeCerts{})
	defer ReleaseEdgeProviders(a)
	defer ReleaseEdgeProviders(b)

	if NodeIdentityOf(a).NodeID() != "na" || NodeIdentityOf(b).NodeID() != "nb" {
		t.Fatal("identity must be per app")
	}
	if st, ok := SyncStatusOf(a); !ok || st.Pending != 3 || !st.HubReachable {
		t.Fatalf("status a: %+v %v", st, ok)
	}
	if _, ok := SyncStatusOf(b); ok {
		t.Fatal("b has no status provider")
	}
	if DeviceCertsOf(a) == nil || DeviceCertsOf(b) != nil {
		t.Fatal("devicecerts must be per app")
	}
	if _, err := DeviceCertsOf(a).Lookup(context.Background(), "x"); err != ErrDeviceCertNotFound {
		t.Fatal(err)
	}

	SetNodeIdentity(a, nil)
	if NodeIdentityOf(a) != nil {
		t.Fatal("nil removes")
	}
	ReleaseEdgeProviders(a)
	if DeviceCertsOf(a) != nil {
		t.Fatal("release drops")
	}
	if _, ok := SyncStatusOf(a); ok {
		t.Fatal("release drops status")
	}
	if NodeIdentityOf(b) == nil {
		t.Fatal("release of a must not touch b")
	}
}
