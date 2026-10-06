package core

import (
	"github.com/tokibase/tokibase/tools/hook"
)

// requestHooks holds the server (net/http bound) app event hooks.
type requestHooks struct {
	onServe                             *hook.Hook[*ServeEvent]
	onRealtimeConnectRequest            *hook.Hook[*RealtimeConnectRequestEvent]
	onRealtimeMessageSend               *hook.Hook[*RealtimeMessageEvent]
	onRealtimeSubscribeRequest          *hook.Hook[*RealtimeSubscribeRequestEvent]
	onSettingsListRequest               *hook.Hook[*SettingsListRequestEvent]
	onSettingsUpdateRequest             *hook.Hook[*SettingsUpdateRequestEvent]
	onFileDownloadRequest               *hook.Hook[*FileDownloadRequestEvent]
	onFileTokenRequest                  *hook.Hook[*FileTokenRequestEvent]
	onRecordAuthRequest                 *hook.Hook[*RecordAuthRequestEvent]
	onRecordAuthWithPasswordRequest     *hook.Hook[*RecordAuthWithPasswordRequestEvent]
	onRecordAuthWithOAuth2Request       *hook.Hook[*RecordAuthWithOAuth2RequestEvent]
	onRecordAuthRefreshRequest          *hook.Hook[*RecordAuthRefreshRequestEvent]
	onRecordRequestPasswordResetRequest *hook.Hook[*RecordRequestPasswordResetRequestEvent]
	onRecordConfirmPasswordResetRequest *hook.Hook[*RecordConfirmPasswordResetRequestEvent]
	onRecordRequestVerificationRequest  *hook.Hook[*RecordRequestVerificationRequestEvent]
	onRecordConfirmVerificationRequest  *hook.Hook[*RecordConfirmVerificationRequestEvent]
	onRecordRequestEmailChangeRequest   *hook.Hook[*RecordRequestEmailChangeRequestEvent]
	onRecordConfirmEmailChangeRequest   *hook.Hook[*RecordConfirmEmailChangeRequestEvent]
	onRecordRequestOTPRequest           *hook.Hook[*RecordCreateOTPRequestEvent]
	onRecordAuthWithOTPRequest          *hook.Hook[*RecordAuthWithOTPRequestEvent]
	onRecordsListRequest                *hook.Hook[*RecordsListRequestEvent]
	onRecordViewRequest                 *hook.Hook[*RecordRequestEvent]
	onRecordCreateRequest               *hook.Hook[*RecordRequestEvent]
	onRecordUpdateRequest               *hook.Hook[*RecordRequestEvent]
	onRecordDeleteRequest               *hook.Hook[*RecordRequestEvent]
	onCollectionsListRequest            *hook.Hook[*CollectionsListRequestEvent]
	onCollectionViewRequest             *hook.Hook[*CollectionRequestEvent]
	onCollectionCreateRequest           *hook.Hook[*CollectionRequestEvent]
	onCollectionUpdateRequest           *hook.Hook[*CollectionRequestEvent]
	onCollectionDeleteRequest           *hook.Hook[*CollectionRequestEvent]
	onCollectionsImportRequest          *hook.Hook[*CollectionsImportRequestEvent]
	onBatchRequest                      *hook.Hook[*BatchRequestEvent]
}

