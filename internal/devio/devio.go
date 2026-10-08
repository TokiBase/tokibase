// Package devio is the device I/O layer of the edge modules: serial ports,
// evdev keyboards (barcode wedge scanners), and a TCP/file dialer with an
// address policy. Pure Go, no cgo; the Linux specific parts use only
// golang.org/x/sys/unix and have stubs that return [ErrUnsupported] elsewhere.
package devio

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// ErrUnsupported is returned by the serial and evdev openers on platforms
// other than Linux.
var ErrUnsupported = errors.ErrUnsupported

// ErrLineTooLong is returned by [LineReader.Next] when a line exceeds the
// maximum; the partial line is discarded up to the next terminator.
var ErrLineTooLong = errors.New("devio: line too long")

// LineReader splits a byte stream into lines terminated by CR, LF or CRLF.
// Empty lines are skipped. Unlike bufio.Scanner it survives read errors such
// as deadline timeouts: buffered bytes are kept and Next can be called again.
type LineReader struct {
	r          io.Reader
	max        int
	buf        []byte
	discarding bool // dropping the rest of an over-long line
	eof        bool
}

// NewLineReader reads lines of at most max bytes (default 4096 when max <= 0).
func NewLineReader(r io.Reader, max int) *LineReader {
	if max <= 0 {
		max = 4096
	}
	return &LineReader{r: r, max: max}
}

// Next returns the next non-empty line without its terminator. A final line
// without terminator is returned before io.EOF.
func (l *LineReader) Next() (string, error) {
	tmp := make([]byte, 512)
	for {
		if i := bytes.IndexAny(l.buf, "\r\n"); i >= 0 {
			line := l.buf[:i]
			if l.discarding {
				l.discarding = false
				l.buf = l.buf[i+1:]
				return "", ErrLineTooLong
			}
			s := string(line)
			l.buf = l.buf[i+1:]
			if len(s) > l.max {
				return "", ErrLineTooLong
			}
			if s == "" {
				continue
			}
			return s, nil
		}
		if l.discarding || len(l.buf) > l.max {
			l.discarding = true
			l.buf = l.buf[:0]
		}
		if l.eof {
			if len(l.buf) > 0 {
				s := string(l.buf)
				l.buf = nil
				return s, nil
			}
			return "", io.EOF
		}
		n, err := l.r.Read(tmp)
		l.buf = append(l.buf, tmp[:n]...)
		if errors.Is(err, io.EOF) {
			l.eof = true
		} else if err != nil {
			return "", err
		}
	}
}

// openFileWrite opens a device file for writing without creating it.
func openFileWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}
