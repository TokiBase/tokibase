//go:build no_sync

package embed_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tokibase/tokibase/embed"
)

func TestSyncUnavailableUnderNoSync(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) {
		o.Sync = &embed.SyncOptions{HubURL: "http://127.0.0.1:1"} // ignored under no_sync
	})
	defer stop(t, inst)
	s := inst.Sync()
	if err := s.Enroll(context.Background(), "", "x"); !errors.Is(err, embed.ErrSyncUnavailable) {
		t.Fatalf("Enroll: %v", err)
	}
	if _, err := s.Status(); !errors.Is(err, embed.ErrSyncUnavailable) {
		t.Fatalf("Status: %v", err)
	}
	if _, err := s.Next("x"); !errors.Is(err, embed.ErrSyncUnavailable) {
		t.Fatalf("Next: %v", err)
	}
	s.SetConditions(true, false, false, false)
	s.OnEvent(func([]byte) {})()
}
