//go:build no_audit

// Package audit is compiled out of this binary by the no_audit build tag.
// Enabled() reports false, so the root package never creates a Log.
package audit

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Actor kinds recorded in audit entries.
const (
	ActorAgent  = "agent"
	ActorSystem = "system"
)

// Entry is one audit record.
type Entry struct {
	ActorKind, ActorID, ActorCollection string
	Action, Collection, Record          string
	Request, After                      *string
}

// Log is never instantiated in builds with the no_audit tag.
type Log struct{}

// VerifyResult is the outcome of a chain verification.
type VerifyResult struct{}

var errCompiledOut = errors.New("audit is not available in this build (no_audit)")

// Enabled reports false in builds with the no_audit tag.
func Enabled() bool { return false }

// Register is a no-op returning nil in builds with the no_audit tag.
func Register(app core.App) *Log { return nil }

// New returns an inert log in builds with the no_audit tag.
func New(app core.App) *Log { return &Log{} }

// Init is a no-op.
func (l *Log) Init() error { return nil }

// Append is a no-op.
func (l *Log) Append(e *Entry) error { return nil }

// Query always fails in builds with the no_audit tag.
func Query(app core.App, since time.Time, limit int, newestFirst bool) ([]Entry, error) {
	return nil, errCompiledOut
}

// Verify always fails in builds with the no_audit tag.
func Verify(app core.App) (VerifyResult, error) { return VerifyResult{}, errCompiledOut }

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "audit", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error { return errCompiledOut },
	}
}
