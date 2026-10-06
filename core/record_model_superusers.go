package core

import (
	"fmt"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

// registerSuperuserHooks registers the superusers hooks that depend on the
// server (HTTP) error types and therefore can't be part of the kernel.
func (app *BaseApp) registerSuperuserHooks() {
	app.OnRecordDelete(kernel.CollectionNameSuperusers).Bind(&hook.Handler[*kernel.RecordEvent]{
		Id: "pbSuperusersRecordDelete",
		Func: func(e *kernel.RecordEvent) error {
			originalApp := e.App
			txErr := e.App.RunInTransaction(func(txApp kernel.App) error {
				e.App = txApp

				total, err := e.App.CountRecords(kernel.CollectionNameSuperusers)
				if err != nil {
					return fmt.Errorf("failed to fetch total superusers count: %w", err)
				}

				if total == 1 {
					return router.NewBadRequestError("You can't delete the only existing superuser", nil)
				}

				return e.Next()
			})
			e.App = originalApp

			return txErr
		},
		Priority: -99,
	})
}