func newRequestHooks() *requestHooks {
	h := &requestHooks{}

	h.onServe = &hook.Hook[*ServeEvent]{}
	h.onRealtimeConnectRequest = &hook.Hook[*RealtimeConnectRequestEvent]{}
	h.onRealtimeMessageSend = &hook.Hook[*RealtimeMessageEvent]{}
	h.onRealtimeSubscribeRequest = &hook.Hook[*RealtimeSubscribeRequestEvent]{}
	h.onSettingsListRequest = &hook.Hook[*SettingsListRequestEvent]{}
	h.onSettingsUpdateRequest = &hook.Hook[*SettingsUpdateRequestEvent]{}
	h.onFileDownloadRequest = &hook.Hook[*FileDownloadRequestEvent]{}
	h.onFileTokenRequest = &hook.Hook[*FileTokenRequestEvent]{}
	h.onRecordAuthRequest = &hook.Hook[*RecordAuthRequestEvent]{}
	h.onRecordAuthWithPasswordRequest = &hook.Hook[*RecordAuthWithPasswordRequestEvent]{}
	h.onRecordAuthWithOAuth2Request = &hook.Hook[*RecordAuthWithOAuth2RequestEvent]{}
	h.onRecordAuthRefreshRequest = &hook.Hook[*RecordAuthRefreshRequestEvent]{}
	h.onRecordRequestPasswordResetRequest = &hook.Hook[*RecordRequestPasswordResetRequestEvent]{}
	h.onRecordConfirmPasswordResetRequest = &hook.Hook[*RecordConfirmPasswordResetRequestEvent]{}
	h.onRecordRequestVerificationRequest = &hook.Hook[*RecordRequestVerificationRequestEvent]{}
	h.onRecordConfirmVerificationRequest = &hook.Hook[*RecordConfirmVerificationRequestEvent]{}
	h.onRecordRequestEmailChangeRequest = &hook.Hook[*RecordRequestEmailChangeRequestEvent]{}
	h.onRecordConfirmEmailChangeRequest = &hook.Hook[*RecordConfirmEmailChangeRequestEvent]{}
	h.onRecordRequestOTPRequest = &hook.Hook[*RecordCreateOTPRequestEvent]{}
	h.onRecordAuthWithOTPRequest = &hook.Hook[*RecordAuthWithOTPRequestEvent]{}
	h.onRecordsListRequest = &hook.Hook[*RecordsListRequestEvent]{}
	h.onRecordViewRequest = &hook.Hook[*RecordRequestEvent]{}
	h.onRecordCreateRequest = &hook.Hook[*RecordRequestEvent]{}
	h.onRecordUpdateRequest = &hook.Hook[*RecordRequestEvent]{}
	h.onRecordDeleteRequest = &hook.Hook[*RecordRequestEvent]{}
	h.onCollectionsListRequest = &hook.Hook[*CollectionsListRequestEvent]{}
	h.onCollectionViewRequest = &hook.Hook[*CollectionRequestEvent]{}
	h.onCollectionCreateRequest = &hook.Hook[*CollectionRequestEvent]{}
	h.onCollectionUpdateRequest = &hook.Hook[*CollectionRequestEvent]{}
	h.onCollectionDeleteRequest = &hook.Hook[*CollectionRequestEvent]{}
	h.onCollectionsImportRequest = &hook.Hook[*CollectionsImportRequestEvent]{}
	h.onBatchRequest = &hook.Hook[*BatchRequestEvent]{}

	return h
}

func (app *BaseApp) OnServe() *hook.Hook[*ServeEvent] {
	return app.hooks.onServe
}

func (app *BaseApp) OnRealtimeConnectRequest() *hook.Hook[*RealtimeConnectRequestEvent] {
	return app.hooks.onRealtimeConnectRequest
}

func (app *BaseApp) OnRealtimeMessageSend() *hook.Hook[*RealtimeMessageEvent] {
	return app.hooks.onRealtimeMessageSend
}

func (app *BaseApp) OnRealtimeSubscribeRequest() *hook.Hook[*RealtimeSubscribeRequestEvent] {
	return app.hooks.onRealtimeSubscribeRequest
}

func (app *BaseApp) OnSettingsListRequest() *hook.Hook[*SettingsListRequestEvent] {
	return app.hooks.onSettingsListRequest
}

func (app *BaseApp) OnSettingsUpdateRequest() *hook.Hook[*SettingsUpdateRequestEvent] {
	return app.hooks.onSettingsUpdateRequest
}

func (app *BaseApp) OnFileDownloadRequest(tags ...string) *hook.TaggedHook[*FileDownloadRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onFileDownloadRequest, tags...)
}

func (app *BaseApp) OnFileTokenRequest(tags ...string) *hook.TaggedHook[*FileTokenRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onFileTokenRequest, tags...)
}

func (app *BaseApp) OnRecordAuthRequest(tags ...string) *hook.TaggedHook[*RecordAuthRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordAuthRequest, tags...)
}

