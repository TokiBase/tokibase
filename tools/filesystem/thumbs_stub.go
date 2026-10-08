//go:build no_thumbs

package filesystem

import "errors"

// createThumb is the no_thumbs stub: image decoding/resizing is compiled out
// (nano, edge); thumb requests fail like any other thumb error and the
// original file is served instead.
func (s *System) createThumb(originalKey, thumbKey, thumbSize string) error {
	return errors.New("thumbs are not available in this build (no_thumbs)")
}
