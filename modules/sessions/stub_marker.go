//go:build no_sessions

package sessions

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("sessions", []string{"_sessions"}, []string{"TOKI_SESSIONS", "TOKI_SESSIONS_ROTATE"}, true)
}
