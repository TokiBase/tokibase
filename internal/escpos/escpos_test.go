package escpos

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func eq(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got % X\nwant % X", name, got, want)
	}
}

func TestDirectivesGolden(t *testing.T) {
	cases := []struct {
		name string
		f    func(b *Builder)
		want []byte
	}{
		{"init", func(b *Builder) { b.Init() }, []byte{0x1B, '@', 0x1B, 't', 0}},
		{"init858", func(b *Builder) { b.cp = CP858; b.Init() }, []byte{0x1B, '@', 0x1B, 't', 19}},
		{"center", func(b *Builder) { b.Align(AlignCenter) }, []byte{0x1B, 'a', 1}},
		{"left", func(b *Builder) { b.Align(AlignLeft) }, []byte{0x1B, 'a', 0}},
		{"right", func(b *Builder) { b.Align(AlignRight) }, []byte{0x1B, 'a', 2}},
		{"size22", func(b *Builder) { b.Size(2, 2) }, []byte{0x1D, '!', 0x11}},
		{"size38", func(b *Builder) { b.Size(3, 8) }, []byte{0x1D, '!', 0x27}},
		{"sizeclamp", func(b *Builder) { b.Size(0, 99) }, []byte{0x1D, '!', 0x07}},
		{"bold", func(b *Builder) { b.Bold(true) }, []byte{0x1B, 'E', 1}},
		{"boldoff", func(b *Builder) { b.Bold(false) }, []byte{0x1B, 'E', 0}},
		{"feed", func(b *Builder) { b.Feed(3) }, []byte{0x1B, 'd', 3}},
		{"cut", func(b *Builder) { b.Cut() }, []byte{0x1D, 'V', 66, 0}},
		{"drawer", func(b *Builder) { b.Drawer() }, []byte{0x1B, 'p', 0, 25, 250}},
		{"line", func(b *Builder) { b.Line("Hi") }, []byte{'H', 'i', 0x0A}},
	}
	for _, c := range cases {
		b := New(CP437)
		c.f(b)
		eq(t, c.name, b.Bytes(), c.want)
	}
}

func TestQRNative(t *testing.T) {
	b := New(CP437)
	if err := b.QR("ABC", 5, QRHigh); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x1D, '(', 'k', 4, 0, 0x31, 0x41, 0x32, 0,
		0x1D, '(', 'k', 3, 0, 0x31, 0x43, 5,
		0x1D, '(', 'k', 3, 0, 0x31, 0x45, 0x33,
		0x1D, '(', 'k', 6, 0, 0x31, 0x50, 0x30, 'A', 'B', 'C',
		0x1D, '(', 'k', 3, 0, 0x31, 0x51, 0x30,
	}
	eq(t, "qr", b.Bytes(), want)
	if New(CP437).QR("", 4, QRLow) == nil {
		t.Error("empty data must fail")
	}
}

