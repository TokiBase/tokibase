//go:build !no_sync && !no_crypto

package sync

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/crypto"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// QC of PR9 (review P9-1 .. P9-12): the strip set belongs to the policy, key
// sweeps are synced, keys are signed, missing keys cost one collection.

// newCryptoHubLate is a hub with the crypto module and the items policy, but NO
// encrypted field yet (the fields are enabled by the test, on existing data).
func newCryptoHubLate(t *testing.T, cryptoMode string) (*hubEnv, *crypto.Module) {
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

// addPlainCollection adds an unencrypted synced collection `plain` on the hub.
func addPlainCollection(t *testing.T, h *hubEnv) *core.Collection {
	t.Helper()
	open := ""
	c := core.NewBaseCollection("plain")
	c.Fields.Add(&core.TextField{Name: "title"})
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	pc, _ := h.app.FindCollectionByNameOrId(PoliciesCollection)
	r := core.NewRecord(pc)
	r.Set("collection", "plain")
	r.Set("direction", DirBoth)
	r.Set("enabled", true)
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *hubEnv) createPlain(t *testing.T, title string) *core.Record {
	t.Helper()
	c, _ := h.app.FindCollectionByNameOrId("plain")
	r := core.NewRecord(c)
	r.Set("title", title)
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func qc9PolicyRow(t *testing.T, h *hubEnv, col string) *core.Record {
	t.Helper()
	r, err := h.app.FindFirstRecordByFilter(PoliciesCollection, "collection={:c}", dbx.Params{"c": col})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hubChangeLines(t *testing.T, h *hubEnv, where string) []chg {
	t.Helper()
	return changeRows(t, h.app, where, nil)
}

// ---- P9-1: the strip set is part of the policy ---------------------------------

func TestQC9StripListIsPersistedInThePolicy(t *testing.T) {
	h, _ := newCryptoHub(t, CryptoStrip)
	got := stripFieldsOfRow(qc9PolicyRow(t, h, "items"))
	if strings.Join(got, ",") != "note,secret" {
		t.Fatalf("strip_fields = %v", got)
	}
	if p := h.pol(t); p == nil || len(p.StripFields) != 2 {
		t.Fatalf("policy view: %+v", p)
	}
	// a flip away from strip clears the list
	r := qc9PolicyRow(t, h, "items")
	r.Set("crypto", "ciphertext")
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if got := stripFieldsOfRow(qc9PolicyRow(t, h, "items")); len(got) != 0 {
		t.Fatalf("strip_fields after ciphertext = %v", got)
	}
}

func TestQC9StripSurvivesCryptoDisable(t *testing.T) {
	h, _ := newCryptoHub(t, CryptoStrip)
	a := newCryptoSpoke(t, h, "a", "-")
	r := h.create(t, map[string]any{"title": "t", "secret": "must-not-travel", "note": "nor-this"})
	a.sync(t)

	// the admin decrypts the fields: the registry no longer lists them, the policy still withholds them
	for _, f := range []string{"secret", "note"} {
		if _, err := crypto.Disable(h.app, "items", f, nil); err != nil {
			t.Fatal(err)
		}
	}
	if kernel.IsSensitive(h.items.Id, "secret") {
		t.Fatal("precondition: the registry must be empty after disable")
	}
	hr := hubItem(t, h, r.Id)
	hr.Set("secret", "plaintext-now")
	hr.Set("title", "t2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	h.create(t, map[string]any{"title": "new", "secret": "plaintext-too"})
	a.sync(t)
	for _, row := range h.changes(t) {
		if strings.Contains(row.Patch, "plaintext") || strings.Contains(row.Patch, "must-not-travel") {
			t.Fatalf("a withheld field was captured: %s", row.Patch)
		}
	}
	sr, _ := a.app.FindRecordById("items", r.Id)
	if sr.GetString("secret") != "" || sr.GetString("title") != "t2" {
		t.Fatalf("spoke secret=%q title=%q", sr.GetString("secret"), sr.GetString("title"))
	}
	// the policy view of the handshake and a new node's snapshot agree
	found := false
	for _, p := range h.m.handshakePolicies() {
		if p.Collection == "items" {
			found = true
			if strings.Join(p.Exclude, ",") != "note,secret" {
				t.Fatalf("exclude after disable = %v", p.Exclude)
			}
		}
	}
	if !found {
		t.Fatal("policy missing")
	}
	b := newCryptoSpoke(t, h, "late", "-")
	b.sync(t)
	recs, _ := b.app.FindAllRecords("items")
	for _, x := range recs {
		if x.GetString("secret") != "" {
			t.Fatalf("snapshot leaked %q", x.GetString("secret"))
		}
	}
	requireConverged(t, h, a, b)

	// typed operations on a withheld field are refused too
	tok := a.token(t)
	st, res, _ := rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, nowHLC(-5000, 0), 0, h.items.Id, r.Id, "u", map[string]any{"secret": map[string]any{"$inc": 1}})))
	if st != 200 || len(res.Results) != 1 || res.Results[0].Status != proto.ResRejected {
		t.Fatalf("a typed op on a stripped field must be rejected: %d %+v", st, res)
	}
}

// A hub without the crypto module (or one whose module is not loaded yet) keeps
// withholding what the policy says.
func TestQC9StripWithoutCryptoModuleAndLateModule(t *testing.T) {
	h := newHub(t)
	pcol, _ := h.app.FindCollectionByNameOrId(PoliciesCollection)
	pr := core.NewRecord(pcol)
	pr.Set("collection", "items")
	pr.Set("direction", DirBoth)
	pr.Set("enabled", true)
	pr.Set("crypto", CryptoStrip)
	pr.Set("strip_fields", []string{"secret"}) // as saved while the module was loaded
	if err := h.app.Save(pr); err != nil {
		t.Fatal(err)
	}
	if got := stripFieldsOfRow(qc9PolicyRow(t, h, "items")); strings.Join(got, ",") != "secret" {
		t.Fatalf("a saved list must be kept: %v", got)
	}
	a := newItemsSpoke(t, h, "a")
	r := h.create(t, map[string]any{"title": "t", "secret": "plain-secret", "note": "n"})
	a.sync(t)
	sr, _ := a.app.FindRecordById("items", r.Id)
	if sr.GetString("secret") != "" || sr.GetString("note") != "n" {
		t.Fatalf("without a crypto module: secret=%q note=%q", sr.GetString("secret"), sr.GetString("note"))
	}
	for _, row := range h.changes(t) {
		if strings.Contains(row.Patch, "plain-secret") {
			t.Fatalf("captured: %s", row.Patch)
		}
	}
	requireConverged(t, h, a)

	// the module is loaded late and a field of the collection is encrypted afterwards
	t.Setenv(crypto.EnvMasterKey, randKey(t))
	crypto.WaitForServers = false
	crypto.Register(h.app)
	if err := crypto.EnsureSchema(h.app); err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.Enable(h.app, "items", "note", crypto.ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	if got := stripFieldsOfRow(qc9PolicyRow(t, h, "items")); strings.Join(got, ",") != "note,secret" {
		t.Fatalf("a field encrypted later must join the strip set: %v", got)
	}
	hr := hubItem(t, h, r.Id)
	hr.Set("note", "after-enable")
	hr.Set("title", "t2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	sr, _ = a.app.FindRecordById("items", r.Id)
	if sr.GetString("title") != "t2" || sr.GetString("secret") != "" || strings.HasPrefix(sr.GetString("note"), crypto.Prefix) {
		t.Fatalf("late module: title=%q secret=%q note=%q", sr.GetString("title"), sr.GetString("secret"), sr.GetString("note"))
	}
	for _, row := range h.changes(t) {
		if strings.Contains(row.Patch, crypto.Prefix) {
			t.Fatalf("ciphertext of a withheld field was captured: %s", row.Patch)
		}
	}
}

// ---- P9-2: key sweeps produce change rows ---------------------------------------

func TestQC9EnableOnSyncedDataReachesSpokes(t *testing.T) {
	h, _ := newCryptoHubLate(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	b := newCryptoSpoke(t, h, "b", "")
	var ids []string
	for _, v := range []string{"one", "two", "three"} {
		ids = append(ids, h.create(t, map[string]any{"title": v, "secret": "s-" + v, "note": "n-" + v}).Id)
	}
	a.sync(t)
	b.sync(t)
	if storedCT(t, a.app, ids[0], "secret") != "s-one" {
		t.Fatal("precondition: plaintext on the spoke")
	}
	before := len(hubChangeLines(t, h, "op='u'"))
	if _, err := crypto.Enable(h.app, "items", "secret", crypto.ModeBlindIndex, nil); err != nil {
		t.Fatal(err)
	}
	rows := hubChangeLines(t, h, "op='u'")
	if len(rows) != before+3 {
		t.Fatalf("the sweep must write one change per record: %d -> %d", before, len(rows))
	}
	var lastH int64
	for _, row := range rows[before:] {
		if !strings.Contains(row.Patch, crypto.Prefix) || row.Tx != "" || row.Node != h.m.NodeID() {
			t.Fatalf("sweep row: %+v", row)
		}
		if row.HLC <= lastH {
			t.Fatal("HLC must increase")
		}
		lastH = row.HLC
	}
	a.sync(t)
	b.sync(t)
	for _, id := range ids {
		hubCT := storedCT(t, h.app, id, "secret")
		if !strings.HasPrefix(hubCT, crypto.Prefix) {
			t.Fatal("hub must hold ciphertext")
		}
		for _, s := range []*itemsSpoke{a, b} {
			if storedCT(t, s.app, id, "secret") != hubCT {
				t.Fatalf("spoke still has %q, hub %q", storedCT(t, s.app, id, "secret"), hubCT)
			}
		}
	}
	if plain(t, a.app, ids[1], "secret") != "s-two" {
		t.Fatal("the spoke must decrypt with the imported key")
	}
	if got, err := crypto.FindByBlindIndex(b.app, "items", "secret", "s-three"); err != nil || len(got) != 1 {
		t.Fatalf("blind index on the spoke after the sweep: %v %v", got, err)
	}
	requireConverged(t, h, a, b)
	// a field the sweep did not touch stays as it is
	if storedCT(t, a.app, ids[0], "note") != "n-one" {
		t.Fatal("note was not enabled")
	}
}

func TestQC9DisableReachesSpokes(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	r := h.create(t, map[string]any{"title": "t", "secret": "to-be-plain", "note": "stays-encrypted"})
	a.sync(t)
	if !strings.HasPrefix(storedCT(t, a.app, r.Id, "secret"), crypto.Prefix) {
		t.Fatal("precondition: ciphertext on the spoke")
	}
	if _, err := crypto.Disable(h.app, "items", "secret", nil); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if got := storedCT(t, a.app, r.Id, "secret"); got != "to-be-plain" {
		t.Fatalf("the spoke must hold the plaintext the hub holds, got %q", got)
	}
	if !strings.HasPrefix(storedCT(t, a.app, r.Id, "note"), crypto.Prefix) {
		t.Fatal("the other field stays encrypted")
	}
	rows, _ := a.app.FindAllRecords("_crypto_fields")
	for _, cf := range rows {
		if cf.GetString("field") == "secret" {
			t.Fatal("the spoke must drop the configuration of the disabled field")
		}
	}
	requireConverged(t, h, a)
	// an edit on the spoke now travels as plaintext
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("secret", "edited-plain")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if storedCT(t, h.app, r.Id, "secret") != "edited-plain" {
		t.Fatalf("hub got %q", storedCT(t, h.app, r.Id, "secret"))
	}
	requireConverged(t, h, a)
}

func TestQC9RotateSweepConvergesAndRetireWaitsForOfflineSpoke(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	b := newCryptoSpoke(t, h, "b", "")
	r := h.create(t, map[string]any{"title": "t", "secret": "v1-secret"})
	a.sync(t)
	b.sync(t)

	if v, _, err := crypto.Rotate(h.app, "items", nil); err != nil || v != 2 {
		t.Fatalf("rotate: %d %v", v, err)
	}
	a.c.ForceHandshake()
	a.sync(t) // a fetches v2 and the swept rows
	if !strings.HasPrefix(storedCT(t, a.app, r.Id, "secret"), crypto.Prefix+"2:") {
		t.Fatalf("the rotation sweep must reach the spoke: %q", storedCT(t, a.app, r.Id, "secret")[:12])
	}
	// b is offline and edits under v1 (later than the sweep: last write wins by HLC, no tie of a few ms)
	time.Sleep(20 * time.Millisecond)
	br, _ := b.app.FindRecordById("items", r.Id)
	br.Set("secret", "offline-edit")
	if err := b.app.Save(br); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedCT(t, b.app, r.Id, "secret"), crypto.Prefix+"1:") {
		t.Fatal("precondition: b writes under v1")
	}

	// no hub row uses v1, but b may still write it: retire refuses and names b
	res, err := crypto.Retire(h.app, "items")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Retired) != 0 || len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0], b.m.NodeID()) {
		t.Fatalf("retire must be blocked by the offline node: %+v", res)
	}

	// b reconnects: the hub learns about its queue; v1 stays for as long as the change is not applied
	b.c.ForceHandshake()
	if _, err := b.c.Handshake(ctxb); err != nil {
		t.Fatal(err)
	}
	if res, _ := crypto.Retire(h.app, "items"); len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0], "v1") {
		t.Fatalf("a node that reports unsent v1 changes must block: %+v", res)
	}
	b.sync(t) // pushes the v1 edit while the hub still has the key
	if plain(t, h.app, r.Id, "secret") != "offline-edit" {
		t.Fatalf("hub: %q", plain(t, h.app, r.Id, "secret"))
	}
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
}

