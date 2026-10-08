//go:build !no_wasm

package wasm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// syncSyncConflict binds the sync.conflict handler only while a loaded module
// declares such an event, so a hub without them runs no extra handler (and
// modules/sync parks the conflict at once: fail closed). Called from Reload
// with h.mu held.
func (h *Host) syncSyncConflict(r *registry) {
	want := false
	for _, m := range r.mods {
		for _, e := range m.Parsed {
			if e.Kind == KindSync {
				want = true
			}
		}
	}
	hooks := kernel.OnSyncConflictFor(h.app)
	switch {
	case want && !h.syncBound:
		hooks.Bind(&hook.Handler[*kernel.SyncConflictEvent]{Id: hookID, Func: h.onSyncConflict})
		h.syncBound = true
	case !want && h.syncBound:
		hooks.Unbind(hookID)
		h.syncBound = false
	}
}

func (h *Host) matchSync(collection, only string) []*Module {
	var out []*Module
	for _, m := range h.reg.Load().mods {
		if only != "" && m.Name != only {
			continue
		}
		for _, e := range m.Parsed {
			if e.matchSync(collection) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// syncConflictBudget caps the time all modules may spend on one conflict:
// the hub holds its apply lock while the guests run.
const syncConflictBudget = 10 * time.Second

var syncResolutions = map[string]bool{
	kernel.SyncResolveAccept: true, kernel.SyncResolveReject: true,
	kernel.SyncResolveMerge: true, kernel.SyncResolvePark: true,
}

// onSyncConflict runs the modules subscribed to the collection (only the one
// named by the policy, when it names one) until one decides. Any failure (trap,
// timeout, bad output, unknown resolution, a rejection `ok:false`) returns an
// error: modules/sync then parks the change with code hook_failed.
func (h *Host) onSyncConflict(e *kernel.SyncConflictEvent) error {
	if e.Collection == nil {
		return e.Next()
	}
	mods := h.matchSync(e.Collection.Name, e.Hook)
	if len(mods) == 0 {
		return e.Next() // nobody answers: the caller parks (fail closed)
	}
	in := buildSyncIn(e)
	dl := time.Now().Add(syncConflictBudget)
	if !e.Deadline.IsZero() && e.Deadline.Before(dl) {
		dl = e.Deadline // the budget of the whole push
	}
	for _, m := range mods {
		ev := &EventIn{Event: "sync.conflict." + e.Collection.Name, Kind: "sync", Phase: "before",
			Collection: e.Collection.Name, Sync: in, Actor: Actor{Kind: "system"}}
		ctx, cancel := context.WithDeadline(context.Background(), dl)
		res, err := h.Invoke(ctx, m, ev, CallOpts{App: e.App})
		cancel()
		if err != nil {
			return err
		}
		if !res.OK {
			msg := res.Message
			if msg == "" {
				msg = "the module refused to decide"
			}
			return fmt.Errorf("wasm module %s: %s", m.Name, msg)
		}
		switch r := strings.TrimSpace(res.Resolution); {
		case r == "":
			continue // this module has no opinion; ask the next
		case !syncResolutions[r]:
			return fmt.Errorf("wasm module %s: unknown resolution %q", m.Name, r)
		default:
			e.Resolution, e.Patch, e.Message = r, res.Patch, res.Message
			return nil
		}
	}
	return e.Next()
}

func buildSyncIn(e *kernel.SyncConflictEvent) *SyncIn {
	hx := func(v uint64) string { return fmt.Sprintf("%016x", v) }
	out := &SyncIn{
		RecordID: e.RecordID, Current: e.Current, CurrentHLC: hx(e.CurrentHLC), CurrentNode: e.CurrentNode,
		Incoming: SyncIncomingIn{Op: e.Incoming.Op, Node: e.Incoming.Node, HLC: hx(e.Incoming.HLC), BaseHLC: hx(e.Incoming.BaseHLC),
			Patch: e.Incoming.Patch,
			Actor: SyncActorIn{Kind: e.Incoming.ActorKind, ID: e.Incoming.ActorID, Collection: e.Incoming.ActorCollection}},
		FieldClocks: map[string]string{},
	}
	for f, c := range e.FieldClocks {
		out.FieldClocks[f] = hx(c)
	}
	return out
}
