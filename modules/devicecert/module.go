//go:build !no_devicecert

// Package devicecert gives an edge node a real X.509 identity: the hub keeps a
// private ECDSA P-256 CA, a node gets a short-lived server leaf from it over
// the sync session (POST /api/sync/devcert, implemented in modules/sync) and
// serves HTTPS on TOKI_DEVICECERT_LISTEN with it. See docs/modules/devicecert.md.
package devicecert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const hookId = "__tokiDeviceCert__"

var (
	sinkMu     sync.Mutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects devicecert.issue and devicecert.revoke to an external
// audit log. Modules must not import each other, so the wiring is in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, record string, details map[string]any) {
	sinkMu.Lock()
	fn := globalSink
	sinkMu.Unlock()
	if fn != nil {
		fn(action, CertsCollection, record, details)
	}
}

// Module is the registered devicecert module. It implements
// [kernel.DeviceCertProvider] and [kernel.EdgeLeafProvider].
type Module struct {
	app core.App
	// Now is the clock (tests inject one).
	now func() time.Time
	// lanIPs lists the addresses that go into the leaf (tests replace it).
	lanIPs func() []string

	leaf *leafStore
	deny *denyList

	caMu sync.Mutex
	ca   *CA

	pendMu      sync.Mutex
	pendingSANs []string

	srvMu     sync.Mutex
	srv       *serverState
	cancelAll context.CancelFunc

	errMu   sync.Mutex
	lastErr string
}

var (
	_ kernel.DeviceCertProvider = (*Module)(nil)
	_ kernel.EdgeLeafProvider   = (*Module)(nil)
)

// New builds a module for app without registering any hook (the CLI uses it).
func New(app core.App) *Module {
	m := &Module{app: app, now: func() time.Time { return time.Now().UTC() }, lanIPs: lanIPs}
	m.leaf = newLeafStore(app.DataDir())
	m.deny = &denyList{m: m, ttl: 15 * time.Second, load: m.loadRevoked}
	return m
}

