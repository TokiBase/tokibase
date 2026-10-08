package apis

import (
	"net/http"

	"github.com/tokibase/tokibase/tools/inflector"
)

// DeviceHeader is the request header that modules/devicecert sets for a
// verified client certificate. Rules read it as @request.headers.x_toki_device.
const DeviceHeader = "X-Toki-Device"

// StripTrustedHeader deletes every header of h whose rule key
// (inflector.Snakecase of the name, as core.RequestInfo builds it) equals the
// rule key of name. Header.Del removes only the canonical hyphen spelling, but
// X_Toki_Device, x.toki.device or X~Toki~Device reach the rules under the same
// key, so a trusted header must be stripped by its rule key.
func StripTrustedHeader(h http.Header, name string) {
	key := inflector.Snakecase(name)
	for k := range h {
		if inflector.Snakecase(k) == key {
			delete(h, k)
		}
	}
}

// IsTrustedHeader reports whether the header name maps to the same rule key as
// one of names.
func IsTrustedHeader(k string, names ...string) bool {
	sk := inflector.Snakecase(k)
	for _, n := range names {
		if sk == inflector.Snakecase(n) {
			return true
		}
	}
	return false
}
