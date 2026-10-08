package edgeguard

import (
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	a := ParseAllow(" gates , staff/abc ,")
	for actor, want := range map[string]bool{"gates/1": true, "staff/abc": true, "staff/x": false, "users/1": false, "": false} {
		if a.MatchActor(actor) != want {
			t.Errorf("%q = %v, want %v", actor, !want, want)
		}
	}
	if !ParseAllow("").Empty() || ParseAllow("a").Empty() {
		t.Error("Empty")
	}
}

func TestThrottle(t *testing.T) {
	now := time.Unix(1000, 0)
	th := NewThrottle(2, time.Minute)
	th.SetClock(func() time.Time { return now })
	if !th.Allow("a") || !th.Allow("a") || th.Allow("a") {
		t.Fatal("limit of 2")
	}
	if !th.Allow("b") {
		t.Fatal("keys are independent")
	}
	now = now.Add(61 * time.Second)
	if !th.Allow("a") {
		t.Fatal("window passed")
	}
	if !NewThrottle(0, time.Minute).Allow("x") {
		t.Fatal("limit 0 disables")
	}
}
