//go:build !no_passkey

package passkey

import (
	"errors"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

// ResolveUser finds an auth record by id or email.
func ResolveUser(app core.App, collection, user string) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil || !col.IsAuth() {
		return nil, errors.New("unknown auth collection " + collection)
	}
	if r, err := app.FindRecordById(col, user); err == nil {
		return r, nil
	}
	if r, err := app.FindAuthRecordByEmail(col, strings.TrimSpace(user)); err == nil {
		return r, nil
	}
	return nil, errors.New("record not found: " + user)
}

// List returns the passkeys of one auth record (CLI).
func List(app core.App, collection, user string) ([]*core.Record, error) {
	rec, err := ResolveUser(app, collection, user)
	if err != nil {
		return nil, err
	}
	if _, err := app.FindCollectionByNameOrId(CollectionName); err != nil {
		return nil, nil
	}
	return app.FindAllRecords(CollectionName, dbx.HashExp{"collection": rec.Collection().Id, "record": rec.Id})
}

// Remove deletes a passkey by id (CLI) and returns it.
func Remove(app core.App, id string) (*core.Record, error) {
	r, err := app.FindRecordById(CollectionName, id)
	if err != nil {
		return nil, errors.New("passkey not found: " + id)
	}
	if err := app.Delete(r); err != nil {
		return nil, err
	}
	audit(ActionDelete, r.GetString("collection"), r.GetString("record"), map[string]any{"passkey": r.Id, "by": "cli"})
	return r, nil
}
