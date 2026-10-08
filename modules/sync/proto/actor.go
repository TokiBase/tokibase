package proto

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Actor grant constants (docs/SYNC_DESIGN.md §1.6, §3.11).
const (
	// PathActor is POST /api/sync/actor; DELETE /api/sync/actor/{aid} revokes.
	PathActor = "/api/sync/actor"
	// HeaderActorToken carries the hub auth token of the user.
	HeaderActorToken = "X-Toki-Actor-Token"
	// HeaderSyncNode is the header of the synthetic replay request; rules read it
	// as @request.headers.x_toki_sync_node.
	HeaderSyncNode = "X-Toki-Sync-Node"
	// ActorAssertionType is the `typ` claim of a grant assertion.
	ActorAssertionType = "toki_sync_actor"
	// CodeActorInvalid answers a user token that fails validation.
	CodeActorInvalid = "sync_actor_invalid"
)

// Per-change actor codes (docs/SYNC_DESIGN.md §1.6).
const (
	CodeActorUnknown      = "actor_unknown"
	CodeActorNodeMismatch = "actor_node_mismatch"
	CodeActorExpired      = "actor_expired"
	CodeActorRevoked      = "actor_revoked"
	CodeActorForbidden    = "actor_forbidden"
)

// ResParked is the push status of a change that waits for an admin (actor
// revoked). It counts as final for the ack.
const ResParked = "parked"

// ActorResponse is the 200 body of POST /api/sync/actor.
type ActorResponse struct {
	AID       string `json:"aid"`
	Exp       string `json:"exp"`
	Assertion string `json:"assertion"`
	// Record is the user's own record without password and tokenKey.
	Record map[string]any `json:"record"`
}

// ActorClaims are the claims of a grant assertion: iss=hub id, sub=aid,
// node=the device the grant is bound to, col/rec=the user record (collection
// id and record id), sid=the hub session of the user token.
type ActorClaims struct {
	jwt.RegisteredClaims
	Typ  string `json:"typ"`
	Node string `json:"node"`
	Col  string `json:"col"`
	Rec  string `json:"rec"`
	Sid  string `json:"sid,omitempty"`
}

// SignActor signs a grant assertion (compact JWS, EdDSA).
func SignActor(hubPriv ed25519.PrivateKey, c *ActorClaims) (string, error) {
	c.Typ = ActorAssertionType
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(hubPriv)
}

// VerifyActor checks the signature, issuer, type and expiry of an assertion.
func VerifyActor(hubPub ed25519.PublicKey, token string, now time.Time) (*ActorClaims, error) {
	if len(hubPub) != ed25519.PublicKeySize {
		return nil, errors.New("bad hub key")
	}
	c := &ActorClaims{}
	_, err := jwt.ParseWithClaims(token, c, func(*jwt.Token) (any, error) { return hubPub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(HubID(hubPub)),
	)
	if err != nil {
		return nil, err
	}
	if c.Typ != ActorAssertionType || c.Subject == "" || c.Node == "" || c.Col == "" || c.Rec == "" {
		return nil, errors.New("invalid actor assertion")
	}
	return c, nil
}
