package lockout

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"regexp"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

var otpPath = regexp.MustCompile(`^/api/collections/([^/]+)/auth-with-otp/?$`)

const maxBody = 1 << 20

// otpMiddleware guards POST /api/collections/{c}/auth-with-otp. The identity
// is the auth record id the submitted otpId belongs to; requests whose otpId
// does not resolve to a record are passed through (nothing to key on).
func (m *Module) otpMiddleware() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + "otp", Priority: hookPriority,
		Func: func(e *core.RequestEvent) error {
			if e.Request.Method != http.MethodPost {
				return e.Next()
			}
			match := otpPath.FindStringSubmatch(e.Request.URL.Path)
			if match == nil {
				return e.Next()
			}
			collection, err := e.App.FindCachedCollectionByNameOrId(match[1])
			if err != nil || !collection.IsAuth() {
				return e.Next()
			}
			otpId := peekOTPId(e.Request)
			if otpId == "" {
				return e.Next()
			}
			otp, err := e.App.FindOTPById(otpId)
			if err != nil || otp.CollectionRef() != collection.Id {
				return e.Next()
			}
			return m.guard(e, collection.Name, otp.RecordRef(), msgOTP, e.Next)
		},
	}
}

// peekOTPId reads otpId from the body and restores the body for the handler.
func peekOTPId(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	orig := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), orig), orig}
	if err != nil || len(buf) > maxBody {
		return ""
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "application/json" || mt == "" {
		var v struct {
			OTPId string `json:"otpId"`
		}
		if json.Unmarshal(buf, &v) == nil {
			return v.OTPId
		}
		return ""
	}
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(buf))
	if mt == "multipart/form-data" {
		_ = clone.ParseMultipartForm(maxBody)
	} else {
		_ = clone.ParseForm()
	}
	return clone.PostFormValue("otpId")
}
