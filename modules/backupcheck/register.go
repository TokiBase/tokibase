package backupcheck

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/routine"
)

const hookId = "__backupcheckCreate__"

// EnvVar disables the post-create verification hook when set to "off".
const EnvVar = "TOKI_BACKUP_VERIFY"

// VerifyTimeout bounds one automatic verification.
const VerifyTimeout = 30 * time.Minute

var pending sync.WaitGroup

// OnResult, when set, is called after every automatic verification
// (modules never import each other, so e.g. the audit module can be wired
// here by the root package).
var OnResult func(app kernel.App, r Report)

// Wait blocks until all in-flight automatic verifications finished (used by tests).
func Wait() { pending.Wait() }

// Register binds a hook on OnBackupCreate that verifies every successfully
// created backup asynchronously and logs the result. TOKI_BACKUP_VERIFY=off
// disables it.
func Register(app core.App) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvVar)), "off") {
		return
	}

	app.OnBackupCreate().Bind(&hook.Handler[*core.BackupEvent]{
		Id:       hookId,
		Priority: -1, // outer: post-Next code runs after the zip is uploaded
		Func: func(e *core.BackupEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			a, name := e.App, e.Name
			routine.FireAndForget(func() {
				ctx, cancel := context.WithTimeout(context.Background(), VerifyTimeout)
				defer cancel()

				r, _ := Verify(ctx, a, name)
				logResult(a, r)
				if OnResult != nil {
					OnResult(a, r)
				}
			}, &pending)

			return nil
		},
	})
}

func logResult(app kernel.App, r Report) {
	attrs := []any{
		slog.String("name", r.Name),
		slog.Int64("sizeBytes", r.SizeBytes),
		slog.Int("collections", r.Collections),
		slog.Int64("records", r.Records),
		slog.Int("liveCollections", r.LiveCollections),
		slog.Int64("liveRecords", r.LiveRecords),
		slog.Int("missingFiles", r.MissingFiles),
		slog.Int64("durationMs", r.Duration.Milliseconds()),
	}
	if r.OK() {
		app.Logger().Info("[backupcheck] backup verified", attrs...)
		return
	}
	attrs = append(attrs,
		slog.Bool("integrityOk", r.IntegrityOK),
		slog.Bool("quickCheckOk", r.QuickCheckOK),
		slog.String("error", r.Error),
	)
	app.Logger().Error("[backupcheck] backup verification FAILED", attrs...)
}
