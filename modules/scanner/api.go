//go:build !no_scanner

package scanner

import (
	_ "embed"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/edgeguard"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// maxClientSeq bounds the client_seq key (the LRU holds up to 2048 of them).
const maxClientSeq = 128

//go:embed wedge.js
var wedgeJS []byte

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

func topicName(sub string) string {
	if i := strings.IndexByte(sub, '?'); i >= 0 {
		return sub[:i]
	}
	return sub
}

// bindRealtime keeps "@scan" away from everyone who may not read scans: the
// topic carries data, unlike the "@sync" poke.
func (m *Module) bindRealtime() {
	m.app.OnRealtimeSubscribeRequest().Bind(&hook.Handler[*core.RealtimeSubscribeRequestEvent]{
		Id: hookId + "topic", Priority: -1000,
		Func: func(e *core.RealtimeSubscribeRequestEvent) error {
			for _, s := range e.Subscriptions {
				if topicName(s) != Topic {
					continue
				}
				if !allowedAuth(e.Auth) {
					return e.ForbiddenError("This account may not read "+Topic+" (see TOKI_SCAN_TOPIC_AUTH and TOKI_SCAN_READ_AUTH).", nil)
				}
				break
			}
			return e.Next()
		},
	})
}

func actorOf(e *core.RequestEvent) string {
	if e.Auth == nil {
		return ""
	}
	return e.Auth.Collection().Name + "/" + e.Auth.Id
}

func scanError(e *core.RequestEvent, status int, code, msg, reason string) error {
	data := map[string]any{"code": code}
	if reason != "" {
		data["reason"] = reason
	}
	return e.JSON(status, map[string]any{"status": status, "message": msg, "data": data})
}

type scanRequest struct {
	Scanner   string `json:"scanner"`
	Code      string `json:"code"`
	ClientSeq any    `json:"client_seq"`
	Symbology string `json:"symbology"`
}

// webScanner resolves the scanner of a POST /api/scan request. The built-in
// "web" scanner exists only while no web scanner is configured.
func (m *Module) webScanner(name string) (*Scanner, int, string) {
	all, err := loadAll(m.app)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to load the scanners"
	}
	configured := false
	for _, sc := range all {
		configured = configured || sc.Kind == KindWeb
	}
	if name != "" {
		for _, sc := range all {
			if sc.Name != name {
				continue
			}
			switch {
			case !sc.Enabled:
				return nil, http.StatusConflict, "the scanner is disabled"
			case sc.Kind != KindWeb:
				return nil, http.StatusBadRequest, "the scanner is not a web scanner"
			}
			return sc, 0, ""
		}
		if name == defaultBaseName && !configured {
			return defaultWeb(), 0, ""
		}
		return nil, http.StatusNotFound, "unknown scanner"
	}
	for _, sc := range all {
		if sc.Enabled && sc.Kind == KindWeb {
			return sc, 0, ""
		}
	}
	if configured {
		return nil, http.StatusConflict, "every web scanner is disabled"
	}
	return defaultWeb(), 0, ""
}

func (m *Module) handleScan(e *core.RequestEvent) error {
	if kernel.IsSyncReplica(e.Request.Context()) {
		return e.ForbiddenError(ErrReplica.Error(), nil)
	}
	var req scanRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("Invalid JSON body.", nil)
	}
	if req.Code == "" {
		return scanError(e, http.StatusBadRequest, "scan_rejected", "code is required", "empty")
	}
	sc, status, msg := m.webScanner(req.Scanner)
	if sc == nil {
		return scanError(e, status, "scan_scanner", msg, "")
	}
	if !canPost(sc, e.Auth) {
		return scanError(e, http.StatusForbidden, "scan_forbidden", "this account may not post scans to this scanner", "")
	}
	seq := seqString(req.ClientSeq)
	if len(seq) > maxClientSeq {
		return scanError(e, http.StatusBadRequest, "scan_rejected", "client_seq is too long", "client_seq")
	}
	if !m.thr.Allow("a:"+actorOf(e)) || !m.thr.Allow("ip:"+e.RealIP()) {
		return e.TooManyRequestsError("Too many scans, slow down.", nil)
	}
	res, err := m.Ingest(e.Request.Context(), sc, req.Code, IngestOptions{
		Source: KindWeb, Actor: actorOf(e), ClientSeq: seq, Symbology: req.Symbology,
	})
	var rj *RejectedError
	switch {
	case errors.As(err, &rj):
		return scanError(e, http.StatusBadRequest, "scan_rejected", "the code was rejected", rj.Reason)
	case errors.Is(err, ErrReplica):
		return e.ForbiddenError(err.Error(), nil)
	case err != nil:
		return e.InternalServerError("Failed to store the scan.", err)
	}
	return e.JSON(http.StatusOK, res)
}

func (m *Module) handleEvents(e *core.RequestEvent) error {
	if !allowedAuth(e.Auth) {
		return e.ForbiddenError("This account may not read scans (see TOKI_SCAN_TOPIC_AUTH and TOKI_SCAN_READ_AUTH).", nil)
	}
	limit, _ := strconv.Atoi(e.Request.URL.Query().Get("limit"))
	items, gap, err := m.Events(e.Request.URL.Query().Get("since"), e.Request.URL.Query().Get("scanner"), limit)
	if err != nil {
		return e.InternalServerError("Failed to read the scans.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items, "gap": gap})
}

func (m *Module) handleScanners(e *core.RequestEvent) error {
	if !allowedAuth(e.Auth) && !edgeguard.ParseAllow(os.Getenv("TOKI_SCAN_POST_COLLECTIONS")).Match(e.Auth) && postMode() != modeAuth {
		return e.ForbiddenError("This account may not list the scanners.", nil)
	}
	st := m.Status()
	if !e.HasSuperuserAuth() { // device paths and errors are for operators
		for i := range st {
			st[i].Device, st[i].LastError = "", ""
		}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": st})
}

func (m *Module) bindRoutes() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "routes",
		Func: func(se *core.ServeEvent) error {
			g := se.Router
			g.POST("/api/scan", m.handleScan).
				Bind(apis.BodyLimit(8<<10), rateTag("scan"), apis.RequireAuth())
			g.GET("/api/scan/events", m.handleEvents).Bind(apis.SkipSuccessActivityLog(), apis.RequireAuth())
			g.GET("/api/scan/scanners", m.handleScanners).Bind(apis.SkipSuccessActivityLog(), apis.RequireAuth())
			g.GET("/scan/wedge.js", func(e *core.RequestEvent) error {
				h := e.Response.Header()
				h.Set("Content-Type", "application/javascript; charset=utf-8")
				h.Set("Cache-Control", "public, max-age=3600")
				h.Set("X-Content-Type-Options", "nosniff")
				_, err := e.Response.Write(wedgeJS)
				return err
			}).Bind(apis.SkipSuccessActivityLog())
			return se.Next()
		},
	})
}
