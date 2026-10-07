package sessions

import (
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

// resolveCollection accepts a collection name or id and returns its id.
func resolveCollection(app core.App, nameOrId string) (*core.Collection, error) {
	c, err := app.FindCachedCollectionByNameOrId(nameOrId)
	if err != nil {
		return nil, err
	}
	if !c.IsAuth() {
		return nil, errors.New("not an auth collection: " + nameOrId)
	}
	return c, nil
}

// List returns the sessions of one user, newest first (collection name or id).
func List(app core.App, collection, userId string) ([]Session, error) {
	if !app.HasTable(TableName) {
		return nil, nil
	}
	c, err := resolveCollection(app, collection)
	if err != nil {
		return nil, err
	}
	rows := []Session{}
	err = app.DB().NewQuery("SELECT " + selectCols + " FROM {{_sessions}} WHERE [[collection]]={:c} AND [[record]]={:r} ORDER BY [[created]] DESC").
		Bind(dbx.Params{"c": c.Id, "r": userId}).All(&rows)
	return rows, err
}

// RevokeUser revokes every active session of a user and returns how many were revoked.
// collection may be a name or an id.
func RevokeUser(app core.App, collection, userId, reason string) (int64, error) {
	if !app.HasTable(TableName) {
		return 0, nil
	}
	c, err := resolveCollection(app, collection)
	if err != nil {
		return 0, err
	}
	res, err := app.DB().NewQuery(`UPDATE {{_sessions}} SET [[revoked]]={:n}, [[revoked_reason]]={:why}
		WHERE [[collection]]={:c} AND [[record]]={:r} AND [[revoked]] IS NULL`).
		Bind(dbx.Params{"n": fmtTime(time.Now()), "why": reason, "c": c.Id, "r": userId}).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		audit(ActionRevokeAll, c.Id, userId, map[string]any{"reason": reason, "revoked": n})
	}
	return n, nil
}

// Revoke revokes one session by its row id (or token id) and reports whether an active one was revoked.
func Revoke(app core.App, id, reason string) (bool, error) {
	if !app.HasTable(TableName) {
		return false, nil
	}
	var s Session
	err := app.DB().NewQuery("SELECT " + selectCols + " FROM {{_sessions}} WHERE [[id]]={:i} OR [[token_id]]={:i}").
		Bind(dbx.Params{"i": id}).One(&s)
	if err != nil {
		return false, nil
	}
	res, err := app.DB().NewQuery(`UPDATE {{_sessions}} SET [[revoked]]={:n}, [[revoked_reason]]={:why} WHERE [[id]]={:i} AND [[revoked]] IS NULL`).
		Bind(dbx.Params{"n": fmtTime(time.Now()), "why": reason, "i": s.Id}).Execute()
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		audit(ActionRevoke, s.Collection, s.Record, map[string]any{"reason": reason, "session": s.Id, "device": s.Device})
	}
	return n > 0, nil
}

// PurgeExpired deletes sessions whose token already expired and returns the count.
func PurgeExpired(app core.App) (int64, error) {
	if !app.HasTable(TableName) {
		return 0, nil
	}
	res, err := app.DB().NewQuery("DELETE FROM {{_sessions}} WHERE [[expires]] < {:n}").
		Bind(dbx.Params{"n": fmtTime(time.Now())}).Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
