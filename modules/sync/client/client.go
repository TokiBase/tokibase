//go:build !no_sync

// Package client is the spoke side transport of sync: enrollment, the signed
// handshake (with clock offset measurement) and authenticated requests to the
// hub. PR2 is a skeleton: there is no push/pull loop yet.
package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Env of the client (docs/SYNC_DESIGN.md §8.3).
const (
	EnvHubURL   = "TOKI_SYNC_HUB_URL"
	EnvHubPin   = "TOKI_SYNC_HUB_PIN"
	EnvInsecure = "TOKI_SYNC_INSECURE"

	// RequestTimeout is the timeout of every request.
	RequestTimeout = 30 * time.Second
)

// Error is a hub answer with a non-2xx status.
type Error struct {
	Status  int
	Code    string
	Message string
	Data    map[string]any
}

func (e *Error) Error() string {
	return fmt.Sprintf("sync hub: %d %s: %s", e.Status, e.Code, e.Message)
}

// IsCode reports whether err is a hub error with the code.
func IsCode(err error, code string) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == code
}

// Options configures a Client.
type Options struct {
	// App holds `_sync_cursors`. When set and HubURL/Cert are empty they are
	// read from the first cursor row.
	App core.App
	// Identity is the node key (required).
	Identity *proto.Identity
	HubURL   string
	Cert     string
	// Clock receives the measured offset (optional).
	Clock *hlc.Clock
	// Now is the raw local wall clock (default time.Now). Tests inject it.
	Now func() time.Time
	// HTTP overrides the transport (tests); the https and pin rules still apply
	// to the URL.
	HTTP *http.Client
	// Insecure allows plain http (also TOKI_SYNC_INSECURE=1).
	Insecure bool
	// Pin is the SPKI sha256 of the hub certificate, hex or base64
	// (also TOKI_SYNC_HUB_PIN).
	Pin        string
	Profile    string
	AppVersion string
}

// Client talks to one hub.
type Client struct {
	o      Options
	base   string
	http   *http.Client
	now    func() time.Time
	nodeID string

	mu       stdsync.Mutex
	token    string
	tokenExp time.Time
	hubID    string
	epoch    string
	offset   time.Duration
}

func envTrue(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// checkURL enforces https unless insecure and returns the base URL.
func checkURL(raw string, insecure bool, pin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("sync: invalid hub url %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !insecure {
			return "", errors.New("sync: the hub url must be https (TOKI_SYNC_INSECURE=1 allows http for tests)")
		}
		if pin != "" {
			return "", errors.New("sync: a hub pin needs an https hub url")
		}
	default:
		return "", fmt.Errorf("sync: unsupported hub url scheme %q", u.Scheme)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// parsePin accepts a sha256 as hex (64 chars) or base64.
func parsePin(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "sha256/"))
	if b, err := hex.DecodeString(s); err == nil && len(b) == sha256.Size {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == sha256.Size {
			return b, nil
		}
	}
	return nil, errors.New("sync: TOKI_SYNC_HUB_PIN must be the sha256 of the hub certificate public key (hex or base64)")
}

func newHTTP(pin string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if pin != "" {
		want, err := parsePin(pin)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// the normal chain verification stays on; the pin is an addition
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("sync: no hub certificate")
				}
				c, err := x509.ParseCertificate(raw[0])
				if err != nil {
					return err
				}
				got := sha256.Sum256(c.RawSubjectPublicKeyInfo)
				if !bytes.Equal(got[:], want) {
					return errors.New("sync: hub certificate does not match TOKI_SYNC_HUB_PIN")
				}
				return nil
			},
		}
	}
	return &http.Client{Timeout: RequestTimeout, Transport: tr}, nil
}

