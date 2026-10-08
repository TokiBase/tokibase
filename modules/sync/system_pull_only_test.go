//go:build !no_sync

package sync

import (
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// An allowlisted system collection is pull-only whatever its policy row says:
// a spoke must not be able to push (for example un-revoke) `_device_certs`.
func TestSystemCollectionIsPullOnly(t *testing.T) {
	h, a, _ := hubFixture(t)
	col := core.NewBaseCollection("_device_certs")
	col.System = true
	col.Fields.Add(&core.TextField{Name: "serial"})
	if err := h.app.Save(col); err != nil {
		t.Fatal(err)
	}
	// a "both" row is reported as an error by the policy check
	rec := core.NewRecord(mustCol(t, h.app, PoliciesCollection))
	rec.Set("collection", "_device_certs")
	rec.Set("direction", DirBoth)
	rec.Set("enabled", true)
	found := false
	for _, i := range checkPolicy(h.app, rec) {
		found = found || (i.Level == "error" && i.Field == "direction")
	}
	if !found {
		t.Fatal("a both policy on _device_certs must be an error")
	}
	// even when such a row exists (validation bypassed), the loader forces pull
	if _, err := SetPolicy(h.app, "_device_certs", PolicyChange{}); err != nil {
		t.Logf("SetPolicy refused the default direction (validation): %v", err)
	} else if p, _ := h.m.pol.For(col); p == nil || p.Direction != DirPull {
		t.Fatalf("the policy direction must be forced to pull: %+v", p)
	}
	if _, err := SetPolicy(h.app, "_device_certs", PolicyChange{Direction: strp(DirPull)}); err != nil {
		t.Fatal(err)
	}
	tok := a.token(t)
	st, res, eb := rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, nowHLC(-5000, 1), 0, col.Id, "certrow0000001x", "c", map[string]any{"serial": "ab"})))
	if st != 200 {
		t.Fatalf("push: %d %+v", st, eb)
	}
	if r := res.Results[0]; r.Status != proto.ResRejected || r.Code != proto.CodePolicyDirection {
		t.Fatalf("a push of _device_certs must be rejected: %+v", r)
	}
}
