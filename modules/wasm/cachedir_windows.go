//go:build !no_wasm && windows

package wasm

import "os"

func checkOwner(os.FileInfo) error { return nil }