func TestQC9ForcedRetireParksTheOfflineEdit(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	b := newCryptoSpoke(t, h, "b", "")
	r := h.create(t, map[string]any{"title": "t", "secret": "v1-secret"})
	b.sync(t)
	if _, _, err := crypto.Rotate(h.app, "items", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the edit is later than the sweep, so last write wins does not supersede it
	br, _ := b.app.FindRecordById("items", r.Id)
	br.Set("secret", "offline-edit")
	if err := b.app.Save(br); err != nil {
		t.Fatal(err)
	}
	res, err := crypto.RetireForce(h.app, "items", true)
	if err != nil || len(res.Retired) != 1 {
		t.Fatalf("forced retire: %+v %v", res, err)
	}
	b.c.ForceHandshake()
	b.sync(t)
	rows := hubChangeLines(t, h, "status='parked'")
	if len(rows) != 1 {
		for _, c := range h.changes(t) {
			t.Logf("hub change seq=%d node=%s op=%s status=%s patch=%.60s", c.Seq, c.Node, c.Op, c.Status, c.Patch)
		}
		var codes []string
		_ = h.app.DB().NewQuery("SELECT COALESCE(code,'') FROM _changes").Column(&codes)
		t.Logf("codes %v", codes)
		t.Fatalf("the edit must be parked, not applied or reverted: %d parked", len(rows))
	}
	var code string
	_ = h.app.DB().NewQuery("SELECT code FROM _changes WHERE status='parked'").Row(&code)
	if code != proto.CodeCryptoRetired {
		t.Fatalf("parked with code %q", code)
	}
	if plain(t, h.app, r.Id, "secret") != "v1-secret" && !strings.HasPrefix(storedCT(t, h.app, r.Id, "secret"), crypto.Prefix+"2:") {
		t.Fatal("the hub row must be unchanged")
	}
}

// ---- P9-3: signed keys ----------------------------------------------------------

func TestQC9KeysSignatureBindsEverything(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := []proto.Key{{Collection: "c1", Version: 1, Wrapped: "AAA"}, {Collection: "c1", Version: 2, Retired: true}}
	errs := []proto.KeyError{{Collection: "c2", Code: proto.CodeKeyExport}}
	sig := proto.SignKeys(priv, "n1", "h1", "100", "nonce", keys, errs)
	if !proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", keys, errs, sig) {
		t.Fatal("valid signature rejected")
	}
	for name, f := range map[string]func() bool{
		"node":  func() bool { return proto.VerifyKeys(pub, "n2", "h1", "100", "nonce", keys, errs, sig) },
		"hub":   func() bool { return proto.VerifyKeys(pub, "n1", "h2", "100", "nonce", keys, errs, sig) },
		"ts":    func() bool { return proto.VerifyKeys(pub, "n1", "h1", "101", "nonce", keys, errs, sig) },
		"nonce": func() bool { return proto.VerifyKeys(pub, "n1", "h1", "100", "other", keys, errs, sig) },
		"extra": func() bool {
			return proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", append(append([]proto.Key{}, keys...), proto.Key{Collection: "c1", Version: 3, Wrapped: "EVIL"}), errs, sig)
		},
		"wrapped": func() bool {
			return proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", []proto.Key{{Collection: "c1", Version: 1, Wrapped: "BBB"}, keys[1]}, errs, sig)
		},
		"retired": func() bool {
			return proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", []proto.Key{keys[0], {Collection: "c1", Version: 2}}, errs, sig)
		},
		"errors": func() bool { return proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", keys, nil, sig) },
		"empty":  func() bool { return proto.VerifyKeys(pub, "n1", "h1", "100", "nonce", keys, errs, "") },
	} {
		if f() {
			t.Fatalf("a changed %s must not verify", name)
		}
	}
}

// proxyHandshake forwards to the hub and lets mut change the JSON of the handshake answer.
func proxyHandshake(t *testing.T, h *hubEnv, mut func(map[string]any)) *httptest.Server {
	t.Helper()
	target, _ := url.Parse(h.srv.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	orig := rp.Director
	rp.Director = func(r *http.Request) {
		orig(r)
		r.Header.Del("Accept-Encoding")
	}
	rp.ModifyResponse = func(res *http.Response) error {
		if !strings.HasSuffix(res.Request.URL.Path, "/handshake") || res.StatusCode != 200 {
			return nil
		}
		raw, _ := io.ReadAll(res.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		mut(m)
		out, _ := json.Marshal(m)
		res.Body = io.NopCloser(bytes.NewReader(out))
		res.ContentLength = int64(len(out))
		res.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
	srv := httptest.NewServer(rp)
	t.Cleanup(srv.Close)
	return srv
}

func TestQC9ForgedKeyEntryIsRejected(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	h.create(t, map[string]any{"title": "t", "secret": "x"})
	mode := "ok"
	px := proxyHandshake(t, h, func(m map[string]any) {
		keys, _ := m["keys"].([]any)
		switch mode {
		case "inject":
			// an attacker-chosen version N+1 for the collection
			m["keys"] = append(keys, map[string]any{"collection": h.items.Id, "version": 2, "wrapped": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 80))})
		case "unsigned":
			delete(m, "keys_sig")
		case "swap":
			if len(keys) > 0 {
				k := keys[0].(map[string]any)
				k["wrapped"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 80))
			}
		}
	})
	t.Setenv(crypto.EnvMasterKey, randKey(t))
	app := newApp(t)
	crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	m := RegisterRole(app, RoleSpoke)
	s := &spokeEnv{app: app, m: m}
	code := h.enroll(t, "victim", nil)
	// enroll against the proxy so that the node talks to the hub through it
	if _, err := client.Join(ctxb, app, client.EnrollParams{HubURL: px.URL, Code: code, Identity: m.Identity(), Profile: "edge", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	cl := s.client(t, h, func(o *client.Options) { o.Backend = backend{m}; o.Interval = 0 })
	for _, md := range []string{"inject", "unsigned", "swap"} {
		mode = md
		if _, err := cl.Handshake(ctxb); err == nil || !strings.Contains(err.Error(), "not signed by the enrolled hub") {
			t.Fatalf("%s: forged keys must be refused, got %v", md, err)
		}
	}
	mode = "ok"
	hs, err := cl.Handshake(ctxb)
	if err != nil || len(hs.Keys) == 0 || hs.KeysSig == "" {
		t.Fatalf("the untouched answer must pass: %v %+v", err, hs)
	}
	var n int
	_ = app.DB().NewQuery("SELECT COUNT(*) FROM _crypto_keys WHERE version>0").Row(&n)
	if n != 0 {
		t.Fatalf("a refused handshake must not store keys, found %d", n)
	}
}

// ---- P9-4: key export failures and the node key ---------------------------------

func TestQC9ExportFailureCostsOnlyTheEncryptedCollection(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	addPlainCollection(t, h)
	r := h.create(t, map[string]any{"title": "t", "secret": "x"})
	a := newCryptoSpoke(t, h, "a", "")
	a.sync(t) // healthy: both collections arrive, the node holds the key
	if _, err := a.app.FindRecordById("items", r.Id); err != nil {
		t.Fatal(err)
	}
	// the stored key no longer unwraps (wrong master key, damaged row)
	var good string
	_ = h.app.DB().NewQuery("SELECT wrapped_dek FROM _crypto_keys WHERE version=1").Row(&good)
	execSQL(t, h, "UPDATE _crypto_keys SET wrapped_dek={:w} WHERE version=1", dbx.Params{"w": "garbage"})
	p := h.createPlain(t, "plain-1")

	hs, err := a.c.Handshake(ctxb)
	if err != nil {
		t.Fatalf("a key export problem must not fail the handshake: %v", err)
	}
	if len(hs.Keys) != 0 || len(hs.KeyErrors) != 1 || hs.KeyErrors[0].Collection != h.items.Id || hs.KeyErrors[0].Code != proto.CodeKeyExport || hs.KeysSig == "" {
		t.Fatalf("keys=%v errors=%v sig=%q", hs.Keys, hs.KeyErrors, hs.KeysSig)
	}
	a.c.ForceHandshake()
	a.sync(t)
	if _, err := a.app.FindRecordById("plain", p.Id); err != nil {
		t.Fatalf("the unencrypted collection must keep syncing: %v", err)
	}

	// a node that joins now has no key at all: the collection is skipped after a few attempts
	b := newCryptoSpoke(t, h, "b", "")
	for i := 0; i < 8; i++ {
		_ = b.c.RunOnce(ctxb)
		if len(client.KeyMissingCollections(b.app)) > 0 {
			break
		}
	}
	if got := client.KeyMissingCollections(b.app); len(got) != 1 || got[0] != h.items.Id {
		t.Fatalf("the collection must be marked key_missing: %v", got)
	}
	b.sync(t)
	if _, err := b.app.FindRecordById("plain", p.Id); err != nil {
		t.Fatalf("the unencrypted collection must keep syncing: %v", err)
	}
	if st, _ := GetStatus(b.app, RoleSpoke); len(st.KeyMissing) != 1 {
		t.Fatalf("toki sync status must show it: %+v", st)
	}
	if st := b.c.Status(); len(st.KeyMissing) != 1 {
		t.Fatalf("client status: %+v", st)
	}

	// the hub is repaired: the collection comes back through a re-bootstrap
	execSQL(t, h, "UPDATE _crypto_keys SET wrapped_dek={:w} WHERE version=1", dbx.Params{"w": good})
	b.c.ForceHandshake()
	for i := 0; i < 6; i++ {
		rr := b.c.RunOnce(ctxb)
		cur, _ := client.LoadCursor(b.app)
		if cur != nil && cur.State == client.StateRebootstrapRequired {
			if err := b.c.Bootstrap(ctxb); err != nil {
				t.Fatalf("re-bootstrap after the key arrived (run %d, %v): %v", i, rr.Err, err)
			}
		}
		if _, err := b.app.FindRecordById("items", r.Id); err == nil {
			break
		}
	}
	if got := client.KeyMissingCollections(b.app); len(got) != 0 {
		t.Fatalf("the mark must clear when the key arrives: %v", got)
	}
	if _, err := b.app.FindRecordById("items", r.Id); err != nil {
		t.Fatalf("the skipped collection must be fetched again: %v", err)
	}
}

func TestQC9SnapshotSkipsACollectionWithoutItsKey(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	addPlainCollection(t, h)
	h.create(t, map[string]any{"title": "t", "secret": "x"})
	p := h.createPlain(t, "plain-1")
	execSQL(t, h, "UPDATE _crypto_keys SET wrapped_dek={:w} WHERE version=1", dbx.Params{"w": "garbage"})
	a := newCryptoSpoke(t, h, "late", "") // joins and bootstraps from a snapshot
	for i := 0; i < 8; i++ {
		r := a.c.RunOnce(ctxb)
		t.Logf("run %d: err=%v keymissing=%v", i, r.Err, client.KeyMissingCollections(a.app))
		if _, err := a.app.FindRecordById("plain", p.Id); err == nil {
			break
		}
	}
	if _, err := a.app.FindRecordById("plain", p.Id); err != nil {
		t.Fatalf("the snapshot must complete for the other collection: %v", err)
	}
	if got := client.KeyMissingCollections(a.app); len(got) != 1 || got[0] != h.items.Id {
		t.Fatalf("key_missing = %v", got)
	}
	if n, _ := a.app.CountRecords("items"); n != 0 {
		t.Fatalf("the skipped collection stays empty, has %d", n)
	}
}

func TestQC9ChangeWithAnUnshippedKeyVersionIsSkippedAfterRetries(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	addPlainCollection(t, h)
	a := newCryptoSpoke(t, h, "a", "")
	a.sync(t)
	if _, _, err := crypto.Rotate(h.app, "items", nil); err != nil {
		t.Fatal(err)
	}
	r := h.create(t, map[string]any{"title": "v2", "secret": "after-rotation"})
	// the hub loses the newest key (restored from an older backup): it never ships v2
	execSQL(t, h, "DELETE FROM _crypto_keys WHERE version=2", nil)
	h.m.pol.invalidate()
	p := h.createPlain(t, "plain-1")
	for i := 0; i < 8; i++ {
		_ = a.c.RunOnce(ctxb)
		if len(client.KeyMissingCollections(a.app)) > 0 {
			break
		}
	}
	if got := client.KeyMissingCollections(a.app); len(got) != 1 {
		t.Fatalf("key_missing = %v", got)
	}
	a.sync(t)
	if _, err := a.app.FindRecordById("plain", p.Id); err != nil {
		t.Fatalf("other collections must not be blocked: %v", err)
	}
	if _, err := a.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the undecodable change must not be applied")
	}
}

func TestQC9LowOrderNodeKeyIsRefusedAtEnrollment(t *testing.T) {
	var lo [][]byte
	lo = append(lo, make([]byte, 32)) // the identity / zero point
	for _, hx := range []string{
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	} {
		b := make([]byte, 32)
		for i := range b {
			b[i] = hexNibble(hx[2*i])<<4 | hexNibble(hx[2*i+1])
		}
		lo = append(lo, b)
		hi := append([]byte{}, b...)
		hi[31] |= 0x80 // bit 255 is ignored by the function
		lo = append(lo, hi)
	}
	for i, p := range lo {
		if validX25519Pub(p) {
			t.Fatalf("low-order point %d accepted", i)
		}
	}
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if !validX25519Pub(k.PublicKey().Bytes()) {
		t.Fatal("a fresh key must be valid")
	}
	if validX25519Pub(k.PublicKey().Bytes()[:31]) {
		t.Fatal("a short key must be refused")
	}

	// over the wire: no certificate is issued
	h := newHub(t)
	s := newSpoke(t)
	code := h.enroll(t, "evil", nil)
	body, _ := json.Marshal(proto.EnrollRequest{
		Code: code, Ed25519Pub: base64.StdEncoding.EncodeToString(s.m.Identity().Pub()),
		X25519Pub: base64.StdEncoding.EncodeToString(make([]byte, 32)), Name: "evil", Profile: "edge",
	})
	res, err := http.Post(h.srv.URL+"/api/sync/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("enroll with the zero point: %d", res.StatusCode)
	}
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	default:
		return c - 'a' + 10
	}
}

// ---- P9-6 / P9-7 ------------------------------------------------------------------

func TestQC9FlipToStripClearsTheSpokeCopyAndFlipBackResends(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	r := h.create(t, map[string]any{"title": "t", "secret": "was-synced", "note": "n"})
	a.sync(t)
	if storedCT(t, a.app, r.Id, "secret") == "" {
		t.Fatal("precondition")
	}
	pr := qc9PolicyRow(t, h, "items")
	pr.Set("crypto", CryptoStrip)
	if err := h.app.Save(pr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	sr, _ := a.app.FindRecordById("items", r.Id)
	if sr.GetString("secret") != "" || sr.GetString("note") != "" {
		t.Fatalf("the spoke must lose the values: %q %q", sr.GetString("secret"), sr.GetString("note"))
	}
	var n int
	_ = a.app.DB().NewQuery("SELECT COUNT(*) FROM _crypto_keys WHERE version>0").Row(&n)
	if n != 0 {
		t.Fatalf("the spoke must drop the data keys, has %d", n)
	}
	requireConverged(t, h, a)

	// back to ciphertext: the nodes get the fields again
	pr = qc9PolicyRow(t, h, "items")
	pr.Set("crypto", "ciphertext")
	if err := h.app.Save(pr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	a.sync(t)
	if got, want := storedCT(t, a.app, r.Id, "secret"), storedCT(t, h.app, r.Id, "secret"); got != want || got == "" {
		t.Fatalf("after the flip back: spoke %q hub %q", got, want)
	}
	if plain(t, a.app, r.Id, "secret") != "was-synced" {
		t.Fatal("the spoke must decrypt again")
	}
	requireConverged(t, h, a)
}

func TestQC9KeysOnlyForCollectionsTheNodeCanPull(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	keysOf := func() int {
		hs, err := a.c.Handshake(ctxb)
		if err != nil {
			t.Fatal(err)
		}
		return len(hs.Keys)
	}
	if keysOf() == 0 {
		t.Fatal("a node of a both-way collection gets the key")
	}
	set := func(dir string, trusted bool) {
		pr := qc9PolicyRow(t, h, "items")
		pr.Set("direction", dir)
		pr.Set("trusted", trusted)
		pr.Set("pull_view_rule", true)
		if err := h.app.Save(pr); err != nil {
			t.Fatal(err)
		}
	}
	h.items.ViewRule = nil // superusers only
	if err := h.app.Save(h.items); err != nil {
		t.Fatal(err)
	}
	set(DirPull, false)
	if keysOf() != 0 {
		t.Fatal("a pull-only node of a collection it can never read gets no key")
	}
	set(DirPull, true)
	if keysOf() == 0 {
		t.Fatal("a trusted collection is pulled: the key is needed")
	}
	set(DirPush, false)
	if keysOf() == 0 {
		t.Fatal("a push-only node still encrypts its own writes: it needs the key")
	}
	set(DirNone, false)
	if keysOf() != 0 {
		t.Fatal("a collection that is not synced needs no key")
	}
}

// ---- P9-8 -------------------------------------------------------------------------

func TestQC9CryptoCommandsAreRefusedOnASpoke(t *testing.T) {
	h, _ := newCryptoHub(t, "")
	a := newCryptoSpoke(t, h, "a", "")
	a.sync(t)
	const want = "sync role spoke"
	if _, _, err := crypto.Rotate(a.app, "items", nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("rotate on a spoke: %v", err)
	}
	if _, err := crypto.Retire(a.app, "items"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("retire on a spoke: %v", err)
	}
	if _, err := crypto.Enable(a.app, "items", "title", crypto.ModeRandom, nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("enable on a spoke: %v", err)
	}
	if _, err := crypto.Disable(a.app, "items", "secret", nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("disable on a spoke: %v", err)
	}
	// the spoke still joins: nothing was changed locally
	a.c.ForceHandshake()
	a.sync(t)
}

func TestQC9NodeCanPullNeedsAPartitionValue(t *testing.T) {
	h := newHub(t)
	p := &policy{Direction: DirPull, PartField: "title", PartParam: "branch", ColID: h.items.Id}
	if h.m.nodeCanPull(p, nil, map[string]any{}) {
		t.Fatal("a node without a value for the partition param never receives a row")
	}
	if !h.m.nodeCanPull(p, nil, map[string]any{"branch": "B1"}) {
		t.Fatal("a node with the param can pull")
	}
}
