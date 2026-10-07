package passkey

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/router"
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
	if err := m.putChallenge("register", col.Id, rec.Id, session); err != nil {
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
	cred, err := m.wa.CreateCredential(u, *session, parsed)
	if err != nil {
		return e.BadRequestError("Passkey registration failed.", err)
	}
	saved, err := m.savePasskey(rec, cred, name)
	if err != nil {
		return e.BadRequestError("Failed to store the passkey (already registered?).", err)
	}
	audit(ActionRegister, col.Name, rec.Id, map[string]any{"passkey": saved.Id, "name": name})
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
	if err := e.App.Delete(r); err != nil {
		return e.InternalServerError("", err)
	}
	audit(ActionDelete, col.Name, rec.Id, map[string]any{"passkey": r.Id, "by": "owner"})
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

	var (
		assertion *protocol.CredentialAssertion
		session   *webauthn.SessionData
		recordId  string
	)
	// An unknown identity (or one without passkeys) falls back to a
	// discoverable ceremony so the response does not reveal whether it exists.
	if id := strings.TrimSpace(body.Identity); id != "" {
		if rec := findByIdentity(e, col, id); rec != nil {
			u, uerr := m.newUser(rec)
			if uerr != nil {
				return e.InternalServerError("", uerr)
			}
			if len(u.creds) > 0 {
				assertion, session, err = m.wa.BeginLogin(u)
				recordId = rec.Id
			}
		}
	}
	if assertion == nil && err == nil {
		assertion, session, err = m.wa.BeginDiscoverableLogin()
	}
	if err != nil {
		return e.InternalServerError("", err)
	}
	if err := m.putChallenge("login", col.Id, recordId, session); err != nil {
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
		passkey  *core.Record
		cred     *webauthn.Credential
		verifyEr error
	)
	lookup := func(rawID []byte) (*core.Record, error) {
		p, err := m.findByCredentialID(col.Id, rawID)
		if err != nil || p.GetString("collection") != col.Id {
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
			known = rec
			return m.newUser(rec)
		}, *session, parsed)
	}
	if verifyEr != nil {
		if known != nil {
			failure(col.Name, known.Id)
		}
		return e.BadRequestError(msgAuthFailed, verifyEr)
	}
	passkey, err = lookup(cred.ID)
	if err != nil || passkey.GetString("record") != known.Id {
		return e.BadRequestError(msgAuthFailed, err)
	}

	if cred.Authenticator.CloneWarning {
		_ = m.touch(passkey, cred) // persists clone_suspected, sign_count unchanged
		m.app.Logger().Warn("passkey: sign count regression, clone suspected", "collection", col.Name, "record", known.Id, "passkey", passkey.Id)
		audit(ActionClone, col.Name, known.Id, map[string]any{"passkey": passkey.Id, "policy": m.cfg.ClonePolicy})
		if m.cfg.ClonePolicy == "deny" {
			failure(col.Name, known.Id)
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
