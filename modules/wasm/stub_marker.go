//go:build no_wasm

package wasm

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("wasm", []string{"_wasm_kv", "_wasm_stats"}, []string{"TOKI_WASM", "TOKI_WASM_HTTP_ALLOW", "TOKI_WASM_CACHE_DIR"}, true)
}
