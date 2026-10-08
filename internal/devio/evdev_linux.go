//go:build linux

package devio

import (
	"os"

	"golang.org/x/sys/unix"
)

// eviocgrab is EVIOCGRAB = _IOW('E', 0x90, int).
const eviocgrab = 0x40044590

// Evdev is an open /dev/input/event* device.
type Evdev struct {
	*os.File
	Grabbed bool
}

// OpenEvdev opens an event device read-only. With grab the kernel gives the
// events to this process only (EVIOCGRAB), so the scanned digits do not also
// type into the console or a browser.
func OpenEvdev(path string, grab bool) (*Evdev, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	d := &Evdev{File: f}
	if grab {
		if err := unix.IoctlSetInt(int(f.Fd()), eviocgrab, 1); err != nil {
			f.Close()
			return nil, err
		}
		d.Grabbed = true
	}
	return d, nil
}

// Scanner returns a [KeyScanner] over the device.
func (d *Evdev) Scanner(max int) (*KeyScanner, error) {
	return NewKeyScanner(d.File, EventSize, max)
}

// Close releases the grab and closes the device.
func (d *Evdev) Close() error {
	if d.Grabbed {
		_ = unix.IoctlSetInt(int(d.File.Fd()), eviocgrab, 0)
	}
	return d.File.Close()
}
