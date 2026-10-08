//go:build !linux

package devio

import "os"

// Evdev is not available on this platform.
type Evdev struct {
	*os.File
	Grabbed bool
}

// OpenEvdev returns [ErrUnsupported] on platforms other than Linux.
func OpenEvdev(path string, grab bool) (*Evdev, error) { return nil, ErrUnsupported }

// Scanner returns [ErrUnsupported].
func (d *Evdev) Scanner(max int) (*KeyScanner, error) { return nil, ErrUnsupported }
