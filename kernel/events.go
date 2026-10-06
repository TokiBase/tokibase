package kernel

import (
	"context"

	"github.com/tokibase/tokibase/tools/filesystem"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/mailer"
)

type HookTagger interface {
	HookTags() []string
}

// -------------------------------------------------------------------

type baseModelEventData struct {
	Model Model
}

func (e *baseModelEventData) Tags() []string {
	if e.Model == nil {
		return nil
	}

	if ht, ok := e.Model.(HookTagger); ok {
		return ht.HookTags()
	}

	return []string{e.Model.TableName()}
}

// -------------------------------------------------------------------

type baseRecordEventData struct {
	Record *Record
}

func (e *baseRecordEventData) Tags() []string {
	if e.Record == nil {
		return nil
	}

	return e.Record.HookTags()
}

// -------------------------------------------------------------------

type baseCollectionEventData struct {
	Collection *Collection
}

// @todo consider storing the original collection name and use that as a tag
// to avoid the  ambiguity when the collection is being modified (#7613);
// for new collection also maybe return empty tags?
func (e *baseCollectionEventData) Tags() []string {
	if e.Collection == nil {
		return nil
	}

	tags := make([]string, 0, 2)

	if e.Collection.Id != "" {
		tags = append(tags, e.Collection.Id)
	}

	if e.Collection.Name != "" {
		tags = append(tags, e.Collection.Name)
	}

	return tags
}

// -------------------------------------------------------------------
// App events data
// -------------------------------------------------------------------

type BootstrapEvent struct {
	hook.Event
	App App
}

type TerminateEvent struct {
	hook.Event
	App       App
	IsRestart bool
}

type BackupEvent struct {
	hook.Event
	App     App
	Context context.Context
	Name    string   // the name of the backup to create/restore.
	Exclude []string // list of dir entries to exclude from the backup create/restore.
}

// -------------------------------------------------------------------
// Settings events data
// -------------------------------------------------------------------

type SettingsReloadEvent struct {
	hook.Event
	App App
}

// -------------------------------------------------------------------
// Mailer events data
// -------------------------------------------------------------------

type MailerEvent struct {
	hook.Event
	App App

	Mailer  mailer.Mailer
	Message *mailer.Message
}

type MailerRecordEvent struct {
	MailerEvent
	baseRecordEventData
	Meta map[string]any
}

// -------------------------------------------------------------------
// Filesystem events data
// -------------------------------------------------------------------

type FilesystemNewWriterEvent struct {
	hook.Event
	*filesystem.NewWriterEvent

	App App
}

type FilesystemDeleteEvent struct {
	hook.Event
	*filesystem.DeleteEvent

	App App
}

// -------------------------------------------------------------------
// Model events data
// -------------------------------------------------------------------

const (
	ModelEventTypeCreate   = "create"
	ModelEventTypeUpdate   = "update"
	ModelEventTypeDelete   = "delete"
	ModelEventTypeValidate = "validate"
)

type ModelEvent struct {
	hook.Event
	App App
	baseModelEventData
	Context context.Context

	// Could be any of the ModelEventType* constants, like:
	// - create
	// - update
	// - delete
	// - validate
	Type string
}

type ModelErrorEvent struct {
	Error error
	ModelEvent
}

// -------------------------------------------------------------------
// Record events data
// -------------------------------------------------------------------

type RecordEvent struct {
	hook.Event
	App App
	baseRecordEventData
	Context context.Context

	// Could be any of the ModelEventType* constants, like:
	// - create
	// - update
	// - delete
	// - validate
	Type string
}

type RecordErrorEvent struct {
	Error error
	RecordEvent
}

func syncModelEventWithRecordEvent(me *ModelEvent, re *RecordEvent) {
	me.App = re.App
	me.Context = re.Context
	me.Type = re.Type

	// @todo enable if after profiling doesn't have significant impact
	// 		 skip for now to avoid excessive checks and assume that the
	// 		 Model and the Record fields still points to the same instance
	//
	// if _, ok := me.Model.(*Record); ok {
	// 	me.Model = re.Record
	// } else if proxy, ok := me.Model.(RecordProxy); ok {
	// 	proxy.SetProxyRecord(re.Record)
	// }
}

