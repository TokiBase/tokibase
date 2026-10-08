//go:build !no_sync

package sync

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// A model of the hub apply step on one record: values, field clocks and the
// record clock, with the same rules as applyOne (hub_apply.go).
type modelRec struct {
	vals      map[string]any
	clocks    map[string]hlc.HLC
	metaHLC   hlc.HLC
	metaNode  string
	conflicts []*ConflictInfo
	status    []string
}

type modelChange struct {
	node  string
	hlc   hlc.HLC
	base  hlc.HLC
	patch map[string]any
}

func newModel(vals map[string]any, at hlc.HLC, node string) *modelRec {
	m := &modelRec{vals: map[string]any{}, clocks: map[string]hlc.HLC{}, metaHLC: at, metaNode: node}
	for k, v := range vals {
		m.vals[k] = v
		m.clocks[k] = at
	}
	return m
}

var modelTypes = map[string]string{"fee": TypeCounter, "scans": TypeCounter, "tags": TypeSet}

func (m *modelRec) apply(strategy string, review bool, c modelChange, hook func() HookDecision) Resolution {
	cur := map[string]any{}
	for k, v := range m.vals {
		cur[k] = v
	}
	d := Resolve(ResolveInput{
		Strategy: strategy, Review: review, Concurrent: c.base != m.metaHLC, MetaHLC: m.metaHLC, MetaNode: m.metaNode,
		Clocks: m.clocks, Base: c.base, HLC: c.hlc, Node: c.node, Patch: c.patch, Types: modelTypes, Current: cur, Hook: hook,
	})
	m.status = append(m.status, fmt.Sprint(d.Verdict, d.Merged))
	if d.Conflict != nil {
		m.conflicts = append(m.conflicts, d.Conflict)
	}
	if d.Verdict != VerdictApply {
		return d
	}
	for k, v := range d.Patch {
		if op, ok := opOf(modelTypes, k, v); ok {
			switch modelTypes[k] {
			case TypeCounter:
				f, _ := m.vals[k].(float64)
				m.vals[k] = f + opDelta(op)
			case TypeSet:
				l, _ := m.vals[k].([]any)
				add, rm := opLists(op)
				m.vals[k] = setApply(l, add, rm)
			}
			continue
		}
		m.vals[k] = v
		if strategy == StratFieldMerge {
			m.clocks[k] = max(m.clocks[k], c.hlc)
		}
	}
	if !d.KeepMeta && hlc.Less(m.metaHLC, m.metaNode, c.hlc, c.node) {
		m.metaHLC, m.metaNode = c.hlc, c.node
	}
	return d
}

func (m *modelRec) canon() string {
	keys := sortedKeys(m.vals)
	s := ""
	for _, k := range keys {
		v := m.vals[k]
		if l, ok := v.([]any); ok {
			strs := make([]string, len(l))
			for i, x := range l {
				strs[i] = fmt.Sprint(x)
			}
			sort.Strings(strs)
			v = strs
		}
		s += fmt.Sprintf("%s=%v;", k, v)
	}
	return s
}

