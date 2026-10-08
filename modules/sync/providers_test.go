//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"testing"

	"github.com/tokibase/tokibase/kernel"
)

func TestKernelProvidersHub(t *testing.T) {
	h := newHub(t)
	id := kernel.NodeIdentityOf(h.app)
	if id == nil {
		t.Fatal("node identity not registered")
	}
	if id.NodeID() != h.m.NodeID() || id.HubID() != h.m.HubID() || id.Cert() != "" {
		t.Fatalf("identity mismatch: %q %q", id.NodeID(), id.HubID())
	}
	msg := []byte("hello")
	sig, err := id.Sign(msg)
	if err != nil || !ed25519.Verify(id.HubPub(), msg, sig) {
		t.Fatalf("sign/verify: %v", err)
	}
	st, ok := kernel.SyncStatusOf(h.app)
	if !ok || st.State != kernel.SyncStateHub || !st.HubReachable {
		t.Fatalf("status %+v %v", st, ok)
	}
}

func TestKernelProvidersSpokeNotEnrolled(t *testing.T) {
	s := newSpoke(t)
	id := kernel.NodeIdentityOf(s.app)
	if id == nil {
		t.Fatal("node identity not registered")
	}
	sig, err := id.Sign([]byte("x"))
	if err != nil || !ed25519.Verify(s.m.Identity().Pub(), []byte("x"), sig) {
		t.Fatalf("sign: %v", err)
	}
	if id.Cert() != "" || id.HubID() != "" {
		t.Fatal("not enrolled: no cert, no hub")
	}
	st, ok := kernel.SyncStatusOf(s.app)
	if !ok || st.State != kernel.SyncStateOffline || st.HubReachable {
		t.Fatalf("status %+v", st)
	}
}
