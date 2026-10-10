//go:build !no_crypto

package crypto

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

func x25519(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSyncKeysWrapUnwrapRekeyUnderLocalMaster(t *testing.T) {
	hub := setup(t, "")
	hub.enableAll(t)
	col := patients(t, hub)
	node := x25519(t)

	keys, _, err := kernel.SyncKeyProviderOf(hub.app).ExportKeys([]string{col.Id}, node.PublicKey().Bytes())
	if err != nil || len(keys) != 1 || keys[0].Version != 1 || len(keys[0].Wrapped) < 60 {
		t.Fatalf("export: %+v %v", keys, err)
	}
	spoke := setup(t, "")
	sp := kernel.SyncKeyProviderOf(spoke.app)
	// not wrapped for this recipient
	if err := sp.ImportKeys(keys, x25519(t).Bytes()); err == nil {
		t.Fatal("another node's key must not unwrap")
	}
	if err := sp.ImportKeys(keys, node.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := sp.ImportKeys(keys, node.Bytes()); err != nil {
		t.Fatalf("import is idempotent: %v", err)
	}
	if !kernel.SyncKeyProviderOf(hub.app).NeedsKeys(col.Id) || sp.NeedsKeys(patients(t, spoke).Id) {
		t.Fatal("NeedsKeys follows the configured (non-stripped) fields")
	}
	_, hubK, _ := hub.m.activeKey(col.Id)
	_, spK, err := spoke.m.activeKey(col.Id)
	if err != nil || string(hubK) != string(spK) {
		t.Fatalf("the spoke must hold the same DEK: %v", err)
	}
	var hw, sw string
	_ = hub.app.DB().NewQuery("SELECT wrapped_dek FROM _crypto_keys WHERE collection={:c} AND version=1").Bind(map[string]any{"c": col.Id}).Row(&hw)
	_ = spoke.app.DB().NewQuery("SELECT wrapped_dek FROM _crypto_keys WHERE collection={:c} AND version=1").Bind(map[string]any{"c": col.Id}).Row(&sw)
	if hw == "" || sw == "" || hw == sw {
		t.Fatal("each node wraps the DEK under its own master key")
	}

	// a different local key of the same version is a conflict, not a silent overwrite
	other := setup(t, "")
	if _, err := other.m.newKey(col.Id); err != nil {
		t.Fatal(err)
	}
	if err := kernel.SyncKeyProviderOf(other.app).ImportKeys(keys, node.Bytes()); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("conflicting local key: %v", err)
	}

	// a node without a master key cannot import
	nokey := setup(t, "-")
	if err := kernel.SyncKeyProviderOf(nokey.app).ImportKeys(keys, node.Bytes()); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("no master key: %v", err)
	}
	// nothing to import is not an error
	if err := kernel.SyncKeyProviderOf(nokey.app).ImportKeys(nil, node.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncKeysRetiredVersion(t *testing.T) {
	hub := setup(t, "")
	hub.enableAll(t)
	col := patients(t, hub)
	if _, _, err := Rotate(hub.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Retire(hub.app, "patients"); err != nil {
		t.Fatal(err)
	}
	node := x25519(t)
	keys, _, err := kernel.SyncKeyProviderOf(hub.app).ExportKeys([]string{col.Id}, node.PublicKey().Bytes())
	if err != nil || len(keys) != 2 || !keys[0].Retired || keys[0].Wrapped != nil || keys[1].Retired || keys[1].Version != 2 {
		t.Fatalf("export after retire: %+v %v", keys, err)
	}
	spoke := setup(t, "")
	if err := kernel.SyncKeyProviderOf(spoke.app).ImportKeys(keys, node.Bytes()); err != nil {
		t.Fatal(err)
	}
	infos, _ := spoke.m.KeyInfos(col.Id)
	if len(infos) != 2 || infos[0].RetiredAt == "" || !infos[1].Active {
		t.Fatalf("spoke key infos: %+v", infos)
	}
}

// A sync apply stores the ciphertext it received unchanged, whatever the local
// master key, and keeps the blind index current.
func TestSyncOriginKeepsIncomingCiphertext(t *testing.T) {
	hub := setup(t, "")
	hub.enableAll(t)
	col := patients(t, hub)
	id := hub.create(t)
	ctDiag, ctSSN := hub.raw(t, id, "diagnosis"), hub.raw(t, id, "ssn")

	// the spoke has the same collection (ids derive from the name) and its own master key
	spoke := setup(t, "")
	c := patients(t, spoke)
	if c.Id != col.Id {
		t.Fatal("collection ids must be identical across nodes")
	}
	node := x25519(t)
	keys, _, _ := kernel.SyncKeyProviderOf(hub.app).ExportKeys([]string{col.Id}, node.PublicKey().Bytes())
	if err := kernel.SyncKeyProviderOf(spoke.app).ImportKeys(keys, node.Bytes()); err != nil {
		t.Fatal(err)
	}
	for f, mode := range map[string]string{"diagnosis": ModeRandom, "ssn": ModeBlindIndex} {
		if err := spoke.app.Save(configRow(t, spoke, "patients", f, mode)); err != nil {
			t.Fatal(err)
		}
	}
	spoke.m.Invalidate()

	// pull: SaveNoValidate (ciphertext arrives untouched in the field)
	rec := core.NewRecord(c)
	rec.Set("id", id)
	rec.Set("name", "Ann")
	rec.Set("diagnosis", ctDiag)
	rec.Set("ssn", ctSSN)
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePull, Node: "nhub"})
	if err := spoke.app.SaveNoValidateWithContext(ctx, rec); err != nil {
		t.Fatal(err)
	}
	var d, s string
	_ = spoke.app.DB().NewQuery("SELECT diagnosis, ssn FROM patients WHERE id={:i}").Bind(map[string]any{"i": id}).Row(&d, &s)
	if d != ctDiag || s != ctSSN {
		t.Fatalf("stored ciphertext must be the incoming one:\n%q\n%q", d, s)
	}
	got, err := spoke.m.findByBlindIndex(c, "ssn", "123-45", 10)
	if err != nil || len(got) != 1 || got[0].Id != id {
		t.Fatalf("blind index: %v %v", got, err)
	}

	// validate path (hub push replay): plaintext for the validators, the same ciphertext stored
	rec2, _ := spoke.app.FindRecordById("patients", id)
	rec2.Set("diagnosis", ctDiag)
	rec2.Set("name", "Anna")
	ctx = kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: "nspoke"})
	if err := spoke.app.SaveWithContext(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	_ = spoke.app.DB().NewQuery("SELECT diagnosis FROM patients WHERE id={:i}").Bind(map[string]any{"i": id}).Row(&d)
	if d != ctDiag {
		t.Fatal("a validated push must keep the ciphertext too")
	}

	// a push with a ciphertext of another record (AAD mismatch) is refused; a pull stores it verbatim
	bad := core.NewRecord(c)
	bad.Set("id", "otherrecord0001")
	bad.Set("diagnosis", ctDiag)
	if err := spoke.app.SaveNoValidateWithContext(kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush}), bad); err == nil {
		t.Fatal("a ciphertext that does not belong to the record must be refused on push")
	}
	bad2 := core.NewRecord(c)
	bad2.Set("id", "otherrecord0002")
	bad2.Set("diagnosis", ctDiag)
	if err := spoke.app.SaveNoValidateWithContext(kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePull}), bad2); err != nil {
		t.Fatalf("pull stores what it cannot read: %v", err)
	}
	// an unknown key version waits for the handshake
	unk := core.NewRecord(c)
	unk.Set("id", "otherrecord0003")
	unk.Set("diagnosis", Prefix+"9:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	err = spoke.app.SaveNoValidateWithContext(kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePull}), unk)
	if !errors.Is(err, kernel.ErrSyncKeyMissing) {
		t.Fatalf("unknown key version: %v", err)
	}
}

func configRow(t *testing.T, e *env, collection, field, mode string) *core.Record {
	t.Helper()
	c, err := e.app.FindCollectionByNameOrId(FieldsCollection)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("collection", collection)
	r.Set("field", field)
	r.Set("mode", mode)
	return r
}

func TestStrippedStateNeedsNoKey(t *testing.T) {
	e := setup(t, "-") // no master key
	col := patients(t, e)
	r := configRow(t, e, "patients", "diagnosis", ModeRandom)
	r.Set("state", StateStripped)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
	if kernel.SyncKeyProviderOf(e.app).NeedsKeys(col.Id) {
		t.Fatal("a stripped field needs no key")
	}
	if !kernel.IsSensitive(col.Id, "diagnosis") {
		t.Fatal("a stripped field stays registered as sensitive (sync consults it)")
	}
	// plain writes work without a master key: the field is a local column
	code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", `{"name":"x","diagnosis":"local"}`)
	if code != 200 || b["diagnosis"] != "local" {
		t.Fatalf("%d %v", code, b)
	}
}
