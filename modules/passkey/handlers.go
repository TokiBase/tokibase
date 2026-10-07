//go:build !no_passkey

package passkey

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/security"
)

const (
	msgAuthFailed = "Failed to authenticate."
	basePath      = "/api/collections/{collection}/passkeys"
)

func (m *Module) bindRoutes(r *router.Router[*core.RequestEvent]) {
	r.POST(basePath+"/register/options", m.registerOptions)
	r.POST(basePath+"/register/verify", m.registerVerify)
	r.GET(basePath, m.list)
	r.DELETE(basePath+"/{id}", m.remove)
	r.POST(basePath+"/login/options", m.loginOptions)
	r.POST(basePath+"/login/verify", m.loginVerify)
}

func (m *Module) authCollection(e *core.RequestEvent) (*core.Collection, error) {
	c, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || !c.IsAuth() {
		return nil, e.NotFoundError("Missing or invalid auth collection context.", err)
	}
	return c, nil
}

// ownAuth returns the authenticated record when it belongs to the collection.
func ownAuth(e *core.RequestEvent, c *core.Collection) (*core.Record, error) {
	if e.Auth == nil || e.Auth.Collection().Id != c.Id {
		return nil, e.UnauthorizedError("The request requires valid record authorization token.", nil)
	}
	return e.Auth, nil
}

type verifyForm struct {
	Credential json.RawMessage `json:"credential" form:"credential"`
	Name       string          `json:"name" form:"name"`
	MfaId      string          `json:"mfaId" form:"mfaId"`
	// Password is the optional re-auth field (see freshAuth).
	Password string `json:"password" form:"password"`
}

// throttle applies the per-IP (and optional per-key) limit of the options endpoints.
func (m *Module) throttle(e *core.RequestEvent, scope, key string) error {
	now := m.now()
	if !m.ipLimit.allow(scope+"|"+e.RealIP(), now, rateLimitPerMinute) ||
		(key != "" && !m.idLimit.allow(scope+"|"+key, now, rateLimitPerMinute)) {
		e.Response.Header().Set("Retry-After", "60")
		return e.TooManyRequestsError("Too many passkey requests, try again later.", nil)
	}
	return nil
}

// freshAuth requires a recent authentication to change the passkeys of rec:
// either the auth token was issued less than FreshAuthWindow ago (refreshable
// session tokens only: impersonation / static tokens never qualify) or the
// request carries the record's current `password`. It returns how the proof
// was made ("token" or "password").
func (m *Module) freshAuth(e *core.RequestEvent, col *core.Collection, rec *core.Record, password string) (string, error) {
	raw := strings.TrimSpace(e.Request.Header.Get("Authorization"))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if claims, err := security.ParseUnverifiedJWT(raw); err == nil {
		exp, _ := claims["exp"].(float64)
		refreshable, _ := claims[core.TokenClaimRefreshable].(bool)
		issued := time.Unix(int64(exp), 0).Add(-col.AuthToken.DurationTime())
		if age := m.now().Sub(issued); refreshable && exp > 0 && age >= -time.Minute && age < FreshAuthWindow {
			return "token", nil
		}
	}
	if password == "" {
		return "", e.ForbiddenError("Recent authentication required: sign in again or submit your current password in the \"password\" field.", nil)
	}
	if isLocked(col.Name, rec) {
		return "", e.ForbiddenError("Recent authentication failed.", errors.New("identity locked"))
	}
	if !rec.ValidatePassword(password) {
		failure(col.Name, rec)
		audit(ActionReauth, col.Name, rec.Id, map[string]any{"ip": e.RealIP()})
		return "", e.ForbiddenError("Recent authentication failed.", nil)
	}
	return "password", nil
}

func (m *Module) readForm(e *core.RequestEvent) (*verifyForm, error) {
	f := &verifyForm{}
	if err := e.BindBody(f); err != nil {
		return nil, e.BadRequestError("An error occurred while loading the submitted data.", err)
	}
	if len(f.Credential) == 0 {
		return nil, e.BadRequestError("An error occurred while validating the submitted data.", map[string]any{
			"credential": map[string]string{"code": "validation_required", "message": "Cannot be blank."},
		})
	}
	return f, nil
}

func (m *Module) registerOptions(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	if err := m.throttle(e, "register|"+col.Id, rec.Id); err != nil {
		return err
	}
	u, err := m.newUser(rec)
	if err != nil {
		return e.InternalServerError("", err)
	}
	if len(u.creds) >= maxPerUser {
		return e.BadRequestError("The maximum number of passkeys was reached.", nil)
	}
	excl := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for _, c := range u.creds {
		excl = append(excl, c.Descriptor())
	}
	creation, session, err := m.wa.BeginRegistration(u, webauthn.WithExclusions(excl))
	if err != nil {
		return e.InternalServerError("", err)
	}
	if err := m.putChallenge("register", col.Id, rec.Id, e.RealIP(), session); err != nil {
		return e.TooManyRequestsError("Too many pending passkey requests.", err)
	}
	return e.JSON(200, creation.Response)
}