func syncRecordEventWithModelEvent(re *RecordEvent, me *ModelEvent) {
	re.App = me.App
	re.Context = me.Context
	re.Type = me.Type
}

func newRecordEventFromModelEvent(me *ModelEvent) (*RecordEvent, bool) {
	record, ok := me.Model.(*Record)
	if !ok {
		proxy, ok := me.Model.(RecordProxy)
		if !ok {
			return nil, false
		}
		record = proxy.ProxyRecord()
	}

	re := new(RecordEvent)
	re.App = me.App
	re.Context = me.Context
	re.Type = me.Type
	re.Record = record

	return re, true
}

func newRecordErrorEventFromModelErrorEvent(me *ModelErrorEvent) (*RecordErrorEvent, bool) {
	recordEvent, ok := newRecordEventFromModelEvent(&me.ModelEvent)
	if !ok {
		return nil, false
	}

	re := new(RecordErrorEvent)
	re.RecordEvent = *recordEvent
	re.Error = me.Error

	return re, true
}

func syncModelErrorEventWithRecordErrorEvent(me *ModelErrorEvent, re *RecordErrorEvent) {
	syncModelEventWithRecordEvent(&me.ModelEvent, &re.RecordEvent)
	me.Error = re.Error
}

func syncRecordErrorEventWithModelErrorEvent(re *RecordErrorEvent, me *ModelErrorEvent) {
	syncRecordEventWithModelEvent(&re.RecordEvent, &me.ModelEvent)
	re.Error = me.Error
}

// -------------------------------------------------------------------
// Collection events data
// -------------------------------------------------------------------

type CollectionEvent struct {
	hook.Event
	App App
	baseCollectionEventData
	Context context.Context

	// Could be any of the ModelEventType* constants, like:
	// - create
	// - update
	// - delete
	// - validate
	Type string
}

type CollectionErrorEvent struct {
	Error error
	CollectionEvent
}

func syncModelEventWithCollectionEvent(me *ModelEvent, ce *CollectionEvent) {
	me.App = ce.App
	me.Context = ce.Context
	me.Type = ce.Type
	me.Model = ce.Collection
}

func syncCollectionEventWithModelEvent(ce *CollectionEvent, me *ModelEvent) {
	ce.App = me.App
	ce.Context = me.Context
	ce.Type = me.Type
	if c, ok := me.Model.(*Collection); ok {
		ce.Collection = c
	}
}

func newCollectionEventFromModelEvent(me *ModelEvent) (*CollectionEvent, bool) {
	record, ok := me.Model.(*Collection)
	if !ok {
		return nil, false
	}

	ce := new(CollectionEvent)
	ce.App = me.App
	ce.Context = me.Context
	ce.Type = me.Type
	ce.Collection = record

	return ce, true
}

func newCollectionErrorEventFromModelErrorEvent(me *ModelErrorEvent) (*CollectionErrorEvent, bool) {
	collectionevent, ok := newCollectionEventFromModelEvent(&me.ModelEvent)
	if !ok {
		return nil, false
	}

	ce := new(CollectionErrorEvent)
	ce.CollectionEvent = *collectionevent
	ce.Error = me.Error

	return ce, true
}

func syncModelErrorEventWithCollectionErrorEvent(me *ModelErrorEvent, ce *CollectionErrorEvent) {
	syncModelEventWithCollectionEvent(&me.ModelEvent, &ce.CollectionEvent)
	me.Error = ce.Error
}

func syncCollectionErrorEventWithModelErrorEvent(ce *CollectionErrorEvent, me *ModelErrorEvent) {
	syncCollectionEventWithModelEvent(&ce.CollectionEvent, &me.ModelEvent)
	ce.Error = me.Error
}

// -------------------------------------------------------------------
// File API events data
// -------------------------------------------------------------------

// -------------------------------------------------------------------
// Collection API events data
// -------------------------------------------------------------------

// -------------------------------------------------------------------
// Realtime API events data
// -------------------------------------------------------------------

// -------------------------------------------------------------------
// Record CRUD API events data
// -------------------------------------------------------------------

type RecordEnrichEvent struct {
	hook.Event
	App App
	baseRecordEventData

	RequestInfo *RequestInfo
}

// -------------------------------------------------------------------
// Auth Record API events data
// -------------------------------------------------------------------
