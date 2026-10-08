//go:build !no_sync

package sync

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// The attest signature of a node verifies with the public key in the JWS node
// cert that the hub published (the app trusts the hub key).
func TestAttestVerifiesWithHubCert(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-attest", nil))
	id := kernel.NodeIdentityOf(s.app)
	if id == nil || id.Cert() == "" {
		t.Fatal("an enrolled spoke must have a cert")
	}
	now := time.Now()
	msg := attestMessage(id.NodeID(), "0123456789abcdef-nonce", now.Unix())
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := proto.VerifyCert(id.HubPub(), id.Cert(), now)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(claims.Pub)
	if err != nil {
		pub, err = base64.RawURLEncoding.DecodeString(claims.Pub)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		t.Fatal("the attest signature must verify with the node cert public key")
	}
	if ed25519.Verify(ed25519.PublicKey(pub), attestMessage(id.NodeID(), "other-nonce-0123456", now.Unix()), sig) {
		t.Fatal("a signature for another nonce must not verify")
	}
}

func TestDevCertSANPolicyD5(t *testing.T) {
	hubLocalIPs = func() map[string]bool { return map[string]bool{"192.168.1.1": true} }
	got := devCertSANs([]string{"8.8.8.8", "192.168.1.1", "192.168.1.7", "edge.toki.local", "EDGE.toki.local", "100.64.1.2", "fd00::5",
		"a.local", "b.local", "c.local", "d.local", "e.local", "a.local.", "x.local.attacker.com", "127.0.0.1"})
	want := []string{"192.168.1.7", "100.64.1.2", "fd00::5", "a.local", "b.local", "c.local", "d.local", "127.0.0.1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRevokedNodeRevokesItsCerts(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-rev", nil))
	f := &fakeCerts{}
	kernel.SetDeviceCerts(h.app, f)
	defer kernel.SetDeviceCerts(h.app, nil)
	rec, err := RevokeNode(h.app, "gate-rev", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.revoked, []string{rec.Id}) {
		t.Fatalf("revoked certs %v, want the node id %s", f.revoked, rec.Id)
	}
}

// A revoked node can not renew its edge certificate: the hub answers 403.
func TestRenewDevCertByRevokedNode(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-rev2", nil))
	kernel.SetDeviceCerts(h.app, &fakeCerts{})
	defer kernel.SetDeviceCerts(h.app, nil)
	spokeSide := &fakeCerts{spki: mustSPKI(t), due: true}
	kernel.SetDeviceCerts(s.app, spokeSide)
	defer kernel.SetDeviceCerts(s.app, nil)
	s.m.loop.Store(s.client(t, h))
	defer s.m.loop.Store(nil)
	if _, err := RevokeNode(h.app, "gate-rev2", true); err != nil {
		t.Fatal(err)
	}
	err := s.m.RenewDevCert(context.Background())
	if !client.IsCode(err, proto.CodeNodeRevoked) {
		t.Fatalf("a revoked node must get %s, got %v", proto.CodeNodeRevoked, err)
	}
	if len(spokeSide.installed) != 0 {
		t.Fatal("nothing may be installed")
	}
}

// _device_certs is the one system collection a sync policy may name.
func TestSystemCollectionAllowlist(t *testing.T) {
	c := core.NewBaseCollection("_device_certs")
	c.System = true
	if !eligible(c) {
		t.Fatal("_device_certs must be eligible")
	}
	o := core.NewBaseCollection("_other_system")
	o.System = true
	if eligible(o) {
		t.Fatal("other system collections stay excluded")
	}
	u := core.NewBaseCollection("_device_certs")
	if eligible(u) && !u.System {
		t.Fatal("a non-system collection with that name must not use the allowlist path")
	}
}

// attestMessage is the message of POST /api/device/attest (modules/devicecert
// AttestMessage; kept here because a no_devicecert build has no such symbol).
func attestMessage(node, nonce string, ts int64) []byte {
	return []byte("toki-attest/v1|" + node + "|" + nonce + "|" + strconv.FormatInt(ts, 10))
}
