package proto

import "encoding/json"

// Routes of PR3.
const (
	PathPush = "/api/sync/push"
	PathPull = "/api/sync/pull"
	PathAck  = "/api/sync/ack"
	// PathPurge is POST /api/sync/purge (superusers; legal erasure of one record).
	PathPurge = "/api/sync/purge"
)

// Topic is the realtime topic on which the hub pokes online spokes. The
// payload is {"seq": N} and carries no record data.
const Topic = "@sync"

// More error codes (docs/SYNC_DESIGN.md §3.12).
const (
	CodeBatchTooLarge = "sync_batch_too_large"
	CodePushGap       = "sync_push_gap"
	CodeRebootstrap   = "sync_rebootstrap_required"
	CodeSchemaBehind  = "sync_schema_behind"
	CodeClockDrift    = "sync_clock_drift"
	// CodeResponseTooLarge is raised by the client when a response is cut at
	// its read limit; the pull page is then halved.
	CodeResponseTooLarge = "sync_response_too_large"
)

// Limits of one push request.
const (
	MaxPushChanges = 500
	MaxPushBytes   = 8 << 20
	// MaxWait is the longest long-poll of /pull.
	MaxWait = 25
)

// Statuses of a push result.
const (
	ResApplied    = "applied"
	ResMerged     = "merged"
	ResSuperseded = "superseded"
	ResDuplicate  = "duplicate"
	ResRejected   = "rejected"
)

// Per-change result codes (docs/SYNC_DESIGN.md §3.12). The hub of PR3 uses a
// subset.
const (
	CodeRuleDenied       = "rule_denied"
	CodeValidationFailed = "validation_failed"
	CodeUniqueViolation  = "unique_violation"
	CodeTombstoned       = "tombstoned"
	CodeLegalTombstone   = "legal_tombstone"
	CodePolicyDirection  = "policy_direction"
	CodePolicyPartition  = "policy_partition"
	CodeFutureHLC        = "future_hlc"
	CodeSuperseded       = "superseded"
	CodeOrphaned         = "orphaned"
	CodeHubWins          = "hub_wins"
	CodeHookRejected     = "hook_rejected"
	CodeHookFailed       = "hook_failed"
)

// PushChange is one change of a push request.
type PushChange struct {
	ID         string          `json:"id"` // "<node>:<origin_seq>"
	HLC        string          `json:"hlc"`
	Base       string          `json:"base"`
	Collection string          `json:"collection"`
	Record     string          `json:"record"`
	Op         string          `json:"op"`
	Patch      json.RawMessage `json:"patch"`
	Hash       string          `json:"hash,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	Tx         string          `json:"tx,omitempty"`
	SV         int64           `json:"sv"`
}

// PushRequest is the body of POST /api/sync/push.
type PushRequest struct {
	Batch         string       `json:"batch"`
	SchemaVersion int64        `json:"schema_version"`
	ClientTime    string       `json:"client_time"`
	Changes       []PushChange `json:"changes"`
}

// PushResult is the outcome of one pushed change.
type PushResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	HubSeq int64  `json:"hub_seq,omitempty"`
	Hash   string `json:"hash,omitempty"`
	// Was is the original status of a duplicate (applied, merged, rejected, ...).
	Was string `json:"was,omitempty"`
}

// PushResponse is the 200 body of POST /api/sync/push.
type PushResponse struct {
	ServerTime   string       `json:"server_time"`
	AckedThrough int64        `json:"acked_through"`
	Results      []PushResult `json:"results"`
}

// Pull notices.
const (
	// NoticeParked: the hub parked the node's change of this record for review.
	NoticeParked = "parked"
	// NoticeInvisible: the node's change was reverted, but the record is outside
	// the view rule of its actor; the hub sends no data and the node keeps its copy.
	NoticeInvisible = "invisible"
)

// Codes of a parked change settled by an operator (`toki sync conflicts --resolve`).
const (
	CodeParkResolved = "park_resolved" // --take hub: rejected, the node gets a revert
	CodeParkAccepted = "park_accepted" // --take incoming|patch: applied as a hub write
)

// CodeParkExpired is the code of a parked change that nobody resolved within
// TOKI_SYNC_PARK_TTL and was rejected.
const CodeParkExpired = "park_expired"

// PullChange is one change of a pull page.
type PullChange struct {
	Seq        int64           `json:"seq"`
	ID         string          `json:"id"`
	Node       string          `json:"node"`
	HLC        string          `json:"hlc"`
	Collection string          `json:"collection"`
	Record     string          `json:"record"`
	Op         string          `json:"op"`
	Patch      json.RawMessage `json:"patch,omitempty"`
	Hash       string          `json:"hash,omitempty"`
	Revert     bool            `json:"revert,omitempty"`
	Evict      bool            `json:"evict,omitempty"`
	// Notice marks an informational row without data (NoticeParked,
	// NoticeInvisible): the node keeps its local value and marks the record.
	Notice string `json:"notice,omitempty"`
	// Code is the reason of a notice (e.g. actor_revoked).
	Code   string            `json:"code,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// PullResponse is the 200 body of GET /api/sync/pull.
type PullResponse struct {
	ServerTime    string       `json:"server_time"`
	SchemaVersion int64        `json:"schema_version"`
	LowWater      int64        `json:"low_water"`
	Changes       []PullChange `json:"changes"`
	Next          int64        `json:"next"`
	More          bool         `json:"more"`
}

// AckRequest is the body of POST /api/sync/ack.
type AckRequest struct {
	PulledThrough int64             `json:"pulled_through"`
	Digest        map[string]string `json:"digest,omitempty"`
}

// AckResponse is the 200 body of POST /api/sync/ack.
type AckResponse struct {
	OK bool `json:"ok"`
	// DigestChecked is false when the node was behind the hub head, so a
	// comparison would only measure the lag.
	DigestChecked  bool     `json:"digest_checked"`
	DigestMismatch []string `json:"digest_mismatch"`
}
