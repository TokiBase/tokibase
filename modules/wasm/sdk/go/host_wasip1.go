//go:build wasip1

package toki

import (
	"errors"
	"unsafe"
)

//go:wasmimport toki records_find
func hostRecordsFind(ptr, n uint32) uint64

//go:wasmimport toki records_save
func hostRecordsSave(ptr, n uint32) uint64

//go:wasmimport toki records_delete
func hostRecordsDelete(ptr, n uint32) uint64

//go:wasmimport toki http_fetch
func hostHTTPFetch(ptr, n uint32) uint64

//go:wasmimport toki mail_send
func hostMailSend(ptr, n uint32) uint64

//go:wasmimport toki kv_get
func hostKVGet(ptr, n uint32) uint64

//go:wasmimport toki kv_set
func hostKVSet(ptr, n uint32) uint64

//go:wasmimport toki jobs_enqueue
func hostJobsEnqueue(ptr, n uint32) uint64

//go:wasmimport toki log
func hostLogRaw(level, ptr, n uint32)

// Buffers handed to the host stay referenced here until toki_free.
var live = map[uint32][]byte{}

//go:wasmexport toki_alloc
func tokiAlloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	b := make([]byte, size)
	p := uint32(uintptr(unsafe.Pointer(&b[0])))
	live[p] = b
	return p
}

//go:wasmexport toki_free
func tokiFree(ptr, size uint32) { delete(live, ptr) }

func callRaw(fn func(ptr, n uint32) uint64, req []byte) ([]byte, error) {
	if len(req) == 0 {
		req = []byte("{}")
	}
	r := fn(uint32(uintptr(unsafe.Pointer(&req[0]))), uint32(len(req)))
	if r == 0 {
		return nil, errors.New("host call failed")
	}
	ptr, n := uint32(r>>32), uint32(r)
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(nil), ptr)), n))
	tokiFree(ptr, n)
	return out, nil
}

func hostLog(level int, msg string) {
	b := []byte(msg)
	if len(b) == 0 {
		return
	}
	hostLogRaw(uint32(level), uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b)))
}
