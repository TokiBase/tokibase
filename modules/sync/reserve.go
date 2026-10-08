//go:build !no_sync

package sync

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/types"
)

// Reservations (docs/SYNC_DESIGN.md §2.8, §3.10, §7.5).
//
// A sequence (`_sync_sequences`) hands out disjoint ranges to nodes
// (`_sync_reservations`). A spoke keeps its ranges in `_sync_reserved` and
// takes values with Next() or through the `reserve:<sequence>` field type of
// the policy, which fills an empty field at create time in the same
// transaction as the record. When the ranges are used up the create fails
// closed (503 sync_reservation_exhausted); a number is never issued twice.
// The hub checks every pushed value against the ranges issued to the pushing
// node.

// Collections of the hub.
const (
	SequencesCollection    = "_sync_sequences"
	ReservationsCollection = "_sync_reservations"

	// AuditReserve is the audit action of range operations.
	AuditReserve = "sync.reserve"

	// ReservationTTL is how long a range stays advisory-valid (design §3.10
	// example: 90 days). The hub never reissues values, so an old range stays
	// valid for the pushes of the node that holds it.
	ReservationTTL = 90 * 24 * time.Hour

	// reservePrefix starts the policy field type.
	reservePrefix = "reserve:"
	// prefetchBelow is the remaining share that triggers a new reservation.
	prefetchBelow = 0.20

	defaultBlock       = 1000
	defaultMaxOpen     = 2
	defaultMaxBlock    = 10000
	retryAfterReserve  = "60"
	reserveBodyLimit   = 16 << 10
	maxReserveListSize = 200
)

// Range statuses.
const (
	RangeActive    = "active"
	RangeExhausted = "exhausted"
	RangeRetired   = "retired"
)

// Reserved ranges on a spoke.
const (
	localActive    = "active"
	localExhausted = "exhausted"
	localRetired   = "retired"
)

// ErrReservationExhausted is returned (wrapped by ReservationError) when a
// spoke has no value left in any range of the sequence.
var ErrReservationExhausted = errors.New("sync: reservation exhausted")

// ReservationError carries the sequence of an exhausted reservation.
type ReservationError struct{ Sequence string }

func (e *ReservationError) Error() string {
	return "sync: no reserved value left for sequence " + strconv.Quote(e.Sequence) + " (connect to the hub to get a new range)"
}

// Unwrap lets errors.Is match ErrReservationExhausted.
func (e *ReservationError) Unwrap() error { return ErrReservationExhausted }

// apiError is the HTTP form of the error: 503 with code sync_reservation_exhausted.
func (e *ReservationError) apiError() *router.ApiError {
	ae := router.NewApiError(http.StatusServiceUnavailable, "No reserved number left for "+e.Sequence+": connect to the hub to get a new range.", nil)
	ae.Data = map[string]any{"code": proto.CodeReservationExhausted, "sequence": e.Sequence}
	return ae
}

// ---- schema -----------------------------------------------------------------

