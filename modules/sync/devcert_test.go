//go:build !no_sync

package sync

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// fakeCerts plays the devicecert module on both sides: Issue on the hub,
// LeafRequest/InstallLeaf on the node.
type fakeCerts struct {
	got       []kernel.DeviceCertRequest
	spki      []byte
	sans      []string
	due       bool
	installed [][2]string
}

func (f *fakeCerts) Issue(_ context.Context, r kernel.DeviceCertRequest) (*kernel.DeviceCert, error) {
	f.got = append(f.got, r)
	return &kernel.DeviceCert{Serial: "ab12", Name: r.Name, Kind: r.Kind, Node: r.Node, NotAfter: time.Now().Add(time.Hour),
		CertPEM: []byte("CERT"), CAPEM: []byte("CA")}, nil
}
func (f *fakeCerts) Lookup(context.Context, string) (*kernel.DeviceCert, error) {
	return nil, kernel.ErrDeviceCertNotFound
}
func (f *fakeCerts) Revoke(context.Context, string) error { return nil }
func (f *fakeCerts) LeafRequest(time.Time) (*kernel.LeafRequest, error) {
	if !f.due {
		return nil, nil
	}
	return &kernel.LeafRequest{SPKI: f.spki, SANs: f.sans}, nil
}
func (f *fakeCerts) InstallLeaf(c, ca []byte) error {
	f.installed = append(f.installed, [2]string{string(c), string(ca)})
	return nil
}

func TestDevCertSANsFilter(t *testing.T) {
	in := []string{"192.168.1.5", "127.0.0.1", "0.0.0.0", "169.254.1.2", "224.0.0.1", "evil.example.com", "x.edge.toki.local",
		"Gate-1.LOCAL", "box.lan", ".local", "printer.home.arpa", "fe80::1", "10.0.0.1"}
	got := devCertSANs(in)
	want := []string{"192.168.1.5", "127.0.0.1", "gate-1.local", "box.lan", "printer.home.arpa", "10.0.0.1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := len(devCertSANs(repeat("10.0.0.", 40))); n != devCertMaxSANs {
		t.Fatalf("SANs must be capped at %d, got %d", devCertMaxSANs, n)
	}
}

func repeat(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, prefix+strconv.Itoa(i))
	}
	return out
}

func mustSPKI(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestDevCertEndpointNeedsNodeToken(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	f := &fakeCerts{}
	kernel.SetDeviceCerts(h.app, f)
	defer kernel.SetDeviceCerts(h.app, nil)

	post := func(auth string, body any) (int, proto.ErrorBody) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", h.srv.URL+client.PathDevCert, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var eb proto.ErrorBody
		_ = json.NewDecoder(res.Body).Decode(&eb)
		return res.StatusCode, eb
	}
	spki := base64.StdEncoding.EncodeToString(mustSPKI(t))
	if st, eb := post("", client.DevCertRequest{SPKI: spki}); st != 401 || eb.Data["code"] != proto.CodeUnauthorized {
		t.Fatalf("no token: %d %v", st, eb)
	}
	if st, _ := post("Bearer garbage", client.DevCertRequest{SPKI: spki}); st != 401 {
		t.Fatalf("garbage token: %d", st)
	}
	if len(f.got) != 0 {
		t.Fatal("nothing must be issued without a node session")
	}

	// with a session: the cert is for the authenticated node, never for a name the body picks
	c := s.client(t, h)
	res, err := c.DevCert(context.Background(), client.DevCertRequest{SPKI: spki, SANs: []string{"192.168.1.9", "evil.example.com", "a.edge.toki.local", "gate.local"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Serial != "ab12" || res.CertPEM != "CERT" || res.CAPEM != "CA" {
		t.Fatalf("response %+v", res)
	}
	if len(f.got) != 1 {
		t.Fatalf("issued %d", len(f.got))
	}
	r := f.got[0]
	if r.Node != s.m.NodeID() || r.Name != s.m.NodeID() || r.Kind != kernel.DeviceCertServer {
		t.Fatalf("request %+v", r)
	}
	if !slices.Equal(r.SANs, []string{"192.168.1.9", "gate.local"}) {
		t.Fatalf("sans %v", r.SANs)
	}
	// a body that is not a public key is refused before the provider is called
	for _, bad := range []string{"", "!!!", base64.StdEncoding.EncodeToString([]byte("not a key"))} {
		if st, _ := post("Bearer "+c.Token(), client.DevCertRequest{SPKI: bad}); st != 400 {
			t.Fatalf("spki %q: %d", bad, st)
		}
	}
	if len(f.got) != 1 {
		t.Fatal("a bad key must not reach the provider")
	}

	// no devicecert module on the hub: 501 with a stable code
	kernel.SetDeviceCerts(h.app, nil)
	if st, eb := post("Bearer "+c.Token(), client.DevCertRequest{SPKI: spki}); st != 501 || eb.Data["code"] != CodeDevCertUnavailable {
		t.Fatalf("no provider: %d %v", st, eb)
	}
}

func TestRenewDevCertFromSpoke(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-2", nil))
	hubSide := &fakeCerts{}
	kernel.SetDeviceCerts(h.app, hubSide)
	defer kernel.SetDeviceCerts(h.app, nil)
	spokeSide := &fakeCerts{spki: mustSPKI(t), sans: []string{"192.168.7.7"}}
	kernel.SetDeviceCerts(s.app, spokeSide)
	defer kernel.SetDeviceCerts(s.app, nil)

	// no loop yet: nothing happens
	if err := s.m.RenewDevCert(context.Background()); err != nil || len(spokeSide.installed) != 0 {
		t.Fatalf("no loop: %v %v", err, spokeSide.installed)
	}
	s.m.loop.Store(s.client(t, h))
	defer s.m.loop.Store(nil)

	// not due: the hub is not asked
	if err := s.m.RenewDevCert(context.Background()); err != nil || len(hubSide.got) != 0 {
		t.Fatalf("not due: %v %d", err, len(hubSide.got))
	}
	// due: the handshake runs by itself, the cert is installed
	spokeSide.due = true
	if err := s.m.RenewDevCert(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(hubSide.got) != 1 || len(spokeSide.installed) != 1 || spokeSide.installed[0] != [2]string{"CERT", "CA"} {
		t.Fatalf("hub %d installed %v", len(hubSide.got), spokeSide.installed)
	}
	if !slices.Equal(hubSide.got[0].SANs, []string{"192.168.7.7"}) {
		t.Fatalf("sans %v", hubSide.got[0].SANs)
	}
}
