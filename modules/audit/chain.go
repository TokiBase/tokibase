//go:build !no_audit

// Package audit implements an append-only, hash-chained audit log of
// privileged and schema-changing actions (see docs/modules/audit.md).
package audit

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/types"
)

// TableName is the audit table (lives in auxiliary.db next to _logs).
const TableName = "_audit"

// Actor kinds.
const (
	ActorSuperuser = "superuser"
	ActorUser      = "user"
	ActorAgent     = "agent"
	ActorSystem    = "system"
)

// Actions.
const (
	ActionRecordCreate     = "record.create"
	ActionRecordUpdate     = "record.update"
	ActionRecordDelete     = "record.delete"
	ActionCollectionCreate = "collection.create"
	ActionCollectionUpdate = "collection.update"
	ActionCollectionDelete = "collection.delete"
	ActionSettingsUpdate   = "settings.update"
	ActionAuthImpersonate  = "auth.impersonate"
	ActionBackupCreate     = "backup.create"
	ActionBackupRestore    = "backup.restore"
	maxJSONBytes           = 64 << 10
	truncatedMarker        = "__audit_truncated__"
	truncatedPreviewBytes  = 1024
	createTableSQL         = `CREATE TABLE IF NOT EXISTS {{_audit}} (
		[[id]]               TEXT PRIMARY KEY NOT NULL,
		[[created]]          TEXT NOT NULL,
		[[seq]]              INTEGER NOT NULL,
		[[actor_kind]]       TEXT NOT NULL,
		[[actor_id]]         TEXT NOT NULL DEFAULT '',
		[[actor_collection]] TEXT NOT NULL DEFAULT '',
		[[impersonated_by]]  TEXT,
		[[action]]           TEXT NOT NULL,
		[[collection]]       TEXT NOT NULL DEFAULT '',
		[[record]]           TEXT NOT NULL DEFAULT '',
		[[before]]           JSON,
		[[after]]            JSON,
		[[diff]]             JSON,
		[[request]]          JSON,
		[[prev_hash]]        TEXT NOT NULL DEFAULT '',
		[[hash]]             TEXT NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_seq ON {{_audit}} ([[seq]]);
	CREATE INDEX IF NOT EXISTS idx_audit_created ON {{_audit}} ([[created]]);`
)

// Entry is a single audit row. JSON columns are kept as raw JSON text.
type Entry struct {
	Id              string  `db:"id"`
	Created         string  `db:"created"`
	Seq             int64   `db:"seq"`
	ActorKind       string  `db:"actor_kind"`
	ActorID         string  `db:"actor_id"`
	ActorCollection string  `db:"actor_collection"`
	ImpersonatedBy  *string `db:"impersonated_by"`
	Action          string  `db:"action"`
	Collection      string  `db:"collection"`
	Record          string  `db:"record"`
	Before          *string `db:"before"`
	After           *string `db:"after"`
	Diff            *string `db:"diff"`
	Request         *string `db:"request"`
	PrevHash        string  `db:"prev_hash"`
	Hash            string  `db:"hash"`
}

// MarshalJSON emits the JSON columns as nested JSON instead of strings.
func (e Entry) MarshalJSON() ([]byte, error) {
	raw := func(s *string) any {
		if s == nil || *s == "" {
			return nil
		}
		return json.RawMessage(*s)
	}
	return json.Marshal(map[string]any{
		"id": e.Id, "created": e.Created, "seq": e.Seq,
		"actor_kind": e.ActorKind, "actor_id": e.ActorID, "actor_collection": e.ActorCollection,
		"impersonated_by": e.ImpersonatedBy,
		"action":          e.Action, "collection": e.Collection, "record": e.Record,
		"before": raw(e.Before), "after": raw(e.After), "diff": raw(e.Diff), "request": raw(e.Request),
		"prev_hash": e.PrevHash, "hash": e.Hash,
	})
}

// canon re-encodes raw JSON with sorted keys, no HTML escaping and
// numbers kept verbatim, so the hash does not depend on storage formatting.
func canon(raw *string) json.RawMessage {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return json.RawMessage("null")
	}
	dec := json.NewDecoder(strings.NewReader(*raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return json.RawMessage(fmt.Sprintf("%q", *raw))
	}
	return encodeCompact(v)
}

