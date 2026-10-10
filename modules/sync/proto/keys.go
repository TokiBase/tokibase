package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"sort"
	"strconv"
)

// keysDigest is what the hub signs over the data keys of a handshake answer:
// the node and the hub, the signed request it answers (ts and nonce, so an
// answer cannot be replayed to another request), and every key entry (the
// version, whether it is retired, the wrapped bytes) plus every KeyError, in a
// canonical order. Fields are length-prefixed, so no entry can be shifted into
// another.
func keysDigest(node, hub, ts, nonce string, keys []Key, errs []KeyError) []byte {
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	put("toki-sync-keys-v1")
	put(node)
	put(hub)
	put(ts)
	put(nonce)
	ks := append([]Key(nil), keys...)
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Collection != ks[j].Collection {
			return ks[i].Collection < ks[j].Collection
		}
		return ks[i].Version < ks[j].Version
	})
	put("keys")
	put(strconv.Itoa(len(ks)))
	for _, k := range ks {
		put(k.Collection)
		put(strconv.Itoa(k.Version))
		put(strconv.FormatBool(k.Retired))
		put(k.Wrapped)
	}
	es := append([]KeyError(nil), errs...)
	sort.Slice(es, func(i, j int) bool { return es[i].Collection < es[j].Collection })
	put("errors")
	put(strconv.Itoa(len(es)))
	for _, e := range es {
		put(e.Collection)
		put(e.Code)
	}
	return h.Sum(nil)
}

// SignKeys signs the keys section of a handshake answer with the hub key.
func SignKeys(hubPriv ed25519.PrivateKey, node, hub, ts, nonce string, keys []Key, errs []KeyError) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(hubPriv, keysDigest(node, hub, ts, nonce, keys, errs)))
}

// VerifyKeys checks SignKeys with the stored hub public key.
func VerifyKeys(hubPub ed25519.PublicKey, node, hub, ts, nonce string, keys []Key, errs []KeyError, sig string) bool {
	if len(hubPub) != ed25519.PublicKeySize {
		return false
	}
	s, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(s) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(hubPub, keysDigest(node, hub, ts, nonce, keys, errs), s)
}
