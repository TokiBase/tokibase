//go:build !no_push

package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apnsProd       = "https://api.push.apple.com"
	apnsSandbox    = "https://api.sandbox.push.apple.com"
	apnsJWTTTL     = 50 * time.Minute // Apple accepts 20-60 minutes
	apnsMinRefresh = 20 * time.Minute
)

// APNsConfig holds the token based auth settings.
type APNsConfig struct {
	KeyPEM  []byte
	KeyID   string
	TeamID  string
	Topic   string // bundle id
	Sandbox bool
}

// APNs is the Apple Push Notification service provider (HTTP/2, JWT auth).
type APNs struct {
	cfg APNsConfig
	key *ecdsa.PrivateKey

	BaseURL string
	Client  *http.Client // Go negotiates HTTP/2 via ALPN against Apple
	Now     func() time.Time

	mu    sync.Mutex
	jwt   string
	jwtAt time.Time
}

// NewAPNs validates cfg and builds the provider.
func NewAPNs(cfg APNsConfig) (*APNs, error) {
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.Topic == "" {
		return nil, errors.New("apns: TOKI_PUSH_APNS_KEY_ID, TOKI_PUSH_APNS_TEAM_ID and TOKI_PUSH_APNS_TOPIC are required")
	}
	k, err := parsePEMKey(cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("apns: key: %w", err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: key is not an EC key (.p8 from Apple is ES256)")
	}
	base := apnsProd
	if cfg.Sandbox {
		base = apnsSandbox
	}
	return &APNs{cfg: cfg, key: ek, BaseURL: base, Client: newHTTPClient(), Now: time.Now}, nil
}

func (a *APNs) Name() string { return PlatformAPNs }

func (a *APNs) providerToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	if a.jwt != "" && now.Sub(a.jwtAt) < apnsJWTTTL {
		return a.jwt, nil
	}
	t, err := signES256(a.key,
		map[string]string{"alg": "ES256", "kid": a.cfg.KeyID},
		map[string]any{"iss": a.cfg.TeamID, "iat": now.Unix()})
	if err != nil {
		return "", permanent("apns: sign provider token: %v", err)
	}
	a.jwt, a.jwtAt = t, now
	return t, nil
}

// dropToken forgets the cached provider token so the next request signs a
// new one. Apple answers TooManyProviderTokenUpdates when the token is
// refreshed more often than every 20 minutes, so a token younger than that is kept.
func (a *APNs) dropToken() {
	a.mu.Lock()
	if a.Now().Sub(a.jwtAt) >= apnsMinRefresh {
		a.jwt = ""
	}
	a.mu.Unlock()
}

// apnsPayload maps a Notification to the APNs JSON body: custom data keys sit
// at the top level next to "aps" (the key "aps" itself is reserved).
func apnsPayload(n *Notification) ([]byte, error) {
	aps := map[string]any{"sound": "default"}
	if n.Title != "" || n.Body != "" {
		alert := map[string]string{}
		if n.Title != "" {
			alert["title"] = n.Title
		}
		if n.Body != "" {
			alert["body"] = n.Body
		}
		aps["alert"] = alert
	}
	out := map[string]any{}
	for k, v := range n.Data {
		if k != "aps" {
			out[k] = v
		}
	}
	out["aps"] = aps
	return json.Marshal(out)
}

// BuildRequest builds the HTTP request for one device token.
func (a *APNs) BuildRequest(ctx context.Context, n *Notification, token string) (*http.Request, error) {
	body, err := apnsPayload(n)
	if err != nil {
		return nil, permanent("apns: encode: %v", err)
	}
	jwt, err := a.providerToken()
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(a.BaseURL, "/") + "/3/device/" + url.PathEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, permanent("apns: request: %v", err)
	}
	req.Header.Set("Authorization", "bearer "+jwt)
	topic := a.cfg.Topic
	if n.Topic != "" {
		topic = n.Topic // per-device app_id
	}
	req.Header.Set("apns-topic", topic)
	req.Header.Set("apns-push-type", "alert")
	prio := "10"
	if n.Priority == PriorityNormal {
		prio = "5"
	}
	req.Header.Set("apns-priority", prio)
	if n.TTLSeconds > 0 {
		req.Header.Set("apns-expiration", strconv.FormatInt(a.Now().Add(time.Duration(n.TTLSeconds)*time.Second).Unix(), 10))
	}
	if n.CollapseKey != "" {
		ck := n.CollapseKey
		if len(ck) > 64 {
			ck = ck[:64]
		}
		req.Header.Set("apns-collapse-id", ck)
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (a *APNs) Send(ctx context.Context, n *Notification, token string) error {
	req, err := a.BuildRequest(ctx, n, token)
	if err != nil {
		return err
	}
	res, err := a.Client.Do(req)
	if err != nil {
		// *url.Error embeds the request URL, which contains the device token
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("apns: request failed: %w", ue.Err)
		}
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return classifyAPNs(a, res.StatusCode, body)
}

func classifyAPNs(a *APNs, status int, body []byte) error {
	if status == 200 {
		return nil
	}
	var e struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &e)
	msg := fmt.Sprintf("apns: HTTP %d %s", status, e.Reason)
	switch {
	case status == 410 || e.Reason == "BadDeviceToken" || e.Reason == "Unregistered":
		return fmt.Errorf("%w (%s)", ErrInvalidToken, msg)
	case e.Reason == "DeviceTokenNotForTopic" || e.Reason == "BadTopic" || e.Reason == "MissingTopic" || e.Reason == "TopicDisallowed" ||
		e.Reason == "InvalidProviderToken" || e.Reason == "BadCertificate" || e.Reason == "BadCertificateEnvironment" || e.Reason == "Forbidden":
		if e.Reason == "InvalidProviderToken" {
			a.dropToken()
		}
		return &ConfigError{Err: errors.New(msg)}
	case status == 403 && e.Reason == "ExpiredProviderToken":
		a.dropToken()
		return &RetryableError{Err: errors.New(msg)}
	case status == 429 || status >= 500:
		return &RetryableError{Err: errors.New(msg)}
	}
	return &PermanentError{Err: errors.New(msg)}
}

func loadAPNsFromEnv() (*APNs, error) {
	p := strings.TrimSpace(os.Getenv("TOKI_PUSH_APNS_KEY_FILE"))
	if p == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("TOKI_PUSH_APNS_KEY_FILE: %w", err)
	}
	sb := strings.TrimSpace(os.Getenv("TOKI_PUSH_APNS_SANDBOX"))
	return NewAPNs(APNsConfig{
		KeyPEM: raw, KeyID: strings.TrimSpace(os.Getenv("TOKI_PUSH_APNS_KEY_ID")),
		TeamID:  strings.TrimSpace(os.Getenv("TOKI_PUSH_APNS_TEAM_ID")),
		Topic:   strings.TrimSpace(os.Getenv("TOKI_PUSH_APNS_TOPIC")),
		Sandbox: sb == "1" || strings.EqualFold(sb, "true"),
	})
}
