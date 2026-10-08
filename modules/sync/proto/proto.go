// Package proto holds the wire types and the identity/crypto helpers shared by
// the sync hub (modules/sync) and the spoke client (modules/sync/client).
// Stdlib crypto plus the JWT library already used by the project.
package proto

// Routes under the hub.
const (
	PathEnroll    = "/api/sync/enroll"
	PathHandshake = "/api/sync/handshake"
	PathPing      = "/api/sync/ping"
)

// Handshake request headers.
const (
	HeaderNode  = "X-Toki-Node"
	HeaderSigTs = "X-Toki-Sig-Ts"
	HeaderNonce = "X-Toki-Sig-Nonce"
	HeaderSig   = "X-Toki-Sig"
)

// Error codes (docs/SYNC_DESIGN.md §3.12 plus the enroll code of §3.2).
const (
	CodeBadRequest     = "sync_bad_request"
	CodeUnauthorized   = "sync_unauthorized"
	CodeNodeRevoked    = "sync_node_revoked"
	CodeEnrollInvalid  = "sync_enroll_invalid"
	CodeRateLimited    = "sync_rate_limited"
	CodeHubUnavailable = "sync_hub_unavailable"
)

// TimeLayout is the wire format of times: RFC3339 UTC with milliseconds.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// SessionTokenType is the `typ` claim of a node session token.
const SessionTokenType = "toki_sync"

// EnrollRequest is the body of POST /api/sync/enroll.
type EnrollRequest struct {
	Code       string `json:"code"`
	Ed25519Pub string `json:"ed25519_pub"`
	X25519Pub  string `json:"x25519_pub"`
	Name       string `json:"name"`
	Profile    string `json:"profile"`
	AppVersion string `json:"app_version"`
}

// EnrollResponse is the 200 body of POST /api/sync/enroll.
type EnrollResponse struct {
	NodeID string `json:"node_id"`
	HubID  string `json:"hub_id"`
	HubURL string `json:"hub_url"`
	Cert   string `json:"cert"`
	HubPub string `json:"hub_pub"`
}

// HandshakeRequest is the body of POST /api/sync/handshake.
type HandshakeRequest struct {
	NodeID        string   `json:"node_id"`
	Cert          string   `json:"cert"`
	ClientTime    string   `json:"client_time"`
	SchemaVersion int64    `json:"schema_version"`
	PullAfter     int64    `json:"pull_after"`
	AckedOrigin   int64    `json:"acked_origin"`
	NextOrigin    int64    `json:"next_origin"`
	HubEpoch      string   `json:"hub_epoch"`
	Profile       string   `json:"profile"`
	AppVersion    string   `json:"app_version"`
	Caps          []string `json:"caps"`
}

// Clock is the clock verdict of the hub (docs/SYNC_DESIGN.md §3.7). In PR2 Ok
// is computed but not enforced.
type Clock struct {
	Ok         bool  `json:"ok"`
	OffsetMs   int64 `json:"offset_ms"`
	MaxDriftMs int64 `json:"max_drift_ms"`
}

// Schema is the schema section of the handshake (bundles arrive with PR8).
type Schema struct {
	Version int64 `json:"version"`
	Bundles []any `json:"bundles"`
}

// Policy is the minimal policy view shipped in the handshake.
type Policy struct {
	Collection string            `json:"collection"`
	Direction  string            `json:"direction"`
	Strategy   string            `json:"strategy"`
	Partition  string            `json:"partition"`
	FieldTypes map[string]string `json:"field_types"`
	Exclude    []string          `json:"exclude"`
	Crypto     string            `json:"crypto"`
}

// HandshakeResponse is the 200 body of POST /api/sync/handshake.
type HandshakeResponse struct {
	SessionToken string         `json:"session_token"`
	Cert         string         `json:"cert,omitempty"` // renewed device certificate (< CertRenewBefore left)
	Expires      string         `json:"expires"`
	HubID        string         `json:"hub_id"`
	HubEpoch     string         `json:"hub_epoch"`
	ServerTime   string         `json:"server_time"`
	Clock        Clock          `json:"clock"`
	Schema       Schema         `json:"schema"`
	Policies     []Policy       `json:"policies"`
	Params       map[string]any `json:"params"`
	Keys         []any          `json:"keys"`
	PushFrom     int64          `json:"push_from"`
	LowWater     int64          `json:"low_water"`
	Rebootstrap  bool           `json:"rebootstrap"`
	Reservations []any          `json:"reservations"`
	PollMs       int64          `json:"poll_ms"`
}

// PingResponse is the 200 body of GET /api/sync/ping.
type PingResponse struct {
	NodeID     string `json:"node_id"`
	ServerTime string `json:"server_time"`
}

// ErrorBody is the PocketBase shaped error body; Data carries `code`.
type ErrorBody struct {
	Status  int            `json:"status"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}
