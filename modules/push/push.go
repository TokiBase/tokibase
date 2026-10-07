package push

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

// Collections and job kind.
const (
	DevicesCollection       = "_push_devices"
	TopicsCollection        = "_push_topics"
	SubscriptionsCollection = "_push_subscriptions"
	JobKind                 = "push.send"

	hookId       = "__tokiPush__"
	hookPriority = 1 << 20
	storeKey     = "__tokiPushModule__"

	defaultMaxFanout = 100000
	defaultMaxJobTry = 8
)

// Enabled reports whether the module is enabled (env TOKI_PUSH=off disables).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_PUSH"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

func maxFanout() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_PUSH_MAX_FANOUT"))); err == nil && n > 0 {
		return n
	}
	return defaultMaxFanout
}

var (
	sinkMu     sync.Mutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects push.send and device disable events to an audit log.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.Lock()
	fn := globalSink
	sinkMu.Unlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// Module is the registered push module.
type Module struct {
	app core.App

	mu        sync.Mutex
	override  map[string]Provider
	env       map[string]Provider
	envErr    map[string]error
	envLoaded bool
}

// Register creates the collections on bootstrap, binds the hooks and the
// endpoints, and registers the `push.send` job handler on the kernel queue.
func Register(app core.App) *Module {
	m := &Module{app: app, override: map[string]Provider{}}
	app.Store().Set(storeKey, m)

	kernel.Jobs(app).Register(JobKind, m.handleJob)

	init := func() {
		if err := ensureCollections(app); err != nil {
			app.Logger().Error("push: failed to initialize collections", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "del", Priority: hookPriority,
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if e.Record.Collection().IsAuth() {
				if err := m.deleteDevicesOf(e.App, e.Record.Collection().Id, e.Record.Id); err != nil {
					e.App.Logger().Warn("push: failed to delete devices of removed record", "error", err)
				}
			}
			return nil
		},
	})
	m.bindRoutes()
	return m
}

// moduleOf returns the module registered for app.
func moduleOf(app kernel.App) (*Module, error) {
	if m, ok := app.Store().Get(storeKey).(*Module); ok && m != nil {
		return m, nil
	}
	return nil, errors.New("push: module is not registered")
}

// SetProvider overrides the provider of a platform (tests, custom gateways).
func (m *Module) SetProvider(platform string, p Provider) {
	m.mu.Lock()
	if p == nil {
		delete(m.override, platform)
	} else {
		m.override[platform] = p
	}
	m.mu.Unlock()
}

func (m *Module) loadEnv() {
	if m.envLoaded {
		return
	}
	m.envLoaded = true
	m.env, m.envErr = map[string]Provider{}, map[string]error{}
	if f, err := loadFCMFromEnv(); err != nil {
		m.envErr[PlatformFCM] = err
	} else if f != nil {
		m.env[PlatformFCM] = f
	}
	if a, err := loadAPNsFromEnv(); err != nil {
		m.envErr[PlatformAPNs] = err
	} else if a != nil {
		m.env[PlatformAPNs] = a
	}
}

// ProviderFor returns the provider for a platform or an error describing why none is available.
func (m *Module) ProviderFor(platform string) (Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.override[platform]; p != nil {
		return p, nil
	}
	m.loadEnv()
	if p := m.env[platform]; p != nil {
		return p, nil
	}
	if err := m.envErr[platform]; err != nil {
		return nil, fmt.Errorf("push: %s provider misconfigured: %w", platform, err)
	}
	return nil, fmt.Errorf("push: %s provider is not configured", platform)
}

// ProviderStatus describes the configuration of every platform (no secrets).
func (m *Module) ProviderStatus() map[string]string {
	out := map[string]string{}
	for _, pl := range []string{PlatformFCM, PlatformAPNs} {
		if _, err := m.ProviderFor(pl); err != nil {
			out[pl] = err.Error()
		} else {
			out[pl] = "configured"
		}
	}
	return out
}

// ---- collections ----

var ensureMu sync.Mutex

func ensureCollections(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if !app.HasTable("_collections") {
		return errors.New("push: _collections table is not ready")
	}
	if _, err := app.FindCachedCollectionByNameOrId(DevicesCollection); err != nil {
		c := core.NewBaseCollection(DevicesCollection)
		c.System = true // rules stay nil: superuser only, users go through /api/push/*
		c.Fields.Add(
			&core.TextField{Name: "collection", Required: true, Max: 100},
			&core.TextField{Name: "record", Required: true, Max: 100},
			&core.TextField{Name: "token", Required: true, Max: 4096},
			&core.SelectField{Name: "platform", Required: true, Values: []string{PlatformFCM, PlatformAPNs}, MaxSelect: 1},
			&core.TextField{Name: "app_id", Max: 200},
			&core.TextField{Name: "locale", Max: 35},
			&core.DateField{Name: "last_seen"},
			&core.BoolField{Name: "enabled"},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_push_devices_token", true, "token", "")
		c.AddIndex("idx_toki_push_devices_owner", false, "collection,record", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCachedCollectionByNameOrId(TopicsCollection); err != nil {
		c := core.NewBaseCollection(TopicsCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 64},
			&core.TextField{Name: "description", Max: 500},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_push_topics_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCachedCollectionByNameOrId(SubscriptionsCollection); err != nil {
		dev, err := app.FindCachedCollectionByNameOrId(DevicesCollection)
		if err != nil {
			return err
		}
		top, err := app.FindCachedCollectionByNameOrId(TopicsCollection)
		if err != nil {
			return err
		}
		c := core.NewBaseCollection(SubscriptionsCollection)
		c.System = true
		c.Fields.Add(
			&core.RelationField{Name: "device", Required: true, CollectionId: dev.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "topic", Required: true, CollectionId: top.Id, MaxSelect: 1, CascadeDelete: true},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_toki_push_subs_pair", true, "device,topic", "")
		c.AddIndex("idx_toki_push_subs_topic", false, "topic", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}

// ---- devices ----

// Device is a row of _push_devices.
type Device struct {
	ID         string `json:"id"`
	Collection string `json:"collection"` // collection name
	Record     string `json:"record"`
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	AppID      string `json:"app_id"`
	Locale     string `json:"locale"`
	LastSeen   string `json:"last_seen"`
	Enabled    bool   `json:"enabled"`

	collectionID string
}

// MaskToken shortens a token for display and logs.
func MaskToken(t string) string {
	if len(t) <= 12 {
		return "***"
	}
	return t[:6] + "..." + t[len(t)-4:]
}

// Masked returns a copy that is safe to print (token shortened).
func (d Device) Masked() Device {
	d.Token = MaskToken(d.Token)
	return d
}

func collectionName(app kernel.App, id string) string {
	if c, err := app.FindCachedCollectionByNameOrId(id); err == nil {
		return c.Name
	}
	return id
}

func deviceOf(app kernel.App, r *core.Record) *Device {
	return &Device{
		ID: r.Id, Collection: collectionName(app, r.GetString("collection")), collectionID: r.GetString("collection"),
		Record: r.GetString("record"), Token: r.GetString("token"), Platform: r.GetString("platform"),
		AppID: r.GetString("app_id"), Locale: r.GetString("locale"),
		LastSeen: r.GetDateTime("last_seen").String(), Enabled: r.GetBool("enabled"),
	}
}

// resolveCollection returns the id of an auth collection given its name or id.
func resolveCollection(app kernel.App, nameOrID string) (*core.Collection, error) {
	c, err := app.FindCachedCollectionByNameOrId(nameOrID)
	if err != nil || !c.IsAuth() {
		return nil, fmt.Errorf("%q is not an auth collection", nameOrID)
	}
	return c, nil
}

// RegisterDevice upserts the device for token and assigns it to the given auth
// record (a token that moves to another user is re-assigned and loses its
// topic subscriptions).
func RegisterDevice(app kernel.App, authCollection *core.Collection, recordID, token, platform, appID, locale string) (*Device, error) {
	if err := validateDevice(token, platform, appID, locale); err != nil {
		return nil, err
	}
	col, err := app.FindCachedCollectionByNameOrId(DevicesCollection)
	if err != nil {
		return nil, err
	}
	r, err := app.FindFirstRecordByData(DevicesCollection, "token", token)
	switch {
	case err == nil:
		if r.GetString("collection") != authCollection.Id || r.GetString("record") != recordID {
			if err := deleteSubscriptions(app, r.Id); err != nil {
				return nil, err
			}
		}
	default:
		r = core.NewRecord(col)
		r.Set("token", token)
	}
	r.Set("collection", authCollection.Id)
	r.Set("record", recordID)
	r.Set("platform", platform)
	r.Set("app_id", appID)
	r.Set("locale", locale)
	r.Set("enabled", true)
	r.Set("last_seen", types.NowDateTime())
	if err := app.Save(r); err != nil {
		return nil, err
	}
	return deviceOf(app, r), nil
}

func validateDevice(token, platform, appID, locale string) error {
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("token is required (at most 4096 characters, no whitespace)")
	}
	if platform != PlatformFCM && platform != PlatformAPNs {
		return errors.New("platform must be fcm or apns")
	}
	if len(appID) > 200 {
		return errors.New("app_id is too long")
	}
	if len(locale) > 35 {
		return errors.New("locale is too long")
	}
	return nil
}

func deleteSubscriptions(app kernel.App, deviceID string) error {
	subs, err := app.FindAllRecords(SubscriptionsCollection, dbx.HashExp{"device": deviceID})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if err := app.Delete(s); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) deleteDevicesOf(app kernel.App, collectionID, recordID string) error {
	recs, err := app.FindAllRecords(DevicesCollection, dbx.HashExp{"collection": collectionID, "record": recordID})
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := app.Delete(r); err != nil {
			return err
		}
	}
	return nil
}

// ListDevices returns devices, optionally filtered by owner (collection name or id + record id).
func ListDevices(app kernel.App, collection, recordID string) ([]*Device, error) {
	exprs := []dbx.Expression{}
	if collection != "" {
		c, err := resolveCollection(app, collection)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, dbx.HashExp{"collection": c.Id, "record": recordID})
	}
	recs, err := app.FindAllRecords(DevicesCollection, exprs...)
	if err != nil {
		return nil, err
	}
	out := make([]*Device, 0, len(recs))
	for _, r := range recs {
		out = append(out, deviceOf(app, r))
	}
	return out, nil
}

// DisableDevice marks a device as disabled (invalid token) and audits it.
func DisableDevice(app kernel.App, deviceID, reason string) error {
	r, err := app.FindRecordById(DevicesCollection, deviceID)
	if err != nil {
		return nil // already gone
	}
	if !r.GetBool("enabled") {
		return nil
	}
	r.Set("enabled", false)
	if err := app.Save(r); err != nil {
		return err
	}
	audit("push.device.disabled", DevicesCollection, r.Id, map[string]any{
		"platform": r.GetString("platform"), "reason": reason,
	})
	return nil
}

// PruneDevices deletes devices not seen for olderThan and returns how many.
func PruneDevices(app kernel.App, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(types.DefaultDateLayout)
	recs, err := app.FindAllRecords(DevicesCollection, dbx.NewExp("[[last_seen]] < {:t}", dbx.Params{"t": cutoff}))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if err := app.Delete(r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---- topics ----

// ValidTopic reports whether name is an acceptable topic name.
func ValidTopic(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// CreateTopic creates a topic (idempotent).
func CreateTopic(app kernel.App, name, description string) error {
	if !ValidTopic(name) {
		return errors.New("topic name must be 1-64 characters of [A-Za-z0-9_.-]")
	}
	if _, err := app.FindFirstRecordByData(TopicsCollection, "name", name); err == nil {
		return nil
	}
	col, err := app.FindCachedCollectionByNameOrId(TopicsCollection)
	if err != nil {
		return err
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("description", description)
	return app.Save(r)
}

// Topic is a row of _push_topics.
type Topic struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListTopics returns all topics sorted by name.
func ListTopics(app kernel.App) ([]Topic, error) {
	recs, err := app.FindAllRecords(TopicsCollection)
	if err != nil {
		return nil, err
	}
	out := make([]Topic, 0, len(recs))
	for _, r := range recs {
		out = append(out, Topic{Name: r.GetString("name"), Description: r.GetString("description")})
	}
	sortTopics(out)
	return out, nil
}

func sortTopics(t []Topic) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j].Name < t[j-1].Name; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

// Subscribe adds the device to a topic (idempotent). The topic must exist.
func Subscribe(app kernel.App, deviceID, topic string) error {
	t, err := app.FindFirstRecordByData(TopicsCollection, "name", topic)
	if err != nil {
		return errTopicNotFound
	}
	if _, err := app.FindFirstRecordByFilter(SubscriptionsCollection, "device = {:d} && topic = {:t}",
		dbx.Params{"d": deviceID, "t": t.Id}); err == nil {
		return nil
	}
	col, err := app.FindCachedCollectionByNameOrId(SubscriptionsCollection)
	if err != nil {
		return err
	}
	r := core.NewRecord(col)
	r.Set("device", deviceID)
	r.Set("topic", t.Id)
	return app.Save(r)
}

var errTopicNotFound = errors.New("topic not found")

// Unsubscribe removes the device from a topic (idempotent).
func Unsubscribe(app kernel.App, deviceID, topic string) error {
	t, err := app.FindFirstRecordByData(TopicsCollection, "name", topic)
	if err != nil {
		return errTopicNotFound
	}
	subs, err := app.FindAllRecords(SubscriptionsCollection, dbx.HashExp{"device": deviceID, "topic": t.Id})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if err := app.Delete(s); err != nil {
			return err
		}
	}
	return nil
}

// ---- sending ----

// UserRef identifies an auth record.
type UserRef struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

// Target selects the recipients of a message.
type Target struct {
	Users  []UserRef `json:"users,omitempty"`
	Topics []string  `json:"topics,omitempty"`
	Tokens []string  `json:"tokens,omitempty"`
}

// Message is a notification plus its recipients.
type Message struct {
	To Target
	Notification
}

type jobPayload struct {
	Device string `json:"device"`
	Notification
}

// ResolveDevices returns the distinct enabled devices matched by t.
func ResolveDevices(app kernel.App, t Target) ([]*Device, error) {
	seen := map[string]bool{}
	var out []*Device
	add := func(recs []*core.Record) {
		for _, r := range recs {
			if seen[r.Id] || !r.GetBool("enabled") {
				continue
			}
			seen[r.Id] = true
			out = append(out, deviceOf(app, r))
		}
	}
	for _, u := range t.Users {
		c, err := resolveCollection(app, u.Collection)
		if err != nil {
			return nil, err
		}
		recs, err := app.FindAllRecords(DevicesCollection, dbx.HashExp{"collection": c.Id, "record": u.ID})
		if err != nil {
			return nil, err
		}
		add(recs)
	}
	for _, name := range t.Topics {
		tr, err := app.FindFirstRecordByData(TopicsCollection, "name", name)
		if err != nil {
			return nil, fmt.Errorf("topic %q not found", name)
		}
		subs, err := app.FindAllRecords(SubscriptionsCollection, dbx.HashExp{"topic": tr.Id})
		if err != nil {
			return nil, err
		}
		for _, s := range subs {
			r, err := app.FindRecordById(DevicesCollection, s.GetString("device"))
			if err == nil {
				add([]*core.Record{r})
			}
		}
	}
	for _, tok := range t.Tokens {
		if r, err := app.FindFirstRecordByData(DevicesCollection, "token", tok); err == nil {
			add([]*core.Record{r})
		}
	}
	return out, nil
}

func (n *Notification) validate() error {
	if n.Title == "" && n.Body == "" {
		return errors.New("title or body is required")
	}
	switch n.Priority {
	case "", PriorityHigh, PriorityNormal:
	default:
		return errors.New("priority must be high or normal")
	}
	if n.TTLSeconds < 0 {
		return errors.New("ttl_seconds must be >= 0")
	}
	if len(n.CollapseKey) > 200 {
		return errors.New("collapse_key is too long")
	}
	return nil
}

// Send enqueues one `push.send` job per enabled device matched by msg.To and
// returns the number of jobs queued. source labels the audit entry.
func Send(app kernel.App, msg Message) (int, error) { return sendFrom(app, msg, "api") }

// SendToUser is Send for one auth record.
func SendToUser(app kernel.App, collection, id string, n Notification) (int, error) {
	return sendFrom(app, Message{To: Target{Users: []UserRef{{Collection: collection, ID: id}}}, Notification: n}, "api")
}

func sendFrom(app kernel.App, msg Message, source string) (int, error) {
	if _, err := moduleOf(app); err != nil {
		return 0, err
	}
	if err := msg.validate(); err != nil {
		return 0, err
	}
	devs, err := ResolveDevices(app, msg.To)
	if err != nil {
		return 0, err
	}
	if len(devs) > maxFanout() {
		return 0, fmt.Errorf("push: %d recipients exceed TOKI_PUSH_MAX_FANOUT (%d)", len(devs), maxFanout())
	}
	q := kernel.Jobs(app)
	ctx := context.Background()
	n := 0
	for _, d := range devs {
		if _, err := q.Enqueue(ctx, JobKind, jobPayload{Device: d.ID, Notification: msg.Notification},
			kernel.MaxAttempts(defaultMaxJobTry)); err != nil {
			audit("push.send", "", "", map[string]any{"source": source, "queued": n, "recipients": len(devs), "error": err.Error()})
			return n, err
		}
		n++
	}
	audit("push.send", "", "", map[string]any{
		"source": source, "queued": n, "users": len(msg.To.Users), "topics": len(msg.To.Topics), "tokens": len(msg.To.Tokens),
	})
	return n, nil
}

// handleJob is the `push.send` job handler.
func (m *Module) handleJob(ctx context.Context, app kernel.App, job *kernel.Job) error {
	var p jobPayload
	if err := jsonUnmarshal(job.Payload, &p); err != nil {
		return nil // corrupt payload cannot succeed later
	}
	r, err := app.FindRecordById(DevicesCollection, p.Device)
	if err != nil || !r.GetBool("enabled") {
		return nil // device removed or disabled after enqueue
	}
	d := deviceOf(app, r)
	prov, err := m.ProviderFor(d.Platform)
	if err != nil {
		return err // visible in `toki jobs` until configured
	}
	return m.deliver(ctx, prov, d, &p.Notification)
}

func (m *Module) deliver(ctx context.Context, prov Provider, d *Device, n *Notification) error {
	err := prov.Send(ctx, n, d.Token)
	if err == nil {
		return nil
	}
	var perm *PermanentError
	switch {
	case errors.Is(err, ErrInvalidToken):
		if derr := DisableDevice(m.app, d.ID, err.Error()); derr != nil {
			m.app.Logger().Warn("push: failed to disable device", "device", d.ID, "error", derr)
		}
		return nil
	case errors.As(err, &perm):
		m.app.Logger().Warn("push: notification rejected", "device", d.ID, "platform", d.Platform, "error", err)
		return nil
	}
	return err // retryable or network error: the queue backs off
}
