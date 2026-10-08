//go:build !no_tlscheck

// Package tlscheck warns (or refuses to start) when the server listens on
// plain HTTP on a non-loopback address without any trusted proxy header
// configured, which usually means clients reach it unencrypted.
package tlscheck

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const hookId = "__tokiTLSCheck__"

// hookPriority makes the check run before any other OnServe handler and
// before the listener is created.
const hookPriority = -1 << 21

// Mode is the check policy (env TOKI_TLS_CHECK).
type Mode string

const (
	ModeWarn   Mode = "warn"
	ModeStrict Mode = "strict"
	ModeOff    Mode = "off"
)

// ModeFromEnv parses TOKI_TLS_CHECK. Unknown values fall back to warn.
func ModeFromEnv() Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_TLS_CHECK"))) {
	case "strict":
		return ModeStrict
	case "off", "false", "0", "disabled":
		return ModeOff
	}
	return ModeWarn
}

// Input describes the serving situation.
type Input struct {
	Addr         string   // listen address (host:port), "" means all interfaces
	TLS          bool     // the server terminates TLS itself
	ProxyHeaders []string // settings.TrustedProxy.Headers
}

// Exposed reports whether the input is "plain HTTP on a reachable address
// without a trusted proxy header".
func Exposed(in Input) bool {
	if in.TLS || len(in.ProxyHeaders) > 0 {
		return false
	}
	return !isLoopbackAddr(in.Addr)
}

func isLoopbackAddr(addr string) bool {
	if strings.HasPrefix(addr, "unix:") || strings.HasPrefix(addr, "/") {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // all interfaces
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsLoopback()
	}
	return false
}

// Message is the warning text.
func Message(addr string) string {
	if addr == "" {
		addr = "all interfaces"
	}
	return fmt.Sprintf("tlscheck: serving plain HTTP on %s with no trusted proxy header set; clients are probably reaching it unencrypted. "+
		"Put it behind nginx/Caddy/Cloudflare with TLS and set Settings > Trusted proxy headers (e.g. X-Forwarded-For), or serve with a domain for automatic HTTPS. "+
		"Silence with TOKI_TLS_CHECK=off.", addr)
}

// Register binds the check on OnServe.
func Register(app core.App) {
	mode := ModeFromEnv()
	if mode == ModeOff {
		return
	}
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       hookId,
		Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			if err := check(e, mode); err != nil {
				return err
			}
			return e.Next()
		},
	})
}

func check(e *core.ServeEvent, mode Mode) error {
	in := Input{TLS: tlsRequested(os.Args[1:])}
	if e.Listener != nil {
		in.Addr = e.Listener.Addr().String()
		if e.Listener.Addr().Network() == "unix" {
			return nil
		}
	} else if e.Server != nil {
		in.Addr = e.Server.Addr
	}
	if s := e.App.Settings(); s != nil {
		in.ProxyHeaders = s.TrustedProxy.Headers
	}
	if !Exposed(in) {
		return nil
	}
	if addr := edgeTLSAddr(); addr != "" {
		// clients have an encrypted, authenticated door: the plain port is the
		// local one (kiosk on the same host, health checks)
		e.App.Logger().Info("tlscheck: plain HTTP is also served, but modules/devicecert serves HTTPS", "plain", in.Addr, "tls", addr)
		return nil
	}
	msg := Message(in.Addr)
	if mode == ModeStrict {
		return fmt.Errorf("%s (TOKI_TLS_CHECK=strict: refusing to start)", msg)
	}
	e.App.Logger().Warn(msg, "addr", in.Addr)
	color.New(color.FgYellow, color.Bold).Fprintln(color.Error, "WARNING "+msg)
	return nil
}

// edgeTLSAddr returns TOKI_DEVICECERT_LISTEN when modules/devicecert is on and
// compiled in (its marker is not the stub's), "" otherwise. The check reads the
// environment so the two modules do not import each other.
func edgeTLSAddr() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_DEVICECERT"))) {
	case "on", "true", "1", "yes":
	default:
		return ""
	}
	addr := strings.TrimSpace(os.Getenv("TOKI_DEVICECERT_LISTEN"))
	if addr == "" {
		return ""
	}
	for _, m := range kernel.ModuleMarkers() {
		if m.Name == "devicecert" {
			if m.Stubbed {
				return ""
			}
			return addr
		}
	}
	return ""
}

// tlsRequested reports whether the `serve` command line asks for HTTPS:
// a --https flag or at least one positional domain.
func tlsRequested(args []string) bool {
	serve := -1
	for i, a := range args {
		if a == "serve" {
			serve = i
			break
		}
	}
	if serve < 0 {
		return false
	}
	valued := map[string]bool{"--origins": true, "--http": true, "--dir": true, "--encryptionEnv": true, "--queryTimeout": true}
	rest := args[serve+1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--https" || strings.HasPrefix(a, "--https="):
			return true
		case strings.HasPrefix(a, "-"):
			if valued[a] {
				i++
			}
		default:
			return true // positional domain
		}
	}
	return false
}
