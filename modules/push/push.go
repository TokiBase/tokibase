//go:build !no_push

package push

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
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

	// maxDevicesPerUser is the number of devices kept per auth record; the
	// least recently seen are evicted on registration.
	maxDevicesPerUser = 20
	// maxSubsPerDevice caps the topic subscriptions of one device.
	maxSubsPerDevice = 100
	// maxTTLSeconds is the FCM maximum (28 days).
	maxTTLSeconds = 2419200

	// VisibilityPublic topics can be listed and joined by any authenticated
	// user; everything else (the default) is private (superuser/server only).
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"

	// ActionProviderError is the audit action of provider configuration/credential failures.
	ActionProviderError = "push.provider_error"

	// breaker: stop disabling devices when this many were disabled within a minute.
	breakerMax    = 200
	breakerWindow = time.Minute
)

var appIDRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// allowedAppIDs returns the optional allowlist TOKI_PUSH_APP_IDS (comma list).
func allowedAppIDs() []string {
	var out []string
	for _, v := range strings.Split(os.Getenv("TOKI_PUSH_APP_IDS"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

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

	mu       sync.Mutex
	override map[string]Provider

	noteMu    sync.Mutex
	noteLast  map[string]time.Time // rate limit of provider_error audits
	disabledT []time.Time          // recent device disables (circuit breaker)
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
			&core.SelectField{Name: "visibility", Values: []string{VisibilityPublic, VisibilityPrivate}, MaxSelect: 1},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_push_topics_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return err
		}
	} else if tc, _ := app.FindCollectionByNameOrId(TopicsCollection); tc != nil && tc.Fields.GetByName("visibility") == nil {
		// upgrade: existing topics have no visibility and therefore stay private
		tc.Fields.Add(&core.SelectField{Name: "visibility", Values: []string{VisibilityPublic, VisibilityPrivate}, MaxSelect: 1})
		if err := app.Save(tc); err != nil {
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
	if err := evictOldDevices(app, authCollection.Id, recordID, r.Id); err != nil {
		app.Logger().Warn("push: failed to evict old devices", "error", err)
	}
	return deviceOf(app, r), nil
}

// evictOldDevices keeps at most maxDevicesPerUser devices per record,
// deleting the least recently seen ones (never keep).
func evictOldDevices(app kernel.App, collectionID, recordID, keep string) error {
	recs, err := app.FindAllRecords(DevicesCollection, dbx.HashExp{"collection": collectionID, "record": recordID})
	if err != nil || len(recs) <= maxDevicesPerUser {
		return err
	}
	sort.Slice(recs, func(i, j int) bool {
		return recs[i].GetDateTime("last_seen").Time().Before(recs[j].GetDateTime("last_seen").Time())
	})
	for _, r := range recs {
		if len(recs) <= maxDevicesPerUser {
			break
		}
		if r.Id == keep {
			continue
		}
		if err := app.Delete(r); err != nil {
			return err
		}
		recs = removeRec(recs, r.Id)
	}
	return nil
}

func removeRec(recs []*core.Record, id string) []*core.Record {
	out := recs[:0:0]
	for _, r := range recs {
		if r.Id != id {
			out = append(out, r)
		}
	}
	return out
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
	if appID != "" {
		if !appIDRe.MatchString(appID) {
			return errors.New("app_id must be a bundle/package id ([A-Za-z0-9._-], starting and ending alphanumeric)")
		}
		if allow := allowedAppIDs(); len(allow) > 0 {
			ok := false
			for _, a := range allow {
				ok = ok || a == appID
			}
			if !ok {
				return errors.New("app_id is not allowed")
			}
		}
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
	return disableDeviceIfNotSeenSince(app, deviceID, reason, time.Time{})
}

// disableDeviceIfNotSeenSince disables the device unless it (re-)registered
// after since (a zero since disables unconditionally): a provider answer for
// a job that was enqueued before the app re-registered the token must not
// kill the fresh registration.
func disableDeviceIfNotSeenSince(app kernel.App, deviceID, reason string, since time.Time) error {
	r, err := app.FindRecordById(DevicesCollection, deviceID)
	if err != nil {
		return nil // already gone
	}
	if !r.GetBool("enabled") {
		return nil
	}
	if !since.IsZero() && r.GetDateTime("last_seen").Time().After(since) {
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

// CreateTopic creates a private topic (idempotent).
func CreateTopic(app kernel.App, name, description string) error {
	return CreateTopicVisibility(app, name, description, VisibilityPrivate)
}

// CreateTopicVisibility creates a topic with visibility "public" (users may
// list and subscribe) or "private" (default; only server-side sends). An
// existing topic gets its visibility updated.
func CreateTopicVisibility(app kernel.App, name, description, visibility string) error {
	if visibility == "" {
		visibility = VisibilityPrivate
	}
	if visibility != VisibilityPublic && visibility != VisibilityPrivate {
		return errors.New("visibility must be public or private")
	}
	if !ValidTopic(name) {
		return errors.New("topic name must be 1-64 characters of [A-Za-z0-9_.-]")
	}
	if r, err := app.FindFirstRecordByData(TopicsCollection, "name", name); err == nil {
		if r.GetString("visibility") == visibility {
			return nil
		}
		r.Set("visibility", visibility)
		return app.Save(r)
	}
	col, err := app.FindCachedCollectionByNameOrId(TopicsCollection)
	if err != nil {
		return err
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("description", description)
	r.Set("visibility", visibility)
	return app.Save(r)
}

// Topic is a row of _push_topics.
type Topic struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

// ListTopics returns all topics sorted by name.
func ListTopics(app kernel.App) ([]Topic, error) { return listTopics(app, false) }

// ListPublicTopics returns the topics users may list and subscribe to.
func ListPublicTopics(app kernel.App) ([]Topic, error) { return listTopics(app, true) }

func listTopics(app kernel.App, publicOnly bool) ([]Topic, error) {
	recs, err := app.FindAllRecords(TopicsCollection)
	if err != nil {
		return nil, err
	}
	out := make([]Topic, 0, len(recs))
	for _, r := range recs {
		v := r.GetString("visibility")
		if v != VisibilityPublic {
			v = VisibilityPrivate
		}
		if publicOnly && v != VisibilityPublic {
			continue
		}
		out = append(out, Topic{Name: r.GetString("name"), Description: r.GetString("description"), Visibility: v})
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

// SubscribePublic is Subscribe for end users: private topics answer as missing.
func SubscribePublic(app kernel.App, deviceID, topic string) error {
	t, err := app.FindFirstRecordByData(TopicsCollection, "name", topic)
	if err != nil || t.GetString("visibility") != VisibilityPublic {
		return errTopicNotFound
	}
	return Subscribe(app, deviceID, topic)
}

// Subscribe adds the device to a topic (idempotent). The topic must exist.
// It does not look at the visibility (server side use); see SubscribePublic.
func Subscribe(app kernel.App, deviceID, topic string) error {
	t, err := app.FindFirstRecordByData(TopicsCollection, "name", topic)
	if err != nil {
		return errTopicNotFound
	}
	if _, err := app.FindFirstRecordByFilter(SubscriptionsCollection, "device = {:d} && topic = {:t}",
		dbx.Params{"d": deviceID, "t": t.Id}); err == nil {
		return nil
	}
	if existing, err := app.FindAllRecords(SubscriptionsCollection, dbx.HashExp{"device": deviceID}); err == nil && len(existing) >= maxSubsPerDevice {
		return errTooManySubs
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

var (
	errTopicNotFound = errors.New("topic not found")
	errTooManySubs   = errors.New("too many subscriptions for this device")
)

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
	// Enqueued (unix ms) is when the job was created; a disable caused by this
	// job is skipped when the device re-registered afterwards.
	Enqueued int64 `json:"enqueued,omitempty"`
	// ExpiresAt (unix seconds, 0 = none) is the absolute end of the TTL.
	ExpiresAt int64 `json:"expires_at,omitempty"`
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
	if n.TTLSeconds < 0 || n.TTLSeconds > maxTTLSeconds {
		return fmt.Errorf("ttl_seconds must be between 0 and %d", maxTTLSeconds)
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
	now := time.Now()
	jp := jobPayload{Enqueued: now.UnixMilli(), Notification: msg.Notification}
	if msg.TTLSeconds > 0 {
		jp.ExpiresAt = now.Add(time.Duration(msg.TTLSeconds) * time.Second).Unix()
	}
	n := 0
	for _, d := range devs {
		jp.Device = d.ID
		if _, err := q.Enqueue(ctx, JobKind, jp,
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
	// the TTL is absolute: a retry after an outage must not get a fresh window
	if p.ExpiresAt > 0 {
		remaining := p.ExpiresAt - time.Now().Unix()
		if remaining <= 0 {
			app.Logger().Info("push: notification expired before delivery, dropped", "device", p.Device)
			return nil
		}
		p.TTLSeconds = int(remaining)
	}
	r, err := app.FindRecordById(DevicesCollection, p.Device)
	if err != nil || !r.GetBool("enabled") {
		return nil // device removed or disabled after enqueue
	}
	d := deviceOf(app, r)
	prov, err := m.ProviderFor(d.Platform)
	if err != nil {
		m.providerError(d.Platform, err)
		return err // visible in `toki jobs` until configured
	}
	if d.Platform == PlatformAPNs && d.AppID != "" {
		p.Topic = d.AppID
	}
	var since time.Time
	if p.Enqueued > 0 {
		since = time.UnixMilli(p.Enqueued)
	}
	return m.deliver(ctx, prov, d, &p.Notification, since)
}

// providerError logs loudly and audits a provider/configuration failure, at
// most once a minute per platform and message (a 100k fan-out would flood).
func (m *Module) providerError(platform string, err error) {
	msg := err.Error()
	key := platform + "|" + msg
	m.noteMu.Lock()
	if m.noteLast == nil {
		m.noteLast = map[string]time.Time{}
	}
	if len(m.noteLast) > 100 {
		m.noteLast = map[string]time.Time{}
	}
	last, seen := m.noteLast[key]
	now := time.Now()
	if seen && now.Sub(last) < time.Minute {
		m.noteMu.Unlock()
		return
	}
	m.noteLast[key] = now
	m.noteMu.Unlock()
	m.app.Logger().Error("push: provider error (jobs are retried, devices are NOT disabled)", "platform", platform, "error", msg)
	audit(ActionProviderError, "", "", map[string]any{"platform": platform, "error": msg})
}

// allowDisable is the circuit breaker: when breakerMax devices were disabled
// within breakerWindow something is systematically wrong (sandbox/prod mix-up,
// wrong key), so further disables are suppressed and audited.
func (m *Module) allowDisable() bool {
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	now := time.Now()
	cut := now.Add(-breakerWindow)
	i := 0
	for i < len(m.disabledT) && m.disabledT[i].Before(cut) {
		i++
	}
	m.disabledT = m.disabledT[i:]
	if len(m.disabledT) >= breakerMax {
		return false
	}
	m.disabledT = append(m.disabledT, now)
	return true
}

var (
	deviceURLRe = regexp.MustCompile(`/3/device/[^\s"']+`)
)

// sanitizedError hides secrets in the message while keeping the error chain
// (errors.Is / errors.As) of the original.
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.err }

// sanitizeErr removes the device token and token-bearing URLs from err's text
// (the text ends up in `last_error` of the job and in logs).
func sanitizeErr(err error, token string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := deviceURLRe.ReplaceAllString(msg, "/3/device/"+"***")
	if token != "" {
		out = strings.ReplaceAll(out, token, MaskToken(token))
		if esc := url.PathEscape(token); esc != token {
			out = strings.ReplaceAll(out, esc, MaskToken(token))
		}
	}
	if out == msg {
		return err
	}
	return &sanitizedError{msg: out, err: err}
}

func (m *Module) deliver(ctx context.Context, prov Provider, d *Device, n *Notification, since time.Time) error {
	err := sanitizeErr(prov.Send(ctx, n, d.Token), d.Token)
	if err == nil {
		return nil
	}
	var perm *PermanentError
	var cfg *ConfigError
	switch {
	case errors.Is(err, ErrInvalidToken):
		if !m.allowDisable() {
			m.providerError(d.Platform, fmt.Errorf("device-token rejections exceed %d per %s, no longer disabling devices (check sandbox/production and credentials): %w", breakerMax, breakerWindow, err))
			return &RetryableError{Err: err}
		}
		if derr := disableDeviceIfNotSeenSince(m.app, d.ID, err.Error(), since); derr != nil {
			m.app.Logger().Warn("push: failed to disable device", "device", d.ID, "error", derr)
		}
		return nil
	case errors.As(err, &cfg):
		m.providerError(d.Platform, err)
		return err // retried with backoff, visible in `toki jobs`
	case errors.As(err, &perm):
		m.app.Logger().Warn("push: notification rejected", "device", d.ID, "platform", d.Platform, "error", err)
		return nil
	}
	return err // retryable or network error: the queue backs off
}
