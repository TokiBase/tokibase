//go:build !no_sync

package sync

import (
	"strings"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Conflict strategies of a policy (docs/SYNC_DESIGN.md §4).
const (
	StratLWW        = "lww"
	StratHubWins    = "hub-wins"
	StratFieldMerge = "field-merge"
	StratHook       = "hook"
)

// validStrategy reports whether s names a strategy ("" = the default lww).
func validStrategy(s string) bool {
	switch s {
	case "", StratLWW, StratHubWins, StratFieldMerge, StratHook:
		return true
	}
	return false
}

// Conflict kinds (the `_sync_conflicts.kind` values this PR writes).
const (
	KindConcurrentField = "concurrent_field"
	KindHubWins         = "hub_wins"
	KindHookFailed      = "hook_failed"
)

// Conflict resolutions and statuses.
const (
	ResolutionAutoLWW   = "auto_lww"
	ResolutionAutoMerge = "auto_merge"
	ResolutionReverted  = "reverted"
	ResolutionParked    = "parked"
	ResolutionAccepted  = "accepted"
	ResolutionRejected  = "rejected"

	ConflictOpen     = "open"
	ConflictResolved = "resolved"
)

// Codes of the hook strategy that are not in proto (hook_parked is the guest's
// own decision to park; hook_failed is a failure and also a conflict kind).
const (
	CodeHookParked = "hook_parked"
)

// Verdict is what the resolver decided for a pushed change.
type Verdict int

const (
	// VerdictApply writes the effective patch.
	VerdictApply Verdict = iota
	// VerdictSuperseded writes nothing (the change lost).
	VerdictSuperseded
	// VerdictReject refuses the change with Code; the node gets a revert.
	VerdictReject
	// VerdictPark writes nothing and keeps the change for an admin.
	VerdictPark
)

// HookDecision is the answer of the hook strategy (the wasm guest).
type HookDecision struct {
	Resolution string
	Patch      map[string]any
	Message    string
	// Err is set when no decision could be made (no handler, trap, timeout,
	// invalid output): fail closed.
	Err error
}

// ResolveInput is everything the resolver needs; it has no database access, so
// the strategies are testable on their own.
type ResolveInput struct {
	Strategy string
	Review   bool
	// Concurrent: the record exists on the hub and change.base != meta.hlc.
	Concurrent bool
	MetaHLC    hlc.HLC
	MetaNode   string
	// Clocks are the field clocks of the record (field-merge).
	Clocks map[string]hlc.HLC
	Base   hlc.HLC
	HLC    hlc.HLC
	Node   string
	Patch  map[string]any
	// Types are the policy field types (counter / set).
	Types map[string]string
	// Current are the hub values of the synced fields (conflict rows only).
	Current map[string]any
	// Hook asks the hook strategy; nil = no handler (park, hook_failed).
	Hook func() HookDecision
}

// ConflictInfo describes the `_sync_conflicts` row to write.
type ConflictInfo struct {
	Kind       string
	Strategy   string
	Resolution string
	Status     string
	Incoming   map[string]any
	Current    map[string]any
	Note       string
}

// Resolution is the resolver's answer.
type Resolution struct {
	Verdict Verdict
	// Patch is the effective patch of VerdictApply.
	Patch map[string]any
	// Merged: the effective patch differs from the pushed one, so the node
	// must receive the hub state to converge (push status "merged").
	Merged bool
	// KeepMeta keeps the record clock of the current winner (an lww loser
	// whose counter/set operations were applied).
	KeepMeta bool
	// Code is the rejection / park code.
	Code     string
	Conflict *ConflictInfo
	// ClockFields are the plain fields written; field-merge raises their
	// clock to the change HLC.
	ClockFields []string
}

// Resolve decides a pushed create/update. A non concurrent change always
// applies (§4.1); the strategy only matters when the writer did not see the
// latest hub version. Counter and set operations never conflict (§4.5) except
// under hub-wins, hook reject and hook park, which refuse the whole change.
func Resolve(in ResolveInput) Resolution {
	plain, typed := splitTyped(in.Types, in.Patch)
	applyAll := func() Resolution {
		return Resolution{Verdict: VerdictApply, Patch: in.Patch, ClockFields: sortedKeys(plain)}
	}
	if !in.Concurrent {
		return applyAll()
	}
	subset := func(m map[string]any, keys []string) map[string]any {
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			if v, ok := m[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	switch in.Strategy {
	case StratHubWins:
		return Resolution{
			Verdict: VerdictReject, Code: proto.CodeHubWins,
			Conflict: &ConflictInfo{Kind: KindHubWins, Strategy: StratHubWins, Resolution: ResolutionReverted, Status: ConflictResolved,
				Incoming: in.Patch, Current: subset(in.Current, sortedKeys(in.Patch))},
		}

	case StratFieldMerge:
		applied := map[string]any{}
		var won, lost []string
		for _, f := range sortedKeys(plain) {
			clock := in.Clocks[f]
			if clock <= in.Base { // unchanged since the writer's base
				applied[f] = plain[f]
				continue
			}
			if in.HLC > clock || (in.HLC == clock && in.Node > in.MetaNode) {
				applied[f] = plain[f]
				won = append(won, f)
			} else {
				lost = append(lost, f)
			}
		}
		eff := map[string]any{}
		for k, v := range applied {
			eff[k] = v
		}
		for k, v := range typed {
			eff[k] = v
		}
		var ci *ConflictInfo
		if cf := append(append([]string{}, won...), lost...); len(cf) > 0 {
			st := ConflictResolved
			if in.Review {
				st = ConflictOpen
			}
			ci = &ConflictInfo{Kind: KindConcurrentField, Strategy: StratFieldMerge, Resolution: ResolutionAutoMerge, Status: st,
				Incoming: subset(in.Patch, cf), Current: subset(in.Current, cf),
				Note: noteFields(won, lost)}
		}
		if len(eff) == 0 && len(lost) > 0 {
			return Resolution{Verdict: VerdictSuperseded, Code: proto.CodeSuperseded, Conflict: ci}
		}
		return Resolution{Verdict: VerdictApply, Patch: eff, Merged: len(lost) > 0, Conflict: ci, ClockFields: sortedKeys(applied)}

	case StratHook:
		return resolveHook(in, plain, typed)
	}

	// lww (also the fallback for an unknown strategy)
	if hlc.Less(in.MetaHLC, in.MetaNode, in.HLC, in.Node) {
		return applyAll() // the incoming change wins; fields not in the patch keep the hub values
	}
	ci := &ConflictInfo{Kind: KindConcurrentField, Strategy: StratLWW, Resolution: ResolutionAutoLWW, Status: ConflictResolved,
		Incoming: subset(in.Patch, sortedKeys(plain)), Current: subset(in.Current, sortedKeys(plain))}
	if len(typed) == 0 {
		return Resolution{Verdict: VerdictSuperseded, Code: proto.CodeSuperseded, Conflict: ci}
	}
	// lost: only the counter/set operations are applied (they never conflict)
	return Resolution{Verdict: VerdictApply, Patch: typed, Merged: true, KeepMeta: true, Conflict: ci}
}

func noteFields(won, lost []string) string {
	var parts []string
	if len(won) > 0 {
		parts = append(parts, "incoming won: "+strings.Join(won, ","))
	}
	if len(lost) > 0 {
		parts = append(parts, "hub kept: "+strings.Join(lost, ","))
	}
	return strings.Join(parts, "; ")
}

func resolveHook(in ResolveInput, plain, typed map[string]any) Resolution {
	d := HookDecision{Err: errNoHookHandler}
	if in.Hook != nil {
		d = in.Hook()
	}
	ci := func(res, status, note string, kind string) *ConflictInfo {
		return &ConflictInfo{Kind: kind, Strategy: StratHook, Resolution: res, Status: status,
			Incoming: in.Patch, Current: in.Current, Note: note}
	}
	park := func(code, kind, note string) Resolution {
		return Resolution{Verdict: VerdictPark, Code: code, Conflict: ci(ResolutionParked, ConflictOpen, note, kind)}
	}
	if d.Err != nil {
		return park(proto.CodeHookFailed, KindHookFailed, "hook failed: "+d.Err.Error())
	}
	switch d.Resolution {
	case kernel.SyncResolveAccept:
		return Resolution{Verdict: VerdictApply, Patch: in.Patch, ClockFields: sortedKeys(plain),
			Conflict: ci(ResolutionAccepted, ConflictResolved, d.Message, KindConcurrentField)}
	case kernel.SyncResolveMerge:
		eff := map[string]any{}
		for k, v := range typed { // counter/set operations never conflict
			eff[k] = v
		}
		for k, v := range d.Patch {
			eff[k] = v
		}
		p2, _ := splitTyped(in.Types, eff)
		return Resolution{Verdict: VerdictApply, Patch: eff, Merged: true, ClockFields: sortedKeys(p2),
			Conflict: ci(ResolutionAutoMerge, ConflictResolved, d.Message, KindConcurrentField)}
	case kernel.SyncResolveReject:
		return Resolution{Verdict: VerdictReject, Code: proto.CodeHookRejected,
			Conflict: ci(ResolutionRejected, ConflictResolved, d.Message, KindConcurrentField)}
	case kernel.SyncResolvePark:
		return park(CodeHookParked, KindConcurrentField, d.Message)
	}
	return park(proto.CodeHookFailed, KindHookFailed, "hook returned an unknown resolution "+quote(d.Resolution))
}

func quote(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return "\"" + s + "\""
}
