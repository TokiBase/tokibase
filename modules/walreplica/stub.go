//go:build no_replica

// Package walreplica is compiled out of this binary by the no_replica build tag.
package walreplica

import (
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// PromoteResult is the outcome of a replica promotion.
type PromoteResult struct {
	FromURL string
	Dir     string
}

// DBStatus is the replication status of one database.
type DBStatus struct{}

// LeaseStatus describes the primary lease.
type LeaseStatus struct{}

// Register is a no-op in builds with the no_replica tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_replica tag.
func SetAuditSink(fn func(PromoteResult)) {}

// Active reports false in builds with the no_replica tag.
func Active(app kernel.App) bool { return false }

// Healthy reports healthy in builds with the no_replica tag.
func Healthy(app kernel.App) (bool, string) { return true, "" }

// Status returns nothing in builds with the no_replica tag.
func Status(app kernel.App) []DBStatus { return nil }

// LeaseInfo returns nil in builds with the no_replica tag.
func LeaseInfo(app kernel.App) *LeaseStatus { return nil }
