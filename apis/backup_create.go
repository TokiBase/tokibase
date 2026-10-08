package apis

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/routine"
	"github.com/tokibase/tokibase/tools/types"
)

// storeKeyBackupJob holds the [backupJobStatus] of the last asynchronous backup.
const storeKeyBackupJob = "@tokiBackupJob"

// backupJobStatus is the state of an asynchronous backup (`POST /api/backups?async=true`).
type backupJobStatus struct {
	// State is "running", "done" or "failed".
	State      string         `json:"state"`
	Name       string         `json:"name"`
	StartedAt  types.DateTime `json:"startedAt"`
	FinishedAt types.DateTime `json:"finishedAt"`
	Error      string         `json:"error,omitempty"`
}

// wantsAsyncBackup reports whether the caller opted in to the 202 flow
// (query `async=true` or header `Prefer: respond-async`).
func wantsAsyncBackup(e *core.RequestEvent) bool {
	if v := strings.ToLower(e.Request.URL.Query().Get("async")); v == "1" || v == "true" {
		return true
	}

	return strings.Contains(strings.ToLower(e.Request.Header.Get("Prefer")), "respond-async")
}

// backupCreateAsync starts the backup in the background and answers 202 with
// the job status; poll `GET /api/backups/status` for the result.
//
// The job is a goroutine of this process (not a durable job): a backup that
// is interrupted by a restart is lost, exactly like the synchronous call.
func backupCreateAsync(e *core.RequestEvent, name string) error {
	if name == "" {
		name = core.GenerateBackupName(e.App, "pb_backup_")
	}

	app := e.App
	status := backupJobStatus{State: "running", Name: name, StartedAt: types.NowDateTime()}
	app.Store().Set(storeKeyBackupJob, status)

	routine.FireAndForget(func() {
		// no request context: the backup must outlive the HTTP request
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		err := app.CreateBackup(ctx, name)

		done := status
		done.FinishedAt = types.NowDateTime()
		if err != nil {
			done.State = "failed"
			done.Error = err.Error()
			app.Logger().Error("Async backup failed", "name", name, "error", err.Error())
		} else {
			done.State = "done"
		}
		app.Store().Set(storeKeyBackupJob, done)
	})

	return e.JSON(http.StatusAccepted, status)
}

// backupStatus returns the state of the last asynchronous backup (state "idle" if none was started).
func backupStatus(e *core.RequestEvent) error {
	status, ok := e.App.Store().Get(storeKeyBackupJob).(backupJobStatus)
	if !ok {
		status = backupJobStatus{State: "idle"}
	}

	return e.JSON(http.StatusOK, status)
}

func backupCreate(e *core.RequestEvent) error {
	if e.App.Store().Has(core.StoreKeyActiveBackup) {
		return e.BadRequestError("Try again later - another backup/restore process has already been started", nil)
	}

	form := new(backupCreateForm)
	form.app = e.App

	err := e.BindBody(form)
	if err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}

	err = form.validate()
	if err != nil {
		return e.BadRequestError("An error occurred while validating the submitted data.", err)
	}

	if wantsAsyncBackup(e) {
		return backupCreateAsync(e, form.Name)
	}

	err = e.App.CreateBackup(context.Background(), form.Name)
	if err != nil {
		return e.BadRequestError("Failed to create backup.", err)
	}

	// we don't retrieve the generated backup file because it may not be
	// available yet due to the eventually consistent nature of some S3 providers
	return e.NoContent(http.StatusNoContent)
}

// -------------------------------------------------------------------

var backupNameRegex = regexp.MustCompile(`^[a-z0-9_-]+\.zip$`)

type backupCreateForm struct {
	app core.App

	Name string `form:"name" json:"name"`
}

func (form *backupCreateForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(
			&form.Name,
			validation.Length(1, 150),
			validation.Match(backupNameRegex),
			validation.By(form.checkUniqueName),
		),
	)
}

func (form *backupCreateForm) checkUniqueName(value any) error {
	v, _ := value.(string)
	if v == "" {
		return nil // nothing to check
	}

	fsys, err := form.app.NewBackupsFilesystem()
	if err != nil {
		return err
	}
	defer fsys.Close()

	if exists, err := fsys.Exists(v); err != nil || exists {
		return validation.NewError("validation_backup_name_exists", "The backup file name is invalid or already exists.")
	}

	return nil
}
