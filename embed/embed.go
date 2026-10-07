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
	// built. Environment is process wide: values persist after Stop and
	// apply to every instance in the process. Entries here win over the
	// profile defaults.
	Env map[string]string

	// HooksDir is an optional pb_hooks directory (JS hooks). Ignored when
	// the binary is built with the no_embed_jsvm tag.
	HooksDir string

	// LogLevel is debug, info (default), warn or error.
	LogLevel string

	// MaxBodyBytes caps the request body for TCP requests and Call.
	// 0 means DefaultMaxBodyBytes, negative means unlimited.
	MaxBodyBytes int64
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
	activeMu sync.Mutex
	active   = map[string]*Instance{}
)

// Instance is a running embedded server.
type Instance struct {
	app     *tokibase.PocketBase
	dir     string
	url     string
	handler http.Handler
	ln      net.Listener

	serveDone chan struct{}
	serveErr  error
	stopOnce  sync.Once
	stopErr   error
	stopped   atomic.Bool
	calls     sync.WaitGroup
	mu        sync.Mutex // guards calls.Add against Stop

	subsMu sync.Mutex
	subs   map[string]subscriptions.Client
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
	for k, v := range penv {
		if _, user := opts.Env[k]; !user && !stubbedEnv[k] {
			if err := os.Setenv(k, v); err != nil {
				release()
				return nil, err
			}
		}
	}
	for k, v := range opts.Env {
		if err := os.Setenv(k, v); err != nil {
			release()
			return nil, err
		}
	}

	var ln net.Listener
	if listen != "-" {
		ln, err = net.Listen("tcp", listen)
		if err != nil {
			release()
			return nil, err
		}
	}

	app := tokibase.NewWithConfig(tokibase.Config{
		DefaultDataDir:  dir,
		HideStartBanner: true,
		SkipFlagParse:   true,
	})
	inst := &Instance{app: app, dir: dir, ln: ln, serveDone: make(chan struct{}), subs: map[string]subscriptions.Client{}}

	fail := func(err error) (*Instance, error) {
		if ln != nil {
			_ = ln.Close()
		}
		_ = app.ResetBootstrapState()
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
			e.Server.Handler = inst.handler
			close(ready)
			return nil
		},
	})

	go func() {
		err := apis.Serve(app, apis.ServeConfig{HttpAddr: listen, AllowedOrigins: []string{"*"}})
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

// Call dispatches an HTTP request through the router in process, without a
// TCP round trip. path may include a query string. Streaming endpoints
// (/api/realtime) are not supported: use Subscribe.
func (i *Instance) Call(method, path string, headers map[string]string, body []byte) (int, map[string]string, []byte, error) {
	if err := i.enter(); err != nil {
		return 0, nil, nil, err
	}
	defer i.calls.Done()

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
	if strings.HasPrefix(u.Path, "/api/realtime") && method == http.MethodGet {
		return 0, nil, nil, errors.New("embed: /api/realtime is a stream; use Subscribe")
	}
	var rdr io.Reader = http.NoBody
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), strings.ToUpper(method), path, rdr)
	if err != nil {
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
	i.handler.ServeHTTP(rec, req)

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
// authenticated view. cancel is idempotent. fn runs on its own goroutine.
func (i *Instance) Subscribe(topic string, fn func(event []byte)) (cancel func()) {
	return i.subscribe(nil, topic, fn)
}

// SubscribeAs is Subscribe with the access of the auth token's record.
func (i *Instance) SubscribeAs(token, topic string, fn func(event []byte)) (cancel func(), err error) {
	rec, err := i.app.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil {
		return nil, fmt.Errorf("embed: invalid auth token: %w", err)
	}
	return i.subscribe(rec, topic, fn), nil
}

func (i *Instance) subscribe(auth *core.Record, topic string, fn func([]byte)) func() {
	client := subscriptions.NewDefaultClient()
	if auth != nil {
		client.Set(apis.RealtimeClientAuthKey, auth)
	}
	client.Subscribe(topic)
	i.app.SubscriptionsBroker().Register(client)

	i.subsMu.Lock()
	i.subs[client.Id()] = client
	i.subsMu.Unlock()

	go func() {
		for msg := range client.Channel() {
			fn(msg.Data)
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
	}
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
// database close) and frees the DataDir for a new Instance. Safe to call
// more than once.
func (i *Instance) Stop(ctx context.Context) error {
	i.stopOnce.Do(func() {
		i.mu.Lock()
		i.stopped.Store(true)
		i.mu.Unlock()

		done := make(chan struct{})
		go func() { i.calls.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		}

		i.subsMu.Lock()
		for id := range i.subs {
			i.app.SubscriptionsBroker().Unregister(id)
		}
		i.subs = map[string]subscriptions.Client{}
		i.subsMu.Unlock()

		ev := new(core.TerminateEvent)
		ev.App = i.app
		err := i.app.OnTerminate().Trigger(ev, func(e *core.TerminateEvent) error {
			return e.App.ClearBootstrap()
		})
		var serveErr error
		select {
		case <-i.serveDone:
			serveErr = i.serveErr
		case <-ctx.Done():
			serveErr = ctx.Err()
		case <-time.After(30 * time.Second):
			serveErr = errors.New("embed: timed out waiting for the server to stop")
		}
		i.stopErr = errors.Join(err, serveErr)

		activeMu.Lock()
		delete(active, i.dir)
		activeMu.Unlock()
	})
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