func (m *Module) registerVerify(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	f, err := m.readForm(e)
	if err != nil {
		return err
	}
	how, err := m.freshAuth(e, col, rec, f.Password)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(f.Name)
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > 64 {
		return e.BadRequestError("An error occurred while validating the submitted data.", map[string]any{
			"name": map[string]string{"code": "validation_length_too_long", "message": "Must be no more than 64 characters."},
		})
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(f.Credential)
	if err != nil {
		return e.BadRequestError("Invalid passkey registration response.", err)
	}
	session, owner, err := m.takeChallenge(parsed.Response.CollectedClientData.Challenge, "register", col.Id)
	if err != nil {
		return e.BadRequestError("Invalid or expired passkey challenge.", err)
	}
	if owner != rec.Id {
		return e.BadRequestError("Invalid or expired passkey challenge.", errors.New("challenge issued to another user"))
	}
	u, err := m.newUser(rec)
	if err != nil {
		return e.InternalServerError("", err)
	}
	if len(u.creds) >= maxPerUser { // re-check: options may have been requested in bulk
		return e.BadRequestError("The maximum number of passkeys was reached.", nil)
	}
	cred, err := m.wa.CreateCredential(u, *session, parsed)
	if err != nil {
		return e.BadRequestError("Passkey registration failed.", err)
	}
	saved, err := m.savePasskey(rec, cred, name)
	if err != nil {
		return e.BadRequestError("Failed to store the passkey (already registered?).", err)
	}
	audit(ActionRegister, col.Name, rec.Id, map[string]any{"passkey": saved.Id, "name": name, "reauth": how, "uvCapable": cred.Flags.UserVerified})
	return e.JSON(200, view(saved))
}

func (m *Module) list(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	rows, err := e.App.FindAllRecords(CollectionName, dbx.HashExp{"collection": col.Id, "record": rec.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, view(r))
	}
	return e.JSON(200, map[string]any{"items": items})
}

func (m *Module) remove(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	r, err := e.App.FindRecordById(CollectionName, e.Request.PathValue("id"))
	if err != nil || r.GetString("collection") != col.Id || r.GetString("record") != rec.Id {
		return e.NotFoundError("", err)
	}
	var body struct {
		Password string `json:"password" form:"password"`
	}
	_ = e.BindBody(&body) // optional body
	how, err := m.freshAuth(e, col, rec, body.Password)
	if err != nil {
		return err
	}
	if err := e.App.Delete(r); err != nil {
		return e.InternalServerError("", err)
	}
	audit(ActionDelete, col.Name, rec.Id, map[string]any{"passkey": r.Id, "by": "owner", "reauth": how})
	return e.NoContent(204)
}

// findByIdentity resolves a submitted identity like password auth does.
func findByIdentity(e *core.RequestEvent, col *core.Collection, identity string) *core.Record {
	fields := col.PasswordAuth.IdentityFields
	if len(fields) == 0 {
		fields = []string{"email"}
	}
	for _, f := range fields {
		var r *core.Record
		var err error
		if f == "email" {
			r, err = e.App.FindAuthRecordByEmail(col, identity)
		} else {
			r, err = e.App.FindFirstRecordByData(col, f, identity)
		}
		if err == nil && r != nil {
			return r
		}
	}
	return nil
}

func (m *Module) loginOptions(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	body := struct {
		Identity string `json:"identity" form:"identity"`
	}{}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	if err := m.throttle(e, "login|"+col.Id, strings.ToLower(strings.TrimSpace(body.Identity))); err != nil {
		return err
	}

	// User verification is always required for login (passkeys replace the
	// password, so presence alone is not enough).
	uv := webauthn.WithUserVerification(protocol.VerificationRequired)
	var (
		assertion *protocol.CredentialAssertion
		session   *webauthn.SessionData
		recordId  string
	)
	// The identity is ignored by default: the answer is always a discoverable
	// ceremony, so the response never reveals whether an account (or a
	// passkey) exists. TOKI_PASSKEY_ALLOW_IDENTITY_HINT=1 restores the
	// allowCredentials hint, at the price of that enumeration oracle.
	if id := strings.TrimSpace(body.Identity); id != "" && m.cfg.IdentityHint {
		if rec := findByIdentity(e, col, id); rec != nil {
			u, uerr := m.newUser(rec)
			if uerr != nil {
				return e.InternalServerError("", uerr)
			}
			if len(u.creds) > 0 {
				assertion, session, err = m.wa.BeginLogin(u, uv)
				recordId = rec.Id
			}
		}
	}
	if assertion == nil && err == nil {
		assertion, session, err = m.wa.BeginDiscoverableLogin(uv)
	}
	if err != nil {
		return e.InternalServerError("", err)
	}
	if err := m.putChallenge("login", col.Id, recordId, e.RealIP(), session); err != nil {
		return e.TooManyRequestsError("Too many pending passkey requests.", err)
	}
	return e.JSON(200, assertion.Response)
}

func (m *Module) loginVerify(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	f, err := m.readForm(e)
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(f.Credential)
	if err != nil {
		return e.BadRequestError(msgAuthFailed, err)
	}
	session, _, err := m.takeChallenge(parsed.Response.CollectedClientData.Challenge, "login", col.Id)
	if err != nil {
		return e.BadRequestError(msgAuthFailed, err)
	}

	var (
		known    *core.Record // record the credential resolved to, once known
		locked   bool
		passkey  *core.Record
		cred     *webauthn.Credential
		verifyEr error
	)
	lookup := func(rawID []byte) (*core.Record, error) {
		p, err := m.findByCredentialID(col.Id, rawID)
		if err != nil {
			return nil, errors.New("unknown credential")
		}
		return p, nil
	}
	if len(session.UserID) > 0 {
		rec, ferr := e.App.FindRecordById(col, string(session.UserID))
		if ferr != nil {
			return e.BadRequestError(msgAuthFailed, ferr)
		}
		known = rec
		if isLocked(col.Name, rec) {
			return e.BadRequestError(msgAuthFailed, errors.New("identity locked"))
		}
		u, uerr := m.newUser(rec)
		if uerr != nil {
			return e.InternalServerError("", uerr)
		}
		cred, verifyEr = m.wa.ValidateLogin(u, *session, parsed)
	} else {
		_, cred, verifyEr = m.wa.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			p, err := lookup(rawID)
			if err != nil {
				return nil, err
			}
			if p.GetString("record") != string(userHandle) {
				known = nil
				return nil, errors.New("user handle mismatch")
			}
			rec, err := e.App.FindRecordById(col, p.GetString("record"))
			if err != nil {
				return nil, err
			}
			if isLocked(col.Name, rec) {
				locked = true
				return nil, errors.New("identity locked")
			}
			known = rec
			return m.newUser(rec)
		}, *session, parsed)
	}
	if verifyEr != nil {
		// Count a failure only for a known credential (the user handle matched
		// its owner) whose signature is well-formed but invalid. Unknown
		// credential ids, bad origin/RP/challenge/UV errors and locked records
		// never count, so an attacker holding only public identifiers cannot
		// lock a victim out.
		var perr *protocol.Error
		if known != nil && !locked && errors.As(verifyEr, &perr) && perr.Type == protocol.ErrAssertionSignature.Type {
			failure(col.Name, known)
		}
		return e.BadRequestError(msgAuthFailed, verifyEr)
	}
	if known == nil || !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return e.BadRequestError(msgAuthFailed, errors.New("user verification required"))
	}
	passkey, err = lookup(cred.ID)
	if err != nil || passkey.GetString("record") != known.Id {
		return e.BadRequestError(msgAuthFailed, errors.New("credential owner mismatch"))
	}

	if cred.Authenticator.CloneWarning {
		_ = m.touch(passkey, cred) // persists clone_suspected, sign_count unchanged
		m.app.Logger().Warn("passkey: sign count regression, clone suspected", "collection", col.Name, "record", known.Id, "passkey", passkey.Id)
		audit(ActionClone, col.Name, known.Id, map[string]any{"passkey": passkey.Id, "policy": m.cfg.ClonePolicy})
		if m.cfg.ClonePolicy == "deny" {
			return e.BadRequestError(msgAuthFailed, errors.New("clone suspected"))
		}
	} else if err := m.touch(passkey, cred); err != nil {
		m.app.Logger().Warn("passkey: failed to update passkey state", "error", err, "passkey", passkey.Id)
	}

	err = apis.RecordAuthResponse(e, known, AuthMethod, map[string]any{"passkey": passkey.Id})
	if err == nil || errors.Is(err, apis.ErrMFA) {
		audit(ActionLogin, col.Name, known.Id, map[string]any{"method": AuthMethod, "passkey": passkey.Id, "mfa": err != nil})
	}
	return err
}
