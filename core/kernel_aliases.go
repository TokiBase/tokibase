// Aliases for the identifiers that moved to the kernel package.

package core

import (
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/store/sqlite"
)

// This file keeps the former core package API by aliasing the identifiers
// that moved to the HTTP-free kernel package (see docs/PHASE0_KERNEL_SPLIT.md).

// Types (aliases, so core.Record and kernel.Record are the same type).
type (
	AppWrapper               = kernel.AppWrapper
	AuthAlertConfig          = kernel.AuthAlertConfig
	AuthOrigin               = kernel.AuthOrigin
	AutodateField            = kernel.AutodateField
	BackupEvent              = kernel.BackupEvent
	BackupsConfig            = kernel.BackupsConfig
	BaseAppConfig            = kernel.BaseAppConfig
	BaseModel                = kernel.BaseModel
	BaseRecordProxy          = kernel.BaseRecordProxy
	BatchConfig              = kernel.BatchConfig
	BoolField                = kernel.BoolField
	BootstrapEvent           = kernel.BootstrapEvent
	Collection               = kernel.Collection
	CollectionErrorEvent     = kernel.CollectionErrorEvent
	CollectionEvent          = kernel.CollectionEvent
	DBConfig                 = kernel.DBConfig
	DBConn                   = kernel.DBConn
	DBConnectFunc            = kernel.DBConnectFunc
	DBOpener                 = kernel.DBOpener
	DBStatus                 = kernel.DBStatus
	WALStatus                = kernel.WALStatus
	DBExporter               = kernel.DBExporter
	DateField                = kernel.DateField
	DriverValuer             = kernel.DriverValuer
	DryRunViewResult         = kernel.DryRunViewResult
	EditorField              = kernel.EditorField
	EmailField               = kernel.EmailField
	EmailTemplate            = kernel.EmailTemplate
	ExpandFetchFunc          = kernel.ExpandFetchFunc
	ExternalAuth             = kernel.ExternalAuth
	Field                    = kernel.Field
	FieldFactoryFunc         = kernel.FieldFactoryFunc
	FieldsList               = kernel.FieldsList
	FileField                = kernel.FileField
	FilesManager             = kernel.FilesManager
	FilesystemDeleteEvent    = kernel.FilesystemDeleteEvent
	FilesystemNewWriterEvent = kernel.FilesystemNewWriterEvent
	GeoPointField            = kernel.GeoPointField
	GetterFinder             = kernel.GetterFinder
	GetterFunc               = kernel.GetterFunc
	HookTagger               = kernel.HookTagger
	JSONField                = kernel.JSONField
	Log                      = kernel.Log
	LogsConfig               = kernel.LogsConfig
	LogsStatsItem            = kernel.LogsStatsItem
	MFA                      = kernel.MFA
	MFAConfig                = kernel.MFAConfig
	MailClientFactory        = kernel.MailClientFactory
	MailerEvent              = kernel.MailerEvent
	MailerRecordEvent        = kernel.MailerRecordEvent
	MaxBodySizeCalculator    = kernel.MaxBodySizeCalculator
	MetaConfig               = kernel.MetaConfig
	Migration                = kernel.Migration
	MigrationsList           = kernel.MigrationsList
	MigrationsRunner         = kernel.MigrationsRunner
	Model                    = kernel.Model
	ModelErrorEvent          = kernel.ModelErrorEvent
	ModelEvent               = kernel.ModelEvent
	MultiValuer              = kernel.MultiValuer
	NumberField              = kernel.NumberField
	OAuth2Config             = kernel.OAuth2Config
	OAuth2KnownFields        = kernel.OAuth2KnownFields
	OAuth2ProviderConfig     = kernel.OAuth2ProviderConfig
	OAuth2ProviderRegistry   = kernel.OAuth2ProviderRegistry
	OTP                      = kernel.OTP
	OTPConfig                = kernel.OTPConfig
	Param                    = kernel.Param
	PasswordAuthConfig       = kernel.PasswordAuthConfig
	PasswordField            = kernel.PasswordField
	PasswordFieldValue       = kernel.PasswordFieldValue
	PostValidator            = kernel.PostValidator
	PreValidator             = kernel.PreValidator
	RateLimitRule            = kernel.RateLimitRule
	RateLimitsConfig         = kernel.RateLimitsConfig
	Record                   = kernel.Record
	RecordEnrichEvent        = kernel.RecordEnrichEvent
	RecordErrorEvent         = kernel.RecordErrorEvent
	RecordEvent              = kernel.RecordEvent
	RecordFieldResolver      = kernel.RecordFieldResolver
	RecordInterceptor        = kernel.RecordInterceptor
	RecordProxy              = kernel.RecordProxy
	RelationField            = kernel.RelationField
	RequestInfo              = kernel.RequestInfo
	S3Config                 = kernel.S3Config
	S3FilesystemFactory      = kernel.S3FilesystemFactory
	SMTPConfig               = kernel.SMTPConfig
	SelectField              = kernel.SelectField
	SetterFinder             = kernel.SetterFinder
	SetterFunc               = kernel.SetterFunc
	Settings                 = kernel.Settings
	SettingsReloadEvent      = kernel.SettingsReloadEvent
	TableInfoRow             = kernel.TableInfoRow
	TerminateEvent           = kernel.TerminateEvent
	TextField                = kernel.TextField
	TokenConfig              = kernel.TokenConfig
	TrustedProxyConfig       = kernel.TrustedProxyConfig
	TxAppInfo                = kernel.TxAppInfo
	URLField                 = kernel.URLField
)

