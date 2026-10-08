//go:build !no_sync

package sync

import (
	"encoding/json"
	"slices"
	"strings"
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

	// ColID is the id of the collection ("" while the collection does not exist).
	ColID string
	// Strategy is lww (default), hub-wins, field-merge or hook.
	Strategy string
	// Hook is the WASM module that decides conflicts (strategy hook); "" = any
	// module subscribed to the collection.
	Hook string
	// Review makes the conflicts that field-merge resolves automatically stay
	// open for an admin (docs/SYNC_DESIGN.md §4.4).
	Review bool
	Crypto string
	Order  int
	// PartField and PartParam are the parsed `partition` ("<field> = @node.<param>");
	// both are "" without partition.
	PartField string
	PartParam string
	// PullViewRule additionally requires the view rule of the collection for the
	// service actor of the node on pull (design §7.7).
	PullViewRule bool
	// Trusted lets a collection whose view rule is null (superusers only) be
	// pulled while PullViewRule is on.
	Trusted bool
	// SkipViewRule turns the default view rule check of pulled rows off for this
	// collection (default: enforced for the service actor of the node, see pullRuleOn).
	SkipViewRule bool
	// EvictInvisible makes the revert of a record outside the view rule evict
	// the local copy instead of only a notice (default: TOKI_SYNC_EVICT_INVISIBLE).
	EvictInvisible bool
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
	// a collection created, renamed or deleted changes what a policy ref resolves to
	invCol := func(e *core.CollectionEvent) error {
		err := e.Next()
		c.invalidate()
		return err
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "polcol", Func: invCol})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "polcol", Func: invCol})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "polcol", Func: invCol})
}

// policyRetry is how long a failed reload keeps serving the last good set
// before it is tried again.
const policyRetry = time.Second

// load returns the policies by collection name and id. A load error is never
// cached as "no policies": with a previous good set that set keeps being used
// (and the reload is retried after policyRetry), without one the error is
// returned and capture refuses the write.
func (c *policyCache) load() (map[string]*policy, error) {
	c.mu.RLock()
	rows, fresh := c.rows, !c.stale && time.Since(c.loaded) < policyTTL
	c.mu.RUnlock()
	if fresh && rows != nil {
		return rows, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stale && c.rows != nil && time.Since(c.loaded) < policyTTL {
		return c.rows, nil // another goroutine reloaded meanwhile
	}
	recs, err := c.m.app.FindAllRecords(PoliciesCollection)
	if err != nil {
		if c.rows == nil {
			c.m.app.Logger().Error("sync: failed to load policies, writes are refused", "error", err)
			return nil, err
		}
		c.m.app.Logger().Warn("sync: failed to reload policies, keeping the last good set", "error", err)
		c.loaded = time.Now().Add(policyRetry - policyTTL)
		c.stale = false
		return c.rows, nil
	}
	out := make(map[string]*policy, len(recs)*2)
	for _, r := range recs {
		if !r.GetBool("enabled") {
			continue
		}
		p := &policy{Direction: r.GetString("direction"), Types: map[string]string{}, Exclude: map[string]struct{}{},
			Strategy: r.GetString("strategy"), Hook: r.GetString("hook"), Review: r.GetBool("review")}
		if p.Direction == "" {
			p.Direction = DirBoth
		}
		if p.Strategy == "" {
			p.Strategy = StratLWW
		}
		p.Crypto = r.GetString("crypto")
		if p.Crypto == "" {
			p.Crypto = "ciphertext"
		}
		p.Order = r.GetInt("order")
		p.PullViewRule, p.Trusted = r.GetBool("pull_view_rule"), r.GetBool("trusted")
		// fail closed: a row that cannot be parsed (SaveNoValidate, import, restore,
		// manual SQL, a spoke that got the row from the hub) must not widen what
		// travels, so the collection is not synced at all until the row is fixed
		var bad []string
		if pf, pp, perr := parsePartition(r.GetString("partition")); perr == nil {
			p.PartField, p.PartParam = pf, pp
		} else {
			bad = append(bad, "partition: "+perr.Error())
		}
		if raw := rawJSON(r, "field_types"); raw != nil {
			if err := json.Unmarshal(raw, &p.Types); err != nil {
				bad = append(bad, "field_types: "+err.Error())
			}
		}
		if raw := rawJSON(r, "exclude"); raw != nil {
			var ex []string
			if err := json.Unmarshal(raw, &ex); err != nil {
				bad = append(bad, "exclude: "+err.Error())
			}
			for _, f := range ex {
				p.Exclude[f] = struct{}{}
			}
		}
		if !slices.Contains(policyStrategies, p.Strategy) {
			bad = append(bad, "unknown strategy "+p.Strategy)
		}
		if !slices.Contains(policyDirections, p.Direction) {
			bad = append(bad, "unknown direction "+p.Direction)
		}
		if len(bad) > 0 {
			c.m.app.Logger().Error("sync: invalid policy row, the collection is NOT synced until it is fixed (see `toki sync policies lint`)",
				"collection", r.GetString("collection"), "problems", strings.Join(bad, "; "))
			p.Direction, p.PartField, p.PartParam = DirNone, "", ""
		}
		ref := r.GetString("collection")
		out[ref] = p
		if col, err := c.m.app.FindCachedCollectionByNameOrId(ref); err == nil && col != nil {
			p.ColID = col.Id
			if systemAllowed(col) {
				p.SkipViewRule = true // rules are null by design (syscollections.go)
			}
			out[col.Id] = p
			out[col.Name] = p
		}
	}
	c.rows, c.loaded, c.stale = out, time.Now(), false
	return out, nil
}

// partitioned returns the enabled policies that have a partition and an existing
// collection (each policy once).
func (c *policyCache) partitioned() ([]*policy, error) {
	rows, err := c.load()
	if err != nil {
		return nil, err
	}
	var out []*policy
	for k, p := range rows {
		if p.PartField != "" && p.ColID != "" && k == p.ColID && p.Direction != DirNone {
			out = append(out, p)
		}
	}
	return out, nil
}

// eligible reports whether col can ever be captured as data: system
// collections (names starting with "_") and views never are.
func eligible(col *core.Collection) bool {
	if systemAllowed(col) { // explicit allowlist (syscollections.go)
		return true
	}
	return col != nil && !col.System && !col.IsView() && len(col.Name) > 0 && col.Name[0] != '_'
}

// For returns the policy that makes col captured, or nil. System collections
// (names starting with "_") and views are never captured as data. A policy
// load error is returned: the caller must refuse the write (fail closed).
func (c *policyCache) For(col *core.Collection) (*policy, error) {
	if !eligible(col) {
		return nil, nil
	}
	rows, err := c.load()
	if err != nil {
		return nil, err
	}
	p := rows[col.Id]
	if p == nil {
		p = rows[col.Name]
	}
	if p == nil || p.Direction == DirNone {
		return nil, nil
	}
	return p, nil
}

func rawJSON(r *core.Record, name string) []byte {
	b, err := json.Marshal(r.Get(name))
	if err != nil || string(b) == "null" || len(b) == 0 {
		return nil
	}
	return b
}
