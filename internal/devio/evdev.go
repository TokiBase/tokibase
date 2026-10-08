package devio

import (
	"encoding/binary"
	"errors"
	"io"
	"unsafe"
)

// inputEvent mirrors struct input_event of <linux/input.h>: a struct timeval
// of two C longs, then type, code and value.
type inputEvent struct {
	Sec, Usec int
	Type      uint16
	Code      uint16
	Value     int32
}

// EventSize is the size of one input_event on this machine: 24 bytes on 64 bit
// systems and 16 on 32 bit ones (C long is 8 or 4 bytes).
var EventSize = int(unsafe.Sizeof(inputEvent{}))

// Linux input constants used by the scanner decoder.
const (
	evKey = 1

	keyEnter      = 28
	keyKPEnter    = 96
	keyLeftShift  = 42
	keyRightShift = 54
	keyCapsLock   = 58
)

// ErrScanTooLong is returned by [KeyScanner.Next] when the scan exceeds the
// maximum length; the input is dropped up to the next Enter.
var ErrScanTooLong = errors.New("devio: scan too long")

// usKeys maps key codes to (plain, shifted) characters of a US keyboard.
var usKeys = map[uint16][2]byte{
	2: {'1', '!'}, 3: {'2', '@'}, 4: {'3', '#'}, 5: {'4', '$'}, 6: {'5', '%'},
	7: {'6', '^'}, 8: {'7', '&'}, 9: {'8', '*'}, 10: {'9', '('}, 11: {'0', ')'},
	12: {'-', '_'}, 13: {'=', '+'},
	16: {'q', 'Q'}, 17: {'w', 'W'}, 18: {'e', 'E'}, 19: {'r', 'R'}, 20: {'t', 'T'},
	21: {'y', 'Y'}, 22: {'u', 'U'}, 23: {'i', 'I'}, 24: {'o', 'O'}, 25: {'p', 'P'},
	26: {'[', '{'}, 27: {']', '}'},
	30: {'a', 'A'}, 31: {'s', 'S'}, 32: {'d', 'D'}, 33: {'f', 'F'}, 34: {'g', 'G'},
	35: {'h', 'H'}, 36: {'j', 'J'}, 37: {'k', 'K'}, 38: {'l', 'L'},
	39: {';', ':'}, 40: {'\'', '"'}, 41: {'`', '~'}, 43: {'\\', '|'},
	44: {'z', 'Z'}, 45: {'x', 'X'}, 46: {'c', 'C'}, 47: {'v', 'V'}, 48: {'b', 'B'},
	49: {'n', 'N'}, 50: {'m', 'M'}, 51: {',', '<'}, 52: {'.', '>'}, 53: {'/', '?'},
	57: {' ', ' '},
	// keypad (num lock assumed on, as scanners send digits)
	71: {'7', '7'}, 72: {'8', '8'}, 73: {'9', '9'}, 74: {'-', '-'},
	75: {'4', '4'}, 76: {'5', '5'}, 77: {'6', '6'}, 78: {'+', '+'},
	79: {'1', '1'}, 80: {'2', '2'}, 81: {'3', '3'}, 82: {'0', '0'}, 83: {'.', '.'},
	98: {'/', '/'}, 55: {'*', '*'},
}

// KeyScanner turns the event stream of a keyboard-wedge barcode scanner into
// scanned strings. A scan ends with Enter.
type KeyScanner struct {
	r       io.Reader
	size    int
	max     int
	shift   int // pressed shift keys
	caps    bool
	buf     []byte
	over    bool
	scratch []byte
}

// NewKeyScanner reads events of eventSize bytes (16 or 24, see [EventSize])
// from r. max bounds one scan (default 512).
func NewKeyScanner(r io.Reader, eventSize, max int) (*KeyScanner, error) {
	if eventSize != 16 && eventSize != 24 {
		return nil, errors.New("devio: event size must be 16 or 24")
	}
	if max <= 0 {
		max = 512
	}
	return &KeyScanner{r: r, size: eventSize, max: max, scratch: make([]byte, eventSize)}, nil
}

// Next blocks until a scan is complete and returns it. Events other than key
// presses are ignored; key repeats are ignored. The byte order is the host's,
// which for every Linux target of the edge profile is little endian.
func (k *KeyScanner) Next() (string, error) {
	for {
		if _, err := io.ReadFull(k.r, k.scratch); err != nil {
			return "", err
		}
		e := k.scratch
		off := k.size - 8 // type, code, value follow the timeval
		typ := binary.LittleEndian.Uint16(e[off:])
		code := binary.LittleEndian.Uint16(e[off+2:])
		val := int32(binary.LittleEndian.Uint32(e[off+4:]))
		if typ != evKey {
			continue
		}
		if code == keyLeftShift || code == keyRightShift {
			switch val {
			case 1:
				k.shift++
			case 0:
				if k.shift > 0 {
					k.shift--
				}
			}
			continue
		}
		if val != 1 { // release or repeat
			continue
		}
		if code == keyCapsLock {
			k.caps = !k.caps
			continue
		}
		if code == keyEnter || code == keyKPEnter {
			s, over := string(k.buf), k.over
			k.buf, k.over = k.buf[:0], false
			if over {
				return "", ErrScanTooLong
			}
			if s == "" {
				continue
			}
			return s, nil
		}
		pair, ok := usKeys[code]
		if !ok {
			continue
		}
		shifted := k.shift > 0
		c := pair[0]
		if c >= 'a' && c <= 'z' {
			shifted = shifted != k.caps
		}
		if shifted {
			c = pair[1]
		}
		if len(k.buf) >= k.max {
			k.over = true
			continue
		}
		k.buf = append(k.buf, c)
	}
}
