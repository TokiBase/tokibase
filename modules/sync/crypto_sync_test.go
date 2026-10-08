//go:build !no_sync && !no_crypto

package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/crypto"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// PR9: encrypted fields over sync (docs/SYNC_DESIGN.md §7.6). Every node has its
// OWN master key; the ciphertext and the record hash are the same everywhere.

func randKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// newCryptoHub is a hub with the crypto module (master key of its own), `secret`
// as a blind-index field and `note` as a random one.
func newCryptoHub(t *testing.T, cryptoMode string) (*hubEnv, *crypto.Module) {
	t.Helper()
	crypto.WaitForServers = false
	crypto.RetireCooldown = 0
	t.Setenv(crypto.EnvMasterKey, randKey(t))
	app := newApp(t)
	cm := crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, app, RoleHub)
	srv := httptest.NewServer(e.mux)
	t.Cleanup(srv.Close)
	h := &hubEnv{env: e, srv: srv}
	for field, mode := range map[string]string{"secret": crypto.ModeBlindIndex, "note": crypto.ModeRandom} {
		if _, err := crypto.Enable(app, "items", field, mode, nil); err != nil {
			t.Fatal(err)
		}
	}
	cm.Invalidate()
	pc, _ := app.FindCollectionByNameOrId(PoliciesCollection)
	r := core.NewRecord(pc)
	r.Set("collection", "items")
	r.Set("direction", DirBoth)
	r.Set("enabled", true)
	if cryptoMode != "" {
		r.Set("crypto", cryptoMode)
	}
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return h, cm
}

// newCryptoSpoke is a spoke with its own master key ("" = crypto module without
// a key, "-" = no crypto module at all).
func newCryptoSpoke(t *testing.T, h *hubEnv, name, masterKey string) *itemsSpoke {
	t.Helper()
	switch masterKey {
	case "":
		t.Setenv(crypto.EnvMasterKey, randKey(t))
	default:
		t.Setenv(crypto.EnvMasterKey, "")
		t.Setenv(crypto.EnvMasterKeyFile, "")
	}
	app := newApp(t)
	if masterKey != "-" {
		crypto.Register(app)
		if err := crypto.EnsureSchema(app); err != nil {
			t.Fatal(err)
		}
	}
	m := RegisterRole(app, RoleSpoke)
	s := &spokeEnv{app: app, m: m}
	return finishSpoke(t, h, s, h.enroll(t, name, nil))
}

// finishSpoke is newItemsSpokeWith for a spoke app that already exists.
func finishSpoke(t *testing.T, h *hubEnv, s *spokeEnv, code string) *itemsSpoke {
	t.Helper()
	open := ""
	c := core.NewBaseCollection("items")
	c.Id = h.items.Id
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "qty"},
		&core.SelectField{Name: "tags", MaxSelect: 4, Values: []string{"a", "b", "c", "d"}},
		&core.FileField{Name: "photo", MaxSelect: 1, MaxSize: 1 << 20},
		&core.TextField{Name: "note"},
		&core.TextField{Name: "secret"},
		&core.NumberField{Name: "total"},
		&core.JSONField{Name: "meta"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := s.app.Save(c); err != nil {
		t.Fatal(err)
	}
	h.join(t, s, code)
	cl := s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
	})
	return &itemsSpoke{spokeEnv: s, c: cl}
}

