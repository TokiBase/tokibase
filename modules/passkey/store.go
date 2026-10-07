package passkey

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/types"
)

var b64 = base64.RawURLEncoding

func kernelHash(collectionId, recordId string) dbx.Expression {
	return dbx.HashExp{"collection": collectionId, "record": recordId}
}

// waUser adapts an auth record to webauthn.User. The user handle is the
// record id, so it is stable and identifies the record on discoverable login.
type waUser struct {
	rec   *core.Record
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte { return []byte(u.rec.Id) }
func (u *waUser) WebAuthnName() string {
	if v := u.rec.Email(); v != "" {
		return v
	}
	return u.rec.Id
}
func (u *waUser) WebAuthnDisplayName() string {
	if v := u.rec.GetString("name"); v != "" {
		return v
	}
	return u.WebAuthnName()
}
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (m *Module) newUser(rec *core.Record) (*waUser, error) {
	rows, err := m.app.FindAllRecords(CollectionName, kernelHash(rec.Collection().Id, rec.Id))
	if err != nil {
		return nil, err
	}
	u := &waUser{rec: rec}
	for _, r := range rows {
		c, err := toCredential(r)
		if err != nil {
			m.app.Logger().Warn("passkey: skipping unreadable passkey", "id", r.Id, "error", err)
			continue
		}
		u.creds = append(u.creds, *c)
	}
	return u, nil
}

func toCredential(r *core.Record) (*webauthn.Credential, error) {
	id, err := b64.DecodeString(r.GetString("credential_id"))
	if err != nil {
		return nil, err
	}
	pk, err := b64.DecodeString(r.GetString("public_key"))
	if err != nil {
		return nil, err
	}
	var aaguid []byte
	if u, err := uuid.Parse(r.GetString("aaguid")); err == nil {
		aaguid = u[:]
	}
	var transports []protocol.AuthenticatorTransport
	if raw := r.GetString("transports"); raw != "" {
		var ts []string
		if json.Unmarshal([]byte(raw), &ts) == nil {
			for _, t := range ts {
				transports = append(transports, protocol.AuthenticatorTransport(t))
			}
		}
	}
	return &webauthn.Credential{
		ID: id, PublicKey: pk, Transport: transports,
		AttestationType: "none",
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			BackupEligible: r.GetBool("backup_eligible"),
			BackupState:    r.GetBool("backup_state"),
		},
		Authenticator: webauthn.Authenticator{AAGUID: aaguid, SignCount: uint32(r.GetInt("sign_count"))},
	}, nil
}

func (m *Module) findByCredentialID(collectionId string, rawID []byte) (*core.Record, error) {
	p, err := m.app.FindFirstRecordByData(CollectionName, "credential_id", b64.EncodeToString(rawID))
	if err != nil {
		return nil, err
	}
	if p.GetString("collection") != collectionId {
		return nil, errors.New("credential belongs to another collection")
	}
	return p, nil
}

func (m *Module) savePasskey(rec *core.Record, c *webauthn.Credential, name string) (*core.Record, error) {
	col, err := m.app.FindCachedCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, err
	}
	r := core.NewRecord(col)
	r.Set("collection", rec.Collection().Id)
	r.Set("record", rec.Id)
	r.Set("credential_id", b64.EncodeToString(c.ID))
	r.Set("public_key", b64.EncodeToString(c.PublicKey))
	if len(c.Authenticator.AAGUID) == 16 {
		if u, err := uuid.FromBytes(c.Authenticator.AAGUID); err == nil {
			r.Set("aaguid", u.String())
		}
	}
	r.Set("sign_count", c.Authenticator.SignCount)
	ts := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		ts = append(ts, string(t))
	}
	r.Set("transports", ts)
	r.Set("backup_eligible", c.Flags.BackupEligible)
	r.Set("backup_state", c.Flags.BackupState)
	r.Set("uv_capable", c.Flags.UserVerified)
	r.Set("name", name)
	if err := m.app.Save(r); err != nil {
		return nil, err
	}
	return r, nil
}

