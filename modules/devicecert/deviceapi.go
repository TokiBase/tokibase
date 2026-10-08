//go:build !no_devicecert

package devicecert

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

const (
	attestPrefix = "toki-attest/v1"
	// MinNonce and MaxNonce bound the nonce of POST /api/device/attest (bytes).
	MinNonce = 16
	MaxNonce = 128
	// AttestSkew is how far the ts of an attest request may be from the node clock.
	AttestSkew = 5 * 60
)

// AttestMessage is the signed message: "toki-attest/v1|<node>|<nonce>|<ts>".
func AttestMessage(node, nonce string, ts int64) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s|%d", attestPrefix, node, nonce, ts))
}

func (m *Module) handleIdentity(e *core.RequestEvent) error {
	ni := kernel.NodeIdentityOf(m.app)
	if ni == nil {
		return e.JSON(http.StatusNotImplemented, map[string]any{"status": 501, "message": "This process has no node identity (sync is off).", "data": map[string]any{}})
	}
	if !m.deviceThr.Allow("i:" + e.RealIP()) {
		return e.TooManyRequestsError("Too many requests.", nil)
	}
	fp := ""
	if leaf, _, _ := m.leaf.current(); leaf != nil {
		fp = Fingerprint(leaf.Raw)
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"node_id": ni.NodeID(), "hub_id": ni.HubID(), "leaf_fp": fp}
	if e.Auth != nil {
		// the node cert JWS carries the partition params (tenant/site ids):
		// only an authenticated caller gets it
		out["cert"] = ni.Cert()
	}
	return e.JSON(http.StatusOK, out)
}

// handleAttest signs a caller-chosen nonce with the node key, so the app can
// verify (against the hub public key it trusts, through the JWS node cert)
// which enrolled device answered. No key material leaves the process.
func (m *Module) handleAttest(e *core.RequestEvent) error {
	ni := kernel.NodeIdentityOf(m.app)
	if ni == nil {
		return e.JSON(http.StatusNotImplemented, map[string]any{"status": 501, "message": "This process has no node identity (sync is off).", "data": map[string]any{}})
	}
	if !m.deviceThr.Allow("a:" + e.RealIP()) {
		return e.TooManyRequestsError("Too many attest requests, slow down.", nil)
	}
	var body struct {
		Nonce string `json:"nonce"`
		TS    *int64 `json:"ts"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid JSON body.", nil)
	}
	if len(body.Nonce) < MinNonce || len(body.Nonce) > MaxNonce {
		return e.BadRequestError(fmt.Sprintf("nonce must be %d to %d bytes.", MinNonce, MaxNonce), nil)
	}
	now := m.now().Unix()
	ts := now
	if body.TS != nil {
		ts = *body.TS
		if d := ts - now; d > AttestSkew || d < -AttestSkew {
			return e.BadRequestError("ts is more than 5 minutes away from the node clock.", nil)
		}
	}
	sig, err := ni.Sign(AttestMessage(ni.NodeID(), body.Nonce, ts))
	if err != nil {
		return e.JSON(http.StatusServiceUnavailable, map[string]any{"status": 503, "message": "The node key is not available yet.", "data": map[string]any{}})
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	out := map[string]any{
		"node_id": ni.NodeID(), "hub_id": ni.HubID(), "nonce": body.Nonce, "ts": ts, "alg": "Ed25519",
		"sig": base64.StdEncoding.EncodeToString(sig),
	}
	if e.Auth != nil { // the cert (with the partition params) only for an authenticated caller
		out["cert"] = ni.Cert()
	}
	return e.JSON(http.StatusOK, out)
}
