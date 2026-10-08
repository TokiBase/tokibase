// Package payments is the provider-neutral plumbing that every payment
// integration repeats: signed webhooks, idempotency, a status state machine,
// reconciliation and entitlements. TokiBase never processes money; providers
// do. This file (types, state machine, provider registry) has no build tag so
// provider adapters compile in every build, including no_payments.
package payments

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Intent statuses.
const (
	StatusCreated           = "created"
	StatusPending           = "pending"
	StatusPaid              = "paid"
	StatusFailed            = "failed"
	StatusExpired           = "expired"
	StatusRefunded          = "refunded"
	StatusPartiallyRefunded = "partially_refunded"
)

// Normalized webhook event types produced by adapters.
const (
	EventPaid     = "paid"
	EventFailed   = "failed"
	EventExpired  = "expired"
	EventRefunded = "refunded" // Amount = refunded amount of this event (0 = whole payment)
	EventApproved = "approved" // buyer approved, merchant must capture (PayPal)
	EventIgnored  = "ignored"  // verified but irrelevant
)

// Errors returned by adapters and the framework.
var (
	ErrUnsupported       = errors.New("payments: operation not supported by this provider")
	ErrInvalidSignature  = errors.New("payments: invalid webhook signature")
	ErrNotConfigured     = errors.New("payments: provider is not configured")
	ErrIllegalTransition = errors.New("payments: illegal status transition")
	ErrUnknownProvider   = errors.New("payments: unknown provider")
)

var transitions = map[string][]string{
	StatusCreated:           {StatusPending, StatusPaid, StatusFailed, StatusExpired},
	StatusPending:           {StatusPaid, StatusFailed, StatusExpired},
	StatusPaid:              {StatusRefunded, StatusPartiallyRefunded},
	StatusPartiallyRefunded: {StatusPartiallyRefunded, StatusRefunded},
	StatusFailed:            nil,
	StatusExpired:           nil,
	StatusRefunded:          nil,
}

// Statuses lists every intent status.
func Statuses() []string {
	return []string{StatusCreated, StatusPending, StatusPaid, StatusFailed, StatusExpired, StatusRefunded, StatusPartiallyRefunded}
}

