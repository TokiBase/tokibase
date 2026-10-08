//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Snapshot bootstrap, spoke side (docs/SYNC_DESIGN.md §3.9).
//
// The position is kept in `_sync_cursors` (snapshot_id, snapshot_after) and
// `_sync_state` (snapshot_start, snapshot_cols), and moves in the same
// transaction as the records of a page, so a crash or a lost connection
// resumes at the next page. snapshot_after is "<collection id>/<last record id>"
// while pages are applied, then one of the phase markers below:
//
//	!ack     all pages applied, pull_after = start_seq: the hub is told (it reactivates a stale node)
//	!pull    acked, pulling the log from start_seq
//	!rebase  pulled, now the unpushed local changes are rebased onto the new state
//
// A local change that was not pushed yet is never lost: before the first page it
// is parked (status `rebase`, same row, same origin_seq), after the pull it is
// replayed on the new data as a fresh local change (new HLC) and the parked row
// turns into a `rebased` filler that keeps the origin_seq sequence of the hub
// contiguous.

// Cursor states (`_sync_cursors.state`).
const (
	StateIdle                = "idle"
	StateBootstrapping       = "bootstrapping"
	StateRebootstrapRequired = "rebootstrap_required"
	StatePaused              = "paused"
)

// Env of the bootstrap.
const (
	EnvAutoHeal       = "TOKI_SYNC_AUTO_HEAL"
	EnvRetention      = "TOKI_SYNC_RETENTION"
	EnvSnapshotPage   = "TOKI_SYNC_SNAPSHOT_PAGE"
	EnvDigestInterval = "TOKI_SYNC_DIGEST_INTERVAL"

	// DefaultRetention is the default of TOKI_SYNC_RETENTION.
	DefaultRetention = 90 * 24 * time.Hour
	// DefaultDigestInterval is the default time between two auto-heal digest checks.
	DefaultDigestInterval = 10 * time.Minute
)

// Local change statuses used by the bootstrap.
const (
	// StatusRebase marks a local change parked while a snapshot replaces the data.
	StatusRebase = "rebase"
	// CodeRebased marks the filler row of a rebased or orphaned change.
	CodeRebased = "rebased"
)

const (
	phasePull   = "!pull"
	phaseRebase = "!rebase"
	phaseAck    = "!ack"

	stateKeyStart = "snapshot_start"
	stateKeyCols  = "snapshot_cols"
)

// EventBootstrapped is emitted when a snapshot bootstrap finished.
const EventBootstrapped = "bootstrapped"

func envFlag(name string) bool { return envTrue(name) }

func parseDur(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil || f <= 0 {
			return 0, false
		}
		return time.Duration(f * float64(24*time.Hour)), true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

func (c *Client) retention() time.Duration {
	if c.o.Retention > 0 {
		return c.o.Retention
	}
	if d, ok := parseDur(os.Getenv(EnvRetention)); ok {
		return d
	}
	return DefaultRetention
}

func (c *Client) autoHeal() bool { return c.o.AutoHeal || envFlag(EnvAutoHeal) }

func (c *Client) snapshotLimit() int {
	n := proto.SnapshotMaxPage
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvSnapshotPage))); err == nil && v > 0 {
		n = min(v, n)
	}
	c.loop.mu.Lock()
	defer c.loop.mu.Unlock()
	if c.loop.snapPage > 0 && c.loop.snapPage < n {
		n = c.loop.snapPage // halved after a response that was too large
	}
	return n
}

func (c *Client) isBootstrapping() bool {
	c.loop.mu.Lock()
	defer c.loop.mu.Unlock()
	return c.loop.booting
}

// ScheduleRebootstrap asks the (possibly not running) loop of this node for a
// fresh snapshot bootstrap: it forgets a bootstrap in progress and sets the
// state to rebootstrap_required, which the next cycle acts on.
func ScheduleRebootstrap(app core.App, reason string) error {
	cur, err := LoadCursor(app)
	if err != nil {
		return err
	}
	if cur == nil {
		return errors.New("sync: this node is not enrolled")
	}
	_, err = app.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET state={:s}, snapshot_id='', snapshot_after='', last_error={:e}").
		Bind(dbx.Params{"s": StateRebootstrapRequired, "e": cleanError(reason)}).Execute()
	return err
}

// snapState is the bootstrap in progress.
type snapState struct {
	ID    string
	Start int64
	Cols  []proto.SnapshotCollection
	After string
}

