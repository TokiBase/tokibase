//go:build !no_denylog

package denylog

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

// Entry is one denial read back from the _logs table.
type Entry struct {
	Id      string         `json:"id"`
	Created string         `json:"created"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// Tail returns the newest denial entries (attribute toki.deny=true) not older
// than since (zero = no limit), newest first.
func Tail(app kernel.App, since time.Duration, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := app.AuxDB().Select("id", "created", "message", "data").
		From(kernel.LogsTableName).
		AndWhere(dbx.NewExp(`json_extract([[data]], '$."`+Attr+`"') = 1`)).
		OrderBy("created DESC", "id DESC").
		Limit(int64(limit))
	if since > 0 {
		q.AndWhere(dbx.NewExp("[[created]] >= {:since}", dbx.Params{
			"since": types.NowDateTime().Add(-since).String(),
		}))
	}

	var rows []struct {
		Id      string             `db:"id"`
		Created string             `db:"created"`
		Message string             `db:"message"`
		Data    types.JSONMap[any] `db:"data"`
	}
	if err := q.All(&rows); err != nil {
		return nil, fmt.Errorf("denylog: %w", err)
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, Entry{Id: r.Id, Created: r.Created, Message: r.Message, Data: r.Data})
	}
	return out, nil
}
