//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// AddActor asks the hub for an actor grant for the user behind hubToken (a hub
// auth token the app got through the normal login), verifies the signed
// assertion, stores the grant in `_sync_actors` and creates the local copy of
// the auth record (without password and tokenKey) when it is missing
// (docs/SYNC_DESIGN.md §1.6).
func (c *Client) AddActor(ctx context.Context, hubToken string) (*proto.ActorResponse, error) {
	if c.o.App == nil {
		return nil, errors.New("sync: the client has no app")
	}
	b, err := c.authed(ctx, http.MethodPost, proto.PathActor, map[string]string{proto.HeaderActorToken: hubToken}, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var out proto.ActorResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("sync: invalid actor answer: %w", err)
	}
	cl, err := proto.VerifyActor(c.hubPub, out.Assertion, c.wallNow())
	if err != nil {
		return nil, fmt.Errorf("sync: invalid actor assertion: %w", err)
	}
	if cl.Subject != out.AID || cl.Node != c.nodeID {
		return nil, errors.New("sync: the actor assertion is not for this node")
	}
	if err := c.upsertActorRecord(ctx, cl.Col, cl.Rec, out.Record); err != nil {
		return nil, err
	}
	_, err = c.o.App.NonconcurrentDB().NewQuery(`INSERT INTO _sync_actors (aid, hub_id, collection, record, exp, assertion, created)
VALUES ({:a},{:h},{:c},{:r},{:e},{:s},{:t})
ON CONFLICT(aid) DO UPDATE SET exp=excluded.exp, assertion=excluded.assertion`).
		Bind(dbx.Params{"a": out.AID, "h": cl.Issuer, "c": cl.Col, "r": cl.Rec, "e": cl.ExpiresAt.UnixMilli(), "s": out.Assertion,
			"t": c.wallNow().UTC().Format(proto.TimeLayout)}).Execute()
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveActor revokes the grant on the hub (best effort when offline: the
// local grant is dropped either way) and forgets it on this device.
func (c *Client) RemoveActor(ctx context.Context, aid string) error {
	_, herr := c.authed(ctx, http.MethodDelete, proto.PathActor+"/"+aid, nil, nil)
	if _, err := c.o.App.NonconcurrentDB().NewQuery("DELETE FROM _sync_actors WHERE aid={:a}").Bind(dbx.Params{"a": aid}).Execute(); err != nil {
		return err
	}
	return herr
}

// LocalToken mints a LOCAL auth token for the user of a grant (signed with the
// local tokenKey, which is never synced), so that local rules see the same
// @request.auth.id offline.
func (c *Client) LocalToken(aid string) (string, error) {
	return LocalToken(c.o.App, aid, c.wallNow())
}

// LocalToken is Client.LocalToken for an app.
func LocalToken(app core.App, aid string, now time.Time) (string, error) {
	var row struct {
		Collection string `db:"collection"`
		Record     string `db:"record"`
		Exp        int64  `db:"exp"`
	}
	if err := app.DB().NewQuery("SELECT collection, record, exp FROM _sync_actors WHERE aid={:a}").Bind(dbx.Params{"a": aid}).One(&row); err != nil {
		return "", errors.New("sync: unknown actor grant")
	}
	if row.Exp <= now.UnixMilli() {
		return "", errors.New("sync: the actor grant expired, ask the hub for a new one (AddActor)")
	}
	rec, err := app.FindRecordById(row.Collection, row.Record)
	if err != nil {
		return "", errors.New("sync: the local record of the actor does not exist")
	}
	return rec.NewAuthToken()
}

// upsertActorRecord creates the local auth record of the actor when it is
// missing. It is a replica write (no change row for the hub).
func (c *Client) upsertActorRecord(ctx context.Context, colID, id string, data map[string]any) error {
	app := c.o.App
	col, err := app.FindCachedCollectionByNameOrId(colID)
	if err != nil || col == nil || !col.IsAuth() {
		return fmt.Errorf("sync: the auth collection %s of the actor does not exist on this node", colID)
	}
	if ex, _ := app.FindRecordById(col, id); ex != nil {
		return nil
	}
	rec := core.NewRecord(col)
	rec.Set("id", id)
	for _, f := range col.Fields {
		name := f.GetName()
		if name == kernel.FieldNameId || name == kernel.FieldNamePassword || name == kernel.FieldNameTokenKey ||
			f.Type() == kernel.FieldTypePassword || f.Type() == kernel.FieldTypeFile {
			continue
		}
		if v, ok := data[name]; ok {
			rec.SetIfFieldExists(name, v)
		}
	}
	ctx = kernel.WithSyncOrigin(ctx, &kernel.SyncOrigin{Mode: kernel.SyncModePull})
	return app.SaveNoValidateWithContext(ctx, rec)
}
