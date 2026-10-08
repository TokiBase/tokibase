package devio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLineReaderPipe(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		for _, chunk := range []string{"12", "34\r", "\n56\n\n\n", "78", "90\r\nlast"} {
			pw.Write([]byte(chunk))
			time.Sleep(2 * time.Millisecond)
		}
		pw.Close()
	}()
	lr := NewLineReader(pr, 16)
	var got []string
	for {
		s, err := lr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	want := []string{"1234", "56", "7890", "last"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestLineReaderTooLongAndRecover(t *testing.T) {
	in := bytes.NewReader([]byte("ok\n" + string(bytes.Repeat([]byte("x"), 100)) + "\nafter\n"))
	lr := NewLineReader(in, 10)
	if s, err := lr.Next(); err != nil || s != "ok" {
		t.Fatal(s, err)
	}
	if _, err := lr.Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want too long, got %v", err)
	}
	if s, err := lr.Next(); err != nil || s != "after" {
		t.Fatal(s, err)
	}
}

type flaky struct {
	chunks [][]byte
	errs   []error
}

func (f *flaky) Read(p []byte) (int, error) {
	if len(f.chunks) == 0 {
		return 0, io.EOF
	}
	c, e := f.chunks[0], f.errs[0]
	f.chunks, f.errs = f.chunks[1:], f.errs[1:]
	return copy(p, c), e
}

func TestLineReaderSurvivesTimeout(t *testing.T) {
	f := &flaky{chunks: [][]byte{[]byte("abc"), nil, []byte("def\n")}, errs: []error{nil, os.ErrDeadlineExceeded, nil}}
	lr := NewLineReader(f, 0)
	if _, err := lr.Next(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("want timeout, got %v", err)
	}
	if s, err := lr.Next(); err != nil || s != "abcdef" {
		t.Fatal(s, err)
	}
}

// ---- evdev ----

type ev struct {
	typ, code uint16
	val       int32
}

func encodeEvents(size int, evs []ev) []byte {
	var b bytes.Buffer
	for _, e := range evs {
		hdr := make([]byte, size-8)
		binary.LittleEndian.PutUint32(hdr, 1700000000)
		b.Write(hdr)
		var tail [8]byte
		binary.LittleEndian.PutUint16(tail[0:], e.typ)
		binary.LittleEndian.PutUint16(tail[2:], e.code)
		binary.LittleEndian.PutUint32(tail[4:], uint32(e.val))
		b.Write(tail[:])
	}
	return b.Bytes()
}

func press(code uint16) []ev {
	return []ev{{1, code, 1}, {0, 0, 0}, {1, code, 0}, {0, 0, 0}}
}

func TestEventSizeMatchesHost(t *testing.T) {
	if EventSize != 16 && EventSize != 24 {
		t.Fatalf("EventSize = %d", EventSize)
	}
}

func TestKeyScannerLayouts(t *testing.T) {
	for _, size := range []int{16, 24} {
		var evs []ev
		for _, c := range []uint16{2, 3, 4} { // 1 2 3
			evs = append(evs, press(c)...)
		}
		evs = append(evs, ev{1, 30, 1}, ev{1, 30, 2}, ev{1, 30, 0}) // a, repeat ignored
		evs = append(evs, ev{4, 4, 458792})                         // EV_MSC ignored
		evs = append(evs, press(28)...)
		ks, err := NewKeyScanner(bytes.NewReader(encodeEvents(size, evs)), size, 0)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ks.Next()
		if err != nil || s != "123a" {
			t.Fatalf("size %d: %q %v", size, s, err)
		}
		if _, err := ks.Next(); err != io.EOF && err != io.ErrUnexpectedEOF {
			t.Fatalf("want EOF, got %v", err)
		}
	}
	if _, err := NewKeyScanner(nil, 20, 0); err == nil {
		t.Fatal("bad size must fail")
	}
}

func TestKeyScannerShiftAndCaps(t *testing.T) {
	var evs []ev
	// shift down, a, 1, shift up -> "A!"
	evs = append(evs, ev{1, 42, 1}, ev{1, 30, 1}, ev{1, 30, 0}, ev{1, 2, 1}, ev{1, 2, 0}, ev{1, 42, 0})
	// plain b
	evs = append(evs, press(48)...)
	// caps on: c -> C, digit 2 stays 2, shift+d -> d (caps xor shift), caps off
	evs = append(evs, press(58)...)
	evs = append(evs, press(46)...)
	evs = append(evs, press(3)...)
	evs = append(evs, ev{1, 54, 1}, ev{1, 32, 1}, ev{1, 32, 0}, ev{1, 54, 0})
	evs = append(evs, press(58)...)
	// symbols: minus, shifted minus (underscore), slash, keypad 5, keypad dot, space
	evs = append(evs, press(12)...)
	evs = append(evs, ev{1, 42, 1}, ev{1, 12, 1}, ev{1, 12, 0}, ev{1, 42, 0})
	evs = append(evs, press(53)...)
	evs = append(evs, press(76)...)
	evs = append(evs, press(83)...)
	evs = append(evs, press(57)...)
	evs = append(evs, press(96)...) // keypad enter
	ks, _ := NewKeyScanner(bytes.NewReader(encodeEvents(24, evs)), 24, 0)
	s, err := ks.Next()
	if err != nil || s != "A!bC2d-_/5. " {
		t.Fatalf("%q %v", s, err)
	}
}

func TestKeyScannerTooLongAndEmpty(t *testing.T) {
	var evs []ev
	evs = append(evs, press(28)...) // empty scan skipped
	for i := 0; i < 10; i++ {
		evs = append(evs, press(2)...)
	}
	evs = append(evs, press(28)...)
	evs = append(evs, press(3)...)
	evs = append(evs, press(28)...)
	ks, _ := NewKeyScanner(bytes.NewReader(encodeEvents(16, evs)), 16, 5)
	if _, err := ks.Next(); !errors.Is(err, ErrScanTooLong) {
		t.Fatalf("want too long, got %v", err)
	}
	if s, err := ks.Next(); err != nil || s != "2" {
		t.Fatal(s, err)
	}
}

// ---- policy and dialer ----

func TestPolicyCIDR(t *testing.T) {
	p := DefaultPolicy()
	allow := []string{"10.1.2.3", "172.16.0.1", "172.31.255.254", "192.168.1.50", "127.0.0.1", "::1", "::ffff:192.168.1.5"}
	deny := []string{"8.8.8.8", "172.32.0.1", "169.254.169.254", "169.254.1.1", "fe80::1", "0.0.0.0", "224.0.0.1", "100.64.0.1", "2001:db8::1", "::ffff:169.254.169.254"}
	for _, s := range allow {
		if err := p.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s should be allowed: %v", s, err)
		}
	}
	for _, s := range deny {
		if err := p.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s should be denied, got %v", s, err)
		}
	}
	// a custom range never opens link-local
	c, err := NewPolicy([]string{"169.254.0.0/16", "100.64.0.0/10", "10.9.9.9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.CheckIP(netip.MustParseAddr("169.254.169.254")) == nil {
		t.Error("metadata address must stay denied")
	}
	if c.CheckIP(netip.MustParseAddr("100.64.5.5")) != nil || c.CheckIP(netip.MustParseAddr("10.9.9.9")) != nil {
		t.Error("custom ranges must work")
	}
	if c.CheckIP(netip.MustParseAddr("10.9.9.8")) == nil {
		t.Error("custom list replaces the defaults")
	}
	if _, err := NewPolicy([]string{"nonsense"}, nil); err == nil {
		t.Error("bad cidr must fail")
	}
}

