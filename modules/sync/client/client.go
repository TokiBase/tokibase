//go:build !no_sync

// Package client is the spoke side transport of sync: enrollment, the signed
// handshake (with clock offset measurement) and authenticated requests to the
// hub. PR2 is a skeleton: there is no push/pull loop yet.
package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// MaxClockOffset bounds the clock offset the client accepts from the hub
	// (also from an authentic answer).
	MaxClockOffset = 7 * 24 * time.Hour
)

// Error is a hub answer with a non-2xx status.
type Error struct {
	Status  int
	Code    string
	Message string
	Data    map[string]any
	// RetryAfter is the parsed Retry-After header (0 when absent).
	RetryAfter time.Duration
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
	// HubID and HubPub (base64) are the hub identity stored at enrollment
	// (read from the cursor when App is set). The handshake signature is bound
	// to HubID and a hub time is only trusted when HubPub signed it.
	HubID  string
	HubPub string
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

	// The fields below configure the sync loop (loop.go).

	// Backend connects the loop to the policies and the hashing of the sync
	// module (required to apply pulled changes).
	Backend Backend
	// Interval is the idle sync interval (default TOKI_SYNC_INTERVAL or 30 s).
	Interval time.Duration
	// Page is the number of changes per push/pull page (default 500).
	Page int
	// Debounce delays the sync after a local write (default 2 s).
	Debounce time.Duration
	// Rand returns a value in [0,1) for the backoff jitter (default math/rand).
	Rand func() float64
	// NoPoke disables the realtime "@sync" subscription (also TOKI_SYNC_POKE=0).
	NoPoke bool
	// Logger receives loop diagnostics (optional).
	Logger *slog.Logger

	// NoAutoBootstrap stops the loop with ErrRebootstrap instead of running the
	// snapshot bootstrap itself (the state stays rebootstrap_required).
	NoAutoBootstrap bool
	// AutoHeal re-bootstraps after two digest or hash mismatches in a row
	// (also TOKI_SYNC_AUTO_HEAL=1).
	AutoHeal bool
	// Retention is the age after which an unpushed local change is orphaned by a
	// bootstrap (default TOKI_SYNC_RETENTION or 90 days).
	Retention time.Duration
	// DigestInterval is the minimum time between two digest checks of the
	// auto-heal (default TOKI_SYNC_DIGEST_INTERVAL or 10 minutes).
	DigestInterval time.Duration
}

// Client talks to one hub.
type Client struct {
	o       Options
	base    string
	http    *http.Client
	now     func() time.Time
	nodeID  string
	host    string // NormalizeHost of the hub host, part of the signed string
	wantHub string
	hubPub  ed25519.PublicKey

	loop loopState

	mu       stdsync.Mutex
	token    string
	tokenExp time.Time
	hubID    string
	epoch    string
	cert     string
	offset   time.Duration
}

