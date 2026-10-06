package core

import (
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// App defines the main PocketBase app interface.
//
// It extends the HTTP-free [kernel.App] with the server specific hooks
// (OnServe and all the *Request hooks, which operate on [RequestEvent]).
//
// Note that the interface is not intended to be implemented manually by users
// and instead they should use core.BaseApp (either directly or as embedded field in a custom struct).
//
// NB! The callbacks of the inherited transaction helpers (RunInTransaction, etc.)
// and the event App fields are typed as [kernel.App]; use [AsApp] (or a type assertion)
// when you need the server hooks from them.
type App interface {
	kernel.App

	// OnServe hook is triggered when the app web server is started
	// (after starting the TCP listener but before initializing the blocking serve task),
	// allowing you to adjust its options and attach new routes or middlewares.
	OnServe() *hook.Hook[*ServeEvent]

	// ---------------------------------------------------------------
	// Realtime API event hooks
	// ---------------------------------------------------------------

	// OnRealtimeConnectRequest hook is triggered when establishing the SSE client connection.
	//
	// Any execution after e.Next() of a hook handler happens after the client disconnects.
	OnRealtimeConnectRequest() *hook.Hook[*RealtimeConnectRequestEvent]

	// OnRealtimeMessageSend hook is triggered when sending an SSE message to a client.
	OnRealtimeMessageSend() *hook.Hook[*RealtimeMessageEvent]

	// OnRealtimeSubscribeRequest hook is triggered when updating the
	// client subscriptions, allowing you to further validate and
	// modify the submitted change.
	OnRealtimeSubscribeRequest() *hook.Hook[*RealtimeSubscribeRequestEvent]

	// ---------------------------------------------------------------
	// Settings API event hooks
	// ---------------------------------------------------------------

	// OnSettingsListRequest hook is triggered on each API Settings list request.
	//
	// Could be used to validate or modify the response before returning it to the client.
	OnSettingsListRequest() *hook.Hook[*SettingsListRequestEvent]

	// OnSettingsUpdateRequest hook is triggered on each API Settings update request.
	//
	// Could be used to additionally validate the request data or
	// implement completely different persistence behavior.
	OnSettingsUpdateRequest() *hook.Hook[*SettingsUpdateRequestEvent]

	// ---------------------------------------------------------------
	// File API event hooks
	// ---------------------------------------------------------------

	// OnFileDownloadRequest hook is triggered before each API File download request.
	//
	// Could be used to validate or modify the file response before
	// returning it to the client.
	OnFileDownloadRequest(tags ...string) *hook.TaggedHook[*FileDownloadRequestEvent]

	// OnFileBeforeTokenRequest hook is triggered on each auth file token API request.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnFileTokenRequest(tags ...string) *hook.TaggedHook[*FileTokenRequestEvent]

	// ---------------------------------------------------------------
	// Record Auth API event hooks
	// ---------------------------------------------------------------

	// OnRecordAuthRequest hook is triggered on each successful API
	// record authentication request (sign-in, token refresh, etc.).
	//
	// Could be used to additionally validate or modify the authenticated
	// record data and token.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordAuthRequest(tags ...string) *hook.TaggedHook[*RecordAuthRequestEvent]

	// OnRecordAuthWithPasswordRequest hook is triggered on each
	// Record auth with password API request.
	//
	// [RecordAuthWithPasswordRequestEvent.Record] could be nil if no matching identity is found, allowing
	// you to manually locate a different Record model (by reassigning [RecordAuthWithPasswordRequestEvent.Record]).
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordAuthWithPasswordRequest(tags ...string) *hook.TaggedHook[*RecordAuthWithPasswordRequestEvent]

	// OnRecordAuthWithOAuth2Request hook is triggered on each Record
	// OAuth2 sign-in/sign-up API request (after token exchange and before external provider linking).
	//
	// If [RecordAuthWithOAuth2RequestEvent.Record] is not set, then the OAuth2
	// request will try to create a new auth Record.
	//
	// To assign or link a different existing record model you can
	// change the [RecordAuthWithOAuth2RequestEvent.Record] field.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordAuthWithOAuth2Request(tags ...string) *hook.TaggedHook[*RecordAuthWithOAuth2RequestEvent]

	// OnRecordAuthRefreshRequest hook is triggered on each Record
	// auth refresh API request (right before generating a new auth token).
	//
	// Could be used to additionally validate the request data or implement
	// completely different auth refresh behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordAuthRefreshRequest(tags ...string) *hook.TaggedHook[*RecordAuthRefreshRequestEvent]

	// OnRecordRequestPasswordResetRequest hook is triggered on
	// each Record request password reset API request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different password reset behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordRequestPasswordResetRequest(tags ...string) *hook.TaggedHook[*RecordRequestPasswordResetRequestEvent]

	// OnRecordConfirmPasswordResetRequest hook is triggered on
	// each Record confirm password reset API request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordConfirmPasswordResetRequest(tags ...string) *hook.TaggedHook[*RecordConfirmPasswordResetRequestEvent]

	// OnRecordRequestVerificationRequest hook is triggered on
	// each Record request verification API request.
	//
	// Could be used to additionally validate the loaded request data or implement
	// completely different verification behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordRequestVerificationRequest(tags ...string) *hook.TaggedHook[*RecordRequestVerificationRequestEvent]

	// OnRecordConfirmVerificationRequest hook is triggered on each
	// Record confirm verification API request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordConfirmVerificationRequest(tags ...string) *hook.TaggedHook[*RecordConfirmVerificationRequestEvent]

	// OnRecordRequestEmailChangeRequest hook is triggered on each
	// Record request email change API request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different request email change behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordRequestEmailChangeRequest(tags ...string) *hook.TaggedHook[*RecordRequestEmailChangeRequestEvent]

	// OnRecordConfirmEmailChangeRequest hook is triggered on each
	// Record confirm email change API request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordConfirmEmailChangeRequest(tags ...string) *hook.TaggedHook[*RecordConfirmEmailChangeRequestEvent]

	// OnRecordRequestOTPRequest hook is triggered on each Record
	// request OTP API request.
	//
	// [RecordCreateOTPRequestEvent.Record] could be nil if no matching identity is found, allowing
	// you to manually create or locate a different Record model (by reassigning [RecordCreateOTPRequestEvent.Record]).
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordRequestOTPRequest(tags ...string) *hook.TaggedHook[*RecordCreateOTPRequestEvent]

	// OnRecordAuthWithOTPRequest hook is triggered on each Record
	// auth with OTP API request.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordAuthWithOTPRequest(tags ...string) *hook.TaggedHook[*RecordAuthWithOTPRequestEvent]

	// ---------------------------------------------------------------
	// Record CRUD API event hooks
	// ---------------------------------------------------------------

	// OnRecordsListRequest hook is triggered on each API Records list request.
	//
	// Could be used to validate or modify the response before returning it to the client.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordsListRequest(tags ...string) *hook.TaggedHook[*RecordsListRequestEvent]

	// OnRecordViewRequest hook is triggered on each API Record view request.
	//
	// Could be used to validate or modify the response before returning it to the client.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordViewRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent]

	// OnRecordCreateRequest hook is triggered on each API Record create request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordCreateRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent]

	// OnRecordUpdateRequest hook is triggered on each API Record update request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordUpdateRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent]

	// OnRecordDeleteRequest hook is triggered on each API Record delete request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different delete behavior.
	//
	// If the optional "tags" list (Collection ids or names) is specified,
	// then all event handlers registered via the created hook will be
	// triggered and called only if their event data origin matches the tags.
	OnRecordDeleteRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent]

	// ---------------------------------------------------------------
	// Collection API event hooks
	// ---------------------------------------------------------------

	// OnCollectionsListRequest hook is triggered on each API Collections list request.
	//
	// Could be used to validate or modify the response before returning it to the client.
	OnCollectionsListRequest() *hook.Hook[*CollectionsListRequestEvent]

	// OnCollectionViewRequest hook is triggered on each API Collection view request.
	//
	// Could be used to validate or modify the response before returning it to the client.
	OnCollectionViewRequest() *hook.Hook[*CollectionRequestEvent]

	// OnCollectionCreateRequest hook is triggered on each API Collection create request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	OnCollectionCreateRequest() *hook.Hook[*CollectionRequestEvent]

	// OnCollectionUpdateRequest hook is triggered on each API Collection update request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different persistence behavior.
	OnCollectionUpdateRequest() *hook.Hook[*CollectionRequestEvent]

	// OnCollectionDeleteRequest hook is triggered on each API Collection delete request.
	//
	// Could be used to additionally validate the request data or implement
	// completely different delete behavior.
	OnCollectionDeleteRequest() *hook.Hook[*CollectionRequestEvent]

	// OnCollectionsBeforeImportRequest hook is triggered on each API
	// collections import request.
	//
	// Could be used to additionally validate the imported collections or
	// to implement completely different import behavior.
	OnCollectionsImportRequest() *hook.Hook[*CollectionsImportRequestEvent]

	// ---------------------------------------------------------------
	// Batch API event hooks
	// ---------------------------------------------------------------

	// OnBatchRequest hook is triggered on each API batch request.
	//
	// Could be used to additionally validate or modify the submitted batch requests.
	OnBatchRequest() *hook.Hook[*BatchRequestEvent]
}
