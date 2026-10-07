package kernel

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
)

// blindIndexStoreKey is the app store key of the registered [BlindIndexProvider].
const blindIndexStoreKey = "__tokiBlindIndexProvider__"

// BlindIndexProvider is the seam through which a module (modules/crypto) makes
// equality comparisons on its blind-indexed fields work in filters and rules
// without the kernel knowing anything about encryption. Modules must not
// import each other, so the module registers an implementation with
// [SetBlindIndexProvider] and the field resolver consults it.
//
// See docs/modules/crypto.md ("Filters, sort, lookup").
type BlindIndexProvider interface {
	// IsBlindIndex reports whether collectionId.field supports equality
	// lookups by value.
	IsBlindIndex(collectionId, field string) bool

	// BlindIndexIDs returns the ids of the records of collection whose field
	// equals value (exact, case sensitive). When enforceVisibility is true the
	// ids of records whose field the caller (info) cannot read must be left
	// out, so that the comparison is not an equality oracle.
	BlindIndexIDs(collection *Collection, field, value string, info *RequestInfo, enforceVisibility bool) ([]string, error)
}

// SetBlindIndexProvider registers (or with nil removes) the provider of the app.
func SetBlindIndexProvider(app App, p BlindIndexProvider) {
	if p == nil {
		app.Store().Remove(blindIndexStoreKey)
		return
	}
	app.Store().Set(blindIndexStoreKey, p)
}

func blindIndexProviderOf(app App) BlindIndexProvider {
	if app == nil {
		return nil
	}
	p, _ := app.Store().Get(blindIndexStoreKey).(BlindIndexProvider)
	return p
}

var rePlaceholderOnly = regexp.MustCompile(`^\{:(\w+)\}$`)

// attachBlindIndex wires the equality rewrite of a blind-index field into
// result (the resolved plain column identifier of the active table alias).
//
// When the field is compared with `=`, `!=`, `?=` or `?!=` to a bound
// non-empty string, the identifier becomes
//
//	CASE WHEN <alias>.id IN (<matching ids>) THEN <the bound value> END
//
// so the unchanged comparison machinery (null handling, multi-match for
// relation paths, parameter binding, both the legacy and the AST emitter)
// compares equal exactly for the matching records. Any other operator or
// operand shape is left untouched.
func (r *runner) attachBlindIndex(collection *Collection, fieldName string, result *search.ResolverResult) {
	p := blindIndexProviderOf(r.resolver.app)
	if p == nil || !p.IsBlindIndex(collection.Id, fieldName) {
		return
	}

	app := r.resolver.app
	info := r.resolver.requestInfo
	// Rules are resolved with hidden fields allowed (as are superuser
	// filters); a client filter of a non superuser is not.
	enforce := !r.resolver.allowHiddenFields && info != nil
	idIdentifier := fmt.Sprintf("[[%s.id]]", r.activeTableAlias)
	mm := result.MultiMatchSubQuery
	mmIdIdentifier := ""
	if mm != nil {
		mmIdIdentifier = fmt.Sprintf("[[%s.id]]", r.multiMatchActiveTableAlias)
	}

	result.BeforeBuild = func(other *search.ResolverResult, op fexpr.SignOp) error {
		switch op {
		case fexpr.SignEq, fexpr.SignNeq, fexpr.SignAnyEq, fexpr.SignAnyNeq:
		default:
			return nil
		}

		m := rePlaceholderOnly.FindStringSubmatch(strings.TrimSpace(other.Identifier))
		if m == nil || len(other.Params) != 1 || other.MultiMatchSubQuery != nil {
			return nil
		}
		value, ok := other.Params[m[1]].(string)
		if !ok || value == "" {
			return nil
		}

		ids, err := p.BlindIndexIDs(collection, fieldName, value, info, enforce)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			ids = []string{"__no_match__"} // never a record id
		}

		prefix := "bi" + security.PseudorandomString(8)
		params := dbx.Params{}
		holders := make([]string, len(ids))
		for i, id := range ids {
			name := fmt.Sprintf("%s_%d", prefix, i)
			params[name] = id
			holders[i] = "{:" + name + "}"
		}
		in := strings.Join(holders, ",")
		build := func(idCol string) string {
			return fmt.Sprintf("(CASE WHEN %s IN (%s) THEN %s END)", idCol, in, other.Identifier)
		}

		result.Identifier = build(idIdentifier)
		if result.Params == nil {
			result.Params = dbx.Params{}
		}
		for k, v := range params {
			result.Params[k] = v
		}
		if mm != nil {
			mm.ValueIdentifier = build(mmIdIdentifier)
			if mm.Params == nil {
				mm.Params = dbx.Params{}
			}
			for k, v := range params {
				mm.Params[k] = v
			}
		}

		return nil
	}
}