// Register binds the module to app: schema on bootstrap, the provider, the
// TLS listener on serve and the health block.
func Register(app core.App) *Module {
	m := New(app)
	init := func() {
		if err := ensureSchema(app); err != nil {
			app.Logger().Error("devicecert: failed to initialize the schema", "error", err)
			return
		}
		if err := m.leaf.load(); err != nil {
			app.Logger().Warn("devicecert: failed to load the stored edge certificate", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: 1 << 20,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
	kernel.SetDeviceCerts(app, m)
	m.bindServe()
	apis.SetHealthExtra(app, "devicecert", func(core.App) any { return m.Health() })
	return m
}

// ---- hub side -----------------------------------------------------------

// hubIdentity returns the node identity when this process is the hub.
func (m *Module) hubIdentity() (kernel.NodeIdentity, error) {
	ni := kernel.NodeIdentityOf(m.app)
	if ni == nil {
		return nil, errors.New("devicecert: the CA needs the sync hub (set TOKI_SYNC_ROLE=hub)")
	}
	if ni.HubID() == "" || ni.NodeID() != ni.HubID() {
		return nil, ErrNotHub
	}
	return ni, nil
}

// IsHub reports whether this process is the sync hub.
func (m *Module) IsHub() bool {
	_, err := m.hubIdentity()
	return err == nil
}

func wrapSecret(ni kernel.NodeIdentity) ([]byte, error) {
	return ni.Sign([]byte(caWrapInfo))
}

// CA loads the hub CA, creating it on first use when create is true. It
// returns (nil, nil) when there is none and create is false.
func (m *Module) CA(create bool) (*CA, error) {
	m.caMu.Lock()
	defer m.caMu.Unlock()
	if m.ca != nil {
		return m.ca, nil
	}
	ni, err := m.hubIdentity()
	if err != nil {
		return nil, err
	}
	if err := ensureSchema(m.app); err != nil {
		return nil, err
	}
	st := stateDB{m.app}
	secret, err := wrapSecret(ni)
	if err != nil {
		return nil, err
	}
	v, ok, err := st.get(stateCA)
	if err != nil {
		return nil, err
	}
	if !ok {
		if !create {
			return nil, nil
		}
		ca, err := NewCA(ni.HubID(), m.now())
		if err != nil {
			return nil, err
		}
		wrapped, err := WrapCAKey(secret, ca)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(storedCA{CertPEM: string(ca.PEM), KeyWrapped: base64.StdEncoding.EncodeToString(wrapped), Epoch: 1})
		if err := st.putIfAbsent(stateCA, string(b)); err != nil {
			return nil, err
		}
		if v, ok, err = st.get(stateCA); err != nil || !ok { // another process may have won
			return nil, errors.Join(err, errors.New("devicecert: the CA was not stored"))
		}
	}
	var sc storedCA
	if err := json.Unmarshal([]byte(v), &sc); err != nil {
		return nil, fmt.Errorf("devicecert: invalid stored CA: %w", err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(sc.KeyWrapped)
	if err != nil {
		return nil, err
	}
	ca, err := UnwrapCA(secret, wrapped, []byte(sc.CertPEM))
	if err != nil {
		return nil, err
	}
	m.ca = ca
	return ca, nil
}

type storedCA struct {
	CertPEM    string `json:"cert_pem"`
	KeyWrapped string `json:"key_wrapped"`
	Epoch      int    `json:"epoch"`
}

// Issue implements [kernel.DeviceCertProvider]. Only the hub issues.
func (m *Module) Issue(ctx context.Context, req kernel.DeviceCertRequest) (*kernel.DeviceCert, error) {
	ca, err := m.CA(true)
	if err != nil {
		return nil, err
	}
	kind := req.Kind
	if kind == "" {
		kind = kernel.DeviceCertServer
	}
	if kind != kernel.DeviceCertServer && kind != kernel.DeviceCertClient {
		return nil, errors.New("devicecert: kind must be server or client")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("devicecert: a name is required")
	}
	spki := req.SPKI
	var keyPEM []byte
	if len(spki) == 0 {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		if spki, err = x509.MarshalPKIXPublicKey(&k.PublicKey); err != nil {
			return nil, err
		}
		if keyPEM, err = KeyPEM(k); err != nil {
			return nil, err
		}
	}
	sans := req.SANs
	if kind == kernel.DeviceCertServer && req.Node != "" {
		sans = append(append([]string{}, sans...), req.Node+NodeDNSSuffix)
	}
	dns, ips := SplitSANs(sans)
	days := req.Days
	if days <= 0 {
		days = LeafDays()
	}
	cert, certPEM, err := ca.Issue(m.now(), LeafParams{Name: name, Kind: kind, SPKI: spki, DNS: dns, IPs: ips, Days: days})
	if err != nil {
		return nil, err
	}
	serial := SerialHex(cert.SerialNumber)
	if err := m.insertCert(serial, name, kind, req.Node, cert.NotAfter, req.RouteScope); err != nil {
		return nil, fmt.Errorf("devicecert: failed to record the certificate: %w", err)
	}
	audit("devicecert.issue", serial, map[string]any{
		"name": name, "kind": string(kind), "node": req.Node, "not_after": cert.NotAfter.UTC().Format(time.RFC3339),
		"dns": dns, "ips": ipStrings(ips),
	})
	return &kernel.DeviceCert{
		Serial: serial, Name: name, Kind: kind, Node: req.Node, NotAfter: cert.NotAfter,
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: ca.PEM,
	}, nil
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// Lookup implements [kernel.DeviceCertProvider].
func (m *Module) Lookup(ctx context.Context, serialOrName string) (*kernel.DeviceCert, error) {
	r, err := m.find(serialOrName)
	if err != nil {
		return nil, err
	}
	return recordToCert(r), nil
}

// Revoke implements [kernel.DeviceCertProvider].
func (m *Module) Revoke(ctx context.Context, serialOrName string) error {
	r, err := m.find(serialOrName)
	if err != nil {
		return err
	}
	if !r.GetDateTime("revoked_at").IsZero() {
		return nil
	}
	r.Set("revoked_at", m.now().UTC().Format("2006-01-02 15:04:05.000Z"))
	if err := m.app.Save(r); err != nil {
		return err
	}
	m.deny.mu.Lock()
	m.deny.set = nil
	m.deny.mu.Unlock()
	audit("devicecert.revoke", r.GetString("serial"), map[string]any{"name": r.GetString("name"), "node": r.GetString("node")})
	return nil
}

// wantedSANs are the addresses this node asks for: its LAN IPs and
// TOKI_DEVICECERT_SANS.
func (m *Module) wantedSANs() []string {
	return append(append([]string{}, m.lanIPs()...), ExtraSANs()...)
}

// selfIssue gives the hub its own edge certificate (the hub is an edge too
// when it serves the TLS listener). It does nothing when no renewal is due.
func (m *Module) selfIssue(ctx context.Context) error {
	ni, err := m.hubIdentity()
	if err != nil {
		return err
	}
	wanted := m.wantedSANs()
	if !m.leaf.due(m.now(), wanted) {
		return nil
	}
	spki, err := m.leaf.spki()
	if err != nil {
		return err
	}
	c, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: ni.HubID(), Kind: kernel.DeviceCertServer, Node: ni.HubID(), SANs: wanted, SPKI: spki})
	if err != nil {
		return err
	}
	m.leaf.setLastReq(wanted)
	return m.leaf.install(c.CertPEM, c.CAPEM, true)
}

// ---- node side ----------------------------------------------------------

// LeafRequest implements [kernel.EdgeLeafProvider].
func (m *Module) LeafRequest(now time.Time) (*kernel.LeafRequest, error) {
	wanted := m.wantedSANs()
	if !m.leaf.due(now, wanted) {
		return nil, nil
	}
	spki, err := m.leaf.spki()
	if err != nil {
		return nil, err
	}
	m.pendMu.Lock()
	m.pendingSANs = wanted
	m.pendMu.Unlock()
	return &kernel.LeafRequest{SPKI: spki, SANs: wanted}, nil
}

// InstallLeaf implements [kernel.EdgeLeafProvider].
func (m *Module) InstallLeaf(certPEM, caPEM []byte) error {
	if err := m.leaf.install(certPEM, caPEM, true); err != nil {
		return err
	}
	m.pendMu.Lock()
	pend := m.pendingSANs
	m.pendMu.Unlock()
	m.leaf.setLastReq(pend)
	return nil
}

// ---- status -------------------------------------------------------------

// LeafInfo describes the edge certificate of this node.
type LeafInfo struct {
	Serial    string   `json:"serial"`
	NotBefore string   `json:"not_before"`
	NotAfter  string   `json:"not_after"`
	DNS       []string `json:"dns,omitempty"`
	IPs       []string `json:"ips,omitempty"`
}

// Health is the `devicecert` block of GET /api/health and `toki devicecert status`.
type Health struct {
	Enabled    bool      `json:"enabled"`
	Role       string    `json:"role"`
	Listen     string    `json:"listen,omitempty"`
	Listening  bool      `json:"listening"`
	Addr       string    `json:"addr,omitempty"`
	MTLS       string    `json:"mtls"`
	Leaf       *LeafInfo `json:"leaf,omitempty"`
	CAFingerpr string    `json:"ca_fingerprint,omitempty"`
	LeafDays   int       `json:"leaf_days"`
	RenewDue   bool      `json:"renew_due"`
	LastError  string    `json:"last_error,omitempty"`
}

// Health reports the state of this node.
func (m *Module) Health() *Health {
	h := &Health{Enabled: Enabled(), Role: "node", Listen: ListenAddr(), MTLS: string(MTLSMode()), LeafDays: LeafDays()}
	if m.IsHub() {
		h.Role = "hub"
	}
	h.Listening, h.Addr = m.listenState()
	m.errMu.Lock()
	h.LastError = m.lastErr
	m.errMu.Unlock()
	leaf, root, _ := m.leaf.current()
	if leaf != nil {
		h.Leaf = leafInfo(leaf)
		h.RenewDue = m.leaf.due(m.now(), m.wantedSANs())
	}
	if root != nil {
		h.CAFingerpr = Fingerprint(root.Raw)
	}
	return h
}

func leafInfo(c *x509.Certificate) *LeafInfo {
	return &LeafInfo{
		Serial: SerialHex(c.SerialNumber), NotBefore: c.NotBefore.UTC().Format(time.RFC3339),
		NotAfter: c.NotAfter.UTC().Format(time.RFC3339), DNS: c.DNSNames, IPs: ipStrings(c.IPAddresses),
	}
}