func stateGet(db dbx.Builder, key string) (string, bool) {
	var v string
	if err := db.NewQuery("SELECT value FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": key}).Row(&v); err != nil {
		return "", false
	}
	return v, true
}

func stateSet(db dbx.Builder, key, val string) error {
	_, err := db.NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, {:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
		Bind(dbx.Params{"k": key, "v": val}).Execute()
	return err
}

// loadSnapshot returns the bootstrap in progress, or nil.
func (c *Client) loadSnapshot() (*snapState, error) {
	cur, err := LoadCursor(c.o.App)
	if err != nil || cur == nil || cur.SnapshotID == "" {
		return nil, err
	}
	db := c.o.App.DB()
	sv, ok1 := stateGet(db, stateKeyStart)
	cv, ok2 := stateGet(db, stateKeyCols)
	st := &snapState{ID: cur.SnapshotID, After: cur.SnapshotAfter}
	if !ok1 || !ok2 || json.Unmarshal([]byte(cv), &st.Cols) != nil {
		return nil, nil // unreadable position: start over
	}
	if st.Start, err = strconv.ParseInt(sv, 10, 64); err != nil {
		return nil, nil
	}
	return st, nil
}

// dropSnapshot forgets the position (the parked local changes stay parked).
func (c *Client) dropSnapshot() error {
	return c.o.App.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		if _, err := db.NewQuery("UPDATE _sync_cursors SET snapshot_id='', snapshot_after=''").Execute(); err != nil {
			return err
		}
		_, err := db.NewQuery("DELETE FROM _sync_state WHERE key IN ('" + stateKeyStart + "','" + stateKeyCols + "')").Execute()
		return err
	})
}

func (c *Client) setCursorAfter(after string) error {
	_, err := c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET snapshot_after={:a}").Bind(dbx.Params{"a": after}).Execute()
	return err
}

// Bootstrap runs the snapshot bootstrap, or resumes the one in progress. It
// leaves the node with the hub's data, the log pulled from start_seq on and
// its unpushed local changes rebased (§3.9).
func (c *Client) Bootstrap(ctx context.Context) error {
	if c.o.App == nil || c.o.Backend == nil {
		return errors.New("sync: the bootstrap needs the app and a backend")
	}
	c.loop.mu.Lock()
	c.loop.booting = true
	c.loop.mu.Unlock()
	defer func() {
		c.loop.mu.Lock()
		c.loop.booting = false
		c.loop.mu.Unlock()
	}()
	c.setState(StateBootstrapping)
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var st *snapState
		if st, err = c.loadSnapshot(); err != nil {
			return err
		}
		if st == nil {
			if st, err = c.beginSnapshot(ctx); err != nil {
				return err
			}
		}
		err = c.runSnapshot(ctx, st)
		if IsCode(err, proto.CodeSnapshotExpired) || errors.Is(err, ErrRebootstrap) {
			// the id expired (24 h), or the log was compacted past start_seq while the
			// snapshot ran: take a new one
			if derr := c.dropSnapshot(); derr != nil {
				return derr
			}
			continue
		}
		return err
	}
	return err
}

