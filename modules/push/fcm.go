//go:build !no_push

package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	fcmScope      = "https://www.googleapis.com/auth/firebase.messaging"
	defaultFCMURL = "https://fcm.googleapis.com"
	defaultTokURI = "https://oauth2.googleapis.com/token"
)

// ServiceAccount is the subset of a Google service account JSON that is used.
type ServiceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// ParseServiceAccount parses and validates a service account JSON document.
func ParseServiceAccount(raw []byte) (*ServiceAccount, *rsa.PrivateKey, error) {
	var sa ServiceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, nil, fmt.Errorf("fcm: invalid service account JSON: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, nil, errors.New("fcm: service account JSON needs project_id, client_email and private_key")
	}
	k, err := parsePEMKey([]byte(sa.PrivateKey))
	if err != nil {
		return nil, nil, fmt.Errorf("fcm: private_key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("fcm: private_key is not an RSA key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokURI
	}
	return &sa, rk, nil
}

// FCM is the Firebase Cloud Messaging HTTP v1 provider.
type FCM struct {
	sa  *ServiceAccount
	key *rsa.PrivateKey

	BaseURL string // default https://fcm.googleapis.com
	Client  *http.Client
	Now     func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewFCM builds the provider from a service account JSON document.
func NewFCM(saJSON []byte) (*FCM, error) {
	sa, key, err := ParseServiceAccount(saJSON)
	if err != nil {
		return nil, err
	}
	return &FCM{sa: sa, key: key, BaseURL: defaultFCMURL, Client: newHTTPClient(), Now: time.Now}, nil
}

func (f *FCM) Name() string { return PlatformFCM }

// accessToken returns a cached OAuth2 token, exchanging a signed JWT when needed.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	if f.token != "" && now.Before(f.expiry.Add(-time.Minute)) {
		return f.token, nil
	}
	assertion, err := signRS256(f.key,
		map[string]string{"alg": "RS256", "typ": "JWT"},
		map[string]any{
			"iss": f.sa.ClientEmail, "scope": fcmScope, "aud": f.sa.TokenURI,
			// iat is backdated to tolerate a host clock slightly ahead of Google
			"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(-30 * time.Second).Add(time.Hour).Unix(),
		})
	if err != nil {
		return "", permanent("fcm: sign assertion: %v", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", permanent("fcm: token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == 429 || res.StatusCode >= 500 {
		return "", retryable("fcm: oauth token endpoint HTTP %d", res.StatusCode)
	}
	if res.StatusCode != 200 {
		// invalid_grant, revoked key, disabled API...: operator-fixable
		return "", configErr("fcm: oauth token endpoint HTTP %d: %s", res.StatusCode, truncate(string(body), 200))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", permanent("fcm: oauth token response has no access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	f.token, f.expiry = tr.AccessToken, now.Add(time.Duration(tr.ExpiresIn)*time.Second)
	return f.token, nil
}

func (f *FCM) dropToken() {
	f.mu.Lock()
	f.token, f.expiry = "", time.Time{}
	f.mu.Unlock()
}

// buildBody maps a Notification to the FCM v1 message envelope.
func fcmBodyJSON(n *Notification) ([]byte, error) { return json.Marshal(fcmBody(n, "dry-run-token")) }

func fcmBody(n *Notification, token string) map[string]any {
	msg := map[string]any{"token": token}
	if n.Title != "" || n.Body != "" {
		msg["notification"] = map[string]string{"title": n.Title, "body": n.Body}
	}
	if len(n.Data) > 0 {
		msg["data"] = n.Data
	}
	prio, apnsPrio := "HIGH", "10"
	if n.Priority == PriorityNormal {
		prio, apnsPrio = "NORMAL", "5"
	}
	android := map[string]any{"priority": prio}
	if n.TTLSeconds > 0 {
		android["ttl"] = fmt.Sprintf("%ds", n.TTLSeconds)
	}
	if n.CollapseKey != "" {
		android["collapse_key"] = n.CollapseKey
	}
	msg["android"] = android
	apnsHeaders := map[string]string{"apns-priority": apnsPrio}
	if n.CollapseKey != "" {
		apnsHeaders["apns-collapse-id"] = n.CollapseKey
	}
	msg["apns"] = map[string]any{"headers": apnsHeaders}
	return map[string]any{"message": msg}
}

func (f *FCM) Send(ctx context.Context, n *Notification, token string) error {
	at, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(fcmBody(n, token))
	if err != nil {
		return permanent("fcm: encode: %v", err)
	}
	u := strings.TrimRight(f.BaseURL, "/") + "/v1/projects/" + url.PathEscape(f.sa.ProjectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return permanent("fcm: request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return classifyFCM(f, res.StatusCode, body)
}

func classifyFCM(f *FCM, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode       string `json:"errorCode"`
				FieldViolations []struct {
					Field string `json:"field"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	code := e.Error.Status
	tokenViolation := false
	for _, d := range e.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
		for _, v := range d.FieldViolations {
			if v.Field == "message.token" {
				tokenViolation = true
			}
		}
	}
	msg := fmt.Sprintf("fcm: HTTP %d %s %s", status, code, truncate(e.Error.Message, 200))
	switch {
	case code == "UNREGISTERED" || (code == "INVALID_ARGUMENT" && tokenViolation):
		// only these identify one dead token. NOT_FOUND / 404 is the project
		// path (wrong project_id, FCM API disabled): a configuration error.
		return fmt.Errorf("%w (%s)", ErrInvalidToken, msg)
	case status == 404 || code == "NOT_FOUND" || status == 403 || code == "PERMISSION_DENIED" || code == "SENDER_ID_MISMATCH":
		f.dropToken()
		return &ConfigError{Err: errors.New(msg)}
	case status == 401:
		f.dropToken() // expired or revoked: next attempt re-exchanges
		return &RetryableError{Err: errors.New(msg)}
	case status == 429 || status >= 500 || code == "QUOTA_EXCEEDED" || code == "UNAVAILABLE" || code == "INTERNAL":
		return &RetryableError{Err: errors.New(msg)}
	}
	return &PermanentError{Err: errors.New(msg)}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---- configuration from env ----

func loadFCMFromEnv() (*FCM, error) {
	var raw []byte
	if p := strings.TrimSpace(os.Getenv("TOKI_PUSH_FCM_SA_FILE")); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("TOKI_PUSH_FCM_SA_FILE: %w", err)
		}
		raw = b
	} else if s := strings.TrimSpace(os.Getenv("TOKI_PUSH_FCM_SA_B64")); s != "" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("TOKI_PUSH_FCM_SA_B64: %w", err)
		}
		raw = b
	} else {
		return nil, nil
	}
	return NewFCM(raw)
}