func envTrue(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
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
	if o.App != nil && (o.HubURL == "" || o.Cert == "" || o.HubID == "" || o.HubPub == "") {
		cur, err := LoadCursor(o.App)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			if o.HubURL != "" && o.Cert != "" {
				cur = &Cursor{}
			} else {
				return nil, errors.New("sync: this node is not enrolled (run `toki sync join <hub-url> <code>`)")
			}
		}
		if o.HubURL == "" {
			o.HubURL = cur.HubURL
		}
		if o.Cert == "" {
			o.Cert = cur.Cert
		}
		if o.HubID == "" {
			o.HubID = cur.HubID
		}
		if o.HubPub == "" {
			o.HubPub = cur.HubPub
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
	} else {
		hc = noRedirect(hc)
	}
	u, _ := url.Parse(base)
	c := &Client{o: o, base: base, http: hc, now: o.Now, nodeID: o.Identity.NodeID(), host: proto.NormalizeHost(u.Host), wantHub: o.HubID, cert: o.Cert}
	if b, err := base64.StdEncoding.DecodeString(o.HubPub); err == nil && len(b) == ed25519.PublicKeySize {
		c.hubPub = b
	}
	if o.App != nil {
		// start from the offset measured last time (a skewed device would take the correction path after every restart)
		if cur, _ := LoadCursor(o.App); cur != nil && cur.ClockOffsetMs != 0 {
			if d := time.Duration(cur.ClockOffsetMs) * time.Millisecond; d <= MaxClockOffset && d >= -MaxClockOffset {
				c.offset = d
				if o.Clock != nil {
					o.Clock.SetOffset(d)
				}
			}
		}
	}
	c.initLoop()
	return c, nil
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
// `_sync_cursors` and keeps the session token for later requests.
//
// When the hub reports a timestamp outside its window, the hub time of that 401
// is used once to correct the offset and the handshake is retried, but only
// when the answer carries the hub signature over its time (verified with the
// stored hub key) and the offset is within MaxClockOffset. If the retry fails,
// the previous offset is restored.
func (c *Client) Handshake(ctx context.Context) (*proto.HandshakeResponse, error) {
	prev := c.Offset()
	corrected := false
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, hubTime, err := c.handshakeOnce(ctx)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var he *Error
		if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusUnauthorized && !hubTime.IsZero() {
			d := hubTime.Sub(c.now())
			if d > MaxClockOffset || d < -MaxClockOffset {
				lastErr = fmt.Errorf("sync: the hub asks for a clock correction of %v, more than %v: refused", d, MaxClockOffset)
				break
			}
			c.setOffset(d)
			corrected = true
			continue
		}
		break
	}
	if corrected {
		c.setOffset(prev)
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

// verifyHubTime checks the signed hub time headers of an answer to the request
// (ts, nonce). It returns the hub time when the signature is valid.
func (c *Client) verifyHubTime(h http.Header, ts, nonce string) (time.Time, bool) {
	st, sig := h.Get(proto.HeaderServerTime), h.Get(proto.HeaderServerSig)
	if st == "" || sig == "" || c.hubPub == nil {
		return time.Time{}, false
	}
	if !proto.VerifyServerTime(c.hubPub, c.nodeID, ts, nonce, st, sig) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, st)
	return t, err == nil
}

// handshakeOnce sends one handshake. On a 401 it also returns the hub time
// when (and only when) the answer proved to come from the enrolled hub.
func (c *Client) handshakeOnce(ctx context.Context) (*proto.HandshakeResponse, time.Time, error) {
	if c.wantHub == "" || c.hubPub == nil {
		return nil, time.Time{}, errors.New("sync: the hub id and key are unknown: enroll first (`toki sync join`)")
	}
	c.mu.Lock()
	cert := c.cert
	c.mu.Unlock()
	req := proto.HandshakeRequest{
		NodeID: c.nodeID, Cert: cert, Profile: c.o.Profile, AppVersion: c.o.AppVersion,
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
		return nil, time.Time{}, err
	}
	ts := strconv.FormatInt(c.wallNow().UnixMilli(), 10)
	nonce := proto.NewNonce()
	hdr := map[string]string{
		proto.HeaderNode: c.nodeID, proto.HeaderSigTs: ts, proto.HeaderNonce: nonce,
		proto.HeaderSig: proto.SignRequest(c.o.Identity.Ed, http.MethodPost, proto.PathHandshake, c.host, c.wantHub, ts, nonce, body),
	}
	res, b, err := c.do(ctx, http.MethodPost, proto.PathHandshake, hdr, body)
	tRecv := c.now()
	if err != nil {
		var hubTime time.Time
		var he *Error
		if res != nil && errors.As(err, &he) && he.Status == http.StatusUnauthorized {
			if t, ok := c.verifyHubTime(res.Header, ts, nonce); ok && he.Data["server_time"] == res.Header.Get(proto.HeaderServerTime) {
				hubTime = t
			}
		}
		return nil, hubTime, err
	}
	// the answer must come from the enrolled hub: signed time and matching hub id
	hubTime, ok := c.verifyHubTime(res.Header, ts, nonce)
	if !ok {
		return nil, time.Time{}, errors.New("sync: the handshake answer is not signed by the enrolled hub")
	}
	var out proto.HandshakeResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, time.Time{}, fmt.Errorf("sync: invalid handshake answer: %w", err)
	}
	if out.HubID != c.wantHub || out.ServerTime != res.Header.Get(proto.HeaderServerTime) {
		return nil, time.Time{}, errors.New("sync: the handshake answer does not match the enrolled hub")
	}
	offset := hubTime.Sub(tSend.Add(tRecv.Sub(tSend) / 2))
	if offset > MaxClockOffset || offset < -MaxClockOffset {
		return nil, time.Time{}, fmt.Errorf("sync: clock offset %v is out of range (max %v)", offset, MaxClockOffset)
	}
	if out.Cert != "" {
		cl, err := proto.VerifyCert(c.hubPub, out.Cert, hubTime)
		if err != nil || cl.Subject != c.nodeID || cl.Pub != base64.StdEncoding.EncodeToString(c.o.Identity.Pub()) {
			return nil, time.Time{}, errors.New("sync: the renewed certificate is invalid")
		}
		c.mu.Lock()
		c.cert = out.Cert
		c.mu.Unlock()
		c.storeCert(out.HubID, out.Cert)
	}
	c.setOffset(offset)
	exp, _ := time.Parse(time.RFC3339Nano, out.Expires)
	c.mu.Lock()
	c.token, c.tokenExp, c.hubID, c.epoch = out.SessionToken, exp, out.HubID, out.HubEpoch
	c.mu.Unlock()
	c.recordOK(out.HubID, out.HubEpoch, offset)
	return &out, hubTime, nil
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
