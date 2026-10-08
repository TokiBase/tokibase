//go:build !no_printer

package printer

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

func rateTag(tag string) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + tag, Priority: -900,
		Func: func(e *core.RequestEvent) error {
			if err := apis.CheckRateLimitTags(e, tag); err != nil {
				return err
			}
			return e.Next()
		},
	}
}

func authMiddleware() *hook.Handler[*core.RequestEvent] {
	if superuserOnly() {
		return apis.RequireSuperuserAuth()
	}
	return apis.RequireAuth()
}

func (m *Module) bindRoutes(se *core.ServeEvent) {
	g := se.Router
	g.POST("/api/print", m.apiPrint).Bind(authMiddleware(), apis.BodyLimit(int64(2*MaxBytes())+(64<<10)), rateTag("print"))
	g.GET("/api/print/printers", m.apiPrinters).Bind(authMiddleware(), rateTag("print"))
	g.GET("/api/print/{id}", m.apiJob).Bind(authMiddleware(), rateTag("print"))
	g.POST("/api/print/{id}/retry", m.apiRetry).Bind(authMiddleware(), rateTag("print"))
}

func actorOf(e *core.RequestEvent) string {
	if e.Auth == nil {
		return ""
	}
	return e.Auth.Collection().Name + "/" + e.Auth.Id
}

func (m *Module) writeErr(e *core.RequestEvent, err error) error {
	var re *RequestError
	switch {
	case errors.As(err, &re):
		return e.JSON(re.Status, map[string]any{"status": re.Status, "message": re.Msg, "data": map[string]any{}})
	case errors.Is(err, ErrSyncReplica):
		return e.ForbiddenError(err.Error(), nil)
	}
	return e.InternalServerError("Failed to queue the print.", err)
}

func (m *Module) apiPrint(e *core.RequestEvent) error {
	var body struct {
		Printer        string `json:"printer"`
		Template       string `json:"template"`
		Data           any    `json:"data"`
		Copies         int    `json:"copies"`
		IdempotencyKey string `json:"idempotency_key"`
		RawB64         string `json:"raw_b64"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request body.", nil)
	}
	rq := Request{
		Printer: body.Printer, Template: body.Template, Data: body.Data, Copies: body.Copies,
		IdempotencyKey: body.IdempotencyKey, Actor: actorOf(e),
	}
	if body.RawB64 != "" {
		if !e.HasSuperuserAuth() {
			return e.ForbiddenError("Raw bytes need a superuser.", nil)
		}
		raw, err := base64.StdEncoding.DecodeString(body.RawB64)
		if err != nil || len(raw) == 0 {
			return e.BadRequestError("raw_b64 is not valid base64.", nil)
		}
		rq.Raw = raw
	}
	res, err := m.Enqueue(e.Request.Context(), rq)
	if err != nil {
		return m.writeErr(e, err)
	}
	return e.JSON(http.StatusOK, res)
}

// visible loads a job and hides the ones of other actors from regular users.
func (m *Module) visible(e *core.RequestEvent) (*core.Record, error) {
	r, err := m.app.FindRecordById(JobsCollection, e.Request.PathValue("id"))
	if err != nil || (!e.HasSuperuserAuth() && r.GetString("actor") != actorOf(e)) {
		return nil, e.NotFoundError("Print job not found.", nil)
	}
	return r, nil
}

func (m *Module) apiJob(e *core.RequestEvent) error {
	r, err := m.visible(e)
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, jobOf(r))
}

func (m *Module) apiRetry(e *core.RequestEvent) error {
	r, err := m.visible(e)
	if err != nil {
		return err
	}
	res, err := m.Retry(e.Request.Context(), r.Id)
	if err != nil {
		return m.writeErr(e, err)
	}
	return e.JSON(http.StatusOK, res)
}

func (m *Module) apiPrinters(e *core.RequestEvent) error {
	list, err := ListPrinters(m.app)
	if err != nil {
		return e.InternalServerError("Failed to list the printers.", err)
	}
	su := e.HasSuperuserAuth()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		if !p.Enabled && !su {
			continue
		}
		depth, _ := m.app.CountRecords(JobsCollection, queueExpr(p.Name))
		row := map[string]any{
			"name": p.Name, "transport": p.Transport, "cols": p.Cols, "default": p.Default,
			"enabled": p.Enabled, "status": m.LastStatus(p.Name), "queue_depth": depth,
		}
		if su {
			row["address"] = p.Address
			row["baud"] = p.Baud
		}
		out = append(out, row)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": out})
}