func permutations(n int) [][]int {
	var out [][]int
	var rec func(a []int, k int)
	rec = func(a []int, k int) {
		if k == len(a) {
			out = append(out, append([]int(nil), a...))
			return
		}
		for i := k; i < len(a); i++ {
			a[k], a[i] = a[i], a[k]
			rec(a, k+1)
			a[k], a[i] = a[i], a[k]
		}
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	rec(idx, 0)
	return out
}

func at(min int, logical uint16) hlc.HLC { return hlc.Make(int64(1_000_000+min*60_000), logical) }

// §4.2 worked example: gate-1 sets status=closed at 10:05, the offline phone
// sets status=disputed at 10:03 with base 09:00. Whatever the arrival order the
// hub ends on closed; the phone's change is superseded when it arrives second.
func TestResolveLWWWorkedExample(t *testing.T) {
	base := at(0, 0) // 09:00
	closed := modelChange{"gate-1", at(65, 0), base, map[string]any{"status": "closed"}}
	disputed := modelChange{"phone", at(63, 0), base, map[string]any{"status": "disputed"}}
	for _, order := range permutations(2) {
		m := newModel(map[string]any{"status": "open"}, base, "hub")
		cs := []modelChange{closed, disputed}
		var last Resolution
		for _, i := range order {
			last = m.apply(StratLWW, false, cs[i], nil)
		}
		if m.vals["status"] != "closed" {
			t.Fatalf("order %v: status %v", order, m.vals["status"])
		}
		if order[0] == 0 { // closed first: the phone arrives concurrent with a lower hlc
			if last.Verdict != VerdictSuperseded || last.Conflict == nil || last.Conflict.Resolution != ResolutionAutoLWW ||
				last.Conflict.Status != ConflictResolved || last.Conflict.Kind != KindConcurrentField {
				t.Fatalf("phone must be superseded with an auto_lww row: %+v", last)
			}
		}
	}
}

// §4.4 worked example (plate/fee/note) in all 24 arrival orders.
func TestResolveFieldMergeWorkedExample(t *testing.T) {
	base := at(0, 0)
	cs := []modelChange{
		{"gate-1", at(65, 0), base, map[string]any{"fee": map[string]any{"$inc": 5000.0}}},
		{"phone", at(67, 0), base, map[string]any{"note": "scratch on door"}},
		{"gate-2", at(66, 0), base, map[string]any{"plate": "B1243"}},
		{"phone", at(68, 0), base, map[string]any{"plate": "B1234X"}},
	}
	for _, order := range permutations(4) {
		m := newModel(map[string]any{"plate": "B1234", "fee": 0.0, "note": ""}, base, "hub")
		for _, i := range order {
			m.apply(StratFieldMerge, false, cs[i], nil)
		}
		if m.vals["fee"] != 5000.0 || m.vals["note"] != "scratch on door" || m.vals["plate"] != "B1234X" {
			t.Fatalf("order %v: %v", order, m.canon())
		}
		var plate *ConflictInfo
		for _, c := range m.conflicts {
			if _, ok := c.Incoming["plate"]; ok {
				plate = c
			}
		}
		if plate == nil || plate.Kind != KindConcurrentField || plate.Resolution != ResolutionAutoMerge || plate.Status != ConflictResolved {
			t.Fatalf("order %v: want one resolved concurrent_field row for plate, got %+v", order, m.conflicts)
		}
		// the row records B1243 vs B1234X: the loser's value is in `incoming` or `current`
		got := map[any]bool{plate.Incoming["plate"]: true, plate.Current["plate"]: true}
		if !got["B1243"] || !got["B1234X"] {
			t.Fatalf("order %v: plate row %v / %v", order, plate.Incoming, plate.Current)
		}
		if len(m.conflicts) != 1 {
			t.Fatalf("order %v: exactly one conflict row expected, got %d", order, len(m.conflicts))
		}
	}
}

func TestResolveFieldMergeReviewKeepsConflictOpen(t *testing.T) {
	base := at(0, 0)
	m := newModel(map[string]any{"plate": "B1"}, base, "hub")
	m.apply(StratFieldMerge, true, modelChange{"a", at(5, 0), base, map[string]any{"plate": "B2"}}, nil)
	m.apply(StratFieldMerge, true, modelChange{"b", at(6, 0), base, map[string]any{"plate": "B3"}}, nil)
	if len(m.conflicts) != 1 || m.conflicts[0].Status != ConflictOpen || m.vals["plate"] != "B3" {
		t.Fatalf("review: want the merge applied and the row open: %+v %v", m.conflicts, m.vals)
	}
}

// §4.5 counter example: hub scans=10, +3, +2, +1 in any order gives 16.
func TestResolveCounterAnyOrder(t *testing.T) {
	base := at(0, 0)
	inc := func(n float64) map[string]any { return map[string]any{"scans": map[string]any{"$inc": n}} }
	cs := []modelChange{{"gate-1", at(5, 0), base, inc(3)}, {"phone", at(6, 0), base, inc(2)}, {"gate-2", at(7, 0), base, inc(1)}}
	for _, strat := range []string{StratLWW, StratFieldMerge, StratHook} {
		for _, order := range permutations(3) {
			m := newModel(map[string]any{"scans": 10.0}, base, "hub")
			for _, i := range order {
				m.apply(strat, false, cs[i], func() HookDecision { return HookDecision{Resolution: kernel.SyncResolveAccept} })
			}
			if m.vals["scans"] != 16.0 {
				t.Fatalf("%s order %v: scans %v, want 16", strat, order, m.vals["scans"])
			}
		}
	}
}

// §4.5 set example: hub {a}; gate-1 adds b; the phone removes a and adds c.
func TestResolveSetAnyOrder(t *testing.T) {
	base := at(0, 0)
	cs := []modelChange{
		{"gate-1", at(5, 0), base, map[string]any{"tags": map[string]any{"$add": []any{"b"}}}},
		{"phone", at(6, 0), base, map[string]any{"tags": map[string]any{"$rm": []any{"a"}, "$add": []any{"c"}}}},
	}
	for _, strat := range []string{StratLWW, StratFieldMerge} {
		for _, order := range permutations(2) {
			m := newModel(map[string]any{"tags": []any{"a"}}, base, "hub")
			for _, i := range order {
				m.apply(strat, false, cs[i], nil)
			}
			if m.canon() != "tags=[b c];" {
				t.Fatalf("%s order %v: %s", strat, order, m.canon())
			}
		}
	}
}

// The documented limit: remove x and add x leave x present only if the add arrives last.
func TestResolveSetKnownLimit(t *testing.T) {
	base := at(0, 0)
	rm := modelChange{"a", at(5, 0), base, map[string]any{"tags": map[string]any{"$rm": []any{"x"}}}}
	add := modelChange{"b", at(6, 0), base, map[string]any{"tags": map[string]any{"$add": []any{"x"}}}}
	for _, c := range []struct {
		order []modelChange
		want  string
	}{{[]modelChange{rm, add}, "tags=[x];"}, {[]modelChange{add, rm}, "tags=[];"}} {
		m := newModel(map[string]any{"tags": []any{"x"}}, base, "hub")
		for _, ch := range c.order {
			m.apply(StratLWW, false, ch, nil)
		}
		if m.canon() != c.want {
			t.Fatalf("got %s want %s", m.canon(), c.want)
		}
	}
}

func TestResolveLWWLoserKeepsTypedOps(t *testing.T) {
	base := at(0, 0)
	m := newModel(map[string]any{"title": "t", "fee": 0.0}, base, "hub")
	m.apply(StratLWW, false, modelChange{"a", at(9, 0), base, map[string]any{"title": "new"}}, nil)
	d := m.apply(StratLWW, false, modelChange{"b", at(5, 0), base,
		map[string]any{"title": "lost", "fee": map[string]any{"$inc": 7.0}}}, nil)
	if d.Verdict != VerdictApply || !d.Merged || !d.KeepMeta || m.vals["fee"] != 7.0 || m.vals["title"] != "new" {
		t.Fatalf("%+v %v", d, m.vals)
	}
	if d.Conflict == nil || d.Conflict.Resolution != ResolutionAutoLWW {
		t.Fatalf("the lost title is recorded: %+v", d.Conflict)
	}
}

func TestResolveHubWinsRejects(t *testing.T) {
	base := at(0, 0)
	m := newModel(map[string]any{"name": "ref"}, base, "hub")
	// not concurrent: applies
	if d := m.apply(StratHubWins, false, modelChange{"a", at(5, 0), base, map[string]any{"name": "x"}}, nil); d.Verdict != VerdictApply {
		t.Fatalf("%+v", d)
	}
	// concurrent: rejected, whatever the hlc, typed ops included
	d := m.apply(StratHubWins, false, modelChange{"b", at(99, 0), base,
		map[string]any{"name": "y", "fee": map[string]any{"$inc": 1.0}}}, nil)
	if d.Verdict != VerdictReject || d.Code != proto.CodeHubWins || d.Conflict == nil || d.Conflict.Kind != KindHubWins ||
		d.Conflict.Resolution != ResolutionReverted || d.Conflict.Status != ConflictResolved || m.vals["name"] != "x" {
		t.Fatalf("%+v %v", d, m.vals)
	}
}

func TestResolveHookDecisions(t *testing.T) {
	base := at(0, 0)
	patch := map[string]any{"status": "paid", "fee": map[string]any{"$inc": 2.0}}
	run := func(h func() HookDecision, _ bool) Resolution {
		return Resolve(ResolveInput{Strategy: StratHook, Concurrent: true, MetaHLC: at(5, 0), MetaNode: "hub", Base: base,
			HLC: at(6, 0), Node: "phone", Patch: patch, Types: modelTypes, Current: map[string]any{"status": "paid"}, Hook: h})
	}
	d := run(func() HookDecision { return HookDecision{Resolution: "accept"} }, false)
	if d.Verdict != VerdictApply || !reflect.DeepEqual(d.Patch, patch) || d.Merged || d.Conflict.Resolution != ResolutionAccepted {
		t.Fatalf("accept: %+v", d)
	}
	d = run(func() HookDecision {
		return HookDecision{Resolution: "merge", Patch: map[string]any{"note": "dup"}, Message: "double payment"}
	}, false)
	if d.Verdict != VerdictApply || !d.Merged || d.Patch["note"] != "dup" || d.Patch["fee"] == nil || d.Patch["status"] != nil ||
		d.Conflict.Note != "double payment" {
		t.Fatalf("merge keeps typed ops and drops the other incoming fields: %+v", d)
	}
	d = run(func() HookDecision { return HookDecision{Resolution: "reject", Message: "no"} }, false)
	if d.Verdict != VerdictReject || d.Code != proto.CodeHookRejected || d.Conflict.Resolution != ResolutionRejected || d.Conflict.Status != ConflictResolved {
		t.Fatalf("reject: %+v", d)
	}
	d = run(func() HookDecision { return HookDecision{Resolution: "park"} }, false)
	if d.Verdict != VerdictPark || d.Code != CodeHookParked || d.Conflict.Status != ConflictOpen || d.Conflict.Resolution != ResolutionParked {
		t.Fatalf("park: %+v", d)
	}
	for name, h := range map[string]func() HookDecision{
		"nil":     nil,
		"error":   func() HookDecision { return HookDecision{Err: errors.New("trap")} },
		"unknown": func() HookDecision { return HookDecision{Resolution: "overwrite"} },
		"empty":   func() HookDecision { return HookDecision{} },
	} {
		d = run(h, false)
		if d.Verdict != VerdictPark || d.Code != proto.CodeHookFailed || d.Conflict.Kind != KindHookFailed || d.Conflict.Status != ConflictOpen {
			t.Fatalf("%s must fail closed: %+v", name, d)
		}
	}
	// a non concurrent change never asks the hook
	d = Resolve(ResolveInput{Strategy: StratHook, Patch: patch, Types: modelTypes,
		Hook: func() HookDecision { t.Fatal("hook called"); return HookDecision{} }})
	if d.Verdict != VerdictApply {
		t.Fatalf("%+v", d)
	}
}

func TestValidateTypedFieldTypes(t *testing.T) {
	e := setup(t)
	types := map[string]string{"qty": TypeCounter, "tags": TypeSet}
	fields := map[string]core.Field{}
	for _, f := range syncedFields(e.items, nil) {
		fields[f.GetName()] = f
	}
	cases := []struct {
		name  string
		patch map[string]any
		isNew bool
		ok    bool
	}{
		{"inc on counter", map[string]any{"qty": map[string]any{"$inc": 3.0}}, false, true},
		{"absolute counter on update", map[string]any{"qty": 0.0}, false, false},
		{"absolute counter on create", map[string]any{"qty": 5.0}, true, true},
		{"inc on a plain number", map[string]any{"total": map[string]any{"$inc": 1000000.0}}, false, false},
		{"add on a plain text", map[string]any{"title": map[string]any{"$add": []any{"x"}}}, false, false},
		{"unknown $ key on counter", map[string]any{"qty": map[string]any{"$inc": 1.0, "$set": 9.0}}, false, false},
		{"string delta", map[string]any{"qty": map[string]any{"$inc": "5"}}, false, false},
		{"unknown op on set", map[string]any{"tags": map[string]any{"$inc": 1.0}}, false, false},
		{"set ops", map[string]any{"tags": map[string]any{"$add": []any{"a"}, "$rm": []any{"b"}}}, false, true},
		{"absolute set on update", map[string]any{"tags": []any{"a"}}, false, false},
		{"absolute set on create", map[string]any{"tags": []any{"a"}}, true, true},
		{"set add not a list", map[string]any{"tags": map[string]any{"$add": "a"}}, false, false},
		{"json field may hold $keys", map[string]any{"meta": map[string]any{"$add": 1.0}}, false, true},
		{"plain values", map[string]any{"title": "x", "total": 3.0}, false, true},
	}
	for _, c := range cases {
		rj := validateTyped(types, fields, c.patch, c.isNew)
		if (rj == nil) != c.ok {
			t.Errorf("%s: got %v", c.name, rj)
		}
		if rj != nil && rj.code != proto.CodeValidationFailed {
			t.Errorf("%s: code %s", c.name, rj.code)
		}
	}
	// a JSON field's {"$add":..} is a plain value for the typed helpers
	if _, ok := opOf(types, "meta", map[string]any{"$add": []any{1.0}}); ok {
		t.Error("meta is not a set")
	}
	if _, ok := opOf(types, "qty", map[string]any{"$inc": 1.0}); !ok {
		t.Error("qty is a counter")
	}
}
