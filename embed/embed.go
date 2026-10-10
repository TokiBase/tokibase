// Package embed runs a TokiBase server inside another Go program (a mobile
// or desktop app, a test, a CLI) without cobra, signals or a fixed port.
//
// Typical use:
//
//	inst, err := embed.Start(embed.Options{DataDir: dir})
//	if err != nil { ... }
//	defer inst.Stop(context.Background())
//	status, _, body, _ := inst.Call("GET", "/api/health", nil, nil)
//
// The set of compiled-in modules is decided at build time by Go build tags
// (see profiles.txt and docs/PROFILES.md); Options.Profile only disables
// modules at run time. See docs/EMBED.md.
package embed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/subscriptions"
)

// DefaultMaxBodyBytes is the request body limit applied when
// Options.MaxBodyBytes is zero.
const DefaultMaxBodyBytes int64 = 4 << 20

// Options configures Start.
type Options struct {
	// DataDir is the data directory (required). On mobile use the app
	// sandbox (Android Context.getFilesDir, iOS Application Support).
	DataDir string

	// Listen is the TCP address to bind. Default "127.0.0.1:0" (ephemeral
	// loopback port). Use "-" to skip the TCP listener entirely: the
	// instance is then reachable only through Call and Subscribe.
	Listen string

	// Profile selects run time defaults: nano (default), edge, solo, team,
	// cluster. Modules missing from the binary stay missing; the profile
	// only turns compiled-in modules off via their TOKI_* switches (switches
	// of modules already compiled out are not set, the boot guard would
	// refuse them).
	Profile string

	// Env sets TOKI_* (and other) environment variables before the app is
	// built. Environment is process wide: the values (and the profile
	// defaults) are applied at Start and the previous values are restored
	// when the last running instance of the process stops. Starts are
	// serialised; running instances with different profiles share the
	// process environment, so use one profile per process. Entries here win
	// over the profile defaults.
	Env map[string]string

	// HooksDir is an optional pb_hooks directory (JS hooks). Ignored when
	// the binary is built with the no_embed_jsvm tag.
	HooksDir string

	// LogLevel is debug, info (default), warn or error.
	LogLevel string

	// AllowedOrigins lists the CORS origins of the TCP listener. Default:
	// none (browsers get no CORS grant, so web pages cannot read responses).
	AllowedOrigins []string

	// AllowedHosts lists extra Host header values (without port) accepted on
	// the TCP listener besides localhost, 127.0.0.1 and [::1]. Other Host
	// values are refused with 403 (DNS rebinding defence). Call is not
	// affected.
	AllowedHosts []string

	// EncryptionEnv is the name of the environment variable that holds the
	// settings encryption key (the --encryptionEnv flag is not parsed in an
	// embedded app).
	EncryptionEnv string

	// MaxBodyBytes caps the request body for TCP requests and Call.
	// 0 means DefaultMaxBodyBytes, negative means unlimited.
	MaxBodyBytes int64

	// Sync makes the instance a sync spoke (profile nano or edge only): it
	// defaults TOKI_SYNC_ROLE=spoke and the hub URL, interval and node key.
	// See Instance.Sync and docs/EMBED.md. Ignored (no error) when the binary
	// is built with the no_sync tag.
	Sync *SyncOptions
}

// profileEnv are the run time switches per profile.
var profileEnv = map[string]map[string]string{
	"solo":    {},
	"team":    {},
	"cluster": {},
	"edge": {
		"TOKI_ADMIN_UI": "off",
		"TOKI_WASM":     "off",
	},
	"nano": {
		"TOKI_ADMIN_UI":      "off",
		"TOKI_WASM":          "off",
		"TOKI_AUDIT":         "off",
		"TOKI_WEBHOOKS":      "off",
		"TOKI_PUSH":          "off",
		"TOKI_BACKUP_VERIFY": "off",
		"TOKI_TLS_CHECK":     "off",
	},
}

var (
	startMu sync.Mutex // serialises Start (process environment is shared)

	envMu    sync.Mutex
	envOrig  = map[string]*string{} // original value per touched key (nil = unset)
	envUsers int
)

