package apis

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/router"
)

// ReplayTimeout bounds one replay transaction (the batch default of 3 s is far
// too small for a tx group of a long offline period).
const ReplayTimeout = 60 * time.Second

// ReplayRecordRequests executes record create/update/delete InternalRequests as
// auth inside ONE transaction (the batch processor, RequestInfo context
// "sync"), triggering OnBatchRequest (batchguard) when len(reqs) > 1. The
// values of ctx (kernel.SyncOrigin) reach the model hooks. It is the hub apply
// path of modules/sync (docs/SYNC_DESIGN.md §5.1).
//
// When a sub-request fails, the error is the *router.ApiError of the failed
// request (its status and RawData tell a rule denial from a validation error).
func ReplayRecordRequests(ctx context.Context, app core.App, auth *core.Record,
	headers map[string]string, reqs []*core.InternalRequest) ([]*BatchRequestResult, error) {
	return ReplayRecordRequestsFrom(ctx, app, auth, "", headers, reqs)
}

// ReplayRecordRequestsFrom is ReplayRecordRequests with the remote address of
// the synthetic request (the last known IP of the node), which feeds
// RealIP() and the rule engine.
func ReplayRecordRequestsFrom(ctx context.Context, app core.App, auth *core.Record, remoteAddr string,
	headers map[string]string, reqs []*core.InternalRequest) ([]*BatchRequestResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/batch", nil)
	if err != nil {
		return nil, err
	}
	r.RequestURI = "/api/batch"
	r.Host = "localhost"
	if remoteAddr != "" {
		if _, _, err := net.SplitHostPort(remoteAddr); err != nil {
			remoteAddr = net.JoinHostPort(remoteAddr, "0")
		}
		r.RemoteAddr = remoteAddr
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	base := &core.RequestEvent{}
	base.App = app
	base.Auth = auth
	base.Request = r
	base.Response = &router.ResponseWriter{ResponseWriter: httptest.NewRecorder()}

	var results []*BatchRequestResult
	run := func(a core.App, e *core.RequestEvent) error {
		bp := batchProcessor{app: a, baseEvent: e, infoContext: core.RequestInfoContextSync}
		if err := bp.Process(reqs, ReplayTimeout); err != nil {
			return err
		}
		results = bp.results
		return nil
	}

	if len(reqs) == 1 {
		// a single change is not a batch: no OnBatchRequest handlers (batchguard)
		err = run(app, base)
	} else {
		event := new(core.BatchRequestEvent)
		event.RequestEvent = base
		event.Batch = reqs
		err = app.OnBatchRequest().Trigger(event, func(e *core.BatchRequestEvent) error {
			if err := run(e.App, e.RequestEvent); err != nil {
				return err
			}
			// batchguard reads the buffered response (assert_post)
			return e.JSON(http.StatusOK, results)
		})
	}
	if err != nil {
		return nil, unwrapReplayError(err)
	}
	return results, nil
}

// unwrapReplayError returns the ApiError of the failed sub-request when err is
// the batch error envelope, else err itself.
func unwrapReplayError(err error) error {
	var found *router.ApiError
	var walk func(error)
	walk = func(e error) {
		switch v := e.(type) {
		case validation.Errors:
			for _, x := range v {
				walk(x)
			}
		case *BatchResponseError:
			if found == nil {
				found = v.err
			}
		}
	}
	var ve validation.Errors
	if errors.As(err, &ve) {
		walk(ve)
	}
	if found != nil {
		return found
	}
	return err
}

// EnrichRecordsForInfo runs the OnRecordEnrich hooks (fieldperm read rules and
// friends) over records for the given request info, as the record API does
// before a record leaves the server. modules/sync uses it to filter what a
// node may receive.
func EnrichRecordsForInfo(app core.App, info *core.RequestInfo, records ...*core.Record) error {
	if len(records) == 0 {
		return nil
	}
	return triggerRecordEnrichHooks(app, info, records, nil)
}
