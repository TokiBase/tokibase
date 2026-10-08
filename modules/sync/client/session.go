//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// tokenValid reports whether the session token has at least 30 s left.
func (c *Client) tokenValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != "" && c.now().Add(c.offset).Add(30*time.Second).Before(c.tokenExp)
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// ensureSession runs the handshake when there is no valid session (or the
// previous one was dropped) and processes its answer.
func (c *Client) ensureSession(ctx context.Context) error {
	c.loop.mu.Lock()
	need := c.loop.needHS
	c.loop.mu.Unlock()
	if !need && c.tokenValid() {
		return nil
	}
	hs, err := c.Handshake(ctx)
	if err != nil {
		return err
	}
	c.handleEpoch(hs)
	if hs.Rebootstrap && !c.isBootstrapping() {
		c.markRebootstrap(hs.LowWater)
		return ErrRebootstrap
	}
	if err := c.applyPolicies(hs.Policies); err != nil {
		return err
	}
	if err := c.reconcile(hs.PushFrom); err != nil {
		return err
	}
	c.loop.mu.Lock()
	c.loop.needHS = false
	c.loop.pushFrom = hs.PushFrom
	c.loop.mu.Unlock()
	return nil
}

// authed sends an authenticated request; one 401 triggers a new handshake.
func (c *Client) authed(ctx context.Context, method, path string, hdr map[string]string, body []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := c.ensureSession(ctx); err != nil {
			return nil, err
		}
		h := map[string]string{"Authorization": "Bearer " + c.Token()}
		for k, v := range hdr {
			h[k] = v
		}
		_, b, err := c.do(ctx, method, path, h, body)
		if err != nil && attempt == 0 && IsCode(err, proto.CodeUnauthorized) {
			c.dropToken()
			c.loop.mu.Lock()
			c.loop.needHS = true
			c.loop.mu.Unlock()
			continue
		}
		return b, err
	}
}

// reconcile aligns the local statuses with the hub's contiguous position:
// everything below pushFrom is final (acked); an "acked" row at or above it
// was lost on the hub (restore, failover) and is sent again.
func (c *Client) reconcile(pushFrom int64) error {
	if c.o.App == nil {
		return nil
	}
	db := c.o.App.NonconcurrentDB()
	if _, err := db.NewQuery("UPDATE _changes SET status='acked' WHERE node={:n} AND origin_seq<{:p} AND status IN ('local','pushed')").
		Bind(dbx.Params{"n": c.nodeID, "p": pushFrom}).Execute(); err != nil {
		return err
	}
	if _, err := db.NewQuery("UPDATE _changes SET status='pushed' WHERE node={:n} AND origin_seq>={:p} AND status='acked'").
		Bind(dbx.Params{"n": c.nodeID, "p": pushFrom}).Execute(); err != nil {
		return err
	}
	_, err := db.NewQuery("UPDATE _sync_cursors SET acked_origin={:a}").Bind(dbx.Params{"a": max(pushFrom-1, 0)}).Execute()
	return err
}

// applyPolicies makes the local `_sync_policies` equal to the hub's list (the
// hub is the authority; schema bundles with the full config arrive in PR8).
func (c *Client) applyPolicies(ps []proto.Policy) error {
	if c.o.App == nil {
		return nil
	}
	app := c.o.App
	pc, err := app.FindCollectionByNameOrId("_sync_policies")
	if err != nil {
		return err
	}
	existing, err := app.FindAllRecords(pc)
	if err != nil {
		return err
	}
	byCol := map[string]int{}
	for i, r := range existing {
		byCol[r.GetString("collection")] = i
	}
	seen := map[string]bool{}
	for _, p := range ps {
		seen[p.Collection] = true
		i, ok := byCol[p.Collection]
		if !ok {
			r := newPolicyRecord(app, pc, p)
			if err := app.Save(r); err != nil {
				return err
			}
			continue
		}
		r := existing[i]
		var ft map[string]string
		var ex []string
		if raw, _ := json.Marshal(r.Get("field_types")); len(raw) > 0 {
			_ = json.Unmarshal(raw, &ft)
		}
		if raw, _ := json.Marshal(r.Get("exclude")); len(raw) > 0 {
			_ = json.Unmarshal(raw, &ex)
		}
		if r.GetString("direction") == p.Direction && r.GetBool("enabled") &&
			reflect.DeepEqual(emptyIfNil(ft), emptyIfNil(p.FieldTypes)) && reflect.DeepEqual(nilIfEmpty(ex), nilIfEmpty(p.Exclude)) {
			continue
		}
		r.Set("direction", p.Direction)
		r.Set("field_types", p.FieldTypes)
		r.Set("exclude", p.Exclude)
		r.Set("enabled", true)
		if err := app.Save(r); err != nil {
			return err
		}
	}
	for _, r := range existing {
		if !seen[r.GetString("collection")] {
			if err := app.Delete(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func emptyIfNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func newPolicyRecord(app core.App, pc *core.Collection, p proto.Policy) *core.Record {
	r := core.NewRecord(pc)
	r.Set("collection", p.Collection)
	r.Set("direction", p.Direction)
	r.Set("field_types", p.FieldTypes)
	r.Set("exclude", p.Exclude)
	r.Set("enabled", true)
	return r
}
