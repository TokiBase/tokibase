package core

import (
	"net/netip"
	"strings"
	"sync"

	"github.com/tokibase/tokibase/tools/inflector"
	"github.com/tokibase/tokibase/tools/router"
)

// Common request store keys used by the middlewares and api handlers.
const (
	RequestEventKeyInfoContext = "infoContext"
)

// RequestEvent defines the PocketBase router handler event.
type RequestEvent struct {
	App App

	cachedRequestInfo *RequestInfo

	Auth *Record

	router.Event

	mu sync.Mutex
}

// RealIP returns the "real" IP address from the configured trusted proxy headers.
//
// If Settings.TrustedProxy is not configured or the found IP is empty,
// it fallbacks to e.RemoteIP().
//
// NB!
// Be careful when used in a security critical context as it relies on
// the trusted proxy to be properly configured and your app to be accessible only through it.
// If you are not sure, use e.RemoteIP().
func (e *RequestEvent) RealIP() string {
	settings := e.App.Settings()

	for _, h := range settings.TrustedProxy.Headers {
		headerValues := e.Request.Header.Values(h)
		if len(headerValues) == 0 {
			continue
		}

		// extract the last header value as it is expected to be the one controlled by the proxy
		ipsList := headerValues[len(headerValues)-1]
		if ipsList == "" {
			continue
		}

		ips := strings.Split(ipsList, ",")

		if settings.TrustedProxy.UseLeftmostIP {
			for _, ip := range ips {
				parsed, err := netip.ParseAddr(strings.TrimSpace(ip))
				if err == nil {
					return parsed.StringExpanded()
				}
			}
		} else {
			for i := len(ips) - 1; i >= 0; i-- {
				parsed, err := netip.ParseAddr(strings.TrimSpace(ips[i]))
				if err == nil {
					return parsed.StringExpanded()
				}
			}
		}
	}

	return e.RemoteIP()
}

// HasSuperuserAuth checks whether the current RequestEvent has superuser authentication loaded.
func (e *RequestEvent) HasSuperuserAuth() bool {
	return e.Auth != nil && e.Auth.IsSuperuser()
}

// RequestInfo parses the current request into RequestInfo instance.
//
// Note that the returned result is cached to avoid copying the request data multiple times
// but the auth state and other common store items are always refreshed in case they were changed by another handler.
func (e *RequestEvent) RequestInfo() (*RequestInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cachedRequestInfo != nil {
		e.cachedRequestInfo.Auth = e.Auth

		infoCtx, _ := e.Get(RequestEventKeyInfoContext).(string)
		if infoCtx != "" {
			e.cachedRequestInfo.Context = infoCtx
		} else {
			e.cachedRequestInfo.Context = RequestInfoContextDefault
		}
	} else {
		// (re)init e.cachedRequestInfo based on the current request event
		if err := e.initRequestInfo(); err != nil {
			return nil, err
		}
	}

	return e.cachedRequestInfo, nil
}

func (e *RequestEvent) initRequestInfo() error {
	infoCtx, _ := e.Get(RequestEventKeyInfoContext).(string)
	if infoCtx == "" {
		infoCtx = RequestInfoContextDefault
	}

	info := &RequestInfo{
		Context: infoCtx,
		Method:  e.Request.Method,
		Query:   map[string]string{},
		Headers: map[string]string{},
		Body:    map[string]any{},
	}

	if err := e.BindBody(&info.Body); err != nil {
		return err
	}

	// extract the first value of all query params
	query := e.Request.URL.Query()
	for k, v := range query {
		if len(v) > 0 {
			info.Query[k] = v[0]
		}
	}

	// extract the first value of all headers and normalizes the keys
	// ("X-Token" is converted to "x_token")
	for k, v := range e.Request.Header {
		if len(v) > 0 {
			info.Headers[inflector.Snakecase(k)] = v[0]
		}
	}

	info.Auth = e.Auth

	e.cachedRequestInfo = info

	return nil
}

//
