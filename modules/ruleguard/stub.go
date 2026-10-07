//go:build no_ruleguard

// Package ruleguard is compiled out of this binary by the no_ruleguard build tag.
package ruleguard

import (
	"errors"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Policy is the public-rule allowlist.
type Policy struct{}

// Finding is one rule lint result.
type Finding struct{}

var errCompiledOut = errors.New("ruleguard is not available in this build (no_ruleguard)")

// Register is a no-op in builds with the no_ruleguard tag.
func Register(app core.App) {}

// Load always fails in builds with the no_ruleguard tag.
func Load(dataDir string) (Policy, error) { return Policy{}, errCompiledOut }

// Lint always fails in builds with the no_ruleguard tag.
func Lint(app kernel.App, pol Policy) ([]Finding, error) { return nil, errCompiledOut }
