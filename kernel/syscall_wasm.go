//go:build js && wasm

package kernel

import "errors"

// https://github.com/tokibase/tokibase/pull/7116
func execve(argv0 string, argv []string, envv []string) error {
	return errors.ErrUnsupported
}
