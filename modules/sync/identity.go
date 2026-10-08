//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"crypto/rand"
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
	keySessionSecret = "session_secret" // base64, HS256 key of node session tokens
	keyEpoch         = "epoch"
	keyHubID         = "hub_id"
)

// hubIdentity is the hub key material, loaded at Init when the role is hub.
type hubIdentity struct {
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
	id     string
	epoch  string
	secret []byte
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

// loadHubIdentity loads or creates the hub key, the session secret and the epoch.
func loadHubIdentity(st dbState) (*hubIdentity, error) {
	var priv ed25519.PrivateKey
	if f := strings.TrimSpace(os.Getenv(EnvHubKeyFile)); f != "" {
		k, err := loadOrCreateHubKeyFile(f)
		if err != nil {
			return nil, err
		}
		priv = k
	} else {
		v, err := st.getOrCreate(keyHubKey, func() (string, error) {
			_, k, err := ed25519.GenerateKey(rand.Reader)
			return base64.StdEncoding.EncodeToString(k), err
		})
		if err != nil {
			return nil, err
		}
		k, err := decodeHubKey(v)
		if err != nil {
			return nil, err
		}
		priv = k
	}
	pub := priv.Public().(ed25519.PublicKey)
	sec, err := st.getOrCreate(keySessionSecret, func() (string, error) {
		b := make([]byte, 32)
		_, err := rand.Read(b)
		return base64.StdEncoding.EncodeToString(b), err
	})
	if err != nil {
		return nil, err
	}
	secret, err := base64.StdEncoding.DecodeString(sec)
	if err != nil || len(secret) < 32 {
		return nil, errf("invalid %s in _sync_state", keySessionSecret)
	}
	epoch, err := st.getOrCreate(keyEpoch, func() (string, error) { return randHex(12) })
	if err != nil {
		return nil, err
	}
	h := &hubIdentity{priv: priv, pub: pub, id: proto.HubID(pub), epoch: epoch, secret: secret}
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

func loadOrCreateHubKeyFile(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		return decodeHubKey(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, rerr
			}
			return decodeHubKey(string(b))
		}
		return nil, err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, werr
	}
	return k, nil
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