// beginSnapshot asks the hub for a snapshot, creates the collections this node
// lacks and parks the unpushed local changes of the collections it replaces.
func (c *Client) beginSnapshot(ctx context.Context) (*snapState, error) {
	b, err := c.authed(ctx, http.MethodPost, proto.PathSnapshot, nil, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var start proto.SnapshotStart
	if err := json.Unmarshal(b, &start); err != nil {
		return nil, err
	}
	if err := c.importSchema(start.Schema); err != nil {
		return nil, fmt.Errorf("sync: cannot create the collections of the snapshot: %w", err)
	}
	if err := c.checkSchema(start); err != nil {
		c.recordError(err)
		return nil, err
	}
	// the collections this node replicates (its policy says pull) and the hub sends
	var cols []proto.SnapshotCollection
	for _, sc := range start.Collections {
		col, err := c.o.App.FindCachedCollectionByNameOrId(sc.ID)
		if err != nil {
			return nil, fmt.Errorf("sync: collection %q (%s) of the snapshot does not exist on this node", sc.Name, sc.ID)
		}
		if pv := c.o.Backend.Policy(col); pv == nil || (pv.Direction != "both" && pv.Direction != "pull") {
			continue
		}
		cols = append(cols, sc)
	}
	colsJSON, _ := json.Marshal(cols)
	c.loop.mu.Lock()
	pushFrom := c.loop.pushFrom
	c.loop.mu.Unlock()
	first := ""
	if len(cols) > 0 {
		first = cols[0].ID + "/"
	} else {
		first = phaseAck
	}
	err = c.o.App.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		for _, sc := range cols {
			// changes below push_from are already on the hub (and in the snapshot)
			if _, err := db.NewQuery("UPDATE _changes SET status='acked' WHERE node={:n} AND collection={:c} AND origin_seq<{:p} AND status IN ('local','pushed')").
				Bind(dbx.Params{"n": c.nodeID, "c": sc.ID, "p": pushFrom}).Execute(); err != nil {
				return err
			}
			if _, err := db.NewQuery("UPDATE _changes SET status={:r} WHERE node={:n} AND collection={:c} AND origin_seq>={:p} AND status IN ('local','pushed')").
				Bind(dbx.Params{"r": StatusRebase, "n": c.nodeID, "c": sc.ID, "p": pushFrom}).Execute(); err != nil {
				return err
			}
		}
		if err := stateSet(db, stateKeyStart, strconv.FormatInt(start.StartSeq, 10)); err != nil {
			return err
		}
		if err := stateSet(db, stateKeyCols, string(colsJSON)); err != nil {
			return err
		}
		if first == phaseAck {
			if _, err := db.NewQuery("UPDATE _sync_cursors SET pull_after={:s}").Bind(dbx.Params{"s": start.StartSeq}).Execute(); err != nil {
				return err
			}
		}
		_, err := db.NewQuery("UPDATE _sync_cursors SET snapshot_id={:i}, snapshot_after={:a}, state={:s}, last_error=''").
			Bind(dbx.Params{"i": start.SnapshotID, "a": first, "s": StateBootstrapping}).Execute()
		return err
	})
	if err != nil {
		return nil, err
	}
	c.emit(Event{Type: EventRebootstrap, Message: "snapshot started"})
	return &snapState{ID: start.SnapshotID, Start: start.StartSeq, Cols: cols, After: first}, nil
}

// importSchema creates the collections of the snapshot that do not exist here.
// Existing collections are left alone (schema bundles, PR8, own the changes).
func (c *Client) importSchema(raw []json.RawMessage) error {
	var missing []map[string]any
	for _, r := range raw {
		var m map[string]any
		if err := json.Unmarshal(r, &m); err != nil {
			return err
		}
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		if _, err := c.o.App.FindCollectionByNameOrId(id); err == nil {
			continue
		}
		if _, err := c.o.App.FindCollectionByNameOrId(name); err == nil {
			continue
		}
		missing = append(missing, m)
	}
	if len(missing) == 0 {
		return nil
	}
	if si, ok := c.o.Backend.(interface {
		ImportCollections([]map[string]any) error
	}); ok {
		return si.ImportCollections(missing) // inside the schema lock exemption
	}
	return c.o.App.ImportCollections(missing, false)
}

// checkSchema fails loudly when a collection this node already has lacks a field
// the hub syncs (not excluded, not a file): applying the snapshot would drop that
// field's data silently and the node would never match the hub (P7-12). Schema
// bundles (PR8) own the real fix; until then the operator adds the field.
func (c *Client) checkSchema(start proto.SnapshotStart) error {
	excluded := map[string]map[string]bool{}
	for _, p := range start.Policies {
		ex := map[string]bool{}
		for _, f := range p.Exclude {
			ex[f] = true
		}
		excluded[p.Collection] = ex
	}
	for _, raw := range start.Schema {
		var m struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Fields []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"fields"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		col, err := c.o.App.FindCachedCollectionByNameOrId(m.ID)
		if err != nil {
			continue
		}
		ex := excluded[m.Name]
		if ex == nil {
			ex = excluded[m.ID]
		}
		for _, f := range m.Fields {
			if f.Type == "file" || ex[f.Name] || col.Fields.GetByName(f.Name) != nil {
				continue
			}
			return fmt.Errorf("sync: collection %q on this node lacks the field %q that the hub syncs; the schema bundle of the hub should have added it: handshake again, or lift the schema lock and add the field", m.Name, f.Name)
		}
	}
	return nil
}

func splitAfter(s string) (col, after string, phase string) {
	if strings.HasPrefix(s, "!") {
		return "", "", s
	}
	col, after, _ = strings.Cut(s, "/")
	return col, after, ""
}

