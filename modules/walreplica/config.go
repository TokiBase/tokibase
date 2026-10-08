//go:build !no_replica

// Package walreplica continuously replicates data.db and auxiliary.db to a
// local path or an S3 compatible bucket by embedding Litestream as a library.
//
// The module is inactive unless TOKI_REPLICA_URL is set. See docs/modules/walreplica.md.
package walreplica

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Environment variable names.
const (
	EnvURL              = "TOKI_REPLICA_URL"
	EnvSyncInterval     = "TOKI_REPLICA_SYNC_INTERVAL"
	EnvRetention        = "TOKI_REPLICA_RETENTION"
	EnvSnapshotInterval = "TOKI_REPLICA_SNAPSHOT_INTERVAL"
	EnvMaxMB            = "TOKI_REPLICA_MAX_MB"
)

// Defaults.
const (
	DefaultSyncInterval     = time.Second
	DefaultRetention        = 24 * time.Hour
	DefaultSnapshotInterval = 6 * time.Hour

	// DefaultMaxMB is the replica size (all databases, all LTX files) above
	// which expired restore points are pruned early. 0 disables the guard.
	DefaultMaxMB int64 = 4096
)

// Names (sub paths) of the replicated databases under the replica URL.
const (
	dataName = "data"
	auxName  = "aux"
)

// Config is the replication configuration.
type Config struct {
	// URL is the replica root: file:///abs/path or s3://bucket/prefix.
	// Empty means the module is inactive.
	URL string

	// SyncInterval is the time between uploads of new WAL changes (RPO).
	SyncInterval time.Duration

	// Retention is how long snapshots (and so the point-in-time restore window) are kept.
	Retention time.Duration

	// SnapshotInterval is how often a full snapshot is written.
	SnapshotInterval time.Duration

	// MaxMB is the replica size guard in MiB (0 = off). Above it the module
	// logs a warning and prunes expired restore points early, oldest first.
	MaxMB int64
}

// Enabled reports whether replication is configured.
func (c Config) Enabled() bool { return strings.TrimSpace(c.URL) != "" }

// FromEnv reads the configuration from the TOKI_REPLICA_* environment variables.
func FromEnv() (Config, error) {
	cfg := Config{
		URL:              strings.TrimSpace(os.Getenv(EnvURL)),
		SyncInterval:     DefaultSyncInterval,
		Retention:        DefaultRetention,
		SnapshotInterval: DefaultSnapshotInterval,
		MaxMB:            DefaultMaxMB,
	}

	if raw := strings.TrimSpace(os.Getenv(EnvMaxMB)); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("walreplica: invalid %s %q (expected a size in MiB, 0 disables the guard)", EnvMaxMB, raw)
		}
		cfg.MaxMB = n
	}

	for _, item := range []struct {
		env string
		dst *time.Duration
	}{
		{EnvSyncInterval, &cfg.SyncInterval},
		{EnvRetention, &cfg.Retention},
		{EnvSnapshotInterval, &cfg.SnapshotInterval},
	} {
		raw := strings.TrimSpace(os.Getenv(item.env))
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("walreplica: invalid %s %q (expected a positive duration such as 1s, 24h)", item.env, raw)
		}
		*item.dst = d
	}

	return cfg, nil
}

// subURL returns the replica URL of a single database ("data" or "aux")
// under the root URL. For S3 compatible stores the AWS_ENDPOINT_URL and
// AWS_REGION environment variables fill the endpoint and region query
// parameters when the URL does not set them (custom endpoints default to
// path style addressing, which R2 and MinIO accept).
func subURL(root, name string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(root))
	if err != nil {
		return "", fmt.Errorf("walreplica: invalid replica url: %w", err)
	}

	switch u.Scheme {
	case "file":
		if u.Host != "" || !path.IsAbs(u.Path) {
			return "", fmt.Errorf("walreplica: file replica url must be file:///absolute/path, got %q", root)
		}
		u.Path = path.Join(u.Path, name)
	case "s3":
		if !s3Supported {
			return "", fmt.Errorf("walreplica: this binary was built without the replica_s3 tag; s3:// replicas need `go build -tags replica_s3`")
		}
		if u.Host == "" {
			return "", fmt.Errorf("walreplica: s3 replica url needs a bucket (s3://bucket/prefix), got %q", root)
		}
		u.Path = "/" + strings.TrimPrefix(path.Join(u.Path, name), "/")

		q := u.Query()
		if q.Get("endpoint") == "" {
			if ep := firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL"); ep != "" {
				q.Set("endpoint", ep)
			}
		}
		if q.Get("region") == "" {
			if r := firstEnv("AWS_REGION", "AWS_DEFAULT_REGION"); r != "" {
				q.Set("region", r)
			} else if q.Get("endpoint") != "" {
				q.Set("region", "auto") // R2 style; ignored by most S3 compatible stores
			}
		}
		u.RawQuery = q.Encode()
	default:
		return "", fmt.Errorf("walreplica: unsupported replica url scheme %q (use file:// or s3://)", u.Scheme)
	}

	return u.String(), nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// redactURL removes credentials and query parameters from a URL for display.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}
