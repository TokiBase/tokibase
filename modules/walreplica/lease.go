//go:build !no_replica

package walreplica

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/kernel"
)

// EnvTakeover set to "1" lets a replicator take over a lease held by another
// (possibly still alive) node. Use it only after the old primary is stopped.
const EnvTakeover = "TOKI_REPLICA_TAKEOVER"

const (
	leaseFileName = ".toki-lease.json"
	nodeIDFile    = ".toki-node-id"

	leaseMinHeartbeat = 10 * time.Second
	leaseMinStale     = 60 * time.Second
)

const (
	blockedKey = "@walreplicaBlocked"
)

// Lease is the object stored at <replica url>/.toki-lease.json while a
// replicator writes to the replica.
type Lease struct {
	NodeID      string    `json:"node_id"`
	Hostname    string    `json:"hostname"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// LeaseStatus is the lease as shown in the health response.
type LeaseStatus struct {
	Held        bool      `json:"held"` // true when this node owns the lease
	NodeID      string    `json:"nodeId"`
	Hostname    string    `json:"hostname"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
	Supported   bool      `json:"supported"` // false for backends without lease support (s3://)
}

// heartbeatInterval is how often the lease is refreshed.
func heartbeatInterval(sync time.Duration) time.Duration {
	return max(leaseMinHeartbeat, 10*sync)
}

// staleAfter is the heartbeat age after which a foreign lease is ignored.
// It is at least 3 heartbeat intervals so a live holder is never judged stale.
func staleAfter(sync time.Duration) time.Duration {
	return max(leaseMinStale, 3*heartbeatInterval(sync))
}

// leasePath returns the filesystem path of the lease for a file:// replica url.
// ok is false for other backends (no lease support yet).
func leasePath(root string) (p string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(root))
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	return filepath.Join(filepath.FromSlash(u.Path), leaseFileName), true
}

func readLease(p string) (*Lease, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil // unreadable lease: treat as absent
	}
	return &l, nil
}

func writeLease(p string, l *Lease) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// loadNodeID returns the node id persisted in dataDir, creating it on first use.
func loadNodeID(dataDir string) (string, error) {
	p := filepath.Join(dataDir, nodeIDFile)
	if raw, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(p, []byte(id+"\n"), 0o600)
}

// leaseHeldError reports a foreign live lease.
type leaseHeldError struct{ holder Lease }

func (e *leaseHeldError) Error() string {
	return fmt.Sprintf("lease held by %s since %s", e.holder.Hostname, e.holder.StartedAt.UTC().Format(time.RFC3339))
}

// leaseManager keeps this node's lease fresh.
type leaseManager struct {
	path     string
	lease    Lease
	interval time.Duration

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// acquireLease writes this node's lease unless a live foreign lease exists.
// It returns (nil, nil) when the backend has no lease support.
func acquireLease(replicaURL, dataDir string, syncInterval time.Duration, now time.Time) (*leaseManager, error) {
	p, ok := leasePath(replicaURL)
	if !ok {
		return nil, nil
	}
	id, err := loadNodeID(dataDir)
	if err != nil {
		return nil, fmt.Errorf("walreplica: node id: %w", err)
	}

	if cur, err := readLease(p); err != nil {
		return nil, fmt.Errorf("walreplica: read lease: %w", err)
	} else if cur != nil && cur.NodeID != id && now.Sub(cur.HeartbeatAt) < staleAfter(syncInterval) {
		if os.Getenv(EnvTakeover) != "1" {
			return nil, &leaseHeldError{holder: *cur}
		}
	}

	host, _ := os.Hostname()
	m := &leaseManager{
		path:     p,
		interval: heartbeatInterval(syncInterval),
		lease: Lease{
			NodeID: id, Hostname: host, PID: os.Getpid(),
			StartedAt: now.UTC(), HeartbeatAt: now.UTC(),
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := writeLease(p, &m.lease); err != nil {
		return nil, fmt.Errorf("walreplica: write lease: %w", err)
	}
	return m, nil
}

func (m *leaseManager) run(onErr func(error)) {
	defer close(m.done)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-t.C:
			// never overwrite a lease another node took over
			if cur, err := readLease(m.path); err == nil && cur != nil && cur.NodeID != m.lease.NodeID {
				onErr(fmt.Errorf("lease taken over by %s", cur.Hostname))
				continue
			}
			m.mu.Lock()
			m.lease.HeartbeatAt = now.UTC()
			l := m.lease
			m.mu.Unlock()
			if err := writeLease(m.path, &l); err != nil {
				onErr(err)
			}
		}
	}
}

// release stops the heartbeat and removes the lease when it is still ours.
func (m *leaseManager) release() {
	close(m.stop)
	<-m.done
	if cur, err := readLease(m.path); err == nil && cur != nil && cur.NodeID == m.lease.NodeID {
		_ = os.Remove(m.path)
	}
}

func (m *leaseManager) status() *LeaseStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lease
	return &LeaseStatus{Held: true, Supported: true, NodeID: l.NodeID, Hostname: l.Hostname, PID: l.PID, StartedAt: l.StartedAt, HeartbeatAt: l.HeartbeatAt}
}

// blocked records why replication did not start.
type blocked struct {
	reason string
	holder *Lease
}

// LeaseInfo returns the lease state for app: the held lease, the foreign lease
// that blocked replication, or nil when replication is inactive or the backend
// has no lease support.
func LeaseInfo(app kernel.App) *LeaseStatus {
	if r := get(app); r != nil && r.lease != nil {
		return r.lease.status()
	}
	if b, ok := app.Store().Get(blockedKey).(*blocked); ok && b.holder != nil {
		h := b.holder
		return &LeaseStatus{Held: false, Supported: true, NodeID: h.NodeID, Hostname: h.Hostname, PID: h.PID, StartedAt: h.StartedAt, HeartbeatAt: h.HeartbeatAt}
	}
	return nil
}