// runSnapshot applies the pages from the saved position, then runs the phases
// that follow (pull, rebase, ack).
func (c *Client) runSnapshot(ctx context.Context, st *snapState) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		colID, after, phase := splitAfter(st.After)
		if phase != "" {
			break
		}
		idx := -1
		for i, sc := range st.Cols {
			if sc.ID == colID {
				idx = i
			}
		}
		if idx < 0 {
			return fmt.Errorf("sync: the snapshot position %q is not a collection of this snapshot", st.After)
		}
		col, err := c.o.App.FindCachedCollectionByNameOrId(colID)
		if err != nil {
			return err
		}
		page, err := c.snapshotPage(ctx, st.ID, colID, after)
		if err != nil {
			return err
		}
		next := colID + "/" + page.Next
		last := false
		if !page.More {
			if idx+1 < len(st.Cols) {
				next = st.Cols[idx+1].ID + "/"
			} else {
				next, last = phaseAck, true
			}
		}
		if err := c.applySnapshotPage(col, after, page, next, last, st.Start); err != nil {
			return err
		}
		st.After = next
	}

	for {
		_, _, phase := splitAfter(st.After)
		switch phase {
		case phaseAck:
			// the node holds the snapshot: it is active again and has pulled start_seq
			// (the pull below needs that, a node flagged stale is refused)
			if _, err := c.ackWith(ctx, st.Start, nil, st.ID); err != nil {
				return err
			}
			st.After = phasePull
			if err := c.setCursorAfter(st.After); err != nil {
				return err
			}
		case phasePull:
			var res Result
			if err := c.pullAll(ctx, &res); err != nil {
				return err
			}
			st.After = phaseRebase
			if err := c.setCursorAfter(st.After); err != nil {
				return err
			}
		case phaseRebase:
			if _, _, err := c.rebaseParked(ctx); err != nil {
				return err
			}
			return c.finishSnapshot()
		default:
			return fmt.Errorf("sync: unknown snapshot phase %q", st.After)
		}
	}
}

func (c *Client) snapshotPage(ctx context.Context, id, col, after string) (*proto.SnapshotPage, error) {
	for {
		limit := c.snapshotLimit()
		q := url.Values{"id": {id}, "collection": {col}, "after": {after}, "limit": {strconv.Itoa(limit)}}
		b, err := c.authed(ctx, http.MethodGet, proto.PathSnapshot+"?"+q.Encode(), nil, nil)
		if err != nil {
			if IsCode(err, proto.CodeResponseTooLarge) && limit > 1 {
				c.loop.mu.Lock()
				c.loop.snapPage = max(limit/2, 1)
				c.loop.mu.Unlock()
				continue
			}
			return nil, err
		}
		var page proto.SnapshotPage
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		return &page, nil
	}
}

// emptyCollection removes the rows, clocks and delete tombstones of col without
// capturing anything (the unpushed local changes are parked, the hub is the
// authority). Legal tombstones are permanent and stay.
func emptyCollection(tx kernel.App, col *core.Collection) error {
	db := tx.NonconcurrentDB()
	if _, err := db.NewQuery("DELETE FROM {{" + col.Name + "}}").Execute(); err != nil {
		return err
	}
	if _, err := db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c}").Bind(dbx.Params{"c": col.Id}).Execute(); err != nil {
		return err
	}
	_, err := db.NewQuery("DELETE FROM _sync_tombstones WHERE collection={:c} AND kind='delete'").Bind(dbx.Params{"c": col.Id}).Execute()
	return err
}