// New builds a client. The hub URL must be https unless TOKI_SYNC_INSECURE=1
// (or Options.Insecure).
func New(o Options) (*Client, error) {
	if o.Identity == nil {
		return nil, errors.New("sync: client needs a node identity")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.App != nil && (o.HubURL == "" || o.Cert == "") {
		cur, err := LoadCursor(o.App)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, errors.New("sync: this node is not enrolled (run `toki sync join <hub-url> <code>`)")
		}
		if o.HubURL == "" {
			o.HubURL = cur.HubURL
		}
		if o.Cert == "" {
			o.Cert = cur.Cert
		}
	}
	if o.HubURL == "" {
		o.HubURL = os.Getenv(EnvHubURL)
	}
	if o.Pin == "" {
		o.Pin = os.Getenv(EnvHubPin)
	}
	o.Insecure = o.Insecure || envTrue(EnvInsecure)
	base, err := checkURL(o.HubURL, o.Insecure, strings.TrimSpace(o.Pin))
	if err != nil {
		return nil, err
	}
	hc := o.HTTP
	if hc == nil {
		if hc, err = newHTTP(strings.TrimSpace(o.Pin)); err != nil {
			return nil, err
		}
	}
	return &Client{o: o, base: base, http: hc, now: o.Now, nodeID: o.Identity.NodeID()}, nil
}

func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode/100 != 2 {
		he := &Error{Status: res.StatusCode}
		var eb proto.ErrorBody
		if json.Unmarshal(b, &eb) == nil {
			he.Message, he.Data = eb.Message, eb.Data
			he.Code, _ = eb.Data["code"].(string)
		}
		return res, b, he
	}
	return res, b, nil
}

// EnrollParams are the inputs of Enroll.
type EnrollParams struct {
	HubURL     string
	Code       string
	Identity   *proto.Identity
	Profile    string
	AppVersion string
	// Insecure, Pin and HTTP as in Options.
	Insecure bool
	Pin      string
	HTTP     *http.Client
}

// Enroll exchanges a one-time code for a device certificate and checks the
// answer: the node id must be derived from our key, the hub id from the hub
// key, and the certificate must verify against that key.
func Enroll(ctx context.Context, p EnrollParams) (*proto.EnrollResponse, error) {
	c, err := New(Options{Identity: p.Identity, HubURL: p.HubURL, Insecure: p.Insecure, Pin: p.Pin, HTTP: p.HTTP})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(proto.EnrollRequest{
		Code: p.Code, Ed25519Pub: base64.StdEncoding.EncodeToString(p.Identity.Pub()),
		X25519Pub: base64.StdEncoding.EncodeToString(p.Identity.KX()), Profile: p.Profile, AppVersion: p.AppVersion,
	})
	_, b, err := c.do(ctx, http.MethodPost, proto.PathEnroll, nil, body)
	if err != nil {
		return nil, err
	}
	var out proto.EnrollResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("sync: invalid enroll answer: %w", err)
	}
	hubPub, err := base64.StdEncoding.DecodeString(out.HubPub)
	if err != nil || len(hubPub) != ed25519.PublicKeySize {
		return nil, errors.New("sync: invalid hub key in the enroll answer")
	}
	if out.NodeID != p.Identity.NodeID() {
		return nil, errors.New("sync: the hub returned a node id that does not match our key")
	}
	if out.HubID != proto.HubID(hubPub) {
		return nil, errors.New("sync: the hub id does not match the hub key")
	}
	cl, err := proto.VerifyCert(hubPub, out.Cert, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sync: invalid device certificate: %w", err)
	}
	if cl.Subject != out.NodeID || cl.Pub != base64.StdEncoding.EncodeToString(p.Identity.Pub()) {
		return nil, errors.New("sync: the certificate is not for our key")
	}
	return &out, nil
}

// Join enrolls and stores the result in `_sync_cursors`.
func Join(ctx context.Context, app core.App, p EnrollParams) (*proto.EnrollResponse, error) {
	res, err := Enroll(ctx, p)
	if err != nil {
		return nil, err
	}
	base, err := checkURL(p.HubURL, p.Insecure || envTrue(EnvInsecure), "")
	if err != nil {
		return nil, err
	}
	if err := StoreEnrollment(app, base, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Handshake runs the signed handshake, measures the clock offset
// (server_time - (t_send+t_recv)/2), applies it to the HLC clock, stores it in
// `_sync_cursors` and keeps the session token for later requests. When the hub
// reports a timestamp outside its window, the hub time it returns is used once
// to correct the offset and the handshake is retried.
func (c *Client) Handshake(ctx context.Context) (*proto.HandshakeResponse, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, err := c.handshakeOnce(ctx)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var he *Error
		if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusUnauthorized {
			if st, _ := he.Data["server_time"].(string); st != "" {
				if t, perr := time.Parse(time.RFC3339Nano, st); perr == nil {
					c.setOffset(t.Sub(c.now()))
					continue
				}
			}
		}
		break
	}
	c.recordError(lastErr)
	return nil, lastErr
}

func (c *Client) setOffset(d time.Duration) {
	if c.o.Clock != nil {
		c.o.Clock.SetOffset(d)
	}
	c.mu.Lock()
	c.offset = d
	c.mu.Unlock()
}

func (c *Client) wallNow() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Add(c.offset)
}

