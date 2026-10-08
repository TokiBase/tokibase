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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/edgeguard"
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
	cas  *caSet

	infoMu sync.Mutex
	infos  map[string]certInfo

	deviceThr *edgeguard.Throttle

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
	m.deny = &denyList{m: m, ttl: 15 * time.Second, grace: denyGrace, load: m.loadRevoked}
	m.deviceThr = edgeguard.NewThrottle(30, time.Minute)
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
	m.bindHTTP()
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

// caRefresh is how long the loaded CA set is trusted before the state table is
// read again (so `toki devicecert rotate-ca` in another process is picked up).
const caRefresh = 10 * time.Second

// caSet is the CA state: the current (signing) CA and the rotated-out roots.
type caSet struct {
	cur    *CA
	epoch  int
	old    []Root // RetireAt set; some may be past their overlap
	raw    string
	rawOld string
	at     time.Time
}

type storedOld struct {
	CertPEM  string `json:"cert_pem"`
	RetireAt string `json:"retire_at"`
}

// roots lists the roots a client certificate may chain to at now.
func (c *caSet) roots(now time.Time) []Root {
	out := []Root{{Cert: c.cur.Cert, PEM: c.cur.PEM}}
	for _, r := range c.old {
		if r.Active(now) {
			out = append(out, r)
		}
	}
	return out
}

// bundle is the PEM bundle given to peers: the signing root first, then the
// rotated-out roots that are still in their overlap (with their retire time).
func (c *caSet) bundle(now time.Time) []byte {
	out := append([]byte{}, c.cur.PEM...)
	for _, r := range c.old {
		if r.Active(now) {
			out = append(out, EncodeRoot(r.PEM, r.RetireAt)...)
		}
	}
	return out
}

// CA loads the hub CA, creating it on first use when create is true. It
// returns (nil, nil) when there is none and create is false.
func (m *Module) CA(create bool) (*CA, error) {
	cs, err := m.loadCAs(create)
	if err != nil || cs == nil {
		return nil, err
	}
	return cs.cur, nil
}

