package push

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// ownDevice returns the device with token only when it belongs to the authenticated record.
func (m *Module) ownDevice(e *core.RequestEvent, token string) (string, bool) {
	r, err := m.app.FindFirstRecordByData(DevicesCollection, "token", token)
	if err != nil {
		return "", false
	}
	if r.GetString("collection") != e.Auth.Collection().Id || r.GetString("record") != e.Auth.Id {
		return "", false // same answer as "missing": do not reveal other users' tokens
	}
	return r.Id, true
}

func (m *Module) bindRoutes() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "routes", Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			e.Router.POST("/api/push/devices", m.apiRegister).Bind(apis.RequireAuth())
			e.Router.DELETE("/api/push/devices/{token}", m.apiDeleteDevice).Bind(apis.RequireAuth())
			e.Router.POST("/api/push/subscribe", m.apiSubscribe(true)).Bind(apis.RequireAuth())
			e.Router.POST("/api/push/unsubscribe", m.apiSubscribe(false)).Bind(apis.RequireAuth())
			e.Router.GET("/api/push/topics", m.apiTopics).Bind(apis.RequireAuth())
			e.Router.POST("/api/push/send", m.apiSend).Bind(apis.RequireSuperuserAuth())
			return e.Next()
		},
	})
}

func (m *Module) apiRegister(e *core.RequestEvent) error {
	var body struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
		AppID    string `json:"app_id"`
		Locale   string `json:"locale"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request body.", nil)
	}
	d, err := RegisterDevice(m.app, e.Auth.Collection(), e.Auth.Id, body.Token, body.Platform, body.AppID, body.Locale)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{
		"id": d.ID, "platform": d.Platform, "app_id": d.AppID, "locale": d.Locale, "enabled": d.Enabled,
	})
}

func (m *Module) apiDeleteDevice(e *core.RequestEvent) error {
	token, err := url.PathUnescape(e.Request.PathValue("token"))
	if err != nil {
		return e.BadRequestError("Invalid token.", nil)
	}
	id, ok := m.ownDevice(e, token)
	if !ok {
		return e.NotFoundError("Device not found.", nil)
	}
	r, err := m.app.FindRecordById(DevicesCollection, id)
	if err != nil {
		return e.NotFoundError("Device not found.", nil)
	}
	if err := m.app.Delete(r); err != nil {
		return e.InternalServerError("Failed to delete the device.", err)
	}
	return e.NoContent(http.StatusNoContent)
}

func (m *Module) apiSubscribe(add bool) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body struct {
			Token string `json:"token"`
			Topic string `json:"topic"`
		}
		if err := e.BindBody(&body); err != nil || body.Token == "" || body.Topic == "" {
			return e.BadRequestError("token and topic are required.", nil)
		}
		id, ok := m.ownDevice(e, body.Token)
		if !ok {
			return e.NotFoundError("Device not found.", nil)
		}
		var err error
		if add {
			err = Subscribe(m.app, id, body.Topic)
		} else {
			err = Unsubscribe(m.app, id, body.Topic)
		}
		if err == errTopicNotFound {
			return e.NotFoundError("Topic not found.", nil)
		}
		if err != nil {
			return e.InternalServerError("Failed to update the subscription.", err)
		}
		return e.JSON(http.StatusOK, map[string]any{"topic": body.Topic, "subscribed": add})
	}
}

func (m *Module) apiTopics(e *core.RequestEvent) error {
	topics, err := ListTopics(m.app)
	if err != nil {
		return e.InternalServerError("Failed to list topics.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": topics})
}

// stringifyData converts the JSON data object to the string map push providers require.
func stringifyData(in map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case string:
			out[k] = t
		case nil:
			out[k] = ""
		case float64, bool:
			out[k] = fmt.Sprint(t)
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return nil, err
			}
			out[k] = string(b)
		}
	}
	return out, nil
}

func (m *Module) apiSend(e *core.RequestEvent) error {
	var body struct {
		To          Target         `json:"to"`
		Title       string         `json:"title"`
		Body        string         `json:"body"`
		Data        map[string]any `json:"data"`
		TTLSeconds  int            `json:"ttl_seconds"`
		CollapseKey string         `json:"collapse_key"`
		Priority    string         `json:"priority"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request body.", nil)
	}
	if len(body.To.Users)+len(body.To.Topics)+len(body.To.Tokens) == 0 {
		return e.BadRequestError("to needs at least one of users, topics, tokens.", nil)
	}
	data, err := stringifyData(body.Data)
	if err != nil {
		return e.BadRequestError("Invalid data.", nil)
	}
	n, err := sendFrom(m.app, Message{To: body.To, Notification: Notification{
		Title: body.Title, Body: body.Body, Data: data, TTLSeconds: body.TTLSeconds,
		CollapseKey: body.CollapseKey, Priority: body.Priority,
	}}, "endpoint")
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"queued": n})
}
