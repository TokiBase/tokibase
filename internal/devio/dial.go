package devio

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Policy decides which addresses and device files an operator-configured
// target may reach. The target always comes from configuration (a superuser
// owned row), never from request input; the policy is a second fence against
// misconfiguration and DNS tricks, and the opposite of internal/netguard, which
// forbids private addresses.
type Policy struct {
	// Allow lists the CIDRs a target may resolve to.
	Allow []netip.Prefix
	// FilePrefixes lists the path prefixes of device files that may be opened.
	FilePrefixes []string
	// DialTimeout bounds the connection attempt (default 3 s).
	DialTimeout time.Duration
	// IOTimeout is the deadline set before every Write and Read of a dialed
	// connection (default 5 s).
	IOTimeout time.Duration
}

// DefaultAllow is RFC 1918 plus loopback.
var DefaultAllow = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "::1/128"}

// DefaultFilePrefixes are the device files of USB printers and serial adapters.
var DefaultFilePrefixes = []string{"/dev/usb/", "/dev/lp", "/dev/ttyUSB", "/dev/ttyACM", "/dev/ttyS", "/dev/ttyAMA"}

// alwaysDeny wins over Allow: link-local (which includes the cloud metadata
// address 169.254.169.254), unspecified, multicast and the "this network" range.
var alwaysDeny = mustPrefixes("169.254.0.0/16", "fe80::/10", "0.0.0.0/8", "224.0.0.0/4", "ff00::/8", "::/128")

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, v := range s {
		out[i] = netip.MustParsePrefix(v)
	}
	return out
}

// DefaultPolicy is the policy used when a config names no CIDRs.
func DefaultPolicy() *Policy {
	p, _ := NewPolicy(nil, nil)
	return p
}

// NewPolicy builds a policy. Empty cidrs/prefixes select the defaults. A bare
// IP is accepted as a /32 or /128.
func NewPolicy(cidrs, filePrefixes []string) (*Policy, error) {
	if len(cidrs) == 0 {
		cidrs = DefaultAllow
	}
	if len(filePrefixes) == 0 {
		filePrefixes = DefaultFilePrefixes
	}
	p := &Policy{FilePrefixes: filePrefixes}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		pf, err := netip.ParsePrefix(c)
		if err != nil {
			a, aerr := netip.ParseAddr(c)
			if aerr != nil {
				return nil, fmt.Errorf("devio: bad cidr %q", c)
			}
			pf = netip.PrefixFrom(a, a.BitLen())
		}
		p.Allow = append(p.Allow, pf.Masked())
	}
	return p, nil
}

// ErrDenied is wrapped by every policy refusal.
var ErrDenied = errors.New("devio: target not allowed by policy")

// CheckIP reports whether a may be dialed.
func (p *Policy) CheckIP(a netip.Addr) error {
	a = a.Unmap()
	for _, d := range alwaysDeny {
		if d.Contains(a) {
			return fmt.Errorf("%w: %s is in %s", ErrDenied, a, d)
		}
	}
	for _, al := range p.Allow {
		if al.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside the allowed ranges", ErrDenied, a)
}

// CheckFile reports whether path may be opened.
func (p *Policy) CheckFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%w: %q is not a clean absolute path", ErrDenied, path)
	}
	for _, pre := range p.FilePrefixes {
		if strings.HasPrefix(path, pre) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not under an allowed device prefix", ErrDenied, path)
}

func (p *Policy) dialTimeout() time.Duration {
	if p.DialTimeout > 0 {
		return p.DialTimeout
	}
	return 3 * time.Second
}

func (p *Policy) ioTimeout() time.Duration {
	if p.IOTimeout > 0 {
		return p.IOTimeout
	}
	return 5 * time.Second
}

// Resolver looks up host names; tests replace it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer opens configured targets under a [Policy].
type Dialer struct {
	Policy   *Policy
	Resolver Resolver // default net.DefaultResolver
}

// Open opens target for writing (and reading when it is a TCP connection):
//
//	tcp://host:port or host:port   TCP socket (raw 9100 printers)
//	file:///dev/usb/lp0 or /dev/usb/lp0   device file
func (d *Dialer) Open(ctx context.Context, target string) (Conn, error) {
	pol := d.Policy
	if pol == nil {
		pol = DefaultPolicy()
	}
	switch {
	case strings.HasPrefix(target, "file://"):
		return d.openFile(pol, strings.TrimPrefix(target, "file://"))
	case strings.HasPrefix(target, "/"):
		return d.openFile(pol, target)
	}
	return d.dialTCP(ctx, pol, strings.TrimPrefix(target, "tcp://"))
}

// Conn is an opened target.
type Conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

func (d *Dialer) openFile(pol *Policy, path string) (Conn, error) {
	if err := pol.CheckFile(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_NONBLOCK, 0)
	if err != nil { // many printers are write-only
		f, err = openFileWrite(path)
		if err != nil {
			return nil, err
		}
	}
	return &fileConn{File: f, timeout: pol.ioTimeout()}, nil
}

type fileConn struct {
	*os.File
	timeout time.Duration
}

func (c *fileConn) Write(p []byte) (int, error) {
	_ = c.File.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.File.Write(p)
}

func (d *Dialer) dialTCP(ctx context.Context, pol *Policy, hostport string) (Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("devio: bad address %q: %w", hostport, err)
	}
	var addrs []netip.Addr
	if a, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{a}
	} else {
		res := d.Resolver
		if res == nil {
			res = net.DefaultResolver
		}
		rctx, cancel := context.WithTimeout(ctx, pol.dialTimeout())
		defer cancel()
		if addrs, err = res.LookupNetIP(rctx, "ip", host); err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("devio: %s does not resolve", host)
		}
	}
	// every answer must pass, then the connection goes to the checked address
	// itself (no second lookup, so DNS rebinding cannot swap it)
	for _, a := range addrs {
		if err := pol.CheckIP(a); err != nil {
			return nil, err
		}
	}
	nd := net.Dialer{Timeout: pol.dialTimeout()}
	var last error
	for _, a := range addrs {
		c, err := nd.DialContext(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return &tcpConn{Conn: c, timeout: pol.ioTimeout()}, nil
		}
		last = err
	}
	return nil, last
}

type tcpConn struct {
	net.Conn
	timeout time.Duration
}

func (c *tcpConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

func (c *tcpConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}