// EnsureReserveCollections creates `_sync_sequences` and `_sync_reservations`
// (superusers only, rules null). It is idempotent.
func EnsureReserveCollections(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(SequencesCollection); c == nil {
		c = core.NewBaseCollection(SequencesCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 64, Pattern: `^[A-Za-z][A-Za-z0-9_]{0,63}$`},
			&core.NumberField{Name: "next"},
			&core.NumberField{Name: "block"},
			&core.NumberField{Name: "max_open_per_node"},
			&core.NumberField{Name: "max_block"},
			&core.TextField{Name: "format", Max: 200},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_sync_sequences_name", true, "[[name]]", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if c, _ := app.FindCollectionByNameOrId(ReservationsCollection); c == nil {
		c = core.NewBaseCollection(ReservationsCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "sequence", Required: true, Max: 64},
			&core.TextField{Name: "node", Required: true, Max: 64},
			&core.NumberField{Name: "start"},
			&core.NumberField{Name: "end"},
			&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{RangeActive, RangeExhausted, RangeRetired}},
			&core.NumberField{Name: "high_water"},
			&core.DateField{Name: "issued"},
			&core.DateField{Name: "expires"},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_sync_reservations_seq_node", false, "[[sequence]], [[node]], [[status]]", "")
		c.AddIndex("idx_sync_reservations_start", true, "[[sequence]], [[start]]", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}

// ---- hub: sequences ----------------------------------------------------------

func recInt(r *core.Record, name string, def int64) int64 {
	if v := int64(r.GetFloat(name)); v > 0 {
		return v
	}
	return def
}

// SequenceOptions are the inputs of CreateSequence (0 = default).
type SequenceOptions struct {
	Name     string
	Start    int64
	Block    int64
	MaxOpen  int64
	MaxBlock int64
	Format   string
	CLI      bool
}

// CreateSequence adds a sequence on the hub.
func CreateSequence(app core.App, o SequenceOptions) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId(SequencesCollection)
	if err != nil {
		return nil, errf("%s not found (is TOKI_SYNC_ROLE=hub?)", SequencesCollection)
	}
	if !reserveRe.MatchString(reservePrefix + o.Name) {
		return nil, errors.New("sequence name must start with a letter and contain letters, digits or '_' (max 64)")
	}
	if ex, _ := app.FindFirstRecordByFilter(col, "name={:n}", dbx.Params{"n": o.Name}); ex != nil {
		return nil, fmt.Errorf("sequence %q already exists", o.Name)
	}
	if o.Start <= 0 {
		o.Start = 1
	}
	if o.Block <= 0 {
		o.Block = defaultBlock
	}
	if o.MaxOpen <= 0 {
		o.MaxOpen = defaultMaxOpen
	}
	if o.MaxBlock <= 0 {
		o.MaxBlock = defaultMaxBlock
	}
	if o.Block > o.MaxBlock {
		return nil, errors.New("block must not exceed max-block")
	}
	r := core.NewRecord(col)
	r.Set("name", o.Name)
	r.Set("next", o.Start)
	r.Set("block", o.Block)
	r.Set("max_open_per_node", o.MaxOpen)
	r.Set("max_block", o.MaxBlock)
	r.Set("format", o.Format)
	if err := app.Save(r); err != nil {
		return nil, err
	}
	emit(AuditReserve, SequencesCollection, r.Id, map[string]any{"stage": "sequence_created", "name": o.Name, "next": o.Start, "block": o.Block, "cli": o.CLI})
	return r, nil
}

// reserveErr is a refused reservation request.
type reserveErr struct {
	status int
	code   string
	msg    string
}

func (e *reserveErr) Error() string { return e.code + ": " + e.msg }

func limitErr(msg string) *reserveErr {
	return &reserveErr{http.StatusTooManyRequests, proto.CodeReservationLimit, msg}
}