// applyEnv sets the profile switches and user env for one Start. Keys of any
// profile that this Start does not set go back to their original value, so
// a previous instance's profile cannot leak into this one.
func applyEnv(set map[string]string) error {
	envMu.Lock()
	defer envMu.Unlock()
	keys := map[string]bool{}
	for _, pe := range profileEnv {
		for k := range pe {
			keys[k] = true
		}
	}
	for k := range set {
		keys[k] = true
	}
	// the node key of an earlier instance must not leak into this one
	keys["TOKI_SYNC_NODE_KEY"] = true
	for k := range keys {
		if _, seen := envOrig[k]; !seen {
			if v, ok := os.LookupEnv(k); ok {
				envOrig[k] = &v
			} else {
				envOrig[k] = nil
			}
		}
		var err error
		if v, ok := set[k]; ok {
			err = os.Setenv(k, v)
		} else if o := envOrig[k]; o != nil {
			err = os.Setenv(k, *o)
		} else {
			err = os.Unsetenv(k)
		}
		if err != nil {
			return err
		}
	}
	envUsers++
	return nil
}

// releaseEnv undoes applyEnv; the originals return with the last user.
func releaseEnv() {
	envMu.Lock()
	defer envMu.Unlock()
	envUsers--
	if envUsers > 0 {
		return
	}
	for k, o := range envOrig {
		if o != nil {
			_ = os.Setenv(k, *o)
		} else {
			_ = os.Unsetenv(k)
		}
	}
	envOrig = map[string]*string{}
}

var (
	activeMu sync.Mutex
	active   = map[string]*Instance{}
)

// Instance is a running embedded server.
type Instance struct {
	app     *tokibase.PocketBase
	envHeld bool
	dir     string
	url     string
	profile string // Options.Profile after defaulting
	syncHub string // Options.Sync.HubURL
	handler http.Handler
	ln      net.Listener

	serveDone  chan struct{}
	serveErr   error
	stopMu     sync.Mutex // serialises Stop
	stopDone   bool
	stopErr    error
	terminated bool
	stopped    atomic.Bool
	calls      sync.WaitGroup
	mu         sync.Mutex // guards calls.Add against Stop

	subsMu sync.Mutex
	subs   map[string]subscriptions.Client

	// evCancels are the Sync().OnEvent cancel functions, called by Stop.
	evMu      sync.Mutex
	evCancels []func()
}

