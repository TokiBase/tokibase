//go:build !no_sync

package sync

import (
	"strconv"
	stdsync "sync"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/subscriptions"
	"strings"
)

// notifier wakes the long-polls of /pull: wait returns a channel that is
// closed by the next fire.
type notifier struct {
	mu stdsync.Mutex
	ch chan struct{}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

func (n *notifier) fire() {
	n.mu.Lock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
	n.mu.Unlock()
}

// headSeq is the highest hub seq in `_changes`.
func (m *Module) headSeq() int64 {
	var h int64
	_ = m.app.DB().NewQuery("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='_changes'),0)").Row(&h)
	return h
}

// notifyHead wakes the long-polls and pokes the online spokes after a
// committed apply batch.
func (m *Module) notifyHead() {
	m.notify.fire()
	m.poke(m.headSeq())
}

// poke broadcasts {"seq": N} on the realtime topic "@sync". The payload carries
// no record data. Subscribing to the topic needs a node session token (see
// bindHubNotify), so only enrolled nodes receive it at all.
func (m *Module) poke(seq int64) {
	msg := subscriptions.Message{Name: proto.Topic, Data: []byte(`{"seq":` + strconv.FormatInt(seq, 10) + `}`)}
	for _, c := range m.app.SubscriptionsBroker().Clients() {
		if c.HasSubscription(proto.Topic) {
			go c.Send(msg)
		}
	}
}

func topicName(sub string) string {
	if i := strings.IndexByte(sub, '?'); i >= 0 {
		return sub[:i]
	}
	return sub
}

// bindHubNotify (hub only) wakes pulls and pokes spokes after hub-local
// writes, and restricts the "@sync" topic to node sessions.
func (m *Module) bindHubNotify() {
	if m.role != RoleHub {
		return
	}
	after := func(e *core.RecordEvent) error {
		err := e.Next()
		if o := kernel.SyncOriginFrom(e.Context); o != nil && o.Mode == kernel.SyncModePush {
			return err // the push handler notifies once per batch
		}
		if m.ready.Load() {
			if p, _ := m.pol.For(e.Record.Collection()); p != nil {
				m.notifyHead()
			}
		}
		return err
	}
	for _, b := range []func(...string) *hook.TaggedHook[*core.RecordEvent]{
		m.app.OnRecordAfterCreateSuccess, m.app.OnRecordAfterUpdateSuccess, m.app.OnRecordAfterDeleteSuccess,
	} {
		b().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "notify", Priority: 1000, Func: after})
	}
	m.app.OnRealtimeSubscribeRequest().Bind(&hook.Handler[*core.RealtimeSubscribeRequestEvent]{
		Id: hookId + "topic", Priority: -1000,
		Func: func(e *core.RealtimeSubscribeRequestEvent) error {
			for _, s := range e.Subscriptions {
				if topicName(s) != proto.Topic {
					continue
				}
				if _, status, _, msg := m.sessionNode(e.App, e.Request.Header.Get("Authorization")); status != 0 {
					return e.ForbiddenError(msg, nil)
				}
				break
			}
			return e.Next()
		},
	})
}