// Functions.
var (
	DefaultDBConnect               = sqlite.DefaultConnect
	DefaultFieldHelpValidationRule = kernel.DefaultFieldHelpValidationRule
	DefaultFieldIdValidationRule   = kernel.DefaultFieldIdValidationRule
	DefaultFieldNameValidationRule = kernel.DefaultFieldNameValidationRule
	GenerateDefaultRandomId        = kernel.GenerateDefaultRandomId
	GenerateBackupName             = kernel.GenerateBackupName
	NewAuthCollection              = kernel.NewAuthCollection
	NewAuthOrigin                  = kernel.NewAuthOrigin
	NewBaseCollection              = kernel.NewBaseCollection
	NewCollection                  = kernel.NewCollection
	NewExternalAuth                = kernel.NewExternalAuth
	NewFieldsList                  = kernel.NewFieldsList
	NewMFA                         = kernel.NewMFA
	NewMigrationsRunner            = kernel.NewMigrationsRunner
	NewOTP                         = kernel.NewOTP
	NewRecord                      = kernel.NewRecord
	NewRecordFieldResolver         = kernel.NewRecordFieldResolver
	NewViewCollection              = kernel.NewViewCollection
)

// Variables.
var (
	DefaultIdRegex           = kernel.DefaultIdRegex
	ErrInvalidFieldValue     = kernel.ErrInvalidFieldValue
	ErrMissingSigningKey     = kernel.ErrMissingSigningKey
	ErrMustBeSystem          = kernel.ErrMustBeSystem
	ErrMustBeSystemAndHidden = kernel.ErrMustBeSystemAndHidden
	ErrNotAuthRecord         = kernel.ErrNotAuthRecord
	ErrUnknownField          = kernel.ErrUnknownField
	Fields                   = kernel.Fields
	SystemDynamicFieldNames  = kernel.SystemDynamicFieldNames

	// AppMigrations and SystemMigrations point to the kernel lists (pointers, so they are shared).
	AppMigrations    = &kernel.AppMigrations
	SystemMigrations = &kernel.SystemMigrations
)

// Constants.
const (
	CollectionNameAuthOrigins         = kernel.CollectionNameAuthOrigins
	CollectionNameExternalAuths       = kernel.CollectionNameExternalAuths
	CollectionNameMFAs                = kernel.CollectionNameMFAs
	CollectionNameOTPs                = kernel.CollectionNameOTPs
	CollectionNameSuperusers          = kernel.CollectionNameSuperusers
	CollectionTypeAuth                = kernel.CollectionTypeAuth
	CollectionTypeBase                = kernel.CollectionTypeBase
	CollectionTypeView                = kernel.CollectionTypeView
	DefaultAuxMaxIdleConns            = kernel.DefaultAuxMaxIdleConns
	DefaultAuxMaxOpenConns            = kernel.DefaultAuxMaxOpenConns
	DefaultDataMaxIdleConns           = kernel.DefaultDataMaxIdleConns
	DefaultDataMaxOpenConns           = kernel.DefaultDataMaxOpenConns
	DefaultEditorFieldMaxSize         = kernel.DefaultEditorFieldMaxSize
	DefaultFileFieldMaxSize           = kernel.DefaultFileFieldMaxSize
	DefaultIdAlphabet                 = kernel.DefaultIdAlphabet
	DefaultIdLength                   = kernel.DefaultIdLength
	DefaultInstallerEmail             = kernel.DefaultInstallerEmail
	DefaultJSONFieldMaxSize           = kernel.DefaultJSONFieldMaxSize
	DefaultMigrationsTable            = kernel.DefaultMigrationsTable
	DefaultQueryTimeout               = kernel.DefaultQueryTimeout
	EmailPlaceholderAlertInfo         = kernel.EmailPlaceholderAlertInfo
	EmailPlaceholderAppName           = kernel.EmailPlaceholderAppName
	EmailPlaceholderAppURL            = kernel.EmailPlaceholderAppURL
	EmailPlaceholderOTP               = kernel.EmailPlaceholderOTP
	EmailPlaceholderOTPId             = kernel.EmailPlaceholderOTPId
	EmailPlaceholderToken             = kernel.EmailPlaceholderToken
	FieldNameCollectionId             = kernel.FieldNameCollectionId
	FieldNameCollectionName           = kernel.FieldNameCollectionName
	FieldNameEmail                    = kernel.FieldNameEmail
	FieldNameEmailVisibility          = kernel.FieldNameEmailVisibility
	FieldNameExpand                   = kernel.FieldNameExpand
	FieldNameId                       = kernel.FieldNameId
	FieldNamePassword                 = kernel.FieldNamePassword
	FieldNameTokenKey                 = kernel.FieldNameTokenKey
	FieldNameVerified                 = kernel.FieldNameVerified
	FieldTypeAutodate                 = kernel.FieldTypeAutodate
	FieldTypeBool                     = kernel.FieldTypeBool
	FieldTypeDate                     = kernel.FieldTypeDate
	FieldTypeEditor                   = kernel.FieldTypeEditor
	FieldTypeEmail                    = kernel.FieldTypeEmail
	FieldTypeFile                     = kernel.FieldTypeFile
	FieldTypeGeoPoint                 = kernel.FieldTypeGeoPoint
	FieldTypeJSON                     = kernel.FieldTypeJSON
	FieldTypeNumber                   = kernel.FieldTypeNumber
	FieldTypePassword                 = kernel.FieldTypePassword
	FieldTypeRelation                 = kernel.FieldTypeRelation
	FieldTypeSelect                   = kernel.FieldTypeSelect
	FieldTypeText                     = kernel.FieldTypeText
	FieldTypeURL                      = kernel.FieldTypeURL
	InterceptorActionAfterCreate      = kernel.InterceptorActionAfterCreate
	InterceptorActionAfterCreateError = kernel.InterceptorActionAfterCreateError
	InterceptorActionAfterDelete      = kernel.InterceptorActionAfterDelete
	InterceptorActionAfterDeleteError = kernel.InterceptorActionAfterDeleteError
	InterceptorActionAfterUpdate      = kernel.InterceptorActionAfterUpdate
	InterceptorActionAfterUpdateError = kernel.InterceptorActionAfterUpdateError
	InterceptorActionCreate           = kernel.InterceptorActionCreate
	InterceptorActionCreateExecute    = kernel.InterceptorActionCreateExecute
	InterceptorActionDelete           = kernel.InterceptorActionDelete
	InterceptorActionDeleteExecute    = kernel.InterceptorActionDeleteExecute
	InterceptorActionUpdate           = kernel.InterceptorActionUpdate
	InterceptorActionUpdateExecute    = kernel.InterceptorActionUpdateExecute
	InterceptorActionValidate         = kernel.InterceptorActionValidate
	LocalAutocertCacheDirName         = kernel.LocalAutocertCacheDirName
	LocalBackupsDirName               = kernel.LocalBackupsDirName
	LocalNotifyDirName                = kernel.LocalNotifyDirName
	LocalStorageDirName               = kernel.LocalStorageDirName
	LocalTempDirName                  = kernel.LocalTempDirName
	LogsTableName                     = kernel.LogsTableName
	MFAMethodOAuth2                   = kernel.MFAMethodOAuth2
	MFAMethodOTP                      = kernel.MFAMethodOTP
	MFAMethodPassword                 = kernel.MFAMethodPassword
	ModelEventTypeCreate              = kernel.ModelEventTypeCreate
	ModelEventTypeDelete              = kernel.ModelEventTypeDelete
	ModelEventTypeUpdate              = kernel.ModelEventTypeUpdate
	ModelEventTypeValidate            = kernel.ModelEventTypeValidate
	RateLimitRuleAudienceAll          = kernel.RateLimitRuleAudienceAll
	RateLimitRuleAudienceAuth         = kernel.RateLimitRuleAudienceAuth
	RateLimitRuleAudienceGuest        = kernel.RateLimitRuleAudienceGuest
	RequestInfoContextBatch           = kernel.RequestInfoContextBatch
	RequestInfoContextSync            = kernel.RequestInfoContextSync
	RequestInfoContextDefault         = kernel.RequestInfoContextDefault
	RequestInfoContextExpand          = kernel.RequestInfoContextExpand
	RequestInfoContextOAuth2          = kernel.RequestInfoContextOAuth2
	RequestInfoContextOTP             = kernel.RequestInfoContextOTP
	RequestInfoContextPasswordAuth    = kernel.RequestInfoContextPasswordAuth
	RequestInfoContextProtectedFile   = kernel.RequestInfoContextProtectedFile
	RequestInfoContextRealtime        = kernel.RequestInfoContextRealtime
	StoreKeyActiveBackup              = kernel.StoreKeyActiveBackup
	StoreKeyCachedCollections         = kernel.StoreKeyCachedCollections
	TokenClaimCollectionId            = kernel.TokenClaimCollectionId
	TokenClaimEmail                   = kernel.TokenClaimEmail
	TokenClaimId                      = kernel.TokenClaimId
	TokenClaimNewEmail                = kernel.TokenClaimNewEmail
	TokenClaimRefreshable             = kernel.TokenClaimRefreshable
	TokenClaimType                    = kernel.TokenClaimType
	TokenTypeAuth                     = kernel.TokenTypeAuth
	TokenTypeEmailChange              = kernel.TokenTypeEmailChange
	TokenTypeFile                     = kernel.TokenTypeFile
	TokenTypePasswordReset            = kernel.TokenTypePasswordReset
	TokenTypeVerification             = kernel.TokenTypeVerification
)