// applyUsed raises the high-water mark of the node's ranges from the usage the
// node reports (clamped to the range, never lowered).
func applyUsed(db dbx.Builder, nodeID, seq string, used map[string]int64) error {
	for id, v := range used {
		if v <= 0 {
			continue
		}
		if _, err := db.NewQuery(`UPDATE ` + ReservationsCollection + ` SET
  high_water=CASE WHEN {:v}>"end" THEN "end" WHEN {:v}>COALESCE(high_water,0) THEN {:v} ELSE COALESCE(high_water,0) END,
  status=CASE WHEN status='active' AND {:v}>="end" THEN 'exhausted' ELSE status END
  WHERE id={:id} AND node={:n} AND sequence={:s}`).
			Bind(dbx.Params{"v": v, "id": id, "n": nodeID, "s": seq}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// allocate issues a range of the sequence to a node.
func (m *Module) allocate(nodeID string, req proto.ReserveRequest) (*proto.ReserveResponse, error) {
	var out *proto.ReserveResponse
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		// first statement is a write: takes the SQLite write lock before any read
		if _, err := db.NewQuery("UPDATE " + SequencesCollection + " SET next=CASE WHEN COALESCE(next,0)<1 THEN 1 ELSE next END WHERE name={:n}").
			Bind(dbx.Params{"n": req.Sequence}).Execute(); err != nil {
			return err
		}
		seq, err := tx.FindFirstRecordByFilter(SequencesCollection, "name={:n}", dbx.Params{"n": req.Sequence})
		if err != nil || seq == nil {
			return &reserveErr{http.StatusNotFound, "sync_unknown_sequence", "unknown sequence"}
		}
		if err := applyUsed(db, nodeID, req.Sequence, req.Used); err != nil {
			return err
		}
		block, maxOpen, maxBlock := recInt(seq, "block", defaultBlock), recInt(seq, "max_open_per_node", defaultMaxOpen), recInt(seq, "max_block", defaultMaxBlock)
		count := req.Count
		if count <= 0 {
			count = block
		}
		if count > maxBlock {
			return limitErr(fmt.Sprintf("count %d exceeds max_block %d of the sequence", count, maxBlock))
		}
		var open int64
		if err := db.NewQuery("SELECT COUNT(*) FROM " + ReservationsCollection + " WHERE sequence={:s} AND node={:n} AND status='active'").
			Bind(dbx.Params{"s": req.Sequence, "n": nodeID}).Row(&open); err != nil {
			return err
		}
		if open >= maxOpen {
			return limitErr(fmt.Sprintf("the node already holds %d open ranges of this sequence (max_open_per_node %d)", open, maxOpen))
		}
		start := int64(seq.GetFloat("next"))
		end := start + count - 1
		if _, err := db.NewQuery("UPDATE " + SequencesCollection + " SET next={:e} WHERE id={:id}").
			Bind(dbx.Params{"e": end + 1, "id": seq.Id}).Execute(); err != nil {
			return err
		}
		rc, err := tx.FindCollectionByNameOrId(ReservationsCollection)
		if err != nil {
			return err
		}
		now := types.NowDateTime()
		exp := now.Add(ReservationTTL)
		r := core.NewRecord(rc)
		r.Set("sequence", req.Sequence)
		r.Set("node", nodeID)
		r.Set("start", start)
		r.Set("end", end)
		r.Set("status", RangeActive)
		r.Set("high_water", 0)
		r.Set("issued", now)
		r.Set("expires", exp)
		if err := tx.SaveNoValidate(r); err != nil {
			return err
		}
		out = &proto.ReserveResponse{
			Sequence: req.Sequence, Format: seq.GetString("format"),
			Ranges: []proto.ReserveRange{{ID: r.Id, Start: start, End: end, Expires: exp.Time().UTC().Format(proto.TimeLayout)}},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rg := out.Ranges[0]
	emit(AuditReserve, ReservationsCollection, rg.ID, map[string]any{"stage": "issued", "node": nodeID, "sequence": req.Sequence, "start": rg.Start, "end": rg.End})
	return out, nil
}

// ranges lists the ranges of a node (active only unless all).
func (m *Module) nodeRanges(db dbx.Builder, nodeID string, all bool) ([]proto.Reservation, error) {
	q := "SELECT id, sequence, start, \"end\", COALESCE(high_water,0) AS hw, COALESCE(expires,'') AS expires FROM " + ReservationsCollection + " WHERE node={:n}"
	if !all {
		q += " AND status='active'"
	}
	q += " ORDER BY sequence, start LIMIT " + strconv.Itoa(maxReserveListSize)
	var rows []struct {
		ID       string  `db:"id"`
		Sequence string  `db:"sequence"`
		Start    float64 `db:"start"`
		End      float64 `db:"end"`
		HW       float64 `db:"hw"`
		Expires  string  `db:"expires"`
	}
	if err := db.NewQuery(q).Bind(dbx.Params{"n": nodeID}).All(&rows); err != nil {
		return nil, err
	}
	out := make([]proto.Reservation, 0, len(rows))
	for _, r := range rows {
		rem := int64(r.End) - int64(math.Max(r.HW, r.Start-1))
		rg := proto.Reservation{ID: r.ID, Sequence: r.Sequence, Start: int64(r.Start), End: int64(r.End), RemainingHint: max(rem, 0)}
		if r.Expires != "" {
			if t, err := types.ParseDateTime(r.Expires); err == nil {
				rg.Expires = t.Time().UTC().Format(proto.TimeLayout)
			}
		}
		out = append(out, rg)
	}
	return out, nil
}

// handshakeReservations is the `reservations` section of the handshake.
func (m *Module) handshakeReservations(nodeID string) []proto.Reservation {
	if !m.app.HasTable(ReservationsCollection) {
		return []proto.Reservation{}
	}
	rs, err := m.nodeRanges(m.app.DB(), nodeID, false)
	if err != nil {
		m.app.Logger().Error("sync: failed to list the reservations", "error", err)
		return []proto.Reservation{}
	}
	return rs
}

// retireReservations retires every range of a node (revoked node). Pushes of
// the node are refused anyway; values are never reissued.
func retireReservations(app core.App, nodeID string) {
	if !app.HasTable(ReservationsCollection) {
		return
	}
	res, err := app.NonconcurrentDB().NewQuery("UPDATE " + ReservationsCollection + " SET status='retired' WHERE node={:n} AND status!='retired'").
		Bind(dbx.Params{"n": nodeID}).Execute()
	if err != nil {
		app.Logger().Error("sync: failed to retire the reservations of a revoked node", "node", nodeID, "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		emit(AuditReserve, ReservationsCollection, nodeID, map[string]any{"stage": "retired", "node": nodeID, "reason": "revoked", "ranges": n})
	}
}

// ReleaseRange retires one range by id (CLI and the node's own release). used
// is the highest value known to be used (0 = none). Values are never reissued.
func ReleaseRange(app core.App, id string, nodeID string, used int64, cli bool) error {
	var row struct {
		Node     string  `db:"node"`
		Sequence string  `db:"sequence"`
		Status   string  `db:"status"`
		End      float64 `db:"end"`
	}
	if err := app.DB().NewQuery("SELECT node, sequence, status, \"end\" FROM " + ReservationsCollection + " WHERE id={:id}").Bind(dbx.Params{"id": id}).One(&row); err != nil {
		if isNoRows(err) {
			return fmt.Errorf("range %q not found", id)
		}
		return err
	}
	if nodeID != "" && row.Node != nodeID {
		return fmt.Errorf("range %q not found", id)
	}
	db := app.NonconcurrentDB()
	if used > 0 {
		if err := applyUsed(db, row.Node, row.Sequence, map[string]int64{id: used}); err != nil {
			return err
		}
	}
	if _, err := db.NewQuery("UPDATE " + ReservationsCollection + " SET status='retired' WHERE id={:id}").Bind(dbx.Params{"id": id}).Execute(); err != nil {
		return err
	}
	emit(AuditReserve, ReservationsCollection, id, map[string]any{"stage": "released", "node": row.Node, "sequence": row.Sequence, "cli": cli})
	return nil
}

// ---- hub: routes --------------------------------------------------------------

func (m *Module) bindReserve() {
	if m.role == RoleHub {
		m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
			Id: hookId + "routes-reserve",
			Func: func(se *core.ServeEvent) error {
				g := se.Router
				g.POST(proto.PathReserve, m.reserveHandler).
					Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(reserveBodyLimit), rateTag("sync:reserve"), m.nodeAuth())
				g.GET(proto.PathReserve, m.reserveListHandler).
					Bind(apis.SkipSuccessActivityLog(), rateTag("sync:reserve"), m.nodeAuth())
				g.POST(proto.PathReserveRelease, m.reserveReleaseHandler).
					Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(reserveBodyLimit), rateTag("sync:reserve"), m.nodeAuth())
				return se.Next()
			},
		})
	}
	m.app.OnRecordCreate().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "reserve", Priority: -1 << 18, Func: m.autofill})
}