func (m *Module) loadCAs(create bool) (*caSet, error) {
	m.caMu.Lock()
	defer m.caMu.Unlock()
	now := m.now()
	if m.cas != nil && now.Sub(m.cas.at) < caRefresh {
		return m.cas, nil
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
		ca, err := NewCA(ni.HubID(), now)
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
		if os.Getenv("TOKI_SYNC_HUB_KEY_FILE") == "" {
			m.app.Logger().Warn("devicecert: the hub key is stored in data.db next to the wrapped CA key, so a copy of data.db can mint certificates; set TOKI_SYNC_HUB_KEY_FILE to keep the hub key outside the database")
		}
	}
	rawOld, _, err := st.get(stateCAOld)
	if err != nil {
		return nil, err
	}
	if m.cas != nil && m.cas.raw == v && m.cas.rawOld == rawOld {
		m.cas.at = now
		return m.cas, nil
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
	cs := &caSet{cur: ca, epoch: sc.Epoch, raw: v, rawOld: rawOld, at: now}
	if rawOld != "" {
		var olds []storedOld
		if err := json.Unmarshal([]byte(rawOld), &olds); err != nil {
			return nil, fmt.Errorf("devicecert: invalid stored old CAs: %w", err)
		}
		for _, o := range olds {
			c, err := ParseCertPEM([]byte(o.CertPEM))
			if err != nil {
				return nil, err
			}
			t, err := time.Parse(time.RFC3339, o.RetireAt)
			if err != nil {
				return nil, err
			}
			cs.old = append(cs.old, Root{Cert: c, PEM: []byte(o.CertPEM), RetireAt: t})
		}
	}
	m.cas = cs
	return cs, nil
}

type storedCA struct {
	CertPEM    string `json:"cert_pem"`
	KeyWrapped string `json:"key_wrapped"`
	Epoch      int    `json:"epoch"`
}

// RotateCA creates a new CA. The previous root stays trusted for overlapDays
// (peers and nodes get both roots in the bundle); the new one signs from now
// on. The old root is listed with its retire time.
func (m *Module) RotateCA(overlapDays int) (*CA, time.Time, error) {
	m.caMu.Lock()
	m.cas = nil // read the stored state, not a cache
	m.caMu.Unlock()
	cs, err := m.loadCAs(true)
	if err != nil {
		return nil, time.Time{}, err
	}
	ni, err := m.hubIdentity()
	if err != nil {
		return nil, time.Time{}, err
	}
	secret, err := wrapSecret(ni)
	if err != nil {
		return nil, time.Time{}, err
	}
	now := m.now()
	ca, err := NewCA(ni.HubID(), now)
	if err != nil {
		return nil, time.Time{}, err
	}
	wrapped, err := WrapCAKey(secret, ca)
	if err != nil {
		return nil, time.Time{}, err
	}
	retire := now.Add(time.Duration(overlapDays) * 24 * time.Hour)
	olds := []storedOld{{CertPEM: string(cs.cur.PEM), RetireAt: retire.UTC().Format(time.RFC3339)}}
	for _, r := range cs.old { // drop the roots that are past their overlap
		if r.Active(now) {
			olds = append(olds, storedOld{CertPEM: string(r.PEM), RetireAt: r.RetireAt.UTC().Format(time.RFC3339)})
		}
	}
	ob, _ := json.Marshal(olds)
	nb, _ := json.Marshal(storedCA{CertPEM: string(ca.PEM), KeyWrapped: base64.StdEncoding.EncodeToString(wrapped), Epoch: cs.epoch + 1})
	st := stateDB{m.app}
	// the old list first: a crash between the two leaves the previous root
	// both current and listed, which is harmless
	if err := st.put(stateCAOld, string(ob)); err != nil {
		return nil, time.Time{}, err
	}
	if err := st.put(stateCA, string(nb)); err != nil {
		return nil, time.Time{}, err
	}
	m.caMu.Lock()
	m.cas = nil
	m.caMu.Unlock()
	audit("devicecert.rotate_ca", Fingerprint(ca.Cert.Raw), map[string]any{
		"new": Fingerprint(ca.Cert.Raw), "old": Fingerprint(cs.cur.Cert.Raw), "overlap_until": retire.UTC().Format(time.RFC3339), "epoch": cs.epoch + 1,
	})
	return ca, retire, nil
}

// Issue implements [kernel.DeviceCertProvider]. Only the hub issues.
func (m *Module) Issue(ctx context.Context, req kernel.DeviceCertRequest) (*kernel.DeviceCert, error) {
	cs, err := m.loadCAs(true)
	if err != nil {
		return nil, err
	}
	ca := cs.cur
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
	scope := ""
	if kind == kernel.DeviceCertServer && req.Node != "" {
		if !validDNS(req.Node + NodeDNSSuffix) {
			return nil, fmt.Errorf("devicecert: node id %q is not usable as a DNS label", req.Node)
		}
		sans = append(append([]string{}, sans...), req.Node+NodeDNSSuffix)
	}
	if kind == kernel.DeviceCertClient {
		if scope, err = NormalizeScope(req.RouteScope); err != nil {
			return nil, err
		}
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
	if err := m.insertCert(serial, name, kind, req.Node, cert.NotAfter, scope); err != nil {
		return nil, fmt.Errorf("devicecert: failed to record the certificate: %w", err)
	}
	audit("devicecert.issue", serial, map[string]any{
		"name": name, "kind": string(kind), "node": req.Node, "not_after": cert.NotAfter.UTC().Format(time.RFC3339),
		"dns": dns, "ips": ipStrings(ips), "route_scope": scope,
	})
	return &kernel.DeviceCert{
		Serial: serial, Name: name, Kind: kind, Node: req.Node, NotAfter: cert.NotAfter,
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: cs.bundle(m.now()), RouteScope: scope,
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

// Revoke implements [kernel.DeviceCertProvider]. A serial revokes one
// certificate; a name revokes every unrevoked certificate of that name (a
// node's leaves, or all certificates issued to one LAN peer).
func (m *Module) Revoke(ctx context.Context, serialOrName string) error {
	rows, err := m.revokeTargets(serialOrName)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !r.GetDateTime("revoked_at").IsZero() {
			continue
		}
		r.Set("revoked_at", m.now().UTC().Format("2006-01-02 15:04:05.000Z"))
		if err := m.app.Save(r); err != nil {
			return err
		}
		audit("devicecert.revoke", r.GetString("serial"), map[string]any{"name": r.GetString("name"), "node": r.GetString("node"), "kind": r.GetString("kind")})
	}
	m.deny.reset()
	m.resetInfos()
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
	cs, err := m.loadCAs(true)
	if err != nil {
		return err
	}
	leaf, _, _ := m.leaf.current()
	// a rotated CA signs the hub leaf again at once
	if !m.leaf.due(m.now(), wanted) && leaf != nil && leaf.CheckSignatureFrom(cs.cur.Cert) == nil {
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
	return m.leaf.install(c.CertPEM, c.CAPEM, true, false)
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
	if ni := kernel.NodeIdentityOf(m.app); ni != nil && ni.NodeID() != "" {
		leaf, err := ParseCertPEM(certPEM)
		if err != nil {
			return err
		}
		if err := leaf.VerifyHostname(ni.NodeID() + NodeDNSSuffix); err != nil {
			return fmt.Errorf("devicecert: the leaf does not carry the node name %s%s", ni.NodeID(), NodeDNSSuffix)
		}
	}
	if err := m.leaf.install(certPEM, caPEM, true, true); err != nil {
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
	// DenyList is the number of revoked serials this node refuses.
	DenyList int `json:"deny_list"`
	// CAs is the number of roots trusted now (the signing root plus the
	// rotated-out ones inside their overlap).
	CAs int `json:"cas"`
	// HubKeyInDB is true on a hub that keeps its key in data.db next to the
	// wrapped CA key (no TOKI_SYNC_HUB_KEY_FILE): the wrapping then protects
	// nothing against a copy of data.db.
	HubKeyInDB bool `json:"hub_key_in_db,omitempty"`
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
	h.CAs = m.leaf.rootCount()
	if h.Role == "hub" {
		h.HubKeyInDB = os.Getenv("TOKI_SYNC_HUB_KEY_FILE") == ""
		if cs, _ := m.loadCAs(false); cs != nil {
			h.CAs = len(cs.roots(m.now()))
		}
	}
	if Enabled() {
		h.DenyList = m.deny.size()
	}
	return h
}

func leafInfo(c *x509.Certificate) *LeafInfo {
	return &LeafInfo{
		Serial: SerialHex(c.SerialNumber), NotBefore: c.NotBefore.UTC().Format(time.RFC3339),
		NotAfter: c.NotAfter.UTC().Format(time.RFC3339), DNS: c.DNSNames, IPs: ipStrings(c.IPAddresses),
	}
}
