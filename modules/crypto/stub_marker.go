//go:build no_crypto

package crypto

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("crypto", []string{"_crypto_fields", "_crypto_keys", "_crypto_index"}, []string{"TOKI_CRYPTO_MASTER_KEY", "TOKI_CRYPTO_MASTER_KEY_FILE"}, true)
}