func (m *Module) reserveHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	var req proto.ReserveRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil || req.Sequence == "" || len(req.Sequence) > 64 || req.Count < 0 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	out, err := m.allocate(nodeID, req)
	if err != nil {
		var re *reserveErr
		if errors.As(err, &re) {
			if re.status == http.StatusTooManyRequests {
				e.Response.Header().Set("Retry-After", retryAfterReserve)
				emit(AuditReserve, ReservationsCollection, nodeID, map[string]any{"stage": "refused", "node": nodeID, "sequence": req.Sequence, "reason": re.msg})
			}
			return syncErr(e, re.status, re.code, re.msg, nil)
		}
		e.App.Logger().Error("sync: reserve failed", "node", nodeID, "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "reservation failed", nil)
	}
	return e.JSON(http.StatusOK, out)
}

func (m *Module) reserveListHandler(e *core.RequestEvent) error {
	rs, err := m.nodeRanges(e.App.DB(), NodeFrom(e), false)
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, proto.ReserveList{Ranges: rs})
}

func (m *Module) reserveReleaseHandler(e *core.RequestEvent) error {
	var req proto.ReleaseRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil || req.ID == "" || len(req.ID) > 64 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	if err := ReleaseRange(e.App, req.ID, NodeFrom(e), req.Used, false); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return syncErr(e, http.StatusNotFound, proto.CodeBadRequest, "range not found", nil)
		}
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ---- hub: push validation ------------------------------------------------------

