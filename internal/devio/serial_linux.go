//go:build linux

package devio

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var baudConst = map[int]uint32{
	1200: unix.B1200, 2400: unix.B2400, 4800: unix.B4800, 9600: unix.B9600,
	19200: unix.B19200, 38400: unix.B38400, 57600: unix.B57600, 115200: unix.B115200, 230400: unix.B230400,
}

// cbaud is the baud rate mask of c_cflag (CBAUD, octal 010017).
const cbaud = 0o010017

// Serial is an open serial port. Read, Write and the Set*Deadline methods come
// from the embedded file; a read that hits the deadline returns
// os.ErrDeadlineExceeded.
type Serial struct {
	*os.File
}

// OpenSerial opens path (for example /dev/ttyUSB0) in raw 8N1 mode.
func OpenSerial(path string, cfg SerialConfig) (*Serial, error) {
	speed, ok := baudConst[cfg.Baud]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrBadBaud, cfg.Baud)
	}
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	_ = unix.SetNonblock(fd, true)
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("devio: %s is not a terminal: %w", path, err)
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.PARODD | unix.CSTOPB | unix.CRTSCTS | cbaud
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | speed
	t.Ispeed, t.Ospeed = speed, speed
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		f.Close()
		return nil, err
	}
	return &Serial{File: f}, nil
}
