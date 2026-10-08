//go:build !no_printer

package printer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/internal/edgeguard"
)

const (
	defaultMaxBytes      = 64 << 10
	hardMaxBytes         = 1 << 20
	defaultRetentionDays = 14
	maxCopies            = 10
	// MaxAttempts is the number of failed transmissions after which a job is dead.
	MaxAttempts = 20
	// DefaultWaitDelay is the polling interval while a printer is out of paper.
	DefaultWaitDelay = 10 * time.Second
	// MaxWaits bounds the paper polls of one job (360 x 10 s = 1 h); then it is dead.
	MaxWaits = 360
	// DefaultMaxQueuedPerActor bounds the unfinished jobs of one actor.
	DefaultMaxQueuedPerActor = 20
	defaultRatePerMin        = 60
	// DefaultStatusTimeout is the wait for the DLE EOT answer.
	DefaultStatusTimeout = time.Second
	// DefaultLockWait is how long a job waits for the per-printer lock before
	// it gives its worker back with a retryable error.
	DefaultLockWait = 30 * time.Second
	// noStatusTTL is how long a printer that never answered DLE EOT is not asked again.
	noStatusTTL = 10 * time.Minute
)

// Enabled reports whether the module is on. It is opt-in: TOKI_PRINTER=on
// (also true, 1, yes).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_PRINTER"))) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// Access modes of TOKI_PRINT_AUTH.
const (
	authService   = "service" // default: superusers and the TOKI_PRINT_ALLOW_COLLECTIONS actors
	authAny       = "auth"    // explicit opt-in: every authenticated record
	authSuperuser = "superuser"
)

// authMode reads TOKI_PRINT_AUTH (service, auth, superuser; default service).
func authMode() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_PRINT_AUTH"))); v {
	case authAny, authSuperuser:
		return v
	}
	return authService
}

// allowedActors is TOKI_PRINT_ALLOW_COLLECTIONS: auth collections ("gate_devices")
// or single records ("gate_devices/abc") that may print in service mode.
func allowedActors() *edgeguard.Allow {
	return edgeguard.ParseAllow(os.Getenv("TOKI_PRINT_ALLOW_COLLECTIONS"))
}

// maxQueuedPerActor is TOKI_PRINT_MAX_QUEUED_PER_ACTOR (default 20, 0 = unlimited).
func maxQueuedPerActor() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_PRINT_MAX_QUEUED_PER_ACTOR"))); err == nil && n >= 0 {
		return n
	}
	return DefaultMaxQueuedPerActor
}

// ratePerMin is TOKI_PRINT_RATE_PER_MIN, the requests per minute of one actor
// and of one IP address (default 60, 0 = unlimited).
func ratePerMin() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_PRINT_RATE_PER_MIN"))); err == nil && n >= 0 {
		return n
	}
	return defaultRatePerMin
}

// MaxBytes is the largest rendered (or raw) payload (env TOKI_PRINT_MAX_BYTES,
// default 64 KiB, at most 1 MiB).
func MaxBytes() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_PRINT_MAX_BYTES"))); err == nil && n > 0 {
		return min(n, hardMaxBytes)
	}
	return defaultMaxBytes
}

// RetentionDays is env TOKI_PRINT_RETENTION_DAYS (default 14).
func RetentionDays() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_PRINT_RETENTION_DAYS"))); err == nil && n > 0 {
		return n
	}
	return defaultRetentionDays
}

// policy builds the dial policy from TOKI_PRINT_ALLOW_CIDRS (default RFC 1918
// plus loopback; link-local and the metadata address are always denied).
func policy(timeout time.Duration) (*devio.Policy, error) {
	var cidrs []string
	for _, c := range strings.Split(os.Getenv("TOKI_PRINT_ALLOW_CIDRS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cidrs = append(cidrs, c)
		}
	}
	p, err := devio.NewPolicy(cidrs, nil)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		p.IOTimeout = timeout
		p.DialTimeout = timeout
	}
	return p, nil
}