func (c *Client) handshakeOnce(ctx context.Context) (*proto.HandshakeResponse, error) {
	req := proto.HandshakeRequest{
		NodeID: c.nodeID, Cert: c.o.Cert, Profile: c.o.Profile, AppVersion: c.o.AppVersion,
		Caps: []string{"gzip"},
	}
	if c.o.App != nil {
		if cur, _ := LoadCursor(c.o.App); cur != nil {
			req.SchemaVersion, req.PullAfter, req.AckedOrigin, req.HubEpoch = cur.SchemaVersion, cur.PullAfter, cur.AckedOrigin, cur.HubEpoch
		}
		var next int64
		if err := c.o.App.DB().NewQuery("SELECT COALESCE(MAX(origin_seq),0)+1 FROM _changes WHERE node={:n}").
			Bind(dbx.Params{"n": c.nodeID}).Row(&next); err == nil {
			req.NextOrigin = next
		}
	}
	tSend := c.now()
	req.ClientTime = c.wallNow().UTC().Format(proto.TimeLayout)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(c.wallNow().UnixMilli(), 10)
	nonce := proto.NewNonce()
	hdr := map[string]string{
		proto.HeaderNode: c.nodeID, proto.HeaderSigTs: ts, proto.HeaderNonce: nonce,
		proto.HeaderSig: proto.SignRequest(c.o.Identity.Ed, http.MethodPost, proto.PathHandshake, ts, nonce, body),
	}
	_, b, err := c.do(ctx, http.MethodPost, proto.PathHandshake, hdr, body)
	tRecv := c.now()
	if err != nil {
		return nil, err
	}
	var out proto.HandshakeResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("sync: invalid handshake answer: %w", err)
	}
	st, err := time.Parse(time.RFC3339Nano, out.ServerTime)
	if err != nil {
		return nil, fmt.Errorf("sync: invalid server_time: %w", err)
	}
	offset := st.Sub(tSend.Add(tRecv.Sub(tSend) / 2))
	c.setOffset(offset)
	exp, _ := time.Parse(time.RFC3339Nano, out.Expires)
	c.mu.Lock()
	c.token, c.tokenExp, c.hubID, c.epoch = out.SessionToken, exp, out.HubID, out.HubEpoch
	c.mu.Unlock()
	c.recordOK(out.HubID, out.HubEpoch, offset)
	return &out, nil
}

// Ping calls GET /api/sync/ping with the session token. It runs the handshake
// first when there is no valid token, and once more when the hub answers 401.
func (c *Client) Ping(ctx context.Context) (*proto.PingResponse, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		tok, valid := c.token, c.token != "" && c.now().Add(c.offsetLocked()).Add(30*time.Second).Before(c.tokenExp)
		c.mu.Unlock()
		if !valid {
			if _, err := c.Handshake(ctx); err != nil {
				return nil, err
			}
			c.mu.Lock()
			tok = c.token
			c.mu.Unlock()
		}
		_, b, err := c.do(ctx, http.MethodGet, proto.PathPing, map[string]string{"Authorization": "Bearer " + tok}, nil)
		if err != nil {
			if attempt == 0 && IsCode(err, proto.CodeUnauthorized) {
				c.mu.Lock()
				c.token = ""
				c.mu.Unlock()
				continue
			}
			return nil, err
		}
		var out proto.PingResponse
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, errors.New("sync: ping failed")
}

// Offset returns the last measured clock offset.
func (c *Client) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

func (c *Client) offsetLocked() time.Duration { return c.offset }

// Token returns the current session token ("" before a handshake).
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}