// applySnapshotPage applies one page and moves the position, in one transaction.
// The first page of a collection (after == "") empties it first. On the last page
// of the last collection the log cursor is set to start_seq.
func (c *Client) applySnapshotPage(col *core.Collection, after string, page *proto.SnapshotPage, next string, last bool, start int64) error {
	var failure error
	err := c.o.App.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		if after == "" {
			if err := emptyCollection(tx, col); err != nil {
				return err
			}
		}
		for _, t := range page.Tombstones {
			h, err := hlc.Parse(t.HLC)
			if err != nil {
				return err
			}
			created := t.Created
			if created == "" {
				created = c.wallNow().UTC().Format(dateLayout)
			}
			if _, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, {:k}, {:h}, {:n}, '', '', {:t}) ON CONFLICT(collection, record) DO NOTHING`).
				Bind(dbx.Params{"c": col.Id, "r": t.Record, "k": t.Kind, "h": int64(h), "n": t.Node, "t": created}).Execute(); err != nil {
				return err
			}
		}
		for i := range page.Records {
			r := &page.Records[i]
			patch, err := json.Marshal(r.Data)
			if err != nil {
				return err
			}
			ch := &proto.PullChange{ID: "snapshot:" + r.ID, Node: r.Node, HLC: r.HLC, Collection: col.Id, Record: r.ID,
				Op: "c", Patch: patch, Hash: r.Hash, Fields: r.Fields}
			sp := fmt.Sprintf("sync_snap_%d", applySP.Add(1))
			if _, err := db.NewQuery("SAVEPOINT " + sp).Execute(); err != nil {
				return err
			}
			if _, aerr := c.applyChangeMode(tx, ch, kernel.SyncModeSnapshot); aerr != nil {
				_, _ = db.NewQuery("ROLLBACK TO " + sp).Execute()
				_, _ = db.NewQuery("RELEASE " + sp).Execute()
				failure = &ApplyError{ID: ch.ID, Err: aerr}
				return failure // the page rolls back, the position stays before it
			}
			if _, err := db.NewQuery("RELEASE " + sp).Execute(); err != nil {
				return err
			}
		}
		if last {
			if _, err := db.NewQuery("UPDATE _sync_cursors SET pull_after={:s}").Bind(dbx.Params{"s": start}).Execute(); err != nil {
				return err
			}
		}
		_, err := db.NewQuery("UPDATE _sync_cursors SET snapshot_after={:a}").Bind(dbx.Params{"a": next}).Execute()
		return err
	})
	if failure != nil {
		c.emit(Event{Type: EventError, Collection: col.Id, Message: "snapshot apply failed: " + failure.Error()})
	}
	return err
}

// ackWith is Ack with the id of a finished snapshot.
func (c *Client) ackWith(ctx context.Context, through int64, digest map[string]string, snapshotID string) (*proto.AckResponse, error) {
	body, _ := json.Marshal(proto.AckRequest{PulledThrough: through, Digest: digest, SnapshotID: snapshotID})
	b, err := c.authed(ctx, http.MethodPost, proto.PathAck, nil, body)
	if err != nil {
		return nil, err
	}
	var ar proto.AckResponse
	if err := json.Unmarshal(b, &ar); err != nil {
		return nil, err
	}
	return &ar, nil
}

// finishSnapshot clears the bootstrap position and returns the node to idle.
func (c *Client) finishSnapshot() error {
	err := c.dropSnapshot()
	if err != nil {
		return err
	}
	if _, err := c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET state={:s}, last_error=''").
		Bind(dbx.Params{"s": StateIdle}).Execute(); err != nil {
		return err
	}
	c.loop.mu.Lock()
	c.loop.hashStreak, c.loop.digestStreak, c.loop.snapPage = 0, 0, 0
	c.loop.needHS = true // the next cycle handshakes again and sees the state the hub has now
	c.loop.mu.Unlock()
	c.setState("idle")
	c.emit(Event{Type: EventBootstrapped, Message: "snapshot bootstrap finished"})
	return nil
}

// handleEpoch stores the hub epoch when it differs from the one this node knew
// (backup restore, replica promote, §3.9) and reports whether it changed. The
// hub decides in the handshake whether the node has to re-bootstrap
// (`rebootstrap`, see epochRequiresRebootstrap); when it does not, the pull
// cursor is already at or below the head at which the new epoch began, and the
// acked changes the hub no longer has are sent again by reconcile (they are
// kept for TOKI_SYNC_SPOKE_KEEP). The cursor never stays above that head.
func (c *Client) handleEpoch(hs *proto.HandshakeResponse) bool {
	if c.o.App == nil || hs.HubEpoch == "" {
		return false
	}
	cur, err := LoadCursor(c.o.App)
	if err != nil || cur == nil || cur.HubEpoch == "" || cur.HubEpoch == hs.HubEpoch {
		return false
	}
	_, err = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET pull_after=MIN(pull_after,{:s}), hub_epoch={:e} WHERE hub_id={:h}").
		Bind(dbx.Params{"s": hs.HubEpochSeq, "e": hs.HubEpoch, "h": cur.HubID}).Execute()
	if err != nil {
		c.recordError(err)
		return false
	}
	if c.o.Logger != nil {
		c.o.Logger.Warn("sync: the hub epoch changed (restore or failover)",
			"old", cur.HubEpoch, "new", hs.HubEpoch, "pull_after", cur.PullAfter, "epoch_seq", hs.HubEpochSeq, "rebootstrap", hs.Rebootstrap)
	}
	c.emit(Event{Type: EventEpoch, Message: "hub epoch changed"})
	return true
}
