//go:build !no_sync

package sync

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	// CodeDevCertUnavailable is the error code when the hub has no devicecert module.
	CodeDevCertUnavailable = "sync_devcert_unavailable"

	// devCertMaxSANs bounds the SANs a node may ask for.
	devCertMaxSANs = 12
	// devCertNodeSuffix is the DNS suffix reserved for the node name.
	devCertNodeSuffix = ".edge.toki.local"
	// devCertRetry is the wait after a failed renewal.
	devCertRetry = 5 * time.Minute
	// devCertMinGap is the shortest time between two renewals.
	devCertMinGap = time.Minute
)

// devCertDNSSuffixes are the DNS suffixes a node may ask for besides its own
// <node_id>.edge.toki.local (private names that no public CA can claim).
var devCertDNSSuffixes = []string{".local", ".lan", ".home.arpa", ".internal"}

// bindDevCert mounts POST /api/sync/devcert on the hub and, on a spoke, starts
// the leaf renewal next to the sync loop (docs/modules/devicecert.md).
func (m *Module) bindDevCert() {
	switch m.role {
	case RoleHub:
		m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
			Id: hookId + "devcert",
			Func: func(se *core.ServeEvent) error {
				se.Router.POST(client.PathDevCert, m.devCertHandler).
					Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(16<<10), rateTag("sync:devcert"), m.nodeAuth())
				return se.Next()
			},
		})
	case RoleSpoke:
		var cancel context.CancelFunc
		m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
			Id: hookId + "devcert",
			Func: func(se *core.ServeEvent) error {
				var ctx context.Context
				ctx, cancel = context.WithCancel(context.Background())
				go m.devCertLoop(ctx)
				return se.Next()
			},
		})
		m.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
			Id: hookId + "devcert",
			Func: func(e *core.TerminateEvent) error {
				if cancel != nil {
					cancel()
				}
				return e.Next()
			},
		})
	}
}

// devCertMaxDNS bounds the DNS names a node may get besides its own.
const devCertMaxDNS = 4

// hubLocalIPs lists the non-loopback addresses of the hub's own interfaces: a
// node can not get a certificate for them (tests replace it).
var hubLocalIPs = func() map[string]bool {
	out := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// devCertSANs keeps the addresses and names a node may get in its leaf: IPs of
// the private ranges (RFC 1918, ULA, carrier-grade NAT) or loopback, never a
// public address, a link-local one, or an address of the hub itself; DNS names
// under private suffixes, at most [devCertMaxDNS], never the bare
// edge.toki.local or a name under it (those belong to nodes, and the hub adds
// the node's own).
func devCertSANs(sans []string) []string {
	hub := hubLocalIPs()
	out := make([]string, 0, len(sans))
	dns := 0
	for _, s := range sans {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || len(out) >= devCertMaxSANs {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if !(ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)) || hub[ip.String()] {
				continue
			}
			out = append(out, ip.String())
			continue
		}
		if s == strings.TrimPrefix(devCertNodeSuffix, ".") || strings.HasSuffix(s, devCertNodeSuffix) || dns >= devCertMaxDNS {
			continue
		}
		for _, suf := range devCertDNSSuffixes {
			if strings.HasSuffix(s, suf) && len(s) > len(suf) {
				out = append(out, s)
				dns++
				break
			}
		}
	}
	return out
}

// devCertHandler is POST /api/sync/devcert (node-authenticated): it certifies
// the public key the node sends, for the node's own name only.
func (m *Module) devCertHandler(e *core.RequestEvent) error {
	p := kernel.DeviceCertsOf(m.app)
	if p == nil {
		return syncErr(e, http.StatusNotImplemented, CodeDevCertUnavailable, "The hub does not issue device certificates (TOKI_DEVICECERT=on).", nil)
	}
	var req client.DevCertRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "Invalid JSON body.", nil)
	}
	spki, err := base64.StdEncoding.DecodeString(req.SPKI)
	if err != nil || len(spki) == 0 || len(spki) > 512 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "spki must be a base64 DER public key.", nil)
	}
	if _, err := x509.ParsePKIXPublicKey(spki); err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "spki is not a valid public key.", nil)
	}
	node := NodeFrom(e)
	c, err := p.Issue(e.Request.Context(), kernel.DeviceCertRequest{
		Name: node, Kind: kernel.DeviceCertServer, Node: node, SANs: devCertSANs(req.SANs), SPKI: spki,
	})
	if err != nil {
		e.App.Logger().Warn("sync: devcert issue failed", "node", node, "error", err)
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "The certificate could not be issued: "+err.Error(), nil)
	}
	return e.JSON(http.StatusOK, client.DevCertResponse{
		Serial: c.Serial, NotAfter: c.NotAfter.UTC().Format(time.RFC3339), CertPEM: string(c.CertPEM), CAPEM: string(c.CAPEM),
	})
}

// devCertLoop renews the edge leaf of a spoke: every poll interval it asks the
// devicecert provider whether a renewal is due (nothing happens when the
// module is off) and, if so, fetches the certificate from the hub through the
// session of the sync loop, so a handshake is run first when needed.
func (m *Module) devCertLoop(ctx context.Context) {
	t := time.NewTicker(client.DevCertPollInterval)
	defer t.Stop()
	var retryAt time.Time
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Now().Before(retryAt) {
			continue
		}
		err := m.RenewDevCert(ctx)
		switch {
		case err == nil:
			retryAt = time.Now().Add(devCertMinGap) // a skewed clock must not renew in a loop
			failures = 0
		case client.IsCode(err, proto.CodeNodeRevoked):
			m.app.Logger().Warn("sync: this node is revoked, the edge certificate is no longer renewed")
			return
		case client.IsCode(err, CodeDevCertUnavailable):
			if failures == 0 {
				m.app.Logger().Warn("sync: the hub does not issue device certificates (TOKI_DEVICECERT=on on the hub), retrying hourly")
			}
			failures++
			retryAt = time.Now().Add(time.Hour)
		default:
			failures++
			wait := min(devCertRetry<<min(failures-1, 4), time.Hour)
			if failures == 1 || failures%10 == 0 {
				m.app.Logger().Warn("sync: failed to renew the edge certificate", "error", err, "retry_in", wait.String())
			}
			retryAt = time.Now().Add(wait)
		}
	}
}

// RenewDevCert fetches a new edge leaf from the hub when the provider says
// one is due. It does nothing without a provider or a running loop.
func (m *Module) RenewDevCert(ctx context.Context) error {
	l := kernel.EdgeLeafOf(m.app)
	c := m.Client()
	if l == nil || c == nil {
		return nil
	}
	req, err := l.LeafRequest(m.now())
	if err != nil || req == nil {
		return err
	}
	res, err := c.DevCert(ctx, client.DevCertRequest{SPKI: base64.StdEncoding.EncodeToString(req.SPKI), SANs: req.SANs})
	if err != nil {
		return err
	}
	return l.InstallLeaf([]byte(res.CertPEM), []byte(res.CAPEM))
}
