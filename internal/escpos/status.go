package escpos

import "fmt"

// StatusQuery is the transmission of the four real-time status bytes
// (DLE EOT 1..4). Write it to the printer and read 4 bytes back.
func StatusQuery() []byte {
	return []byte{DLE, EOT, 1, DLE, EOT, 2, DLE, EOT, 3, DLE, EOT, 4}
}

// Status is the decoded printer state.
type Status struct {
	Offline      bool // n=1 bit 3
	CoverOpen    bool // n=2 bit 2
	FeedPressed  bool // n=2 bit 3
	PaperEndStop bool // n=2 bit 5: printing stopped by paper end
	ErrorStop    bool // n=2 bit 6: printing stopped by an error
	MechError    bool // n=3 bit 2
	CutterError  bool // n=3 bit 3
	Unrecovered  bool // n=3 bit 5
	AutoRecover  bool // n=3 bit 6: recoverable error
	PaperNear    bool // n=4 bits 2,3
	PaperEnd     bool // n=4 bits 5,6
}

// ParseStatus decodes the answer to [StatusQuery]: one byte for each of the
// queries 1 to 4. Bit 7, bit 0 and bit 1 are fixed in a valid answer (bit 4 = 1,
// bits 0,1 vary by model) so only bit 4 set and bit 7 clear are checked.
func ParseStatus(resp []byte) (Status, error) {
	var s Status
	if len(resp) != 4 {
		return s, fmt.Errorf("escpos: status answer is %d bytes, want 4", len(resp))
	}
	for i, v := range resp {
		if v&0x80 != 0 || v&0x10 == 0 {
			return s, fmt.Errorf("escpos: status byte %d (0x%02X) is not a DLE EOT answer", i+1, v)
		}
	}
	n1, n2, n3, n4 := resp[0], resp[1], resp[2], resp[3]
	s.Offline = n1&0x08 != 0
	s.CoverOpen = n2&0x04 != 0
	s.FeedPressed = n2&0x08 != 0
	s.PaperEndStop = n2&0x20 != 0
	s.ErrorStop = n2&0x40 != 0
	s.MechError = n3&0x04 != 0
	s.CutterError = n3&0x08 != 0
	s.Unrecovered = n3&0x20 != 0
	s.AutoRecover = n3&0x40 != 0
	s.PaperNear = n4&0x0C != 0
	s.PaperEnd = n4&0x60 != 0
	return s, nil
}

// Ready reports whether the printer can print: online, cover closed, paper
// present and no error.
func (s Status) Ready() bool {
	return !s.Offline && !s.CoverOpen && !s.PaperEnd && !s.PaperEndStop && !s.ErrorStop &&
		!s.MechError && !s.CutterError && !s.Unrecovered
}

// NeedsAttention reports a state that a retry cannot fix without a person
// (paper out, cover open, hard error).
func (s Status) NeedsAttention() bool {
	return s.PaperEnd || s.PaperEndStop || s.CoverOpen || s.MechError || s.CutterError || s.Unrecovered
}
