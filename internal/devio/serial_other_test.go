//go:build !linux

package devio

import (
	"errors"
	"testing"
)

func TestUnsupportedOutsideLinux(t *testing.T) {
	if _, err := OpenSerial("COM1", SerialConfig{Baud: 9600}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := OpenEvdev("/dev/input/event0", false); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}
