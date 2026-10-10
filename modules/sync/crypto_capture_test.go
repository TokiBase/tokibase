//go:build !no_sync && !no_crypto

package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/crypto"
)

func TestCryptoCiphertextCapturedVerbatim(t *testing.T) {
	crypto.WaitForServers = false
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	t.Setenv(crypto.EnvMasterKey, base64.StdEncoding.EncodeToString(key))
	app := newApp(t)
	cm := crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, app, RoleHub)
	e.policy(t, "items", DirBoth, nil, nil)
	if _, err := crypto.Enable(app, "items", "secret", crypto.ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	cm.Invalidate()

	code, out := e.do(t, e.su, "POST", "/api/collections/items/records", `{"title":"t","secret":"my plaintext"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	var created struct {
		Id string `json:"id"`
	}
	_ = json.Unmarshal(out, &created)

	var stored string
	if err := app.DB().NewQuery("SELECT secret FROM items WHERE id={:id}").Bind(dbx.Params{"id": created.Id}).Row(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || stored == "my plaintext" {
		t.Fatalf("not encrypted at rest: %q", stored)
	}
	rows := e.changes(t)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	p := patchOf(t, rows[0])
	if p["secret"] != stored {
		t.Fatalf("patch must carry the stored ciphertext verbatim:\n patch  %v\n stored %v", p["secret"], stored)
	}
	if strings.Contains(rows[0].Patch, "my plaintext") {
		t.Fatal("plaintext leaked into _changes")
	}
	// the hash covers the ciphertext
	fresh, _ := app.FindRecordById("items", created.Id)
	if want, _ := RecordHash(fresh, e.pol(t)); !bytes.Equal(want, rows[0].Hash) {
		t.Fatal("hash must be computed over the stored ciphertext")
	}

	// an update that does not touch the encrypted field keeps the ciphertext and does not re-emit it
	code, out = e.do(t, e.su, "PATCH", "/api/collections/items/records/"+created.Id, `{"title":"t2"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	rows = e.changes(t)
	if _, has := patchOf(t, rows[1])["secret"]; has {
		t.Fatalf("untouched ciphertext re-emitted: %s", rows[1].Patch)
	}
}