func TestQRRaster(t *testing.T) {
	bm, err := QRBitmap("TOKI-0001", QRMedium, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	// version 1 (21 modules) fits 9 bytes at M: (21+8)*2 = 58 dots
	if bm.W != 58 || bm.H != 58 || len(bm.Data) != 8*58 {
		t.Fatalf("bitmap %dx%d len %d", bm.W, bm.H, len(bm.Data))
	}
	px := func(x, y int) bool { return bm.Data[y*8+x/8]&(0x80>>uint(x%8)) != 0 }
	// quiet zone is light, the finder corner (module 0,0) is dark (2x2 dots)
	for i := 0; i < 8; i++ {
		if px(i, 0) || px(0, i) {
			t.Fatal("quiet zone must be light")
		}
	}
	if !px(8, 8) || !px(9, 9) || !px(8+12, 8) {
		t.Fatal("finder pattern top-left module must be dark")
	}
	// the finder is 7 modules wide with a light ring at module 1
	if px(8+2, 8+2) || !px(8+4, 8+4) {
		t.Fatal("finder ring structure wrong")
	}
	b := New(CP437)
	b.QRRaster = true
	if err := b.QR("TOKI-0001", 2, QRMedium); err != nil {
		t.Fatal(err)
	}
	head := []byte{0x1D, 'v', '0', 0, 8, 0, 58, 0}
	eq(t, "raster header", b.Bytes()[:8], head)
	if b.Len() != 8+8*58 {
		t.Fatalf("raster len %d", b.Len())
	}
	if _, err := QRBitmap(strings.Repeat("x", 5000), QRHigh, 1, 0); !errors.Is(err, ErrQRTooLong) {
		t.Fatalf("too long: %v", err)
	}
}

func TestBarcode(t *testing.T) {
	b := New(CP437)
	if err := b.Barcode(Code128, "AB12"); err != nil {
		t.Fatal(err)
	}
	eq(t, "code128", b.Bytes(), []byte{
		0x1D, 'H', 2, 0x1D, 'h', 80, 0x1D, 'w', 2,
		0x1D, 'k', 73, 6, '{', 'B', 'A', 'B', '1', '2'})
	b = New(CP437)
	if err := b.Barcode(EAN13, "590123412345"); err != nil {
		t.Fatal(err)
	}
	eq(t, "ean13", b.Bytes()[9:], append([]byte{0x1D, 'k', 67, 12}, "590123412345"...))
	for _, bad := range [][2]string{{EAN13, "12345"}, {EAN13, "59012341234A"}, {Code128, ""}, {Code128, "a\x01"}, {"upc", "1"}} {
		if New(CP437).Barcode(bad[0], bad[1]) == nil {
			t.Errorf("%v must fail", bad)
		}
	}
}

func TestCodepages(t *testing.T) {
	cases := []struct {
		cp   Codepage
		in   string
		want []byte
	}{
		{CP437, "Caf\u00e9 \u00f1", []byte{'C', 'a', 'f', 0x82, ' ', 0xA4}},
		{CP437, "\u2500\u00b0", []byte{0xC4, 0xF8}},
		{CP437, "\u20ac", []byte{'?'}},
		{CP858, "\u20ac \u00e9", []byte{0xD5, ' ', 0x82}},
		{CP858, "\u00fc\u00c7", []byte{0x81, 0x80}},
		{WPC1252, "\u20ac\u00e9\u2019\u201c", []byte{0x80, 0xE9, 0x92, 0x93}},
		{WPC1252, "\u0152", []byte{0x8C}},
		{CP437, "a\r\nb\x00\x07", []byte{'a', '\n', 'b'}},
		{CP437, "\u4e2d", []byte{'?'}},
	}
	for _, c := range cases {
		eq(t, c.in, c.cp.Encode(c.in), c.want)
	}
	// every defined byte round trips
	for _, cp := range []Codepage{CP437, CP858, WPC1252} {
		for b := 0x20; b < 0x100; b++ {
			r := cp.decode(byte(b))
			if r == 0 || b == 0x7F {
				continue
			}
			got := cp.Encode(string(r))
			if len(got) != 1 || got[0] != byte(b) {
				t.Errorf("cp %d byte %02X rune %U -> % X", cp, b, r, got)
			}
		}
	}
	if cp, ok := ParseCodepage("cp858"); !ok || cp != CP858 {
		t.Error("ParseCodepage")
	}
	if _, ok := ParseCodepage("klingon"); ok {
		t.Error("unknown codepage")
	}
}

func TestStatusMatrix(t *testing.T) {
	const base = 0x12 // bit 4 set, bit 1 set as most printers answer
	cases := []struct {
		name string
		resp [4]byte
		chk  func(s Status) bool
		rdy  bool
	}{
		{"ready", [4]byte{base, base, base, base}, func(s Status) bool { return true }, true},
		{"offline", [4]byte{base | 0x08, base, base, base}, func(s Status) bool { return s.Offline }, false},
		{"cover", [4]byte{base, base | 0x04, base, base}, func(s Status) bool { return s.CoverOpen }, false},
		{"feed", [4]byte{base, base | 0x08, base, base}, func(s Status) bool { return s.FeedPressed }, true},
		{"paperstop", [4]byte{base, base | 0x20, base, base}, func(s Status) bool { return s.PaperEndStop }, false},
		{"errorstop", [4]byte{base, base | 0x40, base, base}, func(s Status) bool { return s.ErrorStop }, false},
		{"mech", [4]byte{base, base, base | 0x04, base}, func(s Status) bool { return s.MechError }, false},
		{"cutter", [4]byte{base, base, base | 0x08, base}, func(s Status) bool { return s.CutterError }, false},
		{"unrecov", [4]byte{base, base, base | 0x20, base}, func(s Status) bool { return s.Unrecovered }, false},
		{"autorec", [4]byte{base, base, base | 0x40, base}, func(s Status) bool { return s.AutoRecover }, true},
		{"near", [4]byte{base, base, base, base | 0x0C}, func(s Status) bool { return s.PaperNear && !s.PaperEnd }, true},
		{"paperend", [4]byte{base, base, base, base | 0x60}, func(s Status) bool { return s.PaperEnd }, false},
	}
	for _, c := range cases {
		s, err := ParseStatus(c.resp[:])
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.chk(s) {
			t.Errorf("%s: %+v", c.name, s)
		}
		if s.Ready() != c.rdy {
			t.Errorf("%s: Ready=%v want %v (%+v)", c.name, s.Ready(), c.rdy, s)
		}
	}
	s, _ := ParseStatus([]byte{base, base | 0x04, base, base | 0x60})
	if !s.NeedsAttention() {
		t.Error("cover+paper needs attention")
	}
	if _, err := ParseStatus([]byte{base}); err == nil {
		t.Error("short answer")
	}
	if _, err := ParseStatus([]byte{0x00, base, base, base}); err == nil {
		t.Error("bit 4 clear is not a status byte")
	}
	if _, err := ParseStatus([]byte{0x92, base, base, base}); err == nil {
		t.Error("bit 7 set is not a status byte")
	}
	eq(t, "query", StatusQuery(), []byte{0x10, 4, 1, 0x10, 4, 2, 0x10, 4, 3, 0x10, 4, 4})
}

func TestRenderPlanTemplate(t *testing.T) {
	tpl := `@center
@size 2 2
PARKIR SIMPANG
@size 1 1
@left
Plat: {{.plate}}
Masuk: {{date .in "02 Jan 15:04"}}
Tarif: {{money .fee}}
@qr {{.ticket_id}}        # GS ( k native
@barcode code128 {{.ticket_id}}
@feed 3
@cut
@drawer
`
	data := map[string]any{"plate": "B 1234 XYZ", "in": "2026-10-08T07:05:00Z", "fee": 15000, "ticket_id": "T-0001"}
	got, err := Render(tpl, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	w.Write([]byte{0x1B, '@', 0x1B, 't', 0})
	w.Write([]byte{0x1B, 'a', 1, 0x1D, '!', 0x11})
	w.WriteString("PARKIR SIMPANG\n")
	w.Write([]byte{0x1D, '!', 0, 0x1B, 'a', 0})
	w.WriteString("Plat: B 1234 XYZ\nMasuk: 08 Oct 07:05\nTarif: 15.000\n")
	q := New(CP437)
	_ = q.QR("T-0001", 4, QRMedium)
	w.Write(q.Bytes())
	_ = q.Barcode(Code128, "T-0001")
	b2 := New(CP437)
	_ = b2.Barcode(Code128, "T-0001")
	w.Write(b2.Bytes())
	w.Write([]byte{0x1B, 'd', 3, 0x1D, 'V', 66, 0, 0x1B, 'p', 0, 25, 250})
	eq(t, "receipt", got, w.Bytes())
}

func TestRenderDirectivesAndErrors(t *testing.T) {
	got, err := Render("@@mail\n@bold\n@bold off\n@feed\n@codepage cp858\n\xc3\xa9", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "misc", got, []byte{0x1B, '@', 0x1B, 't', 0, '@', 'm', 'a', 'i', 'l', '\n',
		0x1B, 'E', 1, 0x1B, 'E', 0, 0x1B, 'd', 1, 0x1B, 't', 19, 0x82, '\n'})
	bad := []string{"@nope", "@size 9 1", "@size 1", "@feed 300", "@bold maybe", "@qr", "@qr size=99 x", "@barcode ean13 12", "@codepage x", "{{template \"x\"}}", "{{define \"a\"}}x{{end}}", "{{.x | call}}", "{{"}
	for _, tpl := range bad {
		if _, err := Render(tpl, nil, Options{}); err == nil {
			t.Errorf("%q must fail", tpl)
		}
	}
	got, err = Render("@qr size=3 ec=H a b c\n", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte{0x31, 0x43, 3}) || !bytes.Contains(got, []byte{0x31, 0x45, 0x33}) || !bytes.HasSuffix(got, []byte("a b c\x1d(k\x03\x001Q0")) {
		t.Errorf("qr options: % X", got)
	}
	got, _ = Render("@qr size=3 ec=H a b c\n", nil, Options{QRRaster: true})
	if !bytes.Contains(got, []byte{0x1D, 'v', '0'}) {
		t.Error("raster option")
	}
}

func TestTemplateFuncs(t *testing.T) {
	cases := []struct{ tpl, want string }{
		{`{{pad "ab" 5}}|`, "ab   |"},
		{`{{pad "abcdef" 3}}|`, "abc|"},
		{`{{padl "ab" 5}}|`, "   ab|"},
		{`{{padl "abcdef" 3}}|`, "def|"},
		{`{{money 1234567}}`, "1.234.567"},
		{`{{money 999}}`, "999"},
		{`{{money -1500.4}}`, "-1.500"},
		{`{{money "2500"}}`, "2.500"},
		{`{{money 0}}`, "0"},
		{`{{upper "abc"}}`, "ABC"},
		{`{{date 0 "2006-01-02"}}`, "1970-01-01"},
		{`{{date "2026-10-08 14:30:00" "15:04"}}`, "14:30"},
	}
	for _, c := range cases {
		got, err := Render(c.tpl, nil, Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.tpl, err)
		}
		want := append([]byte{0x1B, '@', 0x1B, 't', 0}, []byte(c.want+"\n")...)
		eq(t, c.tpl, got, want)
	}
	loc := time.FixedZone("WIB", 7*3600)
	got, _ := Render(`{{date "2026-10-08T00:30:00Z" "15:04"}}`, nil, Options{Location: loc})
	if !bytes.HasSuffix(got, []byte("07:30\n")) {
		t.Errorf("location: %q", got)
	}
	for _, bad := range []string{`{{money "x"}}`, `{{money true}}`, `{{date "nope" "15:04"}}`} {
		if _, err := Render(bad, nil, Options{}); err == nil {
			t.Errorf("%s must fail", bad)
		}
	}
}

func TestRenderLimits(t *testing.T) {
	if _, err := Render(strings.Repeat("x", 20<<10), nil, Options{}); !errors.Is(err, ErrLimit) {
		t.Errorf("template size: %v", err)
	}
	if _, err := Render(strings.Repeat("{{.a}}", 100), map[string]any{"a": strings.Repeat("y", 4096)}, Options{}); !errors.Is(err, ErrLimit) {
		t.Errorf("output size: %v", err)
	}
	if _, err := Render("x", map[string]any{"a": strings.Repeat("y", 5000)}, Options{}); !errors.Is(err, ErrLimit) {
		t.Errorf("string size: %v", err)
	}
	deep := any("x")
	for i := 0; i < 12; i++ {
		deep = map[string]any{"a": deep}
	}
	if _, err := Render("x", deep, Options{}); !errors.Is(err, ErrLimit) {
		t.Errorf("depth: %v", err)
	}
	wide := make([]any, 6000)
	if _, err := Render("x", map[string]any{"l": wide}, Options{}); !errors.Is(err, ErrLimit) {
		t.Errorf("nodes: %v", err)
	}
	// nested range multiplies output, the output limit stops it
	items := make([]any, 60)
	for i := range items {
		items[i] = "0123456789"
	}
	_, err := Render(`{{range .l}}{{range $.l}}{{range $.l}}{{.}}{{end}}{{end}}{{end}}`, map[string]any{"l": items}, Options{})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("amplification: %v", err)
	}
	// stream limit: many QR rasters
	_, err = Render(strings.Repeat("@qr size=16 hello\n", 50), nil, Options{QRRaster: true})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("stream: %v", err)
	}
	// custom limits
	if _, err := Render("hello", nil, Options{Limits: Limits{MaxTemplate: 3, MaxOutput: 10, MaxBytes: 10, MaxData: 1, MaxDepth: 1, MaxString: 1}}); !errors.Is(err, ErrLimit) {
		t.Errorf("custom: %v", err)
	}
}
