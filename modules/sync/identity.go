//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Env of the identity (docs/SYNC_DESIGN.md §8.3).
const (
	EnvHubKeyFile = "TOKI_SYNC_HUB_KEY_FILE"
	EnvNodeKey    = "TOKI_SYNC_NODE_KEY"

	// NodeKeyFile is the spoke key file inside the data dir.
	NodeKeyFile = "sync_node.key"
)

const (
	keyHubKey        = "hub_key"        // base64 ed25519 private key (64 bytes)
	keySessionSecret = "session_secret" // legacy: removed at boot, the key is derived from the hub key
	keyEpoch         = "epoch"
	keyHubID         = "hub_id"
)

// hubIdentity is the hub key material, loaded at Init when the role is hub.
type hubIdentity struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	id    string
	epoch string
	// epochSeq is the hub head when the epoch began (see epoch.go).
	epochSeq int64
	secret   []byte
}

// getOrCreate returns the state value, generating and storing it once.
func (s dbState) getOrCreate(key string, gen func() (string, error)) (string, error) {
	if v, ok, err := s.Get(key); err != nil {
		return "", err
	} else if ok && v != "" {
		return v, nil
	}
	v, err := gen()
	if err != nil {
		return "", err
	}
	if _, err := s.db.NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, {:v}) ON CONFLICT(key) DO NOTHING").
		Bind(dbx.Params{"k": key, "v": v}).Execute(); err != nil {
		return "", err
	}
	got, _, err := s.Get(key) // another process may have won
	return got, err
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sessionSecretInfo is the HKDF info of the session token key.
const sessionSecretInfo = "toki_sync/session/v1"

// deriveSessionSecret derives the HS256 key of the session tokens from the hub
// key, so it is never stored: whoever has a copy of data.db but not the hub
// key can not mint session tokens.
func deriveSessionSecret(priv ed25519.PrivateKey) ([]byte, error) {
	return hkdf.Key(sha256.New, priv.Seed(), nil, sessionSecretInfo, 32)
}

func (s dbState) del(key string) error {
	_, err := s.db.NewQuery("DELETE FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": key}).Execute()
	return err
}

// loadHubIdentity loads or creates the hub key and the epoch. It never
// replaces an existing identity: when `_sync_state.hub_id` or a stored hub key
// exist, a missing or different key makes it fail (the hub refuses to start)
// instead of minting a new hub id, which would invalidate every certificate.
func loadHubIdentity(st dbState, warn func(msg string, args ...any)) (*hubIdentity, error) {
	file := strings.TrimSpace(os.Getenv(EnvHubKeyFile))
	wantID, _, err := st.Get(keyHubID)
	if err != nil {
		return nil, err
	}
	dbKey, _, err := st.Get(keyHubKey)
	if err != nil {
		return nil, err
	}
	existing := wantID != "" || dbKey != ""

	var priv ed25519.PrivateKey
	if file != "" {
		switch {
		case fileExists(file):
			if priv, err = readHubKeyFile(file, warn); err != nil {
				return nil, err
			}
		case dbKey != "":
			// migration database -> file: the identity stays the same
			if priv, err = decodeHubKey(dbKey); err != nil {
				return nil, err
			}
			if err = writeHubKeyFile(file, priv); err != nil {
				return nil, err
			}
		case existing:
			return nil, errf("%s=%s does not exist but this hub already has an identity (%s): refusing to create a new one. Mount or restore the key file", EnvHubKeyFile, file, wantID)
		default:
			_, k, gerr := ed25519.GenerateKey(rand.Reader)
			if gerr != nil {
				return nil, gerr
			}
			priv = k
			if err = writeHubKeyFile(file, k); errors.Is(err, os.ErrExist) { // lost a race with another process
				priv, err = readHubKeyFile(file, warn)
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
		if dbKey == "" && wantID != "" {
			return nil, errf("this hub has an identity (%s) but no hub key in _sync_state and %s is not set: refusing to create a new identity. Set %s to the key file used before", wantID, EnvHubKeyFile, EnvHubKeyFile)
		}
		v, gerr := st.getOrCreate(keyHubKey, func() (string, error) {
			_, k, err := ed25519.GenerateKey(rand.Reader)
			return base64.StdEncoding.EncodeToString(k), err
		})
		if gerr != nil {
			return nil, gerr
		}
		if priv, err = decodeHubKey(v); err != nil {
			return nil, err
		}
	}
	pub := priv.Public().(ed25519.PublicKey)
	id := proto.HubID(pub)
	if wantID != "" && wantID != id {
		return nil, errf("the hub key does not match the stored hub_id (%s, key gives %s): refusing to start. A new hub key needs `toki sync rotate-hub-key` (not implemented yet)", wantID, id)
	}
	if file != "" && dbKey != "" {
		// the key lives in the file now: drop the copy in data.db (and backups to come)
		if err := st.del(keyHubKey); err != nil {
			return nil, err
		}
	}
	// session_secret is derived from the hub key since the hardening of PR2
	if err := st.del(keySessionSecret); err != nil {
		return nil, err
	}
	secret, err := deriveSessionSecret(priv)
	if err != nil {
		return nil, err
	}
	epoch, err := st.getOrCreate(keyEpoch, func() (string, error) { return randHex(12) })
	if err != nil {
		return nil, err
	}
	h := &hubIdentity{priv: priv, pub: pub, id: id, epoch: epoch, secret: secret}
	if err := st.Set(keyHubID, h.id); err != nil {
		return nil, err
	}
	return h, nil
}

func decodeHubKey(v string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("sync: invalid hub key")
	}
	return ed25519.PrivateKey(b), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readHubKeyFile reads the key file; a file readable by group or others is
// reported through warn.
func readHubKeyFile(path string, warn func(msg string, args ...any)) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 && warn != nil {
		warn("sync: the hub key file is readable by group or others, chmod 600 it", "path", path, "mode", st.Mode().Perm().String())
	}
	return decodeHubKey(string(b))
}

// writeHubKeyFile creates the key file exclusively with mode 0600.
func writeHubKeyFile(path string, k ed25519.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// adoptNodeID makes id the node id of this process. Rows written under a
// previous (placeholder) id are migrated in the same transaction.
func (m *Module) adoptNodeID(id string) error {
	st := dbState{db: m.app.NonconcurrentDB()}
	old, _, err := st.Get(keyNodeID)
	if err != nil {
		return err
	}
	if old == id {
		return nil
	}
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		db := tx.DB()
		if old != "" {
			for _, t := range []string{"_changes", "_sync_meta", "_sync_tombstones"} {
				if _, err := db.NewQuery("UPDATE " + t + " SET node={:n} WHERE node={:o}").Bind(dbx.Params{"n": id, "o": old}).Execute(); err != nil {
					return err
				}
			}
		}
		_, err := db.NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, {:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
			Bind(dbx.Params{"k": keyNodeID, "v": id}).Execute()
		return err
	})
	if err != nil {
		return err
	}
	m.nodeID.Store(id)
	return nil
}

// Identity returns the spoke key material (nil on a hub or before Init).
func (m *Module) Identity() *proto.Identity { return m.spoke }

// HubID returns the id of this hub ("" on a spoke).
func (m *Module) HubID() string {
	if m.hub == nil {
		return ""
	}
	return m.hub.id
}

// HubPub returns the hub public key (nil on a spoke).
func (m *Module) HubPub() ed25519.PublicKey {
	if m.hub == nil {
		return nil
	}
	return m.hub.pub
}

// Epoch returns the hub epoch ("" on a spoke).
func (m *Module) Epoch() string {
	if m.hub == nil {
		return ""
	}
	return m.hub.epoch
}