func (app *BaseApp) OnRecordAuthWithPasswordRequest(tags ...string) *hook.TaggedHook[*RecordAuthWithPasswordRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordAuthWithPasswordRequest, tags...)
}

func (app *BaseApp) OnRecordAuthWithOAuth2Request(tags ...string) *hook.TaggedHook[*RecordAuthWithOAuth2RequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordAuthWithOAuth2Request, tags...)
}

func (app *BaseApp) OnRecordAuthRefreshRequest(tags ...string) *hook.TaggedHook[*RecordAuthRefreshRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordAuthRefreshRequest, tags...)
}

func (app *BaseApp) OnRecordRequestPasswordResetRequest(tags ...string) *hook.TaggedHook[*RecordRequestPasswordResetRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordRequestPasswordResetRequest, tags...)
}

func (app *BaseApp) OnRecordConfirmPasswordResetRequest(tags ...string) *hook.TaggedHook[*RecordConfirmPasswordResetRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordConfirmPasswordResetRequest, tags...)
}

func (app *BaseApp) OnRecordRequestVerificationRequest(tags ...string) *hook.TaggedHook[*RecordRequestVerificationRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordRequestVerificationRequest, tags...)
}

func (app *BaseApp) OnRecordConfirmVerificationRequest(tags ...string) *hook.TaggedHook[*RecordConfirmVerificationRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordConfirmVerificationRequest, tags...)
}

func (app *BaseApp) OnRecordRequestEmailChangeRequest(tags ...string) *hook.TaggedHook[*RecordRequestEmailChangeRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordRequestEmailChangeRequest, tags...)
}

func (app *BaseApp) OnRecordConfirmEmailChangeRequest(tags ...string) *hook.TaggedHook[*RecordConfirmEmailChangeRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordConfirmEmailChangeRequest, tags...)
}

func (app *BaseApp) OnRecordRequestOTPRequest(tags ...string) *hook.TaggedHook[*RecordCreateOTPRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordRequestOTPRequest, tags...)
}

func (app *BaseApp) OnRecordAuthWithOTPRequest(tags ...string) *hook.TaggedHook[*RecordAuthWithOTPRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordAuthWithOTPRequest, tags...)
}

func (app *BaseApp) OnRecordsListRequest(tags ...string) *hook.TaggedHook[*RecordsListRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordsListRequest, tags...)
}

func (app *BaseApp) OnRecordViewRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordViewRequest, tags...)
}

func (app *BaseApp) OnRecordCreateRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordCreateRequest, tags...)
}

func (app *BaseApp) OnRecordUpdateRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordUpdateRequest, tags...)
}

func (app *BaseApp) OnRecordDeleteRequest(tags ...string) *hook.TaggedHook[*RecordRequestEvent] {
	return hook.NewTaggedHook(app.hooks.onRecordDeleteRequest, tags...)
}

func (app *BaseApp) OnCollectionsListRequest() *hook.Hook[*CollectionsListRequestEvent] {
	return app.hooks.onCollectionsListRequest
}

func (app *BaseApp) OnCollectionViewRequest() *hook.Hook[*CollectionRequestEvent] {
	return app.hooks.onCollectionViewRequest
}

func (app *BaseApp) OnCollectionCreateRequest() *hook.Hook[*CollectionRequestEvent] {
	return app.hooks.onCollectionCreateRequest
}

func (app *BaseApp) OnCollectionUpdateRequest() *hook.Hook[*CollectionRequestEvent] {
	return app.hooks.onCollectionUpdateRequest
}

func (app *BaseApp) OnCollectionDeleteRequest() *hook.Hook[*CollectionRequestEvent] {
	return app.hooks.onCollectionDeleteRequest
}

func (app *BaseApp) OnCollectionsImportRequest() *hook.Hook[*CollectionsImportRequestEvent] {
	return app.hooks.onCollectionsImportRequest
}

func (app *BaseApp) OnBatchRequest() *hook.Hook[*BatchRequestEvent] {
	return app.hooks.onBatchRequest
}
