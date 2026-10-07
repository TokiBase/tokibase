package totp

import (
	"encoding/json"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

func hashExp(collectionId, recordId string) dbx.Expression {
	return dbx.HashExp{"collection": collectionId, "record": recordId}
}

func findRow(app core.App, rec *core.Record) *core.Record {
	if _, err := app.FindCachedCollectionByNameOrId(CollectionName); err != nil {
		return nil
	}
	rows, err := app.FindAllRecords(CollectionName, hashExp(rec.Collection().Id, rec.Id))
	if err != nil || len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func recoveryHashes(r *core.Record) []string {
	var out []string
	_ = json.Unmarshal([]byte(r.GetString("recovery_codes")), &out)
	return out
}

func setRecoveryHashes(r *core.Record, h []string) {
	if h == nil {
		h = []string{}
	}
	r.Set("recovery_codes", h)
}

// Status describes the TOTP state of a record (CLI).
type Status struct {
	Exists, Enabled bool
	RecoveryLeft    int
	LastUsed        string
	Created         string
}

func StatusOf(app core.App, rec *core.Record) Status {
	row := findRow(app, rec)
	if row == nil {
		return Status{}
	}
	return Status{
		Exists: true, Enabled: row.GetBool("enabled"), RecoveryLeft: len(recoveryHashes(row)),
		LastUsed: row.GetDateTime("last_used_at").String(), Created: row.GetDateTime("created").String(),
	}
}

// Disable removes the TOTP row of rec (CLI break-glass) and audits it.
func Disable(app core.App, rec *core.Record) (bool, error) {
	row := findRow(app, rec)
	if row == nil {
		return false, nil
	}
	if err := app.Delete(row); err != nil {
		return false, err
	}
	audit(ActionDisabled, rec.Collection().Name, rec.Id, map[string]any{"by": "cli"})
	return true, nil
}