// Start builds the app, bootstraps it, serves it in the background and
// returns once the handler is ready. Only one Instance per DataDir may
// run in a process.
func Start(opts Options) (*Instance, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("embed: DataDir is required")
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}

	profile := opts.Profile
	if profile == "" {
		profile = "nano"
	}
	penv, ok := profileEnv[profile]
	if !ok {
		return nil, fmt.Errorf("embed: unknown profile %q (use nano, edge, solo, team or cluster)", profile)
	}
	minLevel, err := parseLevel(opts.LogLevel)
	if err != nil {
		return nil, err
	}
	listen := opts.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	maxBody := opts.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodyBytes
	}

	startMu.Lock()
	defer startMu.Unlock()

	activeMu.Lock()
	if _, busy := active[dir]; busy {
		activeMu.Unlock()
		return nil, fmt.Errorf("embed: data dir %s is already in use by a running instance", dir)
	}
	active[dir] = nil // reserve
	activeMu.Unlock()
	release := func() {
		activeMu.Lock()
		delete(active, dir)
		activeMu.Unlock()
	}

	// a module compiled out with its no_<tag> already is off, and the stub
	// guard refuses to boot when its env switch is set: skip those keys
	stubbedEnv := map[string]bool{}
	for _, m := range kernel.ModuleMarkers() {
		if m.Stubbed {
			for _, e := range m.Envs {
				stubbedEnv[e] = true
			}
		}
	}
	set := map[string]string{}
	for k, v := range penv {
		if !stubbedEnv[k] {
			set[k] = v
		}
	}
	var syncSet map[string]string
	if opts.Sync != nil {
		if profile != "nano" && profile != "edge" {
			release()
			return nil, fmt.Errorf("embed: Options.Sync needs Profile nano or edge (got %q); a hub is run with the toki binary", profile)
		}
		if !stubbedEnv["TOKI_SYNC_ROLE"] {
			syncSet = map[string]string{"TOKI_SYNC_ROLE": "spoke"}
			if opts.Sync.HubURL != "" {
				syncSet["TOKI_SYNC_HUB_URL"] = opts.Sync.HubURL
			}
			if opts.Sync.Interval != "" {
				syncSet["TOKI_SYNC_INTERVAL"] = opts.Sync.Interval
			}
			if len(opts.Sync.NodeKey) > 0 {
				syncSet["TOKI_SYNC_NODE_KEY"] = strings.TrimSpace(string(opts.Sync.NodeKey))
			}
			// precedence: Options.Sync > Options.Env > profile defaults > process env.
			// An Env entry that contradicts Sync is a configuration error, not
			// something to resolve silently in either direction.
			for k, v := range syncSet {
				if ev, ok := opts.Env[k]; ok && ev != v {
					release()
					return nil, fmt.Errorf("embed: Options.Env[%s] contradicts Options.Sync (remove one of them)", k)
				}
			}
		}
	}
	for k, v := range opts.Env {
		set[k] = v
	}
	for k, v := range syncSet {
		set[k] = v
	}
	if err := applyEnv(set); err != nil {
		release()
		return nil, err
	}

	var ln net.Listener
	if listen != "-" {
		ln, err = net.Listen("tcp", listen)
		if err != nil {
			releaseEnv()
			release()
			return nil, err
		}
	}

	app := tokibase.NewWithConfig(tokibase.Config{
		DefaultDataDir:       dir,
		DefaultEncryptionEnv: opts.EncryptionEnv,
		HideStartBanner:      true,
		SkipFlagParse:        true,
	})
	inst := &Instance{envHeld: true, app: app, dir: dir, ln: ln, serveDone: make(chan struct{}), subs: map[string]subscriptions.Client{}, profile: profile}
	if opts.Sync != nil {
		inst.syncHub = opts.Sync.HubURL
	}

	fail := func(err error) (*Instance, error) {
		if ln != nil {
			_ = ln.Close()
		}
		_ = app.ResetBootstrapState()
		releaseEnv()
		release()
		return nil, err
	}

	if err := registerHooks(app, opts.HooksDir); err != nil {
		return fail(err)
	}

	if err := app.Bootstrap(); err != nil {
		return fail(err)
	}
	if s := app.Settings(); s.Logs.MinLevel != minLevel {
		s.Logs.MinLevel = minLevel
		if err := app.Save(s); err != nil {
			return fail(err)
		}
	}

	ready := make(chan struct{})
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id:       "embedReady",
		Priority: -1000, // run first, finish after the router is built
		Func: func(e *core.ServeEvent) error {
			e.InstallerFunc = nil // no installer link: use Instance.Superuser
			if ln != nil {
				e.Listener = ln
			} else {
				// no TCP: a listener that never accepts
				nl := &nullListener{ch: make(chan struct{})}
				e.Listener = nl
				go func() { <-inst.serveDone; _ = nl.Close() }()
			}
			if err := e.Next(); err != nil {
				return err
			}
			inst.handler = limitBody(e.Server.Handler, maxBody)
			e.Server.Handler = hostGuard(inst.handler, opts.AllowedHosts)
			close(ready)
			return nil
		},
	})

	origins := opts.AllowedOrigins
	if len(origins) == 0 {
		// Serve treats an empty list as "*": use an origin no page can have
		origins = []string{"http://embed.invalid"}
	}
	go func() {
		err := apis.Serve(app, apis.ServeConfig{HttpAddr: listen, AllowedOrigins: origins})
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		inst.serveErr = err
		close(inst.serveDone)
	}()

	select {
	case <-ready:
	case <-inst.serveDone:
		err := inst.serveErr
		if err == nil {
			err = errors.New("embed: server exited during start")
		}
		return fail(err)
	case <-time.After(60 * time.Second):
		return fail(errors.New("embed: timed out waiting for the server to start"))
	}

	if ln != nil {
		inst.url = "http://" + ln.Addr().String()
	}
	activeMu.Lock()
	active[dir] = inst
	activeMu.Unlock()
	return inst, nil
}

func parseLevel(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return -4, nil
	case "", "info":
		return 0, nil
	case "warn", "warning":
		return 4, nil
	case "error":
		return 8, nil
	}
	return 0, fmt.Errorf("embed: unknown LogLevel %q (use debug, info, warn or error)", s)
}