func storedCT(t *testing.T, app core.App, id, field string) string {
	t.Helper()
	var s string
	if err := app.DB().NewQuery("SELECT " + field + " FROM items WHERE id={:id}").Bind(dbx.Params{"id": id}).Row(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func plain(t *testing.T, app core.App, id, field string) string {
	t.Helper()
	r, err := app.FindRecordById("items", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := crypto.Decrypt(app, r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return r.GetString(field)
}

func wrappedDEK(t *testing.T, app core.App, ver int) string {
	t.Helper()
	var s string
	if err := app.DB().NewQuery("SELECT wrapped_dek FROM _crypto_keys WHERE version={:v}").Bind(dbx.Params{"v": ver}).Row(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCryptoSyncDifferentMasterKeysSameCiphertext(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "gate-1", "")
	b := newCryptoSpoke(t, h, "gate-2", "")

	r := h.create(t, map[string]any{"title": "t", "secret": "s3cret-value", "note": "private note"})
	a.sync(t)
	b.sync(t)

	hubCT := storedCT(t, h.app, r.Id, "secret")
	if !strings.HasPrefix(hubCT, crypto.Prefix) {
		t.Fatalf("the hub must store ciphertext, got %q", hubCT)
	}
	for _, s := range []*itemsSpoke{a, b} {
		if got := storedCT(t, s.app, r.Id, "secret"); got != hubCT {
			t.Fatalf("the spoke must store the hub's ciphertext verbatim:\n spoke %q\n hub   %q", got, hubCT)
		}
		if got := storedCT(t, s.app, r.Id, "note"); got != storedCT(t, h.app, r.Id, "note") {
			t.Fatal("random-mode ciphertext must be verbatim too")
		}
		if plain(t, s.app, r.Id, "secret") != "s3cret-value" || plain(t, s.app, r.Id, "note") != "private note" {
			t.Fatal("the spoke must decrypt with the imported key")
		}
	}
	// the nodes hold the SAME data key but never the same wrapped form: each wraps under its own master key
	if wrappedDEK(t, a.app, 1) == wrappedDEK(t, h.app, 1) || wrappedDEK(t, a.app, 1) == wrappedDEK(t, b.app, 1) {
		t.Fatal("a spoke must re-wrap the key under its own master key")
	}
	// hashes and digests are identical (ciphertext hashed verbatim)
	requireConverged(t, h, a, b)
	hh, _ := hubItem(t, h, r.Id), 0
	sr, _ := a.app.FindRecordById("items", r.Id)
	h1, _ := RecordHash(hh, h.pol(t))
	h2, _ := RecordHash(sr, h.pol(t))
	if !bytes.Equal(h1, h2) {
		t.Fatal("record hashes differ between hub and spoke")
	}
	// the blind index works on the spoke (recomputed locally from the same DEK)
	got, err := crypto.FindByBlindIndex(a.app, "items", "secret", "s3cret-value")
	if err != nil || len(got) != 1 || got[0].Id != r.Id {
		t.Fatalf("blind index on the spoke: %v %v", got, err)
	}
	if got, _ := crypto.FindByBlindIndex(b.app, "items", "secret", "nope"); len(got) != 0 {
		t.Fatal("unexpected blind index hit")
	}

	// a spoke edit: encrypted with the imported key, pushed verbatim, decrypted by the hub
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("secret", "edited-by-a")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if storedCT(t, h.app, r.Id, "secret") != storedCT(t, a.app, r.Id, "secret") || storedCT(t, b.app, r.Id, "secret") != storedCT(t, a.app, r.Id, "secret") {
		t.Fatal("the edited ciphertext must be identical on every node")
	}
	for _, app := range []core.App{h.app, a.app, b.app} {
		if plain(t, app, r.Id, "secret") != "edited-by-a" {
			t.Fatal("every node must decrypt the edit")
		}
		got, err := crypto.FindByBlindIndex(app, "items", "secret", "edited-by-a")
		if err != nil || len(got) != 1 {
			t.Fatalf("blind index after the edit: %v %v", got, err)
		}
		if old, _ := crypto.FindByBlindIndex(app, "items", "secret", "s3cret-value"); len(old) != 0 {
			t.Fatal("the old blind index entry must be gone")
		}
	}
	requireConverged(t, h, a, b)

	// a record created on a spoke reaches the hub and the other spoke
	nr := a.create(t, map[string]any{"title": "n", "secret": "created-on-a"})
	a.sync(t)
	b.sync(t)
	if plain(t, h.app, nr.Id, "secret") != "created-on-a" || storedCT(t, h.app, nr.Id, "secret") != storedCT(t, b.app, nr.Id, "secret") {
		t.Fatal("a record created on a spoke must arrive with the same ciphertext")
	}
	requireConverged(t, h, a, b)
}

func TestCryptoSyncSnapshotBootstrapCarriesCiphertext(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	var ids []string
	for _, v := range []string{"one", "two", "three"} {
		ids = append(ids, h.create(t, map[string]any{"title": v, "secret": "s-" + v}).Id)
	}
	a := newCryptoSpoke(t, h, "late", "")
	a.sync(t) // the first handshake bootstraps from a snapshot
	for i, id := range ids {
		if storedCT(t, a.app, id, "secret") != storedCT(t, h.app, id, "secret") {
			t.Fatalf("record %d: ciphertext differs", i)
		}
	}
	requireConverged(t, h, a)
	if got, err := crypto.FindByBlindIndex(a.app, "items", "secret", "s-two"); err != nil || len(got) != 1 {
		t.Fatalf("blind index after the snapshot: %v %v", got, err)
	}
}

func TestCryptoSyncRotationPropagates(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "gate-1", "")
	old := h.create(t, map[string]any{"title": "old", "secret": "before-rotation"})
	a.sync(t)

	ver, _, err := crypto.Rotate(h.app, "items", nil)
	if err != nil || ver != 2 {
		t.Fatalf("rotate: %d %v", ver, err)
	}
	fresh := h.create(t, map[string]any{"title": "new", "secret": "after-rotation"})
	if !strings.HasPrefix(storedCT(t, h.app, fresh.Id, "secret"), crypto.Prefix+"2:") {
		t.Fatal("the new record must use key version 2")
	}
	// the record with version 2 arrives before the key: the apply waits and the handshake brings it
	if r := a.c.RunOnce(ctxb); r.Err == nil || !strings.Contains(r.Err.Error(), "key") {
		t.Fatalf("a ciphertext of an unknown key version must wait for the next handshake: %v", r.Err)
	}
	a.sync(t)
	if plain(t, a.app, fresh.Id, "secret") != "after-rotation" {
		t.Fatal("the spoke must decrypt version 2 after the next handshake")
	}
	var n int
	_ = a.app.DB().NewQuery("SELECT COUNT(*) FROM _crypto_keys WHERE version>0").Row(&n)
	if n != 2 {
		t.Fatalf("the spoke must have both versions, has %d", n)
	}
	// a spoke write now uses version 2
	ar, _ := a.app.FindRecordById("items", fresh.Id)
	ar.Set("secret", "edited-v2")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedCT(t, a.app, fresh.Id, "secret"), crypto.Prefix+"2:") {
		t.Fatal("a spoke must encrypt with the newest version")
	}
	a.sync(t)
	if plain(t, h.app, fresh.Id, "secret") != "edited-v2" {
		t.Fatal("the hub must decrypt the spoke's version 2 ciphertext")
	}

	// retiring version 1 on the hub: the spoke keeps it while a local row still holds a ciphertext of it
	// (the rotation sweep does not emit sync changes, docs/modules/crypto.md "Sync")
	if res, err := crypto.Retire(h.app, "items"); err != nil || len(res.Retired) != 1 {
		t.Fatalf("retire: %+v %v", res, err)
	}
	a.c.ForceHandshake()
	a.sync(t)
	if got := plain(t, a.app, old.Id, "secret"); got != "before-rotation" {
		t.Fatalf("a row still on the retired version must stay readable on the spoke, got %q", got)
	}
}

func TestCryptoSpokeWithoutMasterKeyRefuses(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	h.create(t, map[string]any{"title": "t", "secret": "x"})
	t.Setenv(crypto.EnvMasterKey, "")
	t.Setenv(crypto.EnvMasterKeyFile, "")
	app := newApp(t)
	crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	m := RegisterRole(app, RoleSpoke)
	s := finishSpoke(t, h, &spokeEnv{app: app, m: m}, h.enroll(t, "nokey", nil))
	r := s.c.RunOnce(ctxb)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "master key") || !strings.Contains(r.Err.Error(), "refusing to sync") {
		t.Fatalf("a spoke without a master key must refuse with a clear error, got %v", r.Err)
	}
	if n, _ := app.CountRecords("items"); n != 0 {
		t.Fatal("nothing may be applied")
	}

	// a build without the crypto module refuses as well
	s2 := newCryptoSpoke(t, h, "nomodule", "-")
	r = s2.c.RunOnce(ctxb)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "no crypto module") {
		t.Fatalf("a spoke without the crypto module must refuse, got %v", r.Err)
	}
}

func TestCryptoStripHidesTheFieldAndKeepsHashesEqual(t *testing.T) {
	h, _ := newCryptoHub(t, CryptoStrip)
	// one spoke with the module but no key, one without the module: neither needs a key
	t.Setenv(crypto.EnvMasterKey, "")
	t.Setenv(crypto.EnvMasterKeyFile, "")
	app := newApp(t)
	crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	m := RegisterRole(app, RoleSpoke)
	a := finishSpoke(t, h, &spokeEnv{app: app, m: m}, h.enroll(t, "keyless", nil))
	b := newCryptoSpoke(t, h, "nomodule", "-")

	r := h.create(t, map[string]any{"title": "visible", "secret": "must-not-travel", "note": "nor-this"})
	a.sync(t)
	b.sync(t)

	for _, s := range []*itemsSpoke{a, b} {
		sr, err := s.app.FindRecordById("items", r.Id)
		if err != nil {
			t.Fatal(err)
		}
		if sr.GetString("title") != "visible" || sr.GetString("secret") != "" || sr.GetString("note") != "" {
			t.Fatalf("strip: title=%q secret=%q note=%q", sr.GetString("title"), sr.GetString("secret"), sr.GetString("note"))
		}
	}
	// the handshake ships no keys for it
	if hs, err := a.c.Handshake(ctxb); err != nil || len(hs.Keys) != 0 {
		t.Fatalf("keys for a strip collection: %v %v", hs, err)
	}
	// the hash and the digest of a stripped collection leave the field out on every node
	requireConverged(t, h, a, b)
	for _, row := range h.changes(t) {
		if strings.Contains(row.Patch, "tkc1:") {
			t.Fatalf("a strip collection must not capture ciphertext: %s", row.Patch)
		}
	}
	// an update of the hidden field produces nothing to pull; an update of a visible one still works
	hr := hubItem(t, h, r.Id)
	hr.Set("secret", "changed-secret")
	hr.Set("title", "visible 2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if x, _ := a.app.FindRecordById("items", r.Id); x.GetString("title") != "visible 2" || x.GetString("secret") != "" {
		t.Fatal("strip after an update")
	}
	requireConverged(t, h, a, b)

	// a node that pushes the withheld field is refused
	tok := a.token(t)
	st, res, _ := rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, nowHLC(-5000, 0), 0, h.items.Id, r.Id, "u", map[string]any{"secret": "planted"})))
	if st != 200 || len(res.Results) != 1 || res.Results[0].Status != proto.ResRejected || res.Results[0].Code != proto.CodePolicyCrypto {
		t.Fatalf("a push of a stripped field must be rejected: %d %+v", st, res)
	}
	if plain(t, h.app, r.Id, "secret") != "changed-secret" {
		t.Fatal("the hub value must be untouched")
	}
}

func TestCryptoPushOfUndecryptableCiphertextIsRefused(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "gate-1", "")
	a.sync(t)
	tok := a.token(t)
	// well-formed but not produced with the collection key (or for another record)
	fake := crypto.Prefix + "1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 40))
	st, res, _ := rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, nowHLC(-5000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x", "secret": fake})))
	if st != 200 || len(res.Results) != 1 || res.Results[0].Status != proto.ResRejected {
		t.Fatalf("garbage ciphertext must be refused: %d %+v", st, res)
	}
	if _, err := h.app.FindRecordById("items", "recordaaaaaaaa1"); err == nil {
		t.Fatal("the record must not exist")
	}
}
