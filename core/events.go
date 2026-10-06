package core

import (
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/auth"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/subscriptions"
	"golang.org/x/crypto/acme/autocert"
)

// -------------------------------------------------------------------
// Server (net/http bound) events data
//
// The events below embed *RequestEvent (or the server) and therefore
// can't live in the HTTP-free kernel package.
// -------------------------------------------------------------------

type baseRecordEventData struct {
	Record *kernel.Record
}

func (e *baseRecordEventData) Tags() []string {
	if e.Record == nil {
		return nil
	}

	return e.Record.HookTags()
}

type baseCollectionEventData struct {
	Collection *kernel.Collection
}

func (e *baseCollectionEventData) Tags() []string {
	if e.Collection == nil {
		return nil
	}

	tags := make([]string, 0, 2)

	if e.Collection.Id != "" {
		tags = append(tags, e.Collection.Id)
	}

	if e.Collection.Name != "" {
		tags = append(tags, e.Collection.Name)
	}

	return tags
}

type ServeEvent struct {
	hook.Event
	App         App
	Router      *router.Router[*RequestEvent]
	Server      *http.Server
	CertManager *autocert.Manager

	// Listener allow specifying a custom network listener.
	//
	// Leave it nil to use the default net.Listen("tcp", e.Server.Addr).
	Listener net.Listener

	// InstallerFunc is the "installer" function that is called after
	// successful server tcp bind but only if there is no explicit
	// superuser record created yet.
	//
	// It runs in a separate goroutine and its default value is [apis.DefaultInstallerFunc].
	//
	// It receives a system superuser record as argument that you can use to generate
	// a short-lived auth token (e.g. systemSuperuser.NewStaticAuthToken(30 * time.Minute))
	// and concatenate it as query param for your installer page
	// (if you are using the client-side SDKs, you can then load the
	// token with pb.authStore.save(token) and perform any Web API request
	// e.g. creating a new superuser).
	//
	// Set it to nil if you want to skip the installer.
	InstallerFunc func(app App, systemSuperuser *Record, baseURL string) error

	// @todo experimental
	//
	// UIExtensions is a list with the superuser UI extensions.
	UIExtensions []UIExtension
}

type UIExtension struct {
	// Name is the name of the extension.
	// It is also used as path segment for the registered public extension endpoint
	// (e.g. /_/extensions/{name}/*)
	Name string

	// FS is the extension file system.
	FS fs.FS
}

type SettingsListRequestEvent struct {
	hook.Event
	*RequestEvent

	Settings *Settings
}

type SettingsUpdateRequestEvent struct {
	hook.Event
	*RequestEvent

	OldSettings *Settings
	NewSettings *Settings
}

type FileTokenRequestEvent struct {
	hook.Event
	*RequestEvent
	baseRecordEventData

	Token string
}

type FileDownloadRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record     *Record
	FileField  *FileField
	ServedPath string
	ServedName string

	// ThumbError indicates the a thumb wasn't able to be generated
	// (e.g. because it didn't satisfy the support image formats or it timed out).
	//
	// Note that PocketBase fallbacks to the original file in case of a thumb error,
	// but developers can check the field and provide their own custom thumb generation if necessary.
	ThumbError error
}

type CollectionsListRequestEvent struct {
	hook.Event
	*RequestEvent

	Collections []*Collection
	Result      *search.Result
}

type CollectionsImportRequestEvent struct {
	hook.Event
	*RequestEvent

	CollectionsData []map[string]any
	DeleteMissing   bool
}

type CollectionRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData
}

type RealtimeConnectRequestEvent struct {
	hook.Event
	*RequestEvent

	Client subscriptions.Client

	// IdleTimeout specifies the max duration to wait for a new message
	// before closing the connection.
	//
	// Modifying the value after the connection has been established has no effect.
	//
	// Defaults to 5 minutes.
	IdleTimeout time.Duration

	// MaxTimeout specifies the maximum duration a realtime connection
	// can remain open (including even if there are ongoing messages).
	//
	// Once the specified duration expires, the current connection will
	// be terminated, until a client reconnect is issued (if the client is still active).
	//
	// Modifying the value after the connection has been established has no effect.
	//
	// Defaults to 30 minutes.
	MaxTimeout time.Duration
}

type RealtimeMessageEvent struct {
	hook.Event
	*RequestEvent

	Client  subscriptions.Client
	Message *subscriptions.Message
}

type RealtimeSubscribeRequestEvent struct {
	hook.Event
	*RequestEvent

	Client        subscriptions.Client
	Subscriptions []string
}

type RecordsListRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	// @todo consider removing and maybe add as generic to the search.Result?
	Records []*Record
	Result  *search.Result
}

type RecordRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordCreateOTPRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record   *Record
	Password string
}

type RecordAuthWithOTPRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
	OTP    *OTP
}

type RecordAuthRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record     *Record
	Token      string
	Meta       any
	AuthMethod string
}

type RecordAuthWithPasswordRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record        *Record
	Identity      string
	IdentityField string
	Password      string
}

type RecordAuthWithOAuth2RequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	ProviderName   string
	ProviderClient auth.Provider
	Record         *Record
	OAuth2User     *auth.AuthUser
	CreateData     map[string]any
	IsNewRecord    bool
}

type RecordAuthRefreshRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordRequestPasswordResetRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordConfirmPasswordResetRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordRequestVerificationRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordConfirmVerificationRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record *Record
}

type RecordRequestEmailChangeRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record   *Record
	NewEmail string
}

type RecordConfirmEmailChangeRequestEvent struct {
	hook.Event
	*RequestEvent
	baseCollectionEventData

	Record   *Record
	NewEmail string
}
