//go:build !no_printer

package printer

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tokibase/tokibase/internal/devio"
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

// superuserOnly reports TOKI_PRINT_AUTH=superuser.
func superuserOnly() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TOKI_PRINT_AUTH")), "superuser")
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