// CanTransition reports whether from -> to is allowed. from == to is never a
// transition (callers treat it as an idempotent no-op before asking).
func CanTransition(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CheckTransition returns ErrIllegalTransition (wrapped) when from -> to is not allowed.
func CheckTransition(from, to string) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// IntentSpec is what an adapter receives to create a checkout. Amount is in
// the minor unit of the currency (see MinorExponent).
type IntentSpec struct {
	ID            string         `json:"id"`
	Amount        int64          `json:"amount"`
	Currency      string         `json:"currency"`
	OrderRef      string         `json:"order_ref"`
	Description   string         `json:"description"`
	CustomerID    string         `json:"customer_id"`
	CustomerEmail string         `json:"customer_email"`
	CustomerName  string         `json:"customer_name"`
	Metadata      map[string]any `json:"metadata"`
}

// WebhookEvent is a verified, normalized provider notification.
type WebhookEvent struct {
	EventID     string         `json:"event_id"`
	Type        string         `json:"type"` // Event* constants
	ProviderRef string         `json:"provider_ref"`
	AltRefs     []string       `json:"alt_refs,omitempty"`  // other ids the provider may use for the same payment
	IntentID    string         `json:"intent_id,omitempty"` // our intent id when the provider echoes it
	Amount      int64          `json:"amount,omitempty"`
	Currency    string         `json:"currency,omitempty"`
	Data        map[string]any `json:"data,omitempty"` // provider data to merge into the intent (capture id, ...)
}

// RefundSpec asks the provider to refund (part of) a payment.
type RefundSpec struct {
	RefundID    string         `json:"refund_id"`
	IntentID    string         `json:"intent_id"`
	ProviderRef string         `json:"provider_ref"`
	Data        map[string]any `json:"data"` // intent provider data (capture id, ...)
	Amount      int64          `json:"amount"`
	Currency    string         `json:"currency"`
	Reason      string         `json:"reason"`
}

// RefundResult is the provider answer to a refund.
type RefundResult struct {
	ProviderRef string `json:"provider_ref"`
	Done        bool   `json:"done"` // false = accepted, completes later (webhook)
}

// Status is the provider view of a payment, for reconciliation.
type Status struct {
	State          string         `json:"state"` // pending|paid|failed|expired|refunded|partially_refunded
	Amount         int64          `json:"amount,omitempty"`
	Currency       string         `json:"currency,omitempty"`
	RefundedAmount int64          `json:"refunded_amount,omitempty"`
	Data           map[string]any `json:"data,omitempty"`
}

// Provider is the adapter contract. It is intentionally WASM friendly: every
// input and output is plain bytes/JSON-serializable values and header maps
// have lower-cased keys, so a guest module can implement it later. Credentials
// are read by the adapter from env TOKI_PAYMENTS_<PROVIDER>_*, never passed in.
type Provider interface {
	Name() string
	// CreateIntent creates the provider side checkout and returns the URL the
	// customer must visit and the provider reference used by webhooks.
	CreateIntent(ctx context.Context, in *IntentSpec) (checkoutURL, providerRef string, err error)
	// VerifyWebhook authenticates the request (headers lower-cased) and returns
	// the normalized events. It must return ErrInvalidSignature (wrapped) when
	// authentication fails.
	VerifyWebhook(ctx context.Context, headers map[string]string, body []byte) ([]WebhookEvent, error)
	Refund(ctx context.Context, r *RefundSpec) (*RefundResult, error)
	// FetchStatus asks the provider for the current state (reconciliation).
	FetchStatus(ctx context.Context, providerRef string, data map[string]any) (*Status, error)
}

// Capturer is implemented by providers that need a merchant capture step
// after buyer approval (PayPal Orders v2). The returned status is applied like
// a FetchStatus result.
type Capturer interface {
	Capture(ctx context.Context, providerRef string, data map[string]any) (*Status, error)
}

// Factory builds a provider from the environment; it returns nil, nil when the
// provider is not configured.
type Factory func(getenv func(string) string) (Provider, error)

var (
	facMu     sync.Mutex
	factories = map[string]Factory{}
)

// RegisterFactory registers a provider factory (called from adapter init()).
func RegisterFactory(name string, f Factory) {
	facMu.Lock()
	factories[strings.ToLower(name)] = f
	facMu.Unlock()
}

func factoryNames() []string {
	facMu.Lock()
	defer facMu.Unlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func factory(name string) Factory {
	facMu.Lock()
	defer facMu.Unlock()
	return factories[name]
}

// EnvKey returns "TOKI_PAYMENTS_<PROVIDER>_<NAME>".
func EnvKey(provider, name string) string {
	return "TOKI_PAYMENTS_" + strings.ToUpper(provider) + "_" + strings.ToUpper(name)
}

// Getenv is os.Getenv with whitespace trimmed (the default factory env source).
func Getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }

var zeroExp = map[string]bool{"IDR": true, "JPY": true, "KRW": true, "VND": true, "CLP": true, "ISK": true, "UGX": true, "PYG": true, "XAF": true, "XOF": true, "XPF": true, "RWF": true, "KMF": true, "GNF": true, "DJF": true, "BIF": true, "VUV": true}

// MinorExponent is the number of decimals between a currency's major and the
// stored minor unit. IDR is treated as 0 (providers charge whole rupiah).
func MinorExponent(currency string) int {
	if zeroExp[strings.ToUpper(currency)] {
		return 0
	}
	return 2
}

// FormatAmount renders a minor-unit amount as a decimal string ("12.50", "15000").
func FormatAmount(minor int64, currency string) string {
	e := MinorExponent(currency)
	if e == 0 {
		return fmt.Sprintf("%d", minor)
	}
	neg := ""
	if minor < 0 {
		neg, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", neg, minor/100, minor%100)
}

// ParseAmount converts a decimal string to minor units.
func ParseAmount(s, currency string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	e := MinorExponent(currency)
	for len(frac) < e {
		frac += "0"
	}
	frac = frac[:e]
	var w, f int64
	if _, err := fmt.Sscanf(whole, "%d", &w); err != nil {
		return 0, fmt.Errorf("payments: bad amount %q", s)
	}
	if frac != "" {
		if _, err := fmt.Sscanf(frac, "%d", &f); err != nil {
			return 0, fmt.Errorf("payments: bad amount %q", s)
		}
	}
	if e == 0 {
		return w, nil
	}
	if strings.HasPrefix(s, "-") {
		return w*100 - f, nil
	}
	return w*100 + f, nil
}

// NewFromFactory builds a registered provider with an explicit env source (tests).
func NewFromFactory(name string, getenv func(string) string) (Provider, error) {
	f := factory(strings.ToLower(name))
	if f == nil {
		return nil, ErrUnknownProvider
	}
	return f(getenv)
}
