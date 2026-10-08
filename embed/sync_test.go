//go:build !no_sync

package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tokibase/tokibase/embed"
	"github.com/tokibase/tokibase/modules/sync"
)

// Without Options.Sync the facade exists but reports that this node is no spoke.
func TestSyncFacadeWithoutSpoke(t *testing.T) {
	inst := start(t, t.TempDir(), nil)
	defer stop(t, inst)
	s := inst.Sync()
	if _, err := s.Status(); !errors.Is(err, sync.ErrNotSpoke) {
		t.Fatalf("Status: %v", err)
	}
	if err := s.Enroll(context.Background(), "http://127.0.0.1:1", "x"); !errors.Is(err, sync.ErrNotSpoke) {
		t.Fatalf("Enroll: %v", err)
	}
	if _, err := s.Next("tickets"); !errors.Is(err, sync.ErrNotSpoke) {
		t.Fatalf("Next: %v", err)
	}
	s.SetConditions(true, false, false, false) // no panic
	s.OnEvent(func([]byte) {})()               // no-op cancel
}

// Options.Sync on a nano instance defaults the role to spoke; the node is not
// enrolled yet, so the calls that need a hub say so.
func TestSyncOptionsSpokeDefaults(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) {
		o.Profile = "nano"
		o.Sync = &embed.SyncOptions{HubURL: "http://127.0.0.1:1", Interval: "1s"}
		o.Env = map[string]string{"TOKI_SYNC_INSECURE": "1"}
	})
	defer stop(t, inst)
	s := inst.Sync()
	if _, err := s.Status(); !errors.Is(err, sync.ErrNotEnrolled) {
		t.Fatalf("Status before enroll: %v", err)
	}
	if err := s.Now(context.Background()); !errors.Is(err, sync.ErrNotEnrolled) {
		t.Fatalf("Now before enroll: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Enroll(ctx, "", "not-a-code"); err == nil {
		t.Fatal("enrolling at an unreachable hub must fail")
	}
	if _, err := s.AddActor(ctx, "token"); !errors.Is(err, sync.ErrNotEnrolled) {
		t.Fatalf("AddActor before enroll: %v", err)
	}
	s.SetConditions(false, true, true, false)
	var ev map[string]any
	got := make(chan []byte, 1)
	cancelEv := s.OnEvent(func(b []byte) { got <- b })
	cancelEv()
	cancelEv() // idempotent
	select {
	case b := <-got:
		_ = json.Unmarshal(b, &ev)
	default:
	}
}

func TestSyncOptionsNeedNanoOrEdge(t *testing.T) {
	_, err := embed.Start(embed.Options{DataDir: t.TempDir(), Profile: "solo", Sync: &embed.SyncOptions{HubURL: "http://127.0.0.1:1"}})
	if err == nil {
		t.Fatal("Options.Sync on a solo profile must be refused")
	}
}
