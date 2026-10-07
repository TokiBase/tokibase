//go:build !wasip1

package toki

import "errors"

// On non-WASI targets the host calls are unavailable; the package still
// compiles so guest logic can be unit tested natively.
var errNotWASI = errors.New("toki: host calls require GOOS=wasip1")

func hostRecordsFind(uint32, uint32) uint64   { return 0 }
func hostRecordsSave(uint32, uint32) uint64   { return 0 }
func hostRecordsDelete(uint32, uint32) uint64 { return 0 }
func hostHTTPFetch(uint32, uint32) uint64     { return 0 }
func hostMailSend(uint32, uint32) uint64      { return 0 }
func hostKVGet(uint32, uint32) uint64         { return 0 }
func hostKVSet(uint32, uint32) uint64         { return 0 }
func hostJobsEnqueue(uint32, uint32) uint64   { return 0 }

func callRaw(fn func(ptr, n uint32) uint64, req []byte) ([]byte, error) {
	return nil, errNotWASI
}

func hostLog(level int, msg string) {}
