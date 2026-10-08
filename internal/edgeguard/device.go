package edgeguard

import (
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// deviceKey is the request store key under which modules/devicecert records a
// trusted LAN device: a verified mTLS client certificate whose route_scope
// covers the request path (docs/modules/devicecert.md).
const deviceKey = "toki_device"

// SetDevice marks the request as made by the trusted device name. Only
// modules/devicecert calls it.
func SetDevice(e *core.RequestEvent, name string) { e.Set(deviceKey, name) }

// Device is the name of the trusted device that made the request, "" when
// there is none. It is not a user, not a superuser and not the service actor:
// a route that wants to serve devices checks it explicitly.
func Device(e *core.RequestEvent) string {
	s, _ := e.Get(deviceKey).(string)
	return s
}

// DeviceActor is the actor string of a device ("device:<name>"), "" when the
// request is not from a device.
func DeviceActor(e *core.RequestEvent) string {
	if n := Device(e); n != "" {
		return "device:" + n
	}
	return ""
}

// RequireAuthOrDevice is apis.RequireAuth() that also lets a trusted device
// through (the device was matched against its route scope already).
func RequireAuthOrDevice() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "__tokiAuthOrDevice__",
		Func: func(e *core.RequestEvent) error {
			if e.Auth == nil && Device(e) == "" {
				return e.UnauthorizedError("The request requires valid record authorization token.", nil)
			}
			return e.Next()
		},
	}
}