func encodeCompact(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// ComputeHash returns sha256 (hex) over the canonical JSON of the entry
// fields covered by the chain (diff is derived data and not covered).
func ComputeHash(e *Entry) string {
	payload := struct {
		Seq             int64           `json:"seq"`
		Created         string          `json:"created"`
		ActorKind       string          `json:"actor_kind"`
		ActorID         string          `json:"actor_id"`
		ActorCollection string          `json:"actor_collection"`
		ImpersonatedBy  *string         `json:"impersonated_by"`
		Action          string          `json:"action"`
		Collection      string          `json:"collection"`
		Record          string          `json:"record"`
		Before          json.RawMessage `json:"before"`
		After           json.RawMessage `json:"after"`
		Request         json.RawMessage `json:"request"`
		PrevHash        string          `json:"prev_hash"`
	}{
		e.Seq, e.Created, e.ActorKind, e.ActorID, e.ActorCollection, e.ImpersonatedBy,
		e.Action, e.Collection, e.Record, canon(e.Before), canon(e.After), canon(e.Request), e.PrevHash,
	}
	return security.SHA256(string(encodeCompact(payload)))
}

// Log is the chain writer. Create it with [New].
type Log struct {
	app core.App

	mu       sync.Mutex
	ready    bool
	lastSeq  int64
	lastHash string
}

// New returns a chain writer bound to app.
func New(app core.App) *Log { return &Log{app: app} }

func (l *Log) ensureTable() error {
	_, err := l.app.AuxDB().NewQuery(createTableSQL).Execute()
	return err
}

// load creates the table if missing and caches the last chain row.
// Caller must hold l.mu.
func (l *Log) load() error {
	if l.ready {
		return nil
	}
	if err := l.ensureTable(); err != nil {
		return err
	}
	return l.reloadLast()
}

func (l *Log) reloadLast() error {
	var last Entry
	err := l.app.AuxDB().Select("seq", "hash").From(TableName).OrderBy("seq DESC").Limit(1).One(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil {
		l.lastSeq, l.lastHash = 0, ""
	} else {
		l.lastSeq, l.lastHash = last.Seq, last.Hash
	}
	l.ready = true
	return nil
}

// Init prepares the table and the in-memory chain head (called at bootstrap).
func (l *Log) Init() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.load()
}

// Append seals e (id, created, seq, prev_hash, hash) and inserts it.
// On failure the chain head is not advanced.
func (l *Log) Append(e *Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.load(); err != nil {
		return err
	}

	err := l.insert(e)
	if err != nil {
		// the head may be stale (e.g. restored aux.db): re-read once and retry
		if rerr := l.reloadLast(); rerr != nil {
			return errors.Join(err, rerr)
		}
		err = l.insert(e)
	}
	return err
}

func (l *Log) insert(e *Entry) error {
	if e.Id == "" {
		e.Id = security.RandomStringWithAlphabet(15, "abcdefghijklmnopqrstuvwxyz0123456789")
	}
	e.Created = types.NowDateTime().String()
	e.Seq = l.lastSeq + 1
	e.PrevHash = l.lastHash
	e.Hash = ComputeHash(e)

	_, err := l.app.AuxDB().Insert(TableName, dbx.Params{
		"id": e.Id, "created": e.Created, "seq": e.Seq,
		"actor_kind": e.ActorKind, "actor_id": e.ActorID, "actor_collection": e.ActorCollection,
		"impersonated_by": e.ImpersonatedBy,
		"action":          e.Action, "collection": e.Collection, "record": e.Record,
		"before": e.Before, "after": e.After, "diff": e.Diff, "request": e.Request,
		"prev_hash": e.PrevHash, "hash": e.Hash,
	}).Execute()
	if err != nil {
		return err
	}
	l.lastSeq, l.lastHash = e.Seq, e.Hash
	return nil
}

// VerifyResult is the outcome of [Verify].
type VerifyResult struct {
	Rows      int64  `json:"rows"`
	OK        bool   `json:"ok"`
	BrokenSeq int64  `json:"broken_seq,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Verify walks the chain in seq order and recomputes every hash.
// A missing table is an empty (valid) chain.
func Verify(app core.App) (VerifyResult, error) {
	res := VerifyResult{OK: true}
	if !app.AuxHasTable(TableName) {
		return res, nil
	}

	rows, err := app.AuxDB().Select("*").From(TableName).OrderBy("seq ASC").Rows()
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var prevSeq int64
	var prevHash string
	for rows.Next() {
		var e Entry
		if err := rows.ScanStruct(&e); err != nil {
			return res, err
		}
		res.Rows++

		fail := func(reason string) (VerifyResult, error) {
			res.OK, res.BrokenSeq, res.Reason = false, e.Seq, reason
			return res, nil
		}

		if e.Seq != prevSeq+1 {
			return fail(fmt.Sprintf("seq gap: expected %d, found %d", prevSeq+1, e.Seq))
		}
		if e.PrevHash != prevHash {
			return fail("prev_hash does not match the previous row hash")
		}
		if ComputeHash(&e) != e.Hash {
			return fail("hash mismatch (row content was modified)")
		}
		prevSeq, prevHash = e.Seq, e.Hash
	}
	return res, rows.Err()
}

func dbxGTE(col, val string) dbx.Expression {
	return dbx.NewExp("[["+col+"]] >= {:v}", dbx.Params{"v": val})
}
