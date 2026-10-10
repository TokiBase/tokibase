//go:build !no_sync

package client

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// A collection whose data key never arrives (the hub was restored from a backup
// without the newest key, the export for it fails, its direction was changed)
// must not block the others. After KeyMissingCycles consecutive attempts that
// did not bring the key (each with a forced handshake) the collection is
// marked `key_missing` in `_sync_state`: its pulled changes and its snapshot
// are skipped (the cursor moves on), `toki sync status` lists it, and the node
// re-bootstraps as soon as a handshake brings a key again, to fetch what it
// skipped.

// KeyMissingCycles is the number of failed attempts after which a collection is marked key_missing.
const KeyMissingCycles = 3

const stateKeyMissing = "key_missing:"

// EventKeyMissing is emitted when a collection is marked key_missing.
const EventKeyMissing = "key_missing"

// noteKeyMissing counts one more attempt for the collection and marks it when
// the limit is reached. It reports whether the collection is marked.
func (c *Client) noteKeyMissing(db dbx.Builder, colID string) bool {
	c.loop.mu.Lock()
	if c.loop.keyMiss == nil {
		c.loop.keyMiss = map[string]int{}
	}
	c.loop.keyMiss[colID]++
	n := c.loop.keyMiss[colID]
	c.loop.mu.Unlock()
	if n < KeyMissingCycles {
		return false
	}
	c.loop.mu.Lock()
	top := c.loop.keyMax[colID]
	c.loop.mu.Unlock()
	if err := stateSet(db, stateKeyMissing+colID, time.Now().UTC().Format(proto.TimeLayout)+"|"+strconv.Itoa(top)); err != nil {
		return false
	}
	if c.o.Logger != nil {
		c.o.Logger.Error("sync: the data key of a collection never arrived, the collection is skipped until it does",
			"collection", colID, "attempts", n)
	}
	c.emit(Event{Type: EventKeyMissing, Collection: colID, Message: "the encryption key of this collection is missing; it is skipped"})
	return true
}

// keyMissingMarked reports whether the collection is marked.
func keyMissingMarked(db dbx.Builder, colID string) bool {
	_, ok := stateGet(db, stateKeyMissing+colID)
	return ok
}

// KeyMissingCollections lists the ids of the collections marked key_missing.
func KeyMissingCollections(app core.App) []string {
	if app == nil || !app.HasTable("_sync_state") {
		return nil
	}
	var keys []string
	if err := app.DB().NewQuery("SELECT key FROM _sync_state WHERE key LIKE {:p}").
		Bind(dbx.Params{"p": stateKeyMissing + "%"}).Column(&keys); err != nil {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, stateKeyMissing))
	}
	sort.Strings(out)
	return out
}

// keyApplied resets the attempt counter of a collection whose change applied.
func (c *Client) keyApplied(colID string) {
	c.loop.mu.Lock()
	delete(c.loop.keyMiss, colID)
	c.loop.mu.Unlock()
}

// resolveKeyMissing runs after the keys of a handshake were read. The highest
// key version the hub sent per collection is remembered; a collection that is
// marked key_missing is unmarked when the hub now sends a HIGHER version than
// the one it sent when the mark was set (the hub was restored, the export was
// repaired), and the node schedules a re-bootstrap to fetch what it skipped.
func (c *Client) resolveKeyMissing(hs *proto.HandshakeResponse) {
	if c.o.App == nil {
		return
	}
	failing := map[string]bool{}
	for _, ke := range hs.KeyErrors {
		failing[ke.Collection] = true
	}
	top := map[string]int{}
	for _, k := range hs.Keys {
		if !k.Retired {
			top[k.Collection] = max(top[k.Collection], k.Version)
		}
	}
	c.loop.mu.Lock()
	if c.loop.keyMax == nil {
		c.loop.keyMax = map[string]int{}
	}
	for id, v := range top {
		if v > c.loop.keyMax[id] {
			delete(c.loop.keyMiss, id) // a newer key arrived: the attempts start over
		}
		c.loop.keyMax[id] = v
	}
	c.loop.mu.Unlock()
	db := c.o.App.NonconcurrentDB()
	recovered := false
	for _, id := range KeyMissingCollections(c.o.App) {
		v, _ := stateGet(db, stateKeyMissing+id)
		_, at, _ := strings.Cut(v, "|")
		marked, _ := strconv.Atoi(at)
		if !failing[id] && top[id] > marked {
			_, _ = db.NewQuery("DELETE FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": stateKeyMissing + id}).Execute()
			recovered = true
		}
	}
	if recovered {
		_ = ScheduleRebootstrap(c.o.App, "the encryption key of a skipped collection arrived: fetching its data again")
	}
}

var ctVersionRe = regexp.MustCompile(`tkc1:(\d+):`)

// keyPending lists, per collection id, the data key versions that the unsent
// local changes hold ciphertext of. The hub refuses to retire a version while a
// node reports it. At most 20000 queued rows are looked at.
func (c *Client) keyPending() map[string][]int {
	if c.o.App == nil {
		return nil
	}
	var rows []struct {
		Collection string `db:"collection"`
		Patch      string `db:"patch"`
	}
	if err := c.o.App.DB().NewQuery("SELECT collection, patch FROM _changes WHERE node={:n} AND status IN ('local','pushed') AND patch LIKE {:p} LIMIT 20000").
		Bind(dbx.Params{"n": c.nodeID, "p": "%tkc1:%"}).All(&rows); err != nil || len(rows) == 0 {
		return nil
	}
	seen := map[string]map[int]struct{}{}
	for _, r := range rows {
		for _, m := range ctVersionRe.FindAllStringSubmatch(r.Patch, -1) {
			v, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if seen[r.Collection] == nil {
				seen[r.Collection] = map[int]struct{}{}
			}
			seen[r.Collection][v] = struct{}{}
		}
	}
	out := map[string][]int{}
	for col, set := range seen {
		for v := range set {
			out[col] = append(out[col], v)
		}
		sort.Ints(out[col])
	}
	return out
}
