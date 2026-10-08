//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Partitions (docs/SYNC_DESIGN.md §2.1, §3.5, §7.1): a policy `partition`
// "<field> = @node.<param>" limits a node to the records whose <field> equals the
// parameter that the admin set at enrollment. The hub stores the partition key
// before and after every change (`part_old`, `part_new`).

// partString renders a partition key. It is used for record values, patch values
// (decoded JSON) and node parameters, so that "12", 12 and 12.0 compare equal.
func partString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	case []any:
		if len(t) == 1 {
			return partString(t[0])
		}
		return ""
	case []string:
		if len(t) == 1 {
			return t[0]
		}
		return ""
	}
	return fmt.Sprint(v)
}

// partValue is the partition key of rec for the partition field of p ("" without
// partition or when the record has no value).
func partValue(rec *core.Record, p *policy) string {
	if rec == nil || p == nil || p.PartField == "" {
		return ""
	}
	f := rec.Collection().Fields.GetByName(p.PartField)
	if f == nil {
		return ""
	}
	switch f.Type() {
	case kernel.FieldTypeNumber:
		return partString(rec.GetFloat(p.PartField))
	case kernel.FieldTypeBool:
		return partString(rec.GetBool(p.PartField))
	}
	return partString(rec.GetString(p.PartField))
}

// nodeParams reads the partition parameters (`_sync_nodes.params`) of a node.
func nodeParams(app kernel.App, nodeID string) map[string]string {
	out := map[string]string{}
	var raw string
	if err := app.DB().NewQuery("SELECT COALESCE(params,'') FROM " + NodesCollection + " WHERE id={:id}").
		Bind(dbx.Params{"id": nodeID}).Row(&raw); err != nil || strings.TrimSpace(raw) == "" || raw == "null" {
		return out
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return out
	}
	for k, v := range m {
		out[k] = partString(v)
	}
	return out
}

// param returns the value of a node parameter; ok is false when the node has none.
func (v *viewer) param(name string) (string, bool) {
	if !v.paramsLoaded {
		v.params, v.paramsLoaded = nodeParams(v.app, v.nodeID), true
	}
	s, ok := v.params[name]
	return s, ok
}

// nodePartition is the partition key that the node may see for p ("" = none).
func (v *viewer) nodePartition(p *policy) (string, bool) {
	if p.PartField == "" {
		return "", true
	}
	s, ok := v.param(p.PartParam)
	return s, ok && s != ""
}

// inPartition reports whether the CURRENT state of rec is inside the partition
// of the node (always true without partition).
func (v *viewer) inPartition(rec *core.Record, p *policy) bool {
	if p.PartField == "" {
		return true
	}
	pv, ok := v.nodePartition(p)
	return ok && partValue(rec, p) == pv
}

// partitionExclusion is the SQL that keeps the rows of other partitions out of
// a pull page: for every partitioned collection a row (not a revert, not a
// purge) must have part_new or part_old equal to the node's parameter.
func (m *Module) partitionExclusion(vw *viewer) (string, dbx.Params, error) {
	ps, err := m.pol.partitioned()
	if err != nil || len(ps) == 0 {
		return "", nil, err
	}
	params := dbx.Params{}
	parts := make([]string, 0, len(ps))
	for i, p := range ps {
		ck, vk := "pc"+strconv.Itoa(i), "pv"+strconv.Itoa(i)
		cl := "(collection={:" + ck + "} AND op!='p' AND status!='revert'"
		params[ck] = p.ColID
		if pv, ok := vw.nodePartition(p); ok {
			cl += " AND part_new!={:" + vk + "} AND part_old!={:" + vk + "}"
			params[vk] = pv
		}
		parts = append(parts, cl+")")
	}
	return " AND NOT (" + strings.Join(parts, " OR ") + ")", params, nil
}

// evictChange turns a pulled change into an eviction: the record left the node's
// partition (or its visibility); the node deletes its copy without a tombstone.
func evictChange(pc proto.PullChange) proto.PullChange {
	pc.Op, pc.Evict, pc.Patch, pc.Hash, pc.Fields = "x", true, nil, "", nil
	return pc
}