// limitBody rejects bodies above max bytes with 413 for every route
// (route level limits in apis may only be looser, never tighter).
func limitBody(next http.Handler, max int64) http.Handler {
	if max < 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"status":413,"message":"Request entity too large","data":{}}`)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard refuses TCP requests whose Host is not a loopback name or one of
// extra (DNS rebinding defence).
func hostGuard(next http.Handler, extra []string) http.Handler {
	ok := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range extra {
		ok[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		h = strings.Trim(strings.ToLower(h), "[]")
		if !ok[h] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status":403,"message":"Host not allowed","data":{}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// URL is the base URL of the loopback listener ("" when Listen was "-").
func (i *Instance) URL() string { return i.url }

// App exposes the underlying app for Go callers that need hooks or direct
// data access. It is not available through the mobile bindings.
func (i *Instance) App() core.App { return i.app }

// enter registers an in-flight call; it fails once Stop has begun.
func (i *Instance) enter() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stopped.Load() {
		return errors.New("embed: instance is stopped")
	}
	i.calls.Add(1)
	return nil
}

// DefaultCallTimeout bounds Call.
const DefaultCallTimeout = 60 * time.Second

// Call dispatches an HTTP request through the router in process, without a
// TCP round trip. path may include a query string. Streaming endpoints
// (/api/realtime, any method) are refused: realtime goes through Subscribe.
// Call gives up after DefaultCallTimeout; use CallContext for another limit.
func (i *Instance) Call(method, path string, headers map[string]string, body []byte) (int, map[string]string, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCallTimeout)
	defer cancel()
	return i.CallContext(ctx, method, path, headers, body)
}

// CallContext is Call with a caller supplied context. When ctx ends first the
// call returns ctx.Err(); the handler may still be finishing in the
// background (Stop waits for it).
func (i *Instance) CallContext(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, map[string]string, []byte, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return 0, nil, nil, err
	}
	if p := strings.TrimRight(u.Path, "/"); p == "/api/realtime" || strings.HasPrefix(p, "/api/realtime/") {
		return 0, nil, nil, errors.New("embed: /api/realtime is a stream; use Subscribe")
	}
	if err := i.enter(); err != nil {
		return 0, nil, nil, err
	}
	var rdr io.Reader = http.NoBody
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rdr)
	if err != nil {
		i.calls.Done()
		return 0, nil, nil, err
	}
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:0"
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	done := make(chan struct{})
	go func() {
		defer i.calls.Done()
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				rec.status = http.StatusInternalServerError
			}
		}()
		i.handler.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return 0, nil, nil, ctx.Err()
	}

	rh := make(map[string]string, len(rec.header))
	for k, v := range rec.header {
		rh[k] = strings.Join(v, ", ")
	}
	return rec.status, rh, rec.body.Bytes(), nil
}

type recorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
}
func (r *recorder) Write(b []byte) (int, error) { r.wroteHeader = true; return r.body.Write(b) }
func (r *recorder) Flush()                      {}

// Subscribe receives realtime events for topic (for example "posts/*" or
// "posts/RECORD_ID") as the JSON payload PocketBase sends over SSE
// ({"action":"create","record":{...}}). The subscriber is anonymous: only
// records the public rules allow are delivered. Use SubscribeAs for an
// authenticated view. cancel is idempotent. fn runs on its own goroutine, a
// panic in fn is recovered, and fn must not block (the client channel is
// unbuffered). It fails after Stop.
func (i *Instance) Subscribe(topic string, fn func(event []byte)) (cancel func(), err error) {
	return i.subscribe(nil, topic, fn)
}

// SubscribeAs is Subscribe with the access of the auth token's record.
func (i *Instance) SubscribeAs(token, topic string, fn func(event []byte)) (cancel func(), err error) {
	if i.stopped.Load() {
		return nil, errors.New("embed: instance is stopped")
	}
	rec, err := i.app.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil {
		return nil, fmt.Errorf("embed: invalid auth token: %w", err)
	}
	return i.subscribe(rec, topic, fn)
}

func (i *Instance) subscribe(auth *core.Record, topic string, fn func([]byte)) (func(), error) {
	if fn == nil {
		return nil, errors.New("embed: nil subscriber")
	}
	client := subscriptions.NewDefaultClient()
	if auth != nil {
		client.Set(apis.RealtimeClientAuthKey, auth)
	}
	client.Subscribe(topic)

	i.mu.Lock()
	if i.stopped.Load() {
		i.mu.Unlock()
		return nil, errors.New("embed: instance is stopped")
	}
	i.app.SubscriptionsBroker().Register(client)
	i.subsMu.Lock()
	i.subs[client.Id()] = client
	i.subsMu.Unlock()
	i.mu.Unlock()

	go func() {
		for msg := range client.Channel() {
			func() {
				defer func() { _ = recover() }()
				fn(msg.Data)
			}()
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			i.subsMu.Lock()
			delete(i.subs, client.Id())
			i.subsMu.Unlock()
			i.app.SubscriptionsBroker().Unregister(client.Id())
		})
	}, nil
}

// Superuser creates the superuser or updates its password when the email
// exists. Use it for first run setup.
func (i *Instance) Superuser(email, password string) error {
	if err := i.enter(); err != nil {
		return err
	}
	defer i.calls.Done()
	col, err := i.app.FindCachedCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		return err
	}
	rec, err := i.app.FindAuthRecordByEmail(col, email)
	if err != nil {
		rec = core.NewRecord(col)
		rec.SetEmail(email)
	}
	rec.SetPassword(password)
	return i.app.Save(rec)
}

// Export writes a full backup zip (data.db, auxiliary.db and storage files)
// to w. The zip is the same format as `POST /api/backups` and can be
// restored with the standard restore. A schema+JSONL export is a later PR.
func (i *Instance) Export(ctx context.Context, w io.Writer) error {
	if err := i.enter(); err != nil {
		return err
	}
	defer i.calls.Done()

	name := fmt.Sprintf("embed_export_%d.zip", time.Now().UnixNano())
	if err := i.app.CreateBackup(ctx, name); err != nil {
		return err
	}
	fsys, err := i.app.NewBackupsFilesystem()
	if err != nil {
		return err
	}
	defer fsys.Close()
	defer func() { _ = fsys.Delete(name) }()
	r, err := fsys.GetReader(name)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

// Stop shuts the server down gracefully (OnTerminate hooks, replica flush,
// database close) and frees the DataDir for a new Instance. The DataDir is
// released only after the server has really finished: when ctx expires first
// (a stuck request, a slow shutdown) Stop returns an error, the DataDir stays
// reserved, and calling Stop again continues the shutdown. After a successful
// Stop it is safe to call again.
func (i *Instance) Stop(ctx context.Context) error {
	i.stopMu.Lock()
	defer i.stopMu.Unlock()
	if i.stopDone {
		return i.stopErr
	}

	i.mu.Lock()
	i.stopped.Store(true)
	i.mu.Unlock()

	i.stopSyncLoop(ctx)

	done := make(chan struct{})
	go func() { i.calls.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("embed: stop incomplete, in-flight calls still running (data dir stays locked): %w", ctx.Err())
	}

	i.cancelEvents()

	i.subsMu.Lock()
	for id := range i.subs {
		i.app.SubscriptionsBroker().Unregister(id)
	}
	i.subs = map[string]subscriptions.Client{}
	i.subsMu.Unlock()

	var err error
	if !i.terminated {
		i.terminated = true
		ev := new(core.TerminateEvent)
		ev.App = i.app
		err = i.app.OnTerminate().Trigger(ev, func(e *core.TerminateEvent) error {
			return e.App.ClearBootstrap()
		})
		i.stopErr = err
	} else {
		err = i.stopErr
	}
	var serveErr error
	select {
	case <-i.serveDone:
		serveErr = i.serveErr
	case <-ctx.Done():
		return fmt.Errorf("embed: stop incomplete, server still shutting down (data dir stays locked): %w", ctx.Err())
	case <-time.After(30 * time.Second):
		return errors.New("embed: timed out waiting for the server to stop (data dir stays locked)")
	}
	i.stopErr = errors.Join(err, serveErr)
	i.stopDone = true

	if i.envHeld {
		i.envHeld = false
		releaseEnv()
	}
	activeMu.Lock()
	delete(active, i.dir)
	activeMu.Unlock()
	return i.stopErr
}

// nullListener satisfies net.Listener without ever accepting a connection.
type nullListener struct {
	ch   chan struct{}
	once sync.Once
}

func (l *nullListener) Accept() (net.Conn, error) { <-l.ch; return nil, net.ErrClosed }
func (l *nullListener) Close() error              { l.once.Do(func() { close(l.ch) }); return nil }
func (l *nullListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
