package apis

import (
	"net/http"
	"slices"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/walreplica"
	"github.com/tokibase/tokibase/tools/router"
)

// bindHealthApi registers the health api endpoint.
func bindHealthApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	subGroup := rg.Group("/health")
	subGroup.GET("", healthCheck)
}

// healthCheck returns a 200 OK response if the server is healthy.
func healthCheck(e *core.RequestEvent) error {
	resp := struct {
		Message string         `json:"message"`
		Code    int            `json:"code"`
		Data    map[string]any `json:"data"`
	}{
		Code:    http.StatusOK,
		Message: "API is healthy.",
	}

	// @todo evaluate whether it is worth removing the extra info from the health endpoint
	if e.HasSuperuserAuth() {
		resp.Data = make(map[string]any, 3)
		resp.Data["canBackup"] = !e.App.Store().Has(core.StoreKeyActiveBackup)
		resp.Data["realIP"] = e.RealIP()

		// loosely check if behind a reverse proxy
		// (usually used in the dashboard to remind superusers in case deployed behind reverse-proxy)
		possibleProxyHeader := ""
		headersToCheck := append(
			slices.Clone(e.App.Settings().TrustedProxy.Headers),
			// common proxy headers
			"CF-Connecting-IP", "Fly-Client-IP", "X-Forwarded-For",
		)
		for _, header := range headersToCheck {
			if e.Request.Header.Get(header) != "" {
				possibleProxyHeader = header
				break
			}
		}
		resp.Data["possibleProxyHeader"] = possibleProxyHeader

		// only present when WAL replication is active (modules/walreplica)
		if walreplica.Active(e.App) {
			healthy, reason := walreplica.Healthy(e.App)
			replica := map[string]any{
				"healthy":   healthy,
				"databases": walreplica.Status(e.App),
			}
			if reason != "" {
				replica["reason"] = reason
			}
			if lease := walreplica.LeaseInfo(e.App); lease != nil {
				replica["lease"] = lease
			}
			resp.Data["replica"] = replica
		}
	} else {
		resp.Data = map[string]any{} // ensure that it is returned as object
	}

	return e.JSON(http.StatusOK, resp)
}
