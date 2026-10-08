package proto

import "encoding/json"

// PathSnapshot is POST /api/sync/snapshot (start) and GET /api/sync/snapshot
// (one page), docs/SYNC_DESIGN.md §3.9.
const PathSnapshot = "/api/sync/snapshot"

// CodeSnapshotExpired is the 410 code of a snapshot id that expired (24 h), was
// issued for another node or by another hub epoch: the client starts a new one.
const CodeSnapshotExpired = "sync_snapshot_expired"

// Snapshot limits.
const (
	// SnapshotMaxPage is the most records of one snapshot page.
	SnapshotMaxPage = 1000
)

// SnapshotCollection is one collection of a snapshot, in apply order.
type SnapshotCollection struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

// SnapshotStart is the 200 body of POST /api/sync/snapshot.
type SnapshotStart struct {
	SnapshotID string `json:"snapshot_id"`
	// StartSeq is the hub head when the snapshot started: the spoke pulls from it afterwards.
	StartSeq   int64  `json:"start_seq"`
	ServerTime string `json:"server_time"`
	Expires    string `json:"expires"`
	HubEpoch   string `json:"hub_epoch"`
	// Schema is the export of the collections of the snapshot (a spoke creates
	// the ones it lacks; schema bundles proper are PR8).
	Schema      []json.RawMessage    `json:"schema"`
	Policies    []Policy             `json:"policies"`
	Collections []SnapshotCollection `json:"collections"`
}

// SnapshotRecord is one record of a snapshot page.
type SnapshotRecord struct {
	ID string `json:"id"`
	// Data is the DB export of the synced fields the node may see.
	Data   map[string]any    `json:"data"`
	HLC    string            `json:"hlc"`
	Node   string            `json:"node"`
	Fields map[string]string `json:"fields,omitempty"`
	Hash   string            `json:"hash,omitempty"`
}

// SnapshotTombstone is a tombstone of a snapshot page.
type SnapshotTombstone struct {
	Record  string `json:"record"`
	Kind    string `json:"kind"` // delete | legal
	HLC     string `json:"hlc"`
	Node    string `json:"node"`
	Created string `json:"created,omitempty"`
}

// SnapshotPage is the 200 body of GET /api/sync/snapshot.
type SnapshotPage struct {
	Records    []SnapshotRecord    `json:"records"`
	Tombstones []SnapshotTombstone `json:"tombstones"`
	// Next is the last record id covered by this page ("" when the collection is empty).
	Next string `json:"next"`
	More bool   `json:"more"`
}
