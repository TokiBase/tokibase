// Package escpos builds ESC/POS byte streams for receipt printers and parses
// the real-time status bytes they answer. It is pure Go with no dependencies
// outside the standard library; I/O lives in internal/devio.
package escpos

import (
	"bytes"
	"errors"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

// Control bytes.
const (
	ESC = 0x1B
	GS  = 0x1D
	DLE = 0x10
	EOT = 0x04
	LF  = 0x0A
)

// Align is a text alignment.
type Align byte

// Alignments (ESC a n).
const (
	AlignLeft   Align = 0
	AlignCenter Align = 1
	AlignRight  Align = 2
)

// Builder accumulates a command stream. The zero value is usable (CP437).
type Builder struct {
	buf bytes.Buffer
	cp  Codepage
	// QRRaster makes QR emit a raster image instead of the native GS ( k
	// command, for printers without native QR support.
	QRRaster bool
}

// New returns a Builder that encodes text with cp.
func New(cp Codepage) *Builder { return &Builder{cp: cp} }

// Bytes returns the stream built so far.
func (b *Builder) Bytes() []byte { return b.buf.Bytes() }

// Len is the stream length.
func (b *Builder) Len() int { return b.buf.Len() }

func (b *Builder) raw(p ...byte) *Builder { b.buf.Write(p); return b }

// Raw appends bytes unchanged.
func (b *Builder) Raw(p []byte) *Builder { b.buf.Write(p); return b }

// Init resets the printer (ESC @) and selects the builder codepage (ESC t n).
func (b *Builder) Init() *Builder {
	b.raw(ESC, '@')
	return b.Codepage(b.cp)
}

// Codepage selects cp in the printer (ESC t n) and for later text.
func (b *Builder) Codepage(cp Codepage) *Builder {
	b.cp = cp
	return b.raw(ESC, 't', cp.Code())
}

// Align sets the alignment (ESC a n).
func (b *Builder) Align(a Align) *Builder { return b.raw(ESC, 'a', byte(a)) }

// Size sets the character magnification, 1 to 8 each (GS ! n).
func (b *Builder) Size(w, h int) *Builder {
	w, h = clamp(w, 1, 8), clamp(h, 1, 8)
	return b.raw(GS, '!', byte((w-1)<<4|(h-1)))
}

// Bold turns emphasis on or off (ESC E n).
func (b *Builder) Bold(on bool) *Builder {
	if on {
		return b.raw(ESC, 'E', 1)
	}
	return b.raw(ESC, 'E', 0)
}

// Feed prints and feeds n lines (ESC d n).
func (b *Builder) Feed(n int) *Builder { return b.raw(ESC, 'd', byte(clamp(n, 0, 255))) }

// Cut feeds to the cutting position and makes a partial cut (GS V 66 0).
func (b *Builder) Cut() *Builder { return b.raw(GS, 'V', 66, 0) }

// Drawer pulses cash drawer pin 2 (ESC p 0 25 250: 50 ms on, 500 ms off).
func (b *Builder) Drawer() *Builder { return b.raw(ESC, 'p', 0, 25, 250) }

// Text appends s encoded in the current codepage. Runes outside it become '?'.
// Line breaks in s are sent as LF; carriage returns are dropped.
func (b *Builder) Text(s string) *Builder {
	b.buf.Write(b.cp.Encode(s))
	return b
}

// Line is Text followed by LF.
func (b *Builder) Line(s string) *Builder { return b.Text(s).raw(LF) }

// QR prints a QR code. size is the module size in dots (1 to 16), level the
// error correction. With QRRaster it prints an image, else the native command.
func (b *Builder) QR(data string, size int, level QRLevel) error {
	if len(data) == 0 {
		return errors.New("escpos: empty qr data")
	}
	size = clamp(size, 1, 16)
	if b.QRRaster {
		bm, err := QRBitmap(data, level, size, 4)
		if err != nil {
			return err
		}
		b.Raster(bm)
		return nil
	}
	if len(data) > 7089 {
		return ErrQRTooLong
	}
	b.raw(GS, '(', 'k', 4, 0, 0x31, 0x41, 0x32, 0)          // model 2
	b.raw(GS, '(', 'k', 3, 0, 0x31, 0x43, byte(size))       // module size
	b.raw(GS, '(', 'k', 3, 0, 0x31, 0x45, byte(0x30+level)) // error correction
	n := len(data) + 3
	b.raw(GS, '(', 'k', byte(n), byte(n>>8), 0x31, 0x50, 0x30)
	b.buf.WriteString(data)
	b.raw(GS, '(', 'k', 3, 0, 0x31, 0x51, 0x30) // print
	return nil
}

// Bitmap is a 1 bit image, row-major, MSB first, rows padded to bytes.
type Bitmap struct {
	W, H int
	Data []byte
}

// QR error correction levels (the n-0x30 of GS ( k fn 69).
type QRLevel int

// Levels, lowest to highest recovery.
const (
	QRLow QRLevel = iota
	QRMedium
	QRQuartile
	QRHigh
)

// ErrQRTooLong is returned when the payload does not fit a QR code.
var ErrQRTooLong = errors.New("escpos: qr payload too long")

// QRBitmap encodes data with go-qrcode (already linked into the edge binary
// through modules/totp) and renders it with scale dots per module and a quiet
// zone of quiet modules.
func QRBitmap(data string, level QRLevel, scale, quiet int) (Bitmap, error) {
	scale, quiet = clamp(scale, 1, 16), clamp(quiet, 0, 16)
	lv := [...]qrcode.RecoveryLevel{qrcode.Low, qrcode.Medium, qrcode.High, qrcode.Highest}[clamp(int(level), 0, 3)]
	q, err := qrcode.New(data, lv)
	if err != nil {
		return Bitmap{}, ErrQRTooLong
	}
	q.DisableBorder = true
	m := q.Bitmap()
	w := (len(m) + 2*quiet) * scale
	rb := (w + 7) / 8
	bm := Bitmap{W: w, H: w, Data: make([]byte, rb*w)}
	for y := 0; y < w; y++ {
		my := y/scale - quiet
		if my < 0 || my >= len(m) {
			continue
		}
		for x := 0; x < w; x++ {
			mx := x/scale - quiet
			if mx >= 0 && mx < len(m) && m[my][mx] {
				bm.Data[y*rb+x/8] |= 0x80 >> uint(x%8)
			}
		}
	}
	return bm, nil
}

// Raster prints a bitmap with GS v 0 (normal density).
func (b *Builder) Raster(bm Bitmap) *Builder {
	rb := (bm.W + 7) / 8
	b.raw(GS, 'v', '0', 0, byte(rb), byte(rb>>8), byte(bm.H), byte(bm.H>>8))
	b.buf.Write(bm.Data)
	return b
}

// Barcode symbologies.
const (
	Code128 = "code128"
	EAN13   = "ean13"
)

// Barcode prints a barcode with the number below it (GS H 2, GS h 80, GS w 2,
// GS k m n d...). Code128 uses code set B. EAN13 takes 12 or 13 digits.
func (b *Builder) Barcode(kind, data string) error {
	b.raw(GS, 'H', 2).raw(GS, 'h', 80).raw(GS, 'w', 2)
	switch kind {
	case Code128:
		if data == "" || len(data) > 253 {
			return errors.New("escpos: code128 data must be 1 to 253 characters")
		}
		for i := 0; i < len(data); i++ {
			if data[i] < 0x20 || data[i] > 0x7E {
				return fmt.Errorf("escpos: code128 character %q not printable ASCII", data[i])
			}
		}
		b.raw(GS, 'k', 73, byte(len(data)+2), '{', 'B')
		b.buf.WriteString(data)
	case EAN13:
		if len(data) != 12 && len(data) != 13 {
			return errors.New("escpos: ean13 needs 12 or 13 digits")
		}
		for i := 0; i < len(data); i++ {
			if data[i] < '0' || data[i] > '9' {
				return errors.New("escpos: ean13 needs digits only")
			}
		}
		b.raw(GS, 'k', 67, byte(len(data)))
		b.buf.WriteString(data)
	default:
		return fmt.Errorf("escpos: unknown barcode type %q", kind)
	}
	return nil
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }
