package core

import (
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/filesystem/fshttp"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/mailer"
	"github.com/tokibase/tokibase/tools/mailer/clients"
)

// ensures that the BaseApp implements the App interface.
var _ App = (*BaseApp)(nil)

// BaseApp implements core.App and defines the base PocketBase app structure.
//
// It embeds the HTTP-free [kernel.BaseApp] and adds the server specific hooks
// (OnServe and the *Request hooks).
type BaseApp struct {
	*kernel.BaseApp

	hooks *requestHooks
}

// NewBaseApp creates and returns a new BaseApp instance
// configured with the provided arguments.
//
// To initialize the app, you need to call `app.Bootstrap()`.
func NewBaseApp(config BaseAppConfig) *BaseApp {
	// wire the net/http dependent drivers that the kernel can't import
	if config.MailClientFactory == nil {
		config.MailClientFactory = newMailClient
	}
	if config.S3FilesystemFactory == nil {
		config.S3FilesystemFactory = fshttp.NewS3
	}

	app := &BaseApp{
		BaseApp: kernel.NewBaseApp(config),
		hooks:   newRequestHooks(),
	}

	// expose the outer app to the kernel hook handlers and transaction callbacks
	app.BaseApp.SetOuter(app, wrapBaseApp)

	app.registerBaseHooks()

	return app
}

// wrapBaseApp builds the outer app for the shallow clones of the kernel.BaseApp
// (transaction apps, UnsafeWithoutHooks apps).
func wrapBaseApp(clone *kernel.BaseApp, prev kernel.App, withoutHooks bool) kernel.App {
	hooks := newRequestHooks()

	if !withoutHooks {
		if p, ok := prev.(*BaseApp); ok {
			hooks = p.hooks
		}
	}

	return &BaseApp{BaseApp: clone, hooks: hooks}
}

// AsApp returns the app as core.App (aka. with the server hooks available).
//
// It is needed because the kernel level callbacks and event App fields are
// typed as [kernel.App]. The returned value is the same app instance when it
// already implements core.App, otherwise it returns nil.
func AsApp(app kernel.App) App {
	if v, ok := app.(App); ok {
		return v
	}

	return nil
}

func newMailClient(settings *kernel.Settings) mailer.Mailer {
	if settings.SMTP.Enabled {
		return &clients.SMTPClient{
			Host:       settings.SMTP.Host,
			Port:       settings.SMTP.Port,
			Username:   settings.SMTP.Username,
			Password:   settings.SMTP.Password,
			TLS:        settings.SMTP.TLS,
			AuthMethod: settings.SMTP.AuthMethod,
			LocalName:  settings.SMTP.LocalName,
		}
	}

	return &clients.Sendmail{}
}

func (app *BaseApp) registerBaseHooks() {
	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__pbCronStart__",
		Func: func(e *ServeEvent) error {
			app.Cron().Start()
			return e.Next()
		},
		Priority: 999,
	})

	app.registerSuperuserHooks()
}