func TestPolicyFiles(t *testing.T) {
	p := DefaultPolicy()
	for _, ok := range []string{"/dev/usb/lp0", "/dev/ttyUSB0", "/dev/lp0", "/dev/ttyACM1"} {
		if err := p.CheckFile(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/etc/passwd", "/dev/sda", "/dev/usb/../sda", "dev/usb/lp0", "/dev/usb//lp0", "/dev/null", ""} {
		if err := p.CheckFile(bad); !errors.Is(err, ErrDenied) {
			t.Errorf("%q must be denied, got %v", bad, err)
		}
	}
}

type fakeRes map[string][]netip.Addr

func (f fakeRes) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestDialTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d := &Dialer{Resolver: fakeRes{
		"printer.lan": {netip.MustParseAddr("127.0.0.1")},
		"evil.lan":    {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254")},
		"public.lan":  {netip.MustParseAddr("8.8.8.8")},
	}}
	c, err := d.Open(context.Background(), "tcp://printer.lan:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("hello"))
	c.Close()
	select {
	case b := <-got:
		if string(b) != "hello" {
			t.Fatalf("server got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	for _, target := range []string{"evil.lan:9100", "public.lan:9100", "169.254.169.254:80", "8.8.8.8:9100", "[fe80::1]:9100", "unknown.lan:9100"} {
		if _, err := d.Open(context.Background(), target); err == nil {
			t.Errorf("%s must fail", target)
		}
	}
	if _, err := d.Open(context.Background(), "nohostport"); err == nil {
		t.Error("missing port must fail")
	}
}

func TestDialTimeout(t *testing.T) {
	// 10.255.255.1 is usually unroutable: the dial must give up at DialTimeout
	d := &Dialer{Policy: &Policy{DialTimeout: 150 * time.Millisecond}}
	d.Policy, _ = NewPolicy(nil, nil)
	d.Policy.DialTimeout = 150 * time.Millisecond
	start := time.Now()
	c, err := d.Open(context.Background(), "10.255.255.1:9100")
	if err == nil {
		c.Close()
		t.Skip("10.255.255.1 is reachable here")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("dial took %v", time.Since(start))
	}
}

func TestOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lp0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Dialer{Policy: &Policy{FilePrefixes: []string{dir + "/"}}}
	c, err := d.Open(context.Background(), "file://"+path)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("abc"))
	c.Close()
	if b, _ := os.ReadFile(path); string(b) != "abc" {
		t.Fatalf("file has %q", b)
	}
	if _, err := d.Open(context.Background(), "/etc/hosts"); !errors.Is(err, ErrDenied) {
		t.Fatalf("outside prefix: %v", err)
	}
	if _, err := d.Open(context.Background(), path+"-missing"); err == nil {
		t.Fatal("a missing device must not be created")
	}
	if _, err := os.Stat(path + "-missing"); err == nil {
		t.Fatal("file was created")
	}
}
