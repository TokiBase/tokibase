package totp

import (
	"crypto/subtle"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/types"
)

const (
	basePath      = "/api/collections/{collection}"
	msgAuthFailed = "Failed to authenticate."
)

func (m *Module) bindRoutes(r *router.Router[*core.RequestEvent]) {
	r.POST(basePath+"/totp/setup", m.setup)
	r.POST(basePath+"/totp/confirm", m.confirm)
	r.DELETE(basePath+"/totp", m.disable)
	r.POST(basePath+"/totp/recovery/regenerate", m.regenerate)
	r.POST(basePath+"/auth-with-totp", m.login)
}

type form struct {
	Code     string `json:"code" form:"code"`
	Password string `json:"password" form:"password"`
	MfaId    string `json:"mfaId" form:"mfaId"`
}

func (m *Module) authCollection(e *core.RequestEvent) (*core.Collection, error) {
	c, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || !c.IsAuth() {
		return nil, e.NotFoundError("Missing or invalid auth collection context.", err)
	}
	return c, nil
}

func ownAuth(e *core.RequestEvent, c *core.Collection) (*core.Record, error) {
	if e.Auth == nil || e.Auth.Collection().Id != c.Id {
		return nil, e.UnauthorizedError("The request requires valid record authorization token.", nil)
	}
	return e.Auth, nil
}

func (m *Module) readForm(e *core.RequestEvent) (*form, error) {
	f := &form{}
	if e.Request.ContentLength != 0 {
		if err := e.BindBody(f); err != nil {
			return nil, e.BadRequestError("An error occurred while loading the submitted data.", err)
		}
	}
	return f, nil
}

func (m *Module) key(e *core.RequestEvent) ([]byte, error) {
	k := loadKey(e.App)
	if k == nil {
		return nil, e.Error(503, "TOTP is not configured on this server.", nil)
	}
	return k, nil
}

// freshAuth mirrors the passkey module: a refreshable token issued less than
// FreshAuthWindow ago, or the record's current password.
func (m *Module) freshAuth(e *core.RequestEvent, col *core.Collection, rec *core.Record, password string) error {
	raw := strings.TrimSpace(e.Request.Header.Get("Authorization"))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if claims, err := security.ParseUnverifiedJWT(raw); err == nil {
		exp, _ := claims["exp"].(float64)
		refreshable, _ := claims[core.TokenClaimRefreshable].(bool)
		issued := time.Unix(int64(exp), 0).Add(-col.AuthToken.DurationTime())
		if age := m.now().Sub(issued); refreshable && exp > 0 && age >= -time.Minute && age < FreshAuthWindow {
			return nil
		}
	}
	if password == "" {
		return e.ForbiddenError("Recent authentication required: sign in again or submit your current password in the \"password\" field.", nil)
	}
	if isLocked(col.Name, rec) {
		return e.ForbiddenError("Recent authentication failed.", errors.New("identity locked"))
	}
	if !rec.ValidatePassword(password) {
		failure(col.Name, rec)
		audit(ActionReauth, col.Name, rec.Id, map[string]any{"ip": e.RealIP()})
		return e.ForbiddenError("Recent authentication failed.", nil)
	}
	return nil
}

func (m *Module) issuer() string {
	if v := strings.TrimSpace(os.Getenv(EnvIssuer)); v != "" {
		return v
	}
	if n := m.app.Settings().Meta.AppName; n != "" {
		return n
	}
	return "TokiBase"
}

func account(rec *core.Record) string {
	if v := rec.Email(); v != "" {
		return v
	}
	if v := rec.GetString("username"); v != "" {
		return v
	}
	return rec.Id
}

func otpauthURL(issuer, acct, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(acct) + "?" + v.Encode()
}

