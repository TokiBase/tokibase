//go:build !no_nativeauth

package nativeauth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/auth"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/types"
)

const msgFailed = "Failed to authenticate."

type form struct {
	Provider   string         `json:"provider" form:"provider"`
	IdToken    string         `json:"idToken" form:"idToken"`
	Nonce      string         `json:"nonce" form:"nonce"`
	CreateData map[string]any `json:"createData" form:"createData"`
}

func validationError(e *core.RequestEvent, field, msg string) error {
	return e.BadRequestError("An error occurred while validating the submitted data.", map[string]any{
		field: map[string]string{"code": "validation_invalid_value", "message": msg},
	})
}

func (m *Module) audiences(provider, clientId string) []string {
	var out []string
	if clientId != "" {
		out = append(out, clientId)
	}
	env := EnvGoogleAudiences
	if provider == auth.NameApple {
		env = EnvAppleAudiences
	}
	return append(out, splitList(os.Getenv(env))...)
}

func (m *Module) login(e *core.RequestEvent) error {
	col, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || !col.IsAuth() {
		return e.NotFoundError("Missing or invalid auth collection context.", err)
	}
	if !col.OAuth2.Enabled {
		return e.ForbiddenError("The collection is not configured to allow OAuth2 authentication.", nil)
	}
	e.Set(core.RequestEventKeyInfoContext, core.RequestInfoContextOAuth2)

	f := new(form)
	if err := e.BindBody(f); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}
	f.Provider = strings.ToLower(strings.TrimSpace(f.Provider))
	if f.Provider != auth.NameGoogle && f.Provider != auth.NameApple {
		return validationError(e, "provider", "Provider must be google or apple.")
	}
	cfg, ok := col.OAuth2.GetProviderConfig(f.Provider)
	if !ok {
		return validationError(e, "provider", "Provider with name "+f.Provider+" is missing or is not enabled.")
	}
	if f.IdToken == "" || len(f.IdToken) > 16384 {
		return validationError(e, "idToken", "Missing or invalid idToken.")
	}
	if len(f.Nonce) > 512 {
		return validationError(e, "nonce", "Invalid nonce.")
	}

	now := m.now()
	ip := e.RealIP()
	if !m.limiter.peek("fail|"+ip, now, maxFailures) {
		e.Response.Header().Set("Retry-After", "60")
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}
	fail := func(rec *core.Record, reason string, err error) error {
		m.limiter.allow("fail|"+ip, now, maxFailures+1)
		failure(col.Name, rec)
		rid := ""
		if rec != nil {
			rid = rec.Id
		}
		audit(ActionFailed, col.Name, rid, map[string]any{"provider": f.Provider, "reason": reason, "ip": ip})
		return e.BadRequestError(msgFailed, err)
	}

	v, err := m.verifyToken(f.Provider, f.IdToken, m.audiences(f.Provider, cfg.ClientId))
	if err != nil {
		return fail(nil, "token", err)
	}

	externalAuthRel, err := e.App.FindFirstExternalAuthByExpr(dbx.HashExp{
		"collectionRef": col.Id,
		"provider":      f.Provider,
		"providerId":    v.Sub,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e.InternalServerError("Failed OAuth2 relation check.", err)
	}
	var authRecord *core.Record
	if err == nil && externalAuthRel != nil {
		authRecord, err = e.App.FindRecordById(col, externalAuthRel.RecordRef())
		if err != nil {
			return e.InternalServerError("", err)
		}
		if isLocked(col.Name, authRecord) {
			return fail(authRecord, "locked", errors.New("identity locked"))
		}
	}
	if err := checkNonce(v, f.Nonce); err != nil {
		return fail(authRecord, "nonce", err)
	}
	if !m.replay.markUsed(v.ReplayKey, v.Exp, now) {
		return fail(authRecord, "replay", errors.New("token already used"))
	}
	consumed := false
	defer func() {
		if !consumed { // a failed sign-in (eg. invalid createData) must not burn the token
			m.replay.forget(v.ReplayKey)
		}
	}()

	// Only a provider-verified email may be used to match or create records.
	email := ""
	if v.EmailVerified {
		email = v.Email
	}

	if authRecord == nil && email != "" {
		authRecord, err = e.App.FindAuthRecordByEmail(col.Id, email)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return e.InternalServerError("Failed OAuth2 auth record check.", err)
		}
		if err != nil {
			authRecord = nil
		}
		if authRecord != nil && isLocked(col.Name, authRecord) {
			return fail(authRecord, "locked", errors.New("identity locked"))
		}
	}

	raw := map[string]any{"sub": v.Sub, "email": email, "email_verified": v.EmailVerified}
	if v.Name != "" {
		raw["name"] = v.Name
	}
	if v.Picture != "" {
		raw["picture"] = v.Picture
	}
	user := &auth.AuthUser{
		Id:        v.Sub,
		Name:      v.Name,
		Email:     email,
		AvatarURL: v.Picture,
		RawUser:   raw,
	}
	user.Expiry, _ = types.ParseDateTime(v.Exp)

	event := new(core.RecordAuthWithOAuth2RequestEvent)
	event.RequestEvent = e
	event.Collection = col
	event.ProviderName = f.Provider
	if p, perr := core.InitOAuth2Provider(cfg); perr == nil {
		event.ProviderClient = p
	}
	event.OAuth2User = user
	event.CreateData = f.CreateData
	event.Record = authRecord
	event.IsNewRecord = authRecord == nil

	return e.App.OnRecordAuthWithOAuth2Request().Trigger(event, func(e *core.RecordAuthWithOAuth2RequestEvent) error {
		if err := apis.SubmitOAuth2User(e, externalAuthRel); err != nil {
			var ae *router.ApiError
			if errors.As(err, &ae) {
				return ae
			}
			return e.BadRequestError(msgFailed, err)
		}
		meta := map[string]any{}
		b, err := json.Marshal(e.OAuth2User)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &meta); err != nil {
			return err
		}
		meta["isNew"] = e.IsNewRecord
		consumed = true
		audit(ActionLogin, col.Name, e.Record.Id, map[string]any{"provider": f.Provider, "isNew": e.IsNewRecord, "ip": ip})
		return apis.RecordAuthResponse(e.RequestEvent, e.Record, core.MFAMethodOAuth2, meta)
	})
}