// reserveFields lists the fields of a policy with a `reserve:<sequence>` type
// (field -> sequence).
func reserveFields(p *policy) map[string]string {
	var out map[string]string
	for f, t := range p.Types {
		if strings.HasPrefix(t, reservePrefix) {
			if out == nil {
				out = map[string]string{}
			}
			out[f] = strings.TrimPrefix(t, reservePrefix)
		}
	}
	return out
}

// reserveValue reads the integer in a reserve field value; ok is false for a
// value that is not a whole number, empty is true for "no value".
func reserveValue(v any) (n int64, empty, ok bool) {
	switch x := v.(type) {
	case nil:
		return 0, true, true
	case float64:
		if x == 0 {
			return 0, true, true
		}
		if x != math.Trunc(x) || x < 0 || x > 1<<53 {
			return 0, false, false
		}
		return int64(x), false, true
	case string:
		if x == "" {
			return 0, true, true
		}
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != x {
			return 0, false, false
		}
		return n, false, true
	}
	return 0, false, false
}

// checkReserved enforces design §3.10 on a pushed change: a value in a
// `reserve:` field must fall in a range issued to the pushing node (active or
// exhausted; a retired range counts up to its high-water mark). The
// high-water mark is raised in the apply transaction.
func (m *Module) checkReserved(tx kernel.App, nodeID string, p *policy, col *core.Collection, c *hubChange) *rejection {
	rf := reserveFields(p)
	if len(rf) == 0 || (c.Op != OpCreate && c.Op != OpUpdate) {
		return nil
	}
	db := tx.NonconcurrentDB()
	for _, name := range sortedKeys(rf) {
		raw, present := c.patch[name]
		if !present {
			continue
		}
		seq := rf[name]
		n, empty, ok := reserveValue(raw)
		if empty && ok {
			continue
		}
		bad := func(msg string) *rejection {
			rj := reject(proto.CodeReservationOutOfRange, msg)
			rj.change = c
			rj.conflict = &ConflictInfo{Kind: "reservation_out_of_range", Strategy: StratLWW, Resolution: ResolutionReverted, Status: ConflictResolved,
				Incoming: map[string]any{name: raw}, Note: msg}
			return rj
		}
		if !ok {
			return bad(fmt.Sprintf("field %s: %v is not a reserved number of sequence %s", name, raw, seq))
		}
		var rg struct {
			ID     string  `db:"id"`
			Status string  `db:"status"`
			End    float64 `db:"end"`
			HW     float64 `db:"hw"`
		}
		err := db.NewQuery("SELECT id, status, \"end\", COALESCE(high_water,0) AS hw FROM " + ReservationsCollection +
			" WHERE sequence={:s} AND node={:n} AND start<={:v} AND \"end\">={:v} LIMIT 1").
			Bind(dbx.Params{"s": seq, "n": nodeID, "v": n}).One(&rg)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return &rejection{code: CodeApplyError, msg: err.Error()}
			}
			return bad(fmt.Sprintf("field %s: %d is outside every range of sequence %s issued to this node", name, n, seq))
		}
		if rg.Status == RangeRetired && float64(n) > rg.HW {
			return bad(fmt.Sprintf("field %s: %d belongs to a retired range of sequence %s", name, n, seq))
		}
		if float64(n) > rg.HW {
			if _, err := db.NewQuery(`UPDATE ` + ReservationsCollection + ` SET high_water={:v},
  status=CASE WHEN status='active' AND {:v}>="end" THEN 'exhausted' ELSE status END WHERE id={:id}`).
				Bind(dbx.Params{"v": n, "id": rg.ID}).Execute(); err != nil {
				return &rejection{code: CodeApplyError, msg: err.Error()}
			}
		}
	}
	return nil
}

// ---- local use: Next() and autofill ----------------------------------------------

// Next returns the next reserved value of a sequence. On a spoke it takes the
// value from the local ranges (fail closed with *ReservationError when they
// are used up); on the hub it draws from the sequence itself. Inside a
// transaction the value is part of it, so a rolled back record gives it back.
func Next(app core.App, sequence string) (int64, error) {
	m := moduleOf(app)
	if m == nil {
		return 0, errors.New("sync: not enabled (TOKI_SYNC_ROLE)")
	}
	return m.Next(app, sequence)
}

// Next is the method form of the package function (embed API).
func (m *Module) Next(app core.App, sequence string) (int64, error) {
	if app == nil {
		app = m.app
	}
	var out int64
	err := app.RunInTransaction(func(tx kernel.App) error {
		var err error
		out, err = m.nextIn(tx, sequence)
		return err
	})
	return out, err
}