func (m *Module) setup(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	key, err := m.key(e)
	if err != nil {
		return err
	}
	f, err := m.readForm(e)
	if err != nil {
		return err
	}
	if err := m.freshAuth(e, col, rec, f.Password); err != nil {
		return err
	}
	if _, err := e.App.FindCachedCollectionByNameOrId(CollectionName); err != nil {
		if err := EnsureCollection(e.App); err != nil {
			return e.InternalServerError("", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := findRow(e.App, rec)
	if row != nil && row.GetBool("enabled") {
		return e.BadRequestError("TOTP is already enabled; disable it first.", nil)
	}
	secret, err := newSecret()
	if err != nil {
		return e.InternalServerError("", err)
	}
	b32s := b32.EncodeToString(secret)
	sealed, err := seal(key, b32s)
	if err != nil {
		return e.InternalServerError("", err)
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return e.InternalServerError("", err)
	}
	if row == nil {
		c, err := e.App.FindCachedCollectionByNameOrId(CollectionName)
		if err != nil {
			return e.InternalServerError("", err)
		}
		row = core.NewRecord(c)
		row.Set("collection", col.Id)
		row.Set("record", rec.Id)
	}
	issuer := m.issuer()
	row.Set("secret", sealed)
	row.Set("issuer", issuer)
	row.Set("enabled", false)
	row.Set("last_counter", 0)
	setRecoveryHashes(row, hashes)
	if err := e.App.Save(row); err != nil {
		return e.InternalServerError("", err)
	}
	u := otpauthURL(issuer, account(rec), b32s)
	svg, err := qrSVG(u)
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(200, map[string]any{"secret": b32s, "otpauth_url": u, "qr_svg": svg, "recovery_codes": plain})
}

// consume validates code (TOTP or recovery) for row and, on success, records
// the use (caller holds m.mu). kind is "totp", "recovery" or "".
func (m *Module) consume(key []byte, row *core.Record, code string, allowRecovery bool) (string, error) {
	norm := strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(norm) == digits && allDigits(norm) {
		secretB32, err := open(key, row.GetString("secret"))
		if err != nil {
			return "", err
		}
		secret, err := b32.DecodeString(secretB32)
		if err != nil {
			return "", err
		}
		ctr, ok := match(secret, norm, m.now().Unix())
		if !ok || int64(ctr) <= int64(row.GetFloat("last_counter")) {
			return "", nil
		}
		row.Set("last_counter", float64(ctr))
		row.Set("last_used_at", types.NowDateTime())
		return "totp", m.app.Save(row)
	}
	if !allowRecovery {
		return "", nil
	}
	n := normRecovery(code)
	if len(n) != recoveryLen {
		return "", nil
	}
	want := []byte(hashRecovery(n))
	hashes := recoveryHashes(row)
	hit := -1
	for i, h := range hashes { // no early exit: constant work for every stored hash
		if subtle.ConstantTimeCompare([]byte(h), want) == 1 {
			hit = i
		}
	}
	if hit < 0 {
		return "", nil
	}
	hashes = append(hashes[:hit:hit], hashes[hit+1:]...)
	setRecoveryHashes(row, hashes)
	row.Set("last_used_at", types.NowDateTime())
	return "recovery", m.app.Save(row)
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (m *Module) confirm(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	rec, err := ownAuth(e, col)
	if err != nil {
		return err
	}
	key, err := m.key(e)
	if err != nil {
		return err
	}
	f, err := m.readForm(e)
	if err != nil {
		return err
	}
	if !m.limiter.allow("confirm|"+rec.Id, m.now(), maxAttempts) {
		e.Response.Header().Set("Retry-After", "60")
		return e.TooManyRequestsError("Too many attempts, try again later.", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := findRow(e.App, rec)
	if row == nil {
		return e.BadRequestError("Call totp/setup first.", nil)
	}
	if row.GetBool("enabled") {
		return e.BadRequestError("TOTP is already enabled.", nil)
	}
	kind, err := m.consume(key, row, f.Code, false)
	if err != nil {
		return e.InternalServerError("", err)
	}
	if kind != "totp" {
		return e.BadRequestError("Invalid code.", nil)
	}
	row.Set("enabled", true)
	if err := e.App.Save(row); err != nil {
		return e.InternalServerError("", err)
	}
	audit(ActionEnabled, col.Name, rec.Id, map[string]any{"ip": e.RealIP()})
	return e.JSON(200, map[string]any{"enabled": true})
}

func (m *Module) disable(e *core.RequestEvent) error {
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
	if err := m.freshAuth(e, col, rec, f.Password); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := findRow(e.App, rec)
	if row == nil {
		return e.NotFoundError("TOTP is not set up.", nil)
	}
	if err := e.App.Delete(row); err != nil {
		return e.InternalServerError("", err)
	}
	audit(ActionDisabled, col.Name, rec.Id, map[string]any{"by": "owner", "ip": e.RealIP()})
	return e.NoContent(204)
}

func (m *Module) regenerate(e *core.RequestEvent) error {
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
	if err := m.freshAuth(e, col, rec, f.Password); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row := findRow(e.App, rec)
	if row == nil || !row.GetBool("enabled") {
		return e.BadRequestError("TOTP is not enabled.", nil)
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return e.InternalServerError("", err)
	}
	setRecoveryHashes(row, hashes)
	if err := e.App.Save(row); err != nil {
		return e.InternalServerError("", err)
	}
	audit(ActionRecoveryNew, col.Name, rec.Id, map[string]any{"ip": e.RealIP()})
	return e.JSON(200, map[string]any{"recovery_codes": plain})
}

// login completes an MFA challenge started by another method (mfaId flow).
func (m *Module) login(e *core.RequestEvent) error {
	col, err := m.authCollection(e)
	if err != nil {
		return err
	}
	if !col.MFA.Enabled {
		return e.ForbiddenError("The collection is not configured to allow MFA.", nil)
	}
	key := loadKey(e.App)
	if key == nil {
		return e.Error(503, "TOTP is not configured on this server.", nil)
	}
	f, err := m.readForm(e)
	if err != nil {
		return err
	}
	if f.MfaId == "" || f.Code == "" || len(f.MfaId) > 255 || len(f.Code) > 64 {
		return e.BadRequestError("An error occurred while validating the submitted data.", map[string]any{
			"code": map[string]string{"code": "validation_required", "message": "mfaId and code are required."},
		})
	}
	now := m.now()
	if !m.limiter.allow("mfa|"+f.MfaId, now, maxAttempts) || !m.limiter.allow("ip|"+e.RealIP(), now, 10*maxAttempts) {
		e.Response.Header().Set("Retry-After", "60")
		return e.TooManyRequestsError("Too many attempts, please try again later.", nil)
	}

	// mirror upstream checkMFA (apis/record_helpers.go) validation of the challenge
	mfa, err := e.App.FindMFAById(f.MfaId)
	if err != nil || mfa.HasExpired(col.MFA.DurationTime()) {
		if mfa != nil {
			_ = e.App.Delete(mfa)
		}
		return e.BadRequestError("Invalid or expired MFA session.", err)
	}
	if mfa.CollectionRef() != col.Id {
		return e.BadRequestError("Invalid MFA session.", nil)
	}
	if mfa.Method() == AuthMethod {
		return e.BadRequestError("A different authentication method is required.", nil)
	}
	rec, err := e.App.FindRecordById(col, mfa.RecordRef())
	if err != nil {
		return e.BadRequestError("Invalid MFA session.", err)
	}
	if isLocked(col.Name, rec) {
		return e.BadRequestError(msgAuthFailed, errors.New("identity locked"))
	}

	m.mu.Lock()
	row := findRow(e.App, rec)
	if row == nil || !row.GetBool("enabled") {
		m.mu.Unlock()
		return e.BadRequestError(msgAuthFailed, errors.New("totp not enabled"))
	}
	kind, cerr := m.consume(key, row, f.Code, true)
	m.mu.Unlock()
	if cerr != nil {
		return e.InternalServerError("", cerr)
	}
	if kind == "" {
		failure(col.Name, rec)
		return e.BadRequestError(msgAuthFailed, errors.New("invalid code"))
	}
	if kind == "recovery" {
		audit(ActionRecoveryUse, col.Name, rec.Id, map[string]any{"ip": e.RealIP()})
	}
	// RecordAuthResponse reads mfaId from the body, validates it again, deletes
	// the _mfas record and writes the standard auth response.
	return apis.RecordAuthResponse(e, rec, AuthMethod, map[string]any{"totp": kind})
}
