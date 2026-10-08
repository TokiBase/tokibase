//go:build !no_sync

package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"

	"github.com/pocketbase/dbx"
)

// digester computes the per-collection digest: sha256 over the sorted
// (id, hash) pairs, each as id 0x00 hash 0x0a.
type digester struct {
	h hash.Hash
	n int
}

func newDigester() *digester { return &digester{h: sha256.New()} }

func (d *digester) add(id string, hash []byte) {
	d.h.Write([]byte(id))
	d.h.Write([]byte{0})
	d.h.Write(hash)
	d.h.Write([]byte{'\n'})
	d.n++
}

func (d *digester) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// metaDigest digests `_sync_meta` of one collection (the value compared by
// /ack): ids sorted bytewise.
func metaDigest(db dbx.Builder, colId string) (string, int, error) {
	rows, err := db.NewQuery("SELECT record, hash FROM _sync_meta WHERE collection={:c} ORDER BY record").
		Bind(dbx.Params{"c": colId}).Rows()
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	d := newDigester()
	for rows.Next() {
		var id string
		var h []byte
		if err := rows.Scan(&id, &h); err != nil {
			return "", 0, err
		}
		d.add(id, h)
	}
	return d.sum(), d.n, rows.Err()
}
