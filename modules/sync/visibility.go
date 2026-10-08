//go:build !no_sync

package sync

import (
	"net/http"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// What a node may RECEIVE (docs/SYNC_DESIGN.md §7.7): the view rule of the
// collection and the fieldperm read rules, evaluated for the actor of the node
// through the normal record fetch path, with RequestInfo context "sync".

// authSystemFields are the system fields of auth collections that only travel
// when the policy opts in with field_types {"<field>": "include"}.
var authSystemFields = map[string]bool{"email": true, "emailVisibility": true, "verified": true}

// TypeInclude is the field_types value that opts an auth system field in.
const TypeInclude = "include"

// serviceActor returns the service actor record of a node (nil when none).
func serviceActor(app kernel.App, nodeID string) *core.Record {
	node, err := app.FindRecordById(NodesCollection, nodeID)
	if err != nil {
		return nil
	}
	c, id := node.GetString("actor_collection"), node.GetString("actor_record")
	if c == "" || id == "" {
		return nil
	}
	rec, err := app.FindRecordById(c, id)
	if err != nil {
		return nil
	}
	return rec
}

func syncRequestInfo(actor *core.Record, nodeID, method string) *core.RequestInfo {
	return &core.RequestInfo{
		Context: core.RequestInfoContextSync, Method: method, Auth: actor,
		Headers: map[string]string{"x_toki_sync_node": nodeID}, Query: map[string]string{}, Body: map[string]any{},
	}
}

// viewResult tells what the actor may see of one record.
type viewResult struct {
	visible bool
	hidden  map[string]struct{} // synced fields the actor may not read
}

// viewer evaluates visibility with a per page cache.
type viewer struct {
	app    kernel.App
	nodeID string
	actor  *core.Record
	cache  map[gkey]*viewResult
}

func newViewer(app kernel.App, nodeID string, actor *core.Record) *viewer {
	return &viewer{app: app, nodeID: nodeID, actor: actor, cache: map[gkey]*viewResult{}}
}

// view reports whether the actor can view rec (view rule, when checkRule) and
// which synced fields fieldperm hides from it.
func (v *viewer) view(rec *core.Record, p *policy, checkRule bool) (*viewResult, error) {
	col := rec.Collection()
	k := gkey{col.Id, rec.Id}
	if r, ok := v.cache[k]; ok && (r.visible || !checkRule) {
		return r, nil
	}
	ri := syncRequestInfo(v.actor, v.nodeID, http.MethodGet)
	res := &viewResult{visible: true, hidden: map[string]struct{}{}}
	if checkRule {
		ok, err := v.app.CanAccessRecord(rec, ri, col.ViewRule)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.visible = false
			v.cache[k] = res
			return res, nil
		}
	}
	clone := rec.Clone()
	if err := apis.EnrichRecordsForInfo(v.app, ri, clone); err != nil {
		return nil, err
	}
	pub := clone.PublicExport()
	for _, f := range syncedFields(col, p) {
		name := f.GetName()
		if col.IsAuth() && authSystemFields[name] {
			continue // gated by the policy opt-in, not by email visibility
		}
		if _, ok := pub[name]; !ok {
			res.hidden[name] = struct{}{}
		}
	}
	v.cache[k] = res
	return res, nil
}
