//go:build !no_wasm && !windows

package wasm

import (
	"fmt"
	"os"
	"syscall"
)

func checkOwner(fi os.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("directory is group or world writable (mode %o), chmod 700 it", fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("directory is owned by uid %d, not by the server user (%d)", st.Uid, os.Getuid())
	}
	return nil
}
