//go:build linux

package devio

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPty returns the master and the slave path of a new pseudo terminal.
func openPty(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx: ", err)
	}
	t.Cleanup(func() { m.Close() })
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skip("cannot unlock pty: ", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skip("cannot get pty number: ", err)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

func TestSerialRawAndDeadline(t *testing.T) {
	m, slave := openPty(t)
	s, err := OpenSerial(slave, SerialConfig{Baud: 9600})
	if err != nil {
		t.Skip("cannot open pty slave: ", err)
	}
	defer s.Close()

	tio, err := unix.IoctlGetTermios(int(s.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&(unix.ICANON|unix.ECHO) != 0 || tio.Oflag&unix.OPOST != 0 || tio.Iflag&unix.ICRNL != 0 {
		t.Fatalf("not raw: %+v", tio)
	}
	if tio.Cflag&unix.CSIZE != unix.CS8 || tio.Cflag&unix.PARENB != 0 || tio.Cflag&unix.CSTOPB != 0 {
		t.Fatalf("not 8N1: %x", tio.Cflag)
	}
	if tio.Cflag&cbaud != unix.B9600 {
		t.Fatalf("baud bits %x", tio.Cflag&cbaud)
	}

	// the master writes a scan with CRLF; raw mode must not translate it
	if _, err := m.Write([]byte("8991234\r\n55\r")); err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	lr := NewLineReader(s, 0)
	for _, want := range []string{"8991234", "55"} {
		got, err := lr.Next()
		if err != nil || got != want {
			t.Fatalf("got %q err %v, want %q", got, err, want)
		}
	}
	// nothing more: the read deadline fires
	_ = s.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := lr.Next(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	// and the reader keeps working afterwards
	m.Write([]byte("next\n"))
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	if got, err := lr.Next(); err != nil || got != "next" {
		t.Fatalf("after timeout: %q %v", got, err)
	}
}

func TestSerialErrors(t *testing.T) {
	if _, err := OpenSerial("/dev/null", SerialConfig{Baud: 12345}); !errors.Is(err, ErrBadBaud) {
		t.Fatalf("bad baud: %v", err)
	}
	if _, err := OpenSerial("/dev/null", SerialConfig{Baud: 9600}); err == nil {
		t.Fatal("/dev/null is not a terminal")
	}
	if _, err := OpenSerial("/nonexistent/tty", SerialConfig{Baud: 9600}); err == nil {
		t.Fatal("missing device")
	}
	for _, b := range Bauds {
		if _, ok := baudConst[b]; !ok {
			t.Errorf("baud %d has no constant", b)
		}
		if !ValidBaud(b) {
			t.Errorf("ValidBaud(%d)", b)
		}
	}
	if ValidBaud(1234) {
		t.Error("1234")
	}
}
