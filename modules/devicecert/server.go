//go:build !no_devicecert

package devicecert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// MaintainInterval is how often a hub checks whether its own leaf is due.
var MaintainInterval = time.Minute

type serverState struct {
	srv  *http.Server
	ln   net.Listener
	addr string
}

func (m *Module) setErr(err error) {
	m.errMu.Lock()
	defer m.errMu.Unlock()
	if err == nil {
		m.lastErr = ""
	} else {
		m.lastErr = err.Error()
	}
}

func (m *Module) listenState() (bool, string) {
	m.srvMu.Lock()
	defer m.srvMu.Unlock()
	if m.srv == nil {
		return false, ""
	}
	return true, m.srv.addr
}

// Addr is the address the TLS listener is bound to ("" when it is not running).
func (m *Module) Addr() string {
	_, a := m.listenState()
	return a
}

// bindServe starts the TLS listener after the main server is built and
// stops it on terminate.
func (m *Module) bindServe() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "serve",
		Func: func(se *core.ServeEvent) error {
			if err := se.Next(); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(context.Background())
			m.srvMu.Lock()
			m.cancelAll = cancel
			m.srvMu.Unlock()
			// a hub makes its own leaf; a spoke gets it from the sync loop
			if m.IsHub() {
				if err := m.selfIssue(ctx); err != nil {
					m.setErr(err)
					se.App.Logger().Warn("devicecert: failed to issue the hub edge certificate", "error", err)
				}
				go m.maintain(ctx)
			}
			if addr := ListenAddr(); addr != "" {
				if err := m.startListener(addr, se.Server); err != nil {
					m.setErr(err)
					se.App.Logger().Error("devicecert: failed to start the TLS listener", "addr", addr, "error", err)
				}
			}
			return nil
		},
	})
	m.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId + "stop",
		Func: func(e *core.TerminateEvent) error {
			m.Stop()
			return e.Next()
		},
	})
}

// maintain renews the hub leaf when it is due.
func (m *Module) maintain(ctx context.Context) {
	t := time.NewTicker(MaintainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.selfIssue(ctx); err != nil {
				m.setErr(err)
				m.app.Logger().Warn("devicecert: failed to renew the hub edge certificate", "error", err)
			} else {
				m.setErr(nil)
			}
		}
	}
}

// tlsConfig is the config of the listener. The certificate is looked up on
// every handshake, so a renewed leaf is served at once. The client
// certificate policy follows TOKI_DEVICECERT_MTLS: PR 7 adds the route
// allowlist that a verified client certificate unlocks; for now it is only
// verified against the CA pool and checked against the deny list.
func (m *Module) tlsConfig() *tls.Config {
	base := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.leaf.getCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
	mode := MTLSMode()
	if mode == MTLSOff {
		return base
	}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return m.clientConfig(base, mode), nil
	}
	return base
}

// clientConfig returns a copy of base that asks for client certificates.
func (m *Module) clientConfig(base *tls.Config, mode MTLS) *tls.Config {
	c := base.Clone()
	c.GetConfigForClient = nil
	c.ClientCAs = m.clientPool()
	c.ClientAuth = tls.VerifyClientCertIfGiven
	if mode == MTLSRequire {
		c.ClientAuth = tls.RequireAndVerifyClientCert
	}
	c.VerifyPeerCertificate = m.verifyPeer
	return c
}

// clientPool is the pool of roots a client certificate may chain to.
func (m *Module) clientPool() *x509.CertPool {
	if ca, _ := m.CA(false); ca != nil {
		p := x509.NewCertPool()
		p.AddCert(ca.Cert)
		return p
	}
	if _, _, pool := m.leaf.current(); pool != nil {
		return pool
	}
	return x509.NewCertPool()
}

// ErrRevoked is returned for a client certificate on the deny list.
var ErrRevoked = errors.New("devicecert: the client certificate was revoked")

// verifyPeer runs after the chain is verified (VerifiedChains is set only when
// a client certificate was given) and refuses revoked serials.
func (m *Module) verifyPeer(_ [][]byte, chains [][]*x509.Certificate) error {
	for _, ch := range chains {
		if len(ch) > 0 && m.deny.has(SerialHex(ch[0].SerialNumber)) {
			return ErrRevoked
		}
	}
	return nil
}

func (m *Module) startListener(addr string, main *http.Server) error {
	m.srvMu.Lock()
	defer m.srvMu.Unlock()
	if m.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           main.Handler,
		TLSConfig:         m.tlsConfig(),
		ReadTimeout:       main.ReadTimeout,
		WriteTimeout:      main.WriteTimeout,
		ReadHeaderTimeout: main.ReadHeaderTimeout,
		BaseContext:       main.BaseContext,
		ErrorLog:          main.ErrorLog,
	}
	m.srv = &serverState{srv: srv, ln: ln, addr: ln.Addr().String()}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.setErr(err)
			m.app.Logger().Error("devicecert: the TLS listener stopped", "error", err)
		}
	}()
	m.app.Logger().Info("devicecert: TLS listener started", "addr", m.srv.addr, "mtls", string(MTLSMode()))
	return nil
}

// Stop shuts the listener and the background loop down.
func (m *Module) Stop() {
	m.srvMu.Lock()
	s, cancel := m.srv, m.cancelAll
	m.srv, m.cancelAll = nil, nil
	m.srvMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = s.srv.Shutdown(ctx)
	}
}