func (m *Module) nextIn(tx kernel.App, sequence string) (int64, error) {
	db := tx.NonconcurrentDB()
	if m.role == RoleHub {
		var n int64
		err := db.NewQuery("UPDATE " + SequencesCollection + " SET next=MAX(COALESCE(next,1),1)+1 WHERE name={:n} RETURNING CAST(next-1 AS INTEGER)").
			Bind(dbx.Params{"n": sequence}).Row(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sync: unknown sequence %q", sequence)
		}
		return n, err
	}
	var id string
	var n int64
	err := db.NewQuery(`UPDATE _sync_reserved SET next=next+1 WHERE id=(
  SELECT id FROM _sync_reserved WHERE sequence={:s} AND status='active' AND next<="end" ORDER BY start LIMIT 1)
  RETURNING id, next-1`).Bind(dbx.Params{"s": sequence}).Row(&id, &n)
	if errors.Is(err, sql.ErrNoRows) {
		m.wantReserve(sequence)
		return 0, &ReservationError{Sequence: sequence}
	}
	if err != nil {
		return 0, err
	}
	if _, err := db.NewQuery("UPDATE _sync_reserved SET status='exhausted' WHERE id={:id} AND next>\"end\"").Bind(dbx.Params{"id": id}).Execute(); err != nil {
		return 0, err
	}
	var remaining, capacity int64
	if err := db.NewQuery(`SELECT COALESCE(SUM("end"-next+1),0), COALESCE(MAX("end"-start+1),0) FROM _sync_reserved WHERE sequence={:s} AND status='active'`).
		Bind(dbx.Params{"s": sequence}).Row(&remaining, &capacity); err == nil && float64(remaining) < prefetchBelow*float64(capacity) {
		m.wantReserve(sequence)
	}
	return n, nil
}

// wantReserve asks the client loop for a new range (prefetch below 20 %, or
// when empty). Without a running loop it does nothing.
func (m *Module) wantReserve(sequence string) {
	if c := m.loop.Load(); c != nil {
		c.WantReserve(sequence)
	}
}

// autofill fills the empty reserve fields of a new record (before validation)
// in the transaction of the save.
func (m *Module) autofill(e *core.RecordEvent) error {
	if !m.ready.Load() || kernel.SyncOriginFrom(e.Context) != nil {
		return e.Next() // sync applies carry their own values
	}
	col := e.Record.Collection()
	p, err := m.pol.For(col)
	if err != nil || p == nil {
		return e.Next()
	}
	rf := reserveFields(p)
	if len(rf) == 0 {
		return e.Next()
	}
	var todo []string
	for _, name := range sortedKeys(rf) {
		n, empty, ok := reserveValue(e.Record.Get(name))
		if empty {
			todo = append(todo, name)
			continue
		}
		if m.role == RoleSpoke && !m.issuedLocally(e.App, rf[name], n, ok) {
			// the hub would refuse the value on push and revert the record: say so now
			return router.NewApiError(http.StatusBadRequest, "The value of "+name+" must be a number reserved for this node (leave it empty to get the next one).", nil)
		}
	}
	if len(todo) == 0 {
		return e.Next()
	}
	orig := e.App
	err = orig.RunInTransaction(func(tx kernel.App) error {
		for _, name := range todo {
			n, err := m.nextIn(tx, rf[name])
			if err != nil {
				return err
			}
			if f := col.Fields.GetByName(name); f != nil && f.Type() == kernel.FieldTypeNumber {
				e.Record.Set(name, n)
			} else {
				e.Record.Set(name, strconv.FormatInt(n, 10))
			}
		}
		e.App = tx
		return e.Next()
	})
	e.App = orig
	var re *ReservationError
	if errors.As(err, &re) {
		return re.apiError()
	}
	return err
}

// issuedLocally reports whether n was already handed out from a local range of
// the sequence (an explicit value is only valid if it is one of ours).
func (m *Module) issuedLocally(app kernel.App, seq string, n int64, ok bool) bool {
	if !ok {
		return false
	}
	var c int
	err := app.NonconcurrentDB().NewQuery(`SELECT COUNT(*) FROM _sync_reserved WHERE sequence={:s} AND start<={:n} AND next>{:n} AND status!='retired'`).
		Bind(dbx.Params{"s": seq, "n": n}).Row(&c)
	return err == nil && c > 0
}