// view is the public representation of a passkey (no key material).
func view(r *core.Record) map[string]any {
	var ts []string
	if raw := r.GetString("transports"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &ts)
	}
	if ts == nil {
		ts = []string{}
	}
	return map[string]any{
		"id":             r.Id,
		"name":           r.GetString("name"),
		"created":        r.GetDateTime("created").String(),
		"lastUsed":       r.GetDateTime("last_used").String(),
		"aaguid":         r.GetString("aaguid"),
		"transports":     ts,
		"backupEligible": r.GetBool("backup_eligible"),
		"backupState":    r.GetBool("backup_state"),
		"cloneSuspected": r.GetBool("clone_suspected"),
		"uvCapable":      r.GetBool("uv_capable"),
	}
}

func (m *Module) touch(r *core.Record, c *webauthn.Credential) error {
	r.Set("sign_count", c.Authenticator.SignCount)
	r.Set("backup_state", c.Flags.BackupState)
	r.Set("last_used", types.NowDateTime())
	if c.Authenticator.CloneWarning {
		r.Set("clone_suspected", true)
	}
	return m.app.Save(r)
}

// --- challenges ------------------------------------------------------

var errChallenge = errors.New("unknown, expired or already used challenge")

// putChallenge stores a pending challenge. ip (may be empty) is the client
// IP: at most maxChallengesPerIP pending challenges are kept per IP (the
// oldest are evicted) and at most maxChallenges per collection.
func (m *Module) putChallenge(kind, collectionId, recordId, ip string, s *webauthn.SessionData) error {
	now := m.now()
	db := m.app.AuxDB()
	_, _ = db.NewQuery("DELETE FROM {{_passkey_challenges}} WHERE [[expires]] < {:n}").Bind(dbx.Params{"n": now.UnixMilli()}).Execute()
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM {{_passkey_challenges}} WHERE [[collection]]={:c}").Bind(dbx.Params{"c": collectionId}).Row(&n); err == nil && n >= maxChallenges {
		return errors.New("too many pending challenges")
	}
	if ip != "" {
		// keep room for the new row: evict the oldest of this IP beyond the cap
		_, _ = db.NewQuery(`DELETE FROM {{_passkey_challenges}} WHERE [[ip]]={:ip} AND [[challenge]] IN (
			SELECT [[challenge]] FROM {{_passkey_challenges}} WHERE [[ip]]={:ip} ORDER BY [[expires]] DESC, rowid DESC LIMIT -1 OFFSET {:keep})`).
			Bind(dbx.Params{"ip": ip, "keep": maxChallengesPerIP - 1}).Execute()
	}
	s.Expires = now.Add(ChallengeTTL)
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = db.NewQuery(`INSERT INTO {{_passkey_challenges}} ([[challenge]],[[kind]],[[collection]],[[record]],[[ip]],[[data]],[[expires]]) VALUES ({:c},{:k},{:col},{:r},{:ip},{:d},{:e})`).
		Bind(dbx.Params{"c": s.Challenge, "k": kind, "col": collectionId, "r": recordId, "ip": ip, "d": string(data), "e": now.Add(ChallengeTTL).UnixMilli()}).Execute()
	return err
}

// takeChallenge atomically consumes a challenge (single use) and validates
// its kind, collection and TTL. It returns the session data and bound record id.
func (m *Module) takeChallenge(challenge, kind, collectionId string) (*webauthn.SessionData, string, error) {
	db := m.app.AuxDB()
	var row struct {
		Kind       string `db:"kind"`
		Collection string `db:"collection"`
		Record     string `db:"record"`
		Data       string `db:"data"`
		Expires    int64  `db:"expires"`
	}
	if challenge == "" {
		return nil, "", errChallenge
	}
	if err := db.NewQuery("SELECT [[kind]],[[collection]],[[record]],[[data]],[[expires]] FROM {{_passkey_challenges}} WHERE [[challenge]]={:c}").
		Bind(dbx.Params{"c": challenge}).One(&row); err != nil {
		return nil, "", errChallenge
	}
	res, err := db.NewQuery("DELETE FROM {{_passkey_challenges}} WHERE [[challenge]]={:c}").Bind(dbx.Params{"c": challenge}).Execute()
	if err != nil {
		return nil, "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, "", errChallenge // lost the race to a concurrent verify
	}
	if row.Expires < m.now().UnixMilli() {
		return nil, "", errChallenge
	}
	if row.Kind != kind || row.Collection != collectionId {
		return nil, "", fmt.Errorf("challenge belongs to another ceremony")
	}
	var s webauthn.SessionData
	if err := json.Unmarshal([]byte(row.Data), &s); err != nil {
		return nil, "", err
	}
	return &s, row.Record, nil
}
