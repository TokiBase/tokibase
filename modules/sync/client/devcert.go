//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PathDevCert is the hub endpoint that issues the edge server certificate
// (docs/modules/devicecert.md).
const PathDevCert = "/api/sync/devcert"

// DevCertRequest asks the hub to certify the node's local leaf key.
type DevCertRequest struct {
	// SPKI is the DER SubjectPublicKeyInfo (ECDSA P-256), base64.
	SPKI string `json:"spki"`
	// SANs are the LAN addresses and extra names the node wants in the leaf.
	SANs []string `json:"sans,omitempty"`
}

// DevCertResponse is the issued leaf and the root that signed it.
type DevCertResponse struct {
	Serial   string `json:"serial"`
	NotAfter string `json:"not_after"`
	CertPEM  string `json:"cert_pem"`
	CAPEM    string `json:"ca_pem"`
}

// DevCert asks the hub for the edge server certificate of this node. The
// session handshake runs first when there is no valid session.
func (c *Client) DevCert(ctx context.Context, req DevCertRequest) (*DevCertResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b, err := c.authed(ctx, http.MethodPost, PathDevCert, nil, body)
	if err != nil {
		return nil, err
	}
	var out DevCertResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("sync: invalid devcert answer: %w", err)
	}
	if out.CertPEM == "" || out.CAPEM == "" {
		return nil, fmt.Errorf("sync: the hub answered without a certificate")
	}
	return &out, nil
}

// DevCertPollInterval is how often a spoke checks whether its edge leaf is due
// (a local check; the hub is only asked when it is).
var DevCertPollInterval = 10 * time.Second
