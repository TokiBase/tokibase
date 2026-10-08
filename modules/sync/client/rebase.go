//go:build !no_sync

package client

import (
	"encoding/json"

	"github.com/pocketbase/dbx"
)

// setOp is a pending local set operation.
type setOp struct {
	add []any
	rm  []any
}

// pending is what the node has changed locally on a record and not yet
// handed to the hub (status local or pushed), see docs/SYNC_DESIGN.md §4.5.
type pending struct {
	// any: at least one pending local change exists for the record.
	any       bool
	hasDelete bool
	hlc       map[string]int64   // plain field -> highest pending hlc
	inc       map[string]float64 // counter field -> sum of pending $inc
	sets      map[string][]setOp // set field -> pending ops in order
}

func loadPending(db dbx.Builder, nodeID, colID, recID string) (*pending, error) {
	p := &pending{hlc: map[string]int64{}, inc: map[string]float64{}, sets: map[string][]setOp{}}
	var rows []struct {
		HLC   int64  `db:"hlc"`
		Op    string `db:"op"`
		Patch string `db:"patch"`
	}
	err := db.NewQuery("SELECT hlc, op, patch FROM _changes WHERE node={:n} AND collection={:c} AND record={:r} AND status IN ('local','pushed') ORDER BY origin_seq").
		Bind(dbx.Params{"n": nodeID, "c": colID, "r": recID}).All(&rows)
	if err != nil {
		return nil, err
	}
	p.any = len(rows) > 0
	for _, r := range rows {
		if r.Op == "d" || r.Op == "p" {
			p.hasDelete = true
			continue
		}
		var patch map[string]any
		if json.Unmarshal([]byte(r.Patch), &patch) != nil {
			continue
		}
		for f, v := range patch {
			mp, typed := v.(map[string]any)
			if !typed {
				p.hlc[f] = max(p.hlc[f], r.HLC)
				continue
			}
			if d, ok := mp["$inc"].(float64); ok {
				p.inc[f] += d
			}
			add, _ := mp["$add"].([]any)
			rm, _ := mp["$rm"].([]any)
			if len(add) > 0 || len(rm) > 0 {
				p.sets[f] = append(p.sets[f], setOp{add: add, rm: rm})
			}
		}
	}
	return p, nil
}

// applySet re-applies the pending set operations on the absolute list from
// the hub.
func (p *pending) applySet(field string, list []any) []any {
	ops := p.sets[field]
	if len(ops) == 0 {
		return list
	}
	out := append([]any(nil), list...)
	has := func(v any) int {
		bv, _ := json.Marshal(v)
		for i, x := range out {
			if bx, _ := json.Marshal(x); string(bx) == string(bv) {
				return i
			}
		}
		return -1
	}
	for _, op := range ops {
		for _, v := range op.add {
			if has(v) < 0 {
				out = append(out, v)
			}
		}
		for _, v := range op.rm {
			if i := has(v); i >= 0 {
				out = append(out[:i], out[i+1:]...)
			}
		}
	}
	return out
}
