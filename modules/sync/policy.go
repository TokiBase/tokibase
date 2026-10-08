//go:build !no_sync

package sync

import (
	"encoding/json"
	stdsync "sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// Field types of a policy `field_types` entry that capture understands.
const (
	TypeCounter = "counter"
	TypeSet     = "set"
)

const policyTTL = 5 * time.Second

// policy is the parsed `_sync_policies` row of one collection.
type policy struct {
	Direction string
	Types     map[string]string
	Exclude   map[string]struct{}
}

type policyCache struct {
	m *Module

	mu     stdsync.RWMutex
	rows   map[string]*policy // by collection name AND id
	loaded time.Time
	stale  bool
}

func (c *policyCache) invalidate() {
	c.mu.Lock()
	c.stale = true
	c.mu.Unlock()
}

// bind invalidates the cache whenever a policy row changes.
func (c *policyCache) bind() {
	app := c.m.app
	inv := func(e *core.RecordEvent) error {
		err := e.Next()
		c.invalidate()
		return err
	}
	app.OnRecordAfterCreateSuccess(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "pol", Func: inv})
	app.OnRecordAfterUpdateSuccess(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "pol", Func: inv})
	app.OnRecordAfterDeleteSuccess(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "pol", Func: inv})
}

func (c *policyCache) load() map[string]*policy {
	c.mu.RLock()
	rows, fresh := c.rows, !c.stale && time.Since(c.loaded) < policyTTL
	c.mu.RUnlock()
	if fresh && rows != nil {
		return rows
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	recs, err := c.m.app.FindAllRecords(PoliciesCollection)
	if err != nil {
		c.m.app.Logger().Warn("sync: failed to load policies", "error", err)
		if c.rows == nil {
			c.rows = map[string]*policy{}
		}
		c.loaded = time.Now()
		return c.rows
	}
	out := make(map[string]*policy, len(recs)*2)
	for _, r := range recs {
		if !r.GetBool("enabled") {
			continue
		}
		p := &policy{Direction: r.GetString("direction"), Types: map[string]string{}, Exclude: map[string]struct{}{}}
		if p.Direction == "" {
			p.Direction = DirBoth
		}
		if raw := rawJSON(r, "field_types"); raw != nil {
			_ = json.Unmarshal(raw, &p.Types)
		}
		if raw := rawJSON(r, "exclude"); raw != nil {
			var ex []string
			if json.Unmarshal(raw, &ex) == nil {
				for _, f := range ex {
					p.Exclude[f] = struct{}{}
				}
			}
		}
		ref := r.GetString("collection")
		out[ref] = p
		if col, err := c.m.app.FindCachedCollectionByNameOrId(ref); err == nil && col != nil {
			out[col.Id] = p
			out[col.Name] = p
		}
	}
	c.rows, c.loaded, c.stale = out, time.Now(), false
	return out
}

// For returns the policy that makes col captured, or nil. System collections
// (names starting with "_") and views are never captured as data.
func (c *policyCache) For(col *core.Collection) *policy {
	if col == nil || col.System || col.IsView() || len(col.Name) == 0 || col.Name[0] == '_' {
		return nil
	}
	rows := c.load()
	p := rows[col.Id]
	if p == nil {
		p = rows[col.Name]
	}
	if p == nil || p.Direction == DirNone {
		return nil
	}
	return p
}

func rawJSON(r *core.Record, name string) []byte {
	b, err := json.Marshal(r.Get(name))
	if err != nil || string(b) == "null" || len(b) == 0 {
		return nil
	}
	return b
}
