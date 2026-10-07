//go:build no_passkey

package passkey

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("passkey", []string{"_passkeys", "_passkey_challenges"}, nil, true)
}
