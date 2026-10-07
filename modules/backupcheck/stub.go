//go:build no_backupcheck

// Package backupcheck is compiled out of this binary by the no_backupcheck build tag.
package backupcheck

import (
	"context"
	"errors"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Report is the verification result of one backup.
type Report struct{ Name string }

// OK always reports false.
func (r Report) OK() bool { return false }

// OnResult is never called in builds with the no_backupcheck tag.
var OnResult func(app kernel.App, r Report)

var errCompiledOut = errors.New("backupcheck is not available in this build (no_backupcheck)")

// Wait is a no-op in builds with the no_backupcheck tag.
func Wait() {}

// Register is a no-op in builds with the no_backupcheck tag.
func Register(app core.App) {}

// Latest always fails in builds with the no_backupcheck tag.
func Latest(ctx context.Context, app kernel.App) (string, error) { return "", errCompiledOut }

// Verify always fails in builds with the no_backupcheck tag.
func Verify(ctx context.Context, app kernel.App, name string) (Report, error) {
	return Report{Name: name}, errCompiledOut
}
