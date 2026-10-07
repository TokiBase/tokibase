//go:build !no_wasm

package wasm

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/tetratelabs/wazero"
)

// openCompilationCache returns wazero's compilation cache. With dir set it is
// the on-disk cache, which stores native machine code that is mapped and
// executed without being verified again: anyone who can write the directory
// gets code execution in the server. The directory is therefore created 0700
// and refused (falling back to the in-memory cache, logged at WARN) when it is
// a symlink, not a directory, group/world writable or owned by someone else.
func openCompilationCache(dir string, log *slog.Logger) wazero.CompilationCache {
	if dir == "" {
		return wazero.NewCompilationCache()
	}
	if err := checkCacheDir(dir); err != nil {
		log.Warn("wasm: TOKI_WASM_CACHE_DIR rejected, using the in-memory compilation cache", "dir", dir, "error", err)
		return wazero.NewCompilationCache()
	}
	c, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		log.Warn("wasm: cannot open the compilation cache dir, using the in-memory cache", "dir", dir, "error", err)
		return wazero.NewCompilationCache()
	}
	return c
}

func checkCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	return checkOwner(fi)
}
