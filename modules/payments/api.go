//go:build !no_payments

package payments

import (
	"errors"
	"net/http"
	"strings"

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

func (m *Module) bindRoutes(se *core.ServeEvent) {
	g := se.Router
	g.POST("/api/payments/webhook/{provider}", m.webhookHandler).
		Bind(apis.SkipSuccessActivityLog(), rateTag("payments:webhook"))
	g.POST("/api/payments/intents", m.createHandler).
		Bind(apis.RequireAuth(), apis.BodyLimit(64<<10), rateTag("payments:intent"))
	g.GET("/api/payments/intents/{id}", m.getHandler).
		Bind(apis.RequireAuth(), rateTag("payments:intent"))
	g.POST("/api/payments/intents/{id}/refund", m.refundHandler).
		Bind(apis.RequireSuperuserAuth(), apis.BodyLimit(16<<10), rateTag("payments:refund"))
	g.GET("/api/payments/entitlements/me", m.myEntitlementsHandler).
		Bind(apis.RequireAuth(), rateTag("payments:intent"))
}

// PublicIntent is the client view of an intent (no provider data, grants or errors).
func PublicIntent(r *core.Record) map[string]any {
	md := map[string]any{}
	_ = r.UnmarshalJSONField("metadata", &md)
	return map[string]any{
		"id": r.Id, "status": r.GetString("status"), "provider": r.GetString("provider"),
		"amount": r.GetInt("amount"), "currency": r.GetString("currency"), "order_ref": r.GetString("order_ref"),
		"product": r.GetString("product"), "checkout_url": r.GetString("checkout_url"),
		"idempotency_key": r.GetString("idempotency_key"), "refunded_amount": r.GetInt("refunded_amount"),
		"paid_at": r.GetString("paid_at"), "metadata": md,
		"created": r.GetString("created"), "updated": r.GetString("updated"),
	}
}

func (m *Module) createHandler(e *core.RequestEvent) error {
	var body struct {
		Amount         int64          `json:"amount"`
		Currency       string         `json:"currency"`
		Provider       string         `json:"provider"`
		OrderRef       string         `json:"order_ref"`
		Description    string         `json:"description"`
		Product        string         `json:"product"`
		IdempotencyKey string         `json:"idempotency_key"`
		Metadata       map[string]any `json:"metadata"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	if k := e.Request.Header.Get("Idempotency-Key"); k != "" {
		body.IdempotencyKey = k
	}
	md := body.Metadata
	if !e.HasSuperuserAuth() {
		// clients must not steer redirects of the provider checkout
		delete(md, "return_url")
		delete(md, "cancel_url")
	}
	rec, replay, err := m.CreateIntent(e.Request.Context(), CreateParams{
		Provider: body.Provider, Amount: body.Amount, Currency: body.Currency, OrderRef: body.OrderRef,
		Description: body.Description, Product: body.Product, IdempotencyKey: body.IdempotencyKey,
		Customer: e.Auth, Metadata: md,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyConflict):
			return e.Error(http.StatusConflict, err.Error(), nil)
		case errors.Is(err, ErrInvalid):
			return e.BadRequestError(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), nil)
		case errors.Is(err, ErrTooManyIntents):
			return e.TooManyRequestsError("Too many open payment intents, try again later.", nil)
		}
		m.app.Logger().Error("payments: create intent failed", "error", err)
		return e.Error(http.StatusBadGateway, "The payment provider rejected the request.", nil)
	}
	code := http.StatusCreated
	if replay {
		code = http.StatusOK
	}
	return e.JSON(code, map[string]any{"intent": PublicIntent(rec), "checkout_url": rec.GetString("checkout_url"), "idempotent_replay": replay})
}

func (m *Module) ownerOrSuper(e *core.RequestEvent) (*core.Record, error) {
	r, err := m.GetIntent(e.Request.PathValue("id"))
	if err != nil {
		return nil, e.NotFoundError("", nil)
	}
	if !e.HasSuperuserAuth() && (r.GetString("customer") != e.Auth.Id || r.GetString("customer_collection") != e.Auth.Collection().Name) {
		return nil, e.NotFoundError("", nil)
	}
	return r, nil
}

func (m *Module) getHandler(e *core.RequestEvent) error {
	r, err := m.ownerOrSuper(e)
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, PublicIntent(r))
}

func (m *Module) refundHandler(e *core.RequestEvent) error {
	var body struct {
		Amount         int64  `json:"amount"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	rf, err := m.Refund(e.Request.Context(), e.Request.PathValue("id"), body.Amount, body.Reason, body.IdempotencyKey)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return e.NotFoundError("", nil)
		case errors.Is(err, ErrInvalid):
			return e.BadRequestError(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), nil)
		case errors.Is(err, ErrUnsupported):
			return e.BadRequestError("The provider does not support refunds through the API.", nil)
		}
		m.app.Logger().Error("payments: refund failed", "error", err)
		return e.Error(http.StatusBadGateway, "The payment provider rejected the refund.", nil)
	}
	intent, _ := m.GetIntent(e.Request.PathValue("id"))
	out := map[string]any{"refund": map[string]any{
		"id": rf.Id, "status": rf.GetString("status"), "amount": rf.GetInt("amount"), "currency": rf.GetString("currency"),
	}}
	if intent != nil {
		out["intent"] = PublicIntent(intent)
	}
	return e.JSON(http.StatusOK, out)
}

func (m *Module) myEntitlementsHandler(e *core.RequestEvent) error {
	rs, err := m.app.FindRecordsByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c}", "key", 200, 0,
		map[string]any{"s": e.Auth.Id, "c": e.Auth.Collection().Name})
	if err != nil {
		return e.InternalServerError("", err)
	}
	items := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		items = append(items, map[string]any{
			"key": r.GetString("key"), "status": r.GetString("status"), "until": r.GetString("until"),
			"quota": r.GetInt("quota"), "balance": r.GetInt("balance"),
			"entitled": m.Entitled(r.GetString("subject"), r.GetString("subject_collection"), r.GetString("key")),
		})
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}
