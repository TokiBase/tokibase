//go:build !no_sync

package sync

import stdsync "sync"

// Audit actions emitted by the module.
const (
	AuditNodeEnroll      = "sync.node.enroll"
	AuditNodeRevoke      = "sync.node.revoke"
	AuditHandshakeFailed = "sync.handshake.failed"
)

var (
	auditMu   stdsync.RWMutex
	auditSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects sync events to an external audit log. Modules must not
// import each other, so the wiring happens in tokibase.go (same pattern as
// mcp.SetAuditSink). Details set `cli: true` for actions of the command line.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	auditMu.Lock()
	auditSink = fn
	auditMu.Unlock()
}

func emit(action, collection, record string, details map[string]any) {
	auditMu.RLock()
	fn := auditSink
	auditMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}
