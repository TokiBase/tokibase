package devio

import (
	"errors"
	"time"
)

// SerialConfig configures a serial port (always raw mode, 8 data bits, no
// parity, 1 stop bit, no flow control).
type SerialConfig struct {
	Baud int
}

// Bauds lists the supported baud rates.
var Bauds = []int{1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400}

// ErrBadBaud is returned for a baud rate that is not in [Bauds].
var ErrBadBaud = errors.New("devio: unsupported baud rate")

// ValidBaud reports whether b is in [Bauds].
func ValidBaud(b int) bool {
	for _, v := range Bauds {
		if v == b {
			return true
		}
	}
	return false
}

// Deadliner is implemented by ports with read/write deadlines.
type Deadliner interface {
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}
