package fieldperm

import (
	"regexp"
	"strings"
	"sync"

	"github.com/tokibase/tokibase/core"
)

// reqEntry caches, for the lifetime of one request, the result of read rules
// that do not depend on the record being read (they only reference
// @request.auth.*, other request context, macros and literals). Such a rule is
// evaluated once per (collection, field) instead of once per record.
type reqEntry struct {
	mu      sync.Mutex
	results map[string]bool
}

// requestEntry returns the cache entry of info. owner is true for the caller
// that created it, which must call releaseEntry when its enrich chain ends.
func (m *Module) requestEntry(info *core.RequestInfo) (*reqEntry, bool) {
	fresh := &reqEntry{results: map[string]bool{}}
	v, loaded := m.reqCache.LoadOrStore(info, fresh)
	return v.(*reqEntry), !loaded
}

func (m *Module) releaseEntry(info *core.RequestInfo) { m.reqCache.Delete(info) }

func (m *Module) evalRead(e *core.RecordEnrichEvent, entry *reqEntry, col *core.Collection, field, rule string) (bool, error) {
	if entry == nil || !requestOnly(rule) {
		return evalExisting(e.App, e.Record, e.RequestInfo, rule)
	}
	key := col.Id + "\x00" + field
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if ok, hit := entry.results[key]; hit {
		return ok, nil
	}
	ok, err := evalSubmitted(e.App, e.Record, e.RequestInfo, rule)
	if err != nil {
		return false, err
	}
	entry.results[key] = ok
	return ok, nil
}

var (
	reLiteral = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	reToken   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.@:])(@?[A-Za-z_][A-Za-z0-9_.:@]*)`)
)

var requestOnlyPrefixes = []string{
	"@request.auth.", "@request.context", "@request.method", "@request.headers.", "@request.query.",
}

var macros = map[string]bool{
	"@now": true, "@second": true, "@minute": true, "@hour": true, "@weekday": true, "@day": true,
	"@month": true, "@year": true, "@yesterday": true, "@tomorrow": true, "@todayStart": true,
	"@todayEnd": true, "@monthStart": true, "@monthEnd": true, "@yearStart": true, "@yearEnd": true,
}

// requestOnly reports whether rule can only depend on the request (never on
// the record): every identifier is @request.auth.*, @request.context/method/
// headers/query, a datetime macro, true/false/null. Anything else (a field
// name, @request.body.*, @collection.*) keeps per-record evaluation.
func requestOnly(rule string) bool {
	stripped := reLiteral.ReplaceAllString(rule, "''")
	for _, m := range reToken.FindAllStringSubmatch(stripped, -1) {
		tok := m[1]
		switch strings.ToLower(tok) {
		case "true", "false", "null":
			continue
		}
		if macros[tok] {
			continue
		}
		ok := false
		for _, p := range requestOnlyPrefixes {
			if strings.HasPrefix(tok, p) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
