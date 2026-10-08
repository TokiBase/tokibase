//go:build !linux

package devio

import "os"

// Serial is not available on this platform.
type Serial struct {
	*os.File
}

// OpenSerial returns [ErrUnsupported] on platforms other than Linux.
func OpenSerial(path string, cfg SerialConfig) (*Serial, error) { return nil, ErrUnsupported }
